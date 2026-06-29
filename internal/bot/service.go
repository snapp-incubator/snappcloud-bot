// Package bot is the orchestration core of the SnappCloud bot. For each incoming
// Mattermost message it resolves the user's SSO identity, asks the per-region
// mcp-authz APIs which namespaces the user may access on each cluster, and — only
// if the user is authorized somewhere — forwards the query to Dify with that
// per-cluster scope, then posts the answer. An unauthorized user never reaches
// Dify.
package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/snapp-incubator/snappcloud-bot/internal/authzclient"
	"github.com/snapp-incubator/snappcloud-bot/internal/dify"
	"github.com/snapp-incubator/snappcloud-bot/internal/mattermost"
	"github.com/snapp-incubator/snappcloud-bot/internal/scopetoken"
)

type difyClient interface {
	Chat(ctx context.Context, user, query, conversationID string, inputs map[string]any) (dify.Reply, error)
}

type mmClient interface {
	GetUser(ctx context.Context, userID string) (mattermost.User, error)
	CreatePost(ctx context.Context, channelID, message, rootID string) error
	Typing(ctx context.Context, channelID, parentID string)
}

// Service ties identity, authorization, and the Dify workflow together.
type Service struct {
	mm             mmClient
	dify           difyClient
	resolver       authzclient.Resolver
	conv           *convStore
	identityMap    map[string]string
	botUsername    string
	requireMention bool
	scopeSecret    string        // HMAC secret for the mcp-gateway scope token ("" disables)
	scopeTokenTTL  time.Duration // validity of a minted token
	log            *slog.Logger
}

// Options carries the optional settings for New.
type Options struct {
	ConversationTTL time.Duration
	IdentityMap     map[string]string
	BotUsername     string
	RequireMention  bool
	ScopeSecret     string
	ScopeTokenTTL   time.Duration
}

// New builds the orchestration service.
func New(mm mmClient, d difyClient, resolver authzclient.Resolver, o Options, log *slog.Logger) *Service {
	return &Service{
		mm:             mm,
		dify:           d,
		resolver:       resolver,
		conv:           newConvStore(o.ConversationTTL),
		identityMap:    o.IdentityMap,
		botUsername:    o.BotUsername,
		requireMention: o.RequireMention,
		scopeSecret:    o.ScopeSecret,
		scopeTokenTTL:  o.ScopeTokenTTL,
		log:            log,
	}
}

// StartSweeper runs the conversation-store eviction loop until ctx is cancelled.
func (s *Service) StartSweeper(ctx context.Context) { s.conv.StartSweeper(ctx) }

// maxDifyAttempts: how many times to call Dify before giving up on an empty
// answer (1 retry). A transient upstream stream cut usually succeeds on retry.
const maxDifyAttempts = 3

const (
	msgUnauthorized = "🔒 You have no namespaces you can query on any cluster. If that's unexpected, ask your cluster admin to grant access."
	msgBackendError = "⚙️ Authorization is temporarily unavailable. Please try again in a moment."
	msgDifyError    = "⚠️ I hit an error talking to the workflow. Please try again shortly."
	msgNoAnswer     = "⌛ I couldn't get a complete answer in time (the lookup may have run long). Please try again, ideally narrowing to one cluster or namespace."
)

// OnPost handles one incoming Mattermost post.
func (s *Service) OnPost(ctx context.Context, p mattermost.Post) error {
	// Answer every direct message; in channels, only when @-mentioned.
	if !p.IsDirect() && s.requireMention && !p.Mentioned {
		return nil
	}
	query := strings.TrimSpace(s.stripMention(p.Message))
	if query == "" {
		return nil
	}

	// Show a typing indicator for the whole turn (auth + Dify can be slow).
	// parent_id must be the EXISTING thread root (p.RootID): empty for a
	// top-level message → channel-level typing (visible in the channel); set when
	// the user is already in a thread → typing shows in that thread. Using the
	// post's own id here would make it thread-typing under a not-yet-open thread,
	// which the channel view never shows.
	typingCtx, stopTyping := context.WithCancel(ctx)
	go s.mm.Typing(typingCtx, p.ChannelID, p.RootID)
	defer stopTyping()

	// 1. Resolve the authenticated SSO identity.
	user, err := s.mm.GetUser(ctx, p.UserID)
	if err != nil {
		return err
	}
	identity := s.resolveIdentity(user.Email)
	if identity == "" {
		s.replyTo(ctx, p, msgUnauthorized)
		return nil
	}

	// 2. Authorize across all regions (via the per-region mcp-authz APIs).
	scope, err := s.resolver.Resolve(ctx, identity)
	if err != nil {
		s.log.Error("authorize", "user", identity, "err", err)
		s.replyTo(ctx, p, msgBackendError)
		return nil
	}
	if scope.Empty() {
		s.log.Info("denied", "user", identity, "reason", "no allowed namespaces on any cluster")
		s.replyTo(ctx, p, msgUnauthorized)
		return nil
	}
	s.log.Info("authorized", "user", identity, "clusters", scope.Clusters())

	// 3. Forward to Dify, continuing this thread/DM's conversation (memory).
	// Retry on an empty answer (usually a transient upstream stream cut).
	inputs := map[string]any{"allowed_namespaces": formatScope(scope)}

	// Mint a FRESH scope token each message and embed it in the query. Dify
	// freezes conversation `inputs` at creation, so a per-message input never
	// reaches an ongoing conversation (and would expire). The query is sent every
	// message, so the agent always reads a current token from the latest message.
	difyQuery := query
	if s.scopeSecret != "" {
		tok, terr := scopetoken.Sign(identity, scopetoken.Scope(scope), s.scopeTokenTTL, s.scopeSecret)
		if terr != nil {
			s.log.Error("sign scope token", "user", identity, "err", terr)
		} else {
			difyQuery = query + "\n\n" + scopeOpen + tok + scopeClose
		}
	}
	convKey := convKeyFor(identity, p)
	convID := s.conv.get(convKey)
	s.log.Debug("dify request", "user", identity, "queryLen", len(query), "conversation", convID != "")

	var reply dify.Reply
	var derr error
	for attempt := 1; attempt <= maxDifyAttempts; attempt++ {
		reply, derr = s.dify.Chat(ctx, identity, difyQuery, convID, inputs)
		// Remember the conversation id whenever Dify returned one (even on a
		// retryable empty answer) so the next message continues the thread.
		if reply.ConversationID != "" {
			convID = reply.ConversationID
			s.conv.put(convKey, convID)
		}
		if derr == nil {
			break
		}
		if errors.Is(derr, dify.ErrEmptyAnswer) && attempt < maxDifyAttempts {
			s.log.Warn("dify empty answer, retrying", "user", identity, "attempt", attempt, "err", derr)
			continue
		}
		break
	}
	if derr != nil {
		s.log.Error("dify", "user", identity, "err", derr)
		// A stale/invalid conversation id (4xx) — forget it so the next try is fresh.
		if convID == "" || !errors.Is(derr, dify.ErrEmptyAnswer) {
			s.conv.drop(convKey)
		}
		if errors.Is(derr, dify.ErrEmptyAnswer) {
			s.replyTo(ctx, p, msgNoAnswer)
		} else {
			s.replyTo(ctx, p, msgDifyError)
		}
		return nil
	}
	s.replyTo(ctx, p, sanitize(reply.Answer))
	return nil
}

// Markers that wrap the per-message scope token embedded in the query. The agent
// is instructed to read the token between them and never echo it.
const (
	scopeOpen  = "<<SCOPE_TOKEN>>"
	scopeClose = "<<END_SCOPE_TOKEN>>"
)

// thinkRe matches reasoning blocks some "thinking" models wrap their
// chain-of-thought in. scopeRe matches a leaked token block (belt and braces, in
// case the model echoes it despite instructions).
var (
	thinkRe = regexp.MustCompile(`(?is)<think(?:ing)?>.*?</think(?:ing)?>`)
	scopeRe = regexp.MustCompile(`(?s)` + regexp.QuoteMeta(scopeOpen) + `.*?` + regexp.QuoteMeta(scopeClose))
)

// sanitize strips model reasoning blocks and any leaked scope-token block from
// the answer before it reaches the user.
func sanitize(s string) string {
	s = thinkRe.ReplaceAllString(s, "")
	s = scopeRe.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

// formatScope renders the per-cluster scope as a deterministic, agent-readable
// block, e.g.:
//
//	okd4-teh-1: team-a, team-b
//	okd4-ts-2: team-c
func formatScope(scope authzclient.Scope) string {
	clusters := scope.Clusters()
	lines := make([]string, 0, len(clusters))
	for _, c := range clusters {
		ns := append([]string(nil), scope[c]...)
		sort.Strings(ns)
		lines = append(lines, fmt.Sprintf("%s: %s", c, strings.Join(ns, ", ")))
	}
	return strings.Join(lines, "\n")
}

func (s *Service) stripMention(msg string) string {
	if s.botUsername == "" {
		return msg
	}
	return strings.ReplaceAll(msg, "@"+s.botUsername, "")
}

func (s *Service) resolveIdentity(email string) string {
	email = strings.TrimSpace(email)
	if mapped, ok := s.identityMap[email]; ok {
		return mapped
	}
	return email
}

// Mattermost rejects posts longer than its MaxPostSize (default 16383 chars).
// Split safely below that. maxPostParts is a flood guard only — the whole answer
// is delivered across posts; nothing is shown to the user about splitting.
const (
	maxPostRunes = 16000
	maxPostParts = 40
)

// replyTo answers a post. In channels it threads the reply under the original
// (mentioned) message; in direct messages it posts plainly. Long answers are
// split transparently into multiple posts (Mattermost caps post length); the
// user sees no truncation notice — the full answer is delivered.
func (s *Service) replyTo(ctx context.Context, p mattermost.Post, msg string) {
	root := ""
	if !p.IsDirect() {
		root = p.ThreadRoot()
	}
	parts := splitMessage(msg, maxPostRunes)
	if len(parts) > maxPostParts {
		s.log.Warn("answer exceeded post-part guard, dropping tail", "channel", p.ChannelID, "parts", len(parts))
		parts = parts[:maxPostParts]
	}
	for _, part := range parts {
		if err := s.mm.CreatePost(ctx, p.ChannelID, part, root); err != nil {
			s.log.Error("post reply", "channel", p.ChannelID, "err", err)
			return
		}
	}
}

// splitMessage breaks msg into chunks of at most max runes, preferring line
// boundaries. The whole message is returned across chunks (no truncation).
func splitMessage(msg string, max int) []string {
	if len([]rune(msg)) <= max {
		return []string{msg}
	}
	var parts []string
	var b strings.Builder
	bn := 0 // rune count in b
	flush := func() {
		if bn > 0 {
			parts = append(parts, b.String())
			b.Reset()
			bn = 0
		}
	}
	for _, line := range strings.Split(msg, "\n") {
		lr := []rune(line)
		for len(lr) > max { // a single over-long line: hard-split
			flush()
			parts = append(parts, string(lr[:max]))
			lr = lr[max:]
		}
		if bn+len(lr)+1 > max {
			flush()
		}
		if bn > 0 {
			b.WriteByte('\n')
			bn++
		}
		b.WriteString(string(lr))
		bn += len(lr)
	}
	flush()
	return parts
}

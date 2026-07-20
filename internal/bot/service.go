// Package bot is the orchestration core of the SnappCloud bot. For each incoming
// Mattermost message it resolves the user's SSO identity, asks the per-region
// mcp-authz APIs which namespaces the user may access on each cluster, and — only
// if the user is authorized somewhere — runs the in-bot agent (which drives the
// per-cluster MCP servers and enforces namespace scope), then posts the answer.
// An unauthorized user never reaches the MCP servers.
package bot

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/snapp-incubator/snappcloud-bot/internal/authzclient"
	"github.com/snapp-incubator/snappcloud-bot/internal/mattermost"
)

// answerer runs the enforced agent loop and returns the answer text. history is
// the prior thread transcript for memory.
type answerer interface {
	Answer(ctx context.Context, scope authzclient.Scope, query, history string) (string, error)
}

type mmClient interface {
	GetUser(ctx context.Context, userID string) (mattermost.User, error)
	CreatePost(ctx context.Context, channelID, message, rootID string) error
	Typing(ctx context.Context, channelID, parentID string)
}

// Service ties identity, authorization, and the agent together.
type Service struct {
	mm             mmClient
	brain          answerer
	resolver       authzclient.Resolver
	conv           *convStore
	identityMap    map[string]string
	botUsername    string
	requireMention bool
	log            *slog.Logger
}

// Options carries the optional settings for New.
type Options struct {
	ConversationTTL time.Duration
	MemoryPath      string // file to persist conversation memory across restarts ("" = in-memory)
	IdentityMap     map[string]string
	BotUsername     string
	RequireMention  bool
}

// New builds the orchestration service.
func New(mm mmClient, brain answerer, resolver authzclient.Resolver, o Options, log *slog.Logger) *Service {
	return &Service{
		mm:             mm,
		brain:          brain,
		resolver:       resolver,
		conv:           newConvStore(o.ConversationTTL, o.MemoryPath),
		identityMap:    o.IdentityMap,
		botUsername:    o.BotUsername,
		requireMention: o.RequireMention,
		log:            log,
	}
}

// StartSweeper runs the conversation-store eviction loop until ctx is cancelled.
func (s *Service) StartSweeper(ctx context.Context) { s.conv.StartSweeper(ctx) }

const (
	msgUnauthorized = "🔒 You have no namespaces you can query on any cluster. If that's unexpected, ask your cluster admin to grant access."
	msgBackendError = "⚙️ Authorization is temporarily unavailable. Please try again in a moment."
	msgAgentError   = "⚠️ I hit an error answering that. Please try again shortly."
)

// maxTranscriptRunes caps the stored per-thread memory transcript.
const maxTranscriptRunes = 6000

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

	// Show a typing indicator for the whole turn (auth + agent can be slow).
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

	// Refresh command: flush the caller's cached scope and report live access.
	// Lets a user pick up an authorization change without waiting out the cache.
	if isRefreshCommand(query) {
		if inv, ok := s.resolver.(authzclient.Invalidator); ok {
			inv.Invalidate(identity)
		}
		scope, err := s.resolver.Resolve(ctx, identity)
		if err != nil {
			s.replyTo(ctx, p, msgBackendError)
			return nil
		}
		s.log.Info("access refreshed", "user", identity, "clusters", scope.Clusters())
		s.replyTo(ctx, p, refreshSummary(scope))
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

	// 3. Run the in-bot agent over the user's authorized clusters, with this
	// thread/DM's transcript for memory. The agent drives the MCP servers and
	// enforces namespace scope on every result.
	convKey := convKeyFor(identity, p)
	history := s.conv.get(convKey)
	s.log.Debug("agent request", "user", identity, "queryLen", len(query), "hasHistory", history != "")

	answer, aerr := s.brain.Answer(ctx, scope, query, history)
	if aerr != nil {
		s.log.Error("agent", "user", identity, "err", aerr)
		s.replyTo(ctx, p, msgAgentError)
		return nil
	}
	clean := sanitize(answer)
	s.conv.put(convKey, appendTranscript(history, query, clean))
	s.replyTo(ctx, p, clean)
	return nil
}

// appendTranscript grows the per-thread memory transcript, trimmed to the last
// maxTranscriptRunes runes so it stays bounded.
func appendTranscript(history, query, answer string) string {
	t := history + "\nUser: " + query + "\nAssistant: " + answer
	t = strings.TrimSpace(t)
	r := []rune(t)
	if len(r) > maxTranscriptRunes {
		r = r[len(r)-maxTranscriptRunes:]
	}
	return string(r)
}

// thinkRe matches reasoning blocks some "thinking" models wrap their
// chain-of-thought in. Stripped so they don't reach the user. (Plain-text
// reasoning with no tags can't be removed here — that needs a non-thinking
// model.)
var thinkRe = regexp.MustCompile(`(?is)<think(?:ing)?>.*?</think(?:ing)?>`)

func sanitize(s string) string {
	return strings.TrimSpace(thinkRe.ReplaceAllString(s, ""))
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

// refreshVerbs are the message texts that trigger a scope-cache refresh.
var refreshVerbs = map[string]bool{
	"refresh": true, "reload": true, "refresh access": true,
	"reload access": true, "refresh my access": true, "sync access": true,
}

// isRefreshCommand reports whether the message is a request to re-check access.
func isRefreshCommand(query string) bool {
	return refreshVerbs[strings.ToLower(strings.TrimSpace(query))]
}

// refreshSummary renders the caller's live access after a cache flush.
func refreshSummary(scope authzclient.Scope) string {
	if scope.Empty() {
		return "🔄 Access refreshed. You currently have no namespaces on any cluster."
	}
	var sb strings.Builder
	sb.WriteString("🔄 Access refreshed. You can now query:\n")
	for _, c := range scope.Clusters() {
		cs := scope[c]
		ns := append([]string(nil), cs.Namespaces...)
		sort.Strings(ns)
		admin := ""
		if cs.ClusterWide {
			admin = " _(cluster-admin)_"
		}
		fmt.Fprintf(&sb, "• **%s**%s: %s\n", c, admin, strings.Join(ns, ", "))
	}
	return sb.String()
}

// Package bot is the orchestration core of the SnappCloud bot. For each incoming
// Mattermost message it resolves the user's SSO identity, asks the per-region
// mcp-authz APIs which namespaces the user may access on each cluster, and — only
// if the user is authorized somewhere — runs the in-bot agent (which drives the
// per-cluster MCP servers and enforces namespace scope), then posts the answer.
// An unauthorized user never reaches the MCP servers.
package bot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/snapp-incubator/snappcloud-bot/internal/authzclient"
	"github.com/snapp-incubator/snappcloud-bot/internal/mattermost"
	"github.com/snapp-incubator/snappcloud-bot/internal/metrics"
	"github.com/snapp-incubator/snappcloud-bot/internal/schedule"
)

// newReqID returns a short random hex id correlating one message's log lines.
func newReqID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req"
	}
	return hex.EncodeToString(b[:])
}

// answerer runs the enforced agent loop and returns the answer text. history is
// the prior thread transcript for memory.
type answerer interface {
	Answer(ctx context.Context, scope authzclient.Scope, user, query, history, reqID string) (string, error)
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
	limiter        *RateLimiter
	sched          *schedule.Store
	maxQueryRunes  int
	log            *slog.Logger
}

// Options carries the optional settings for New.
type Options struct {
	ConversationTTL time.Duration
	MemoryPath      string // file to persist conversation memory across restarts ("" = in-memory)
	IdentityMap     map[string]string
	BotUsername     string
	RequireMention  bool
	// RatePerMin limits messages per minute per user (0 = unlimited). RateBurst
	// is the allowed short burst (defaults to RatePerMin).
	RatePerMin int
	RateBurst  int
	// MaxQueryRunes rejects overly long messages (0 = a sane default).
	MaxQueryRunes int
	// Limiter, when set, is shared with the HTTP API so one identity has a
	// single budget across both entrypoints. Nil builds one from RatePerMin.
	Limiter *RateLimiter
	// Schedules enables user-defined recurring queries. Nil disables the feature.
	Schedules *schedule.Store
}

// New builds the orchestration service.
func New(mm mmClient, brain answerer, resolver authzclient.Resolver, o Options, log *slog.Logger) *Service {
	maxQ := o.MaxQueryRunes
	if maxQ <= 0 {
		maxQ = 4000
	}
	limiter := o.Limiter
	if limiter == nil {
		limiter = NewRateLimiter(o.RatePerMin, o.RateBurst)
	}
	return &Service{
		mm:             mm,
		brain:          brain,
		resolver:       resolver,
		conv:           newConvStore(o.ConversationTTL, o.MemoryPath),
		identityMap:    o.IdentityMap,
		botUsername:    o.BotUsername,
		requireMention: o.RequireMention,
		limiter:        limiter,
		sched:          o.Schedules,
		maxQueryRunes:  maxQ,
		log:            log,
	}
}

// StartSweeper runs the conversation-store eviction loop until ctx is cancelled.
func (s *Service) StartSweeper(ctx context.Context) {
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.limiter.Sweep()
			}
		}
	}()
	s.conv.StartSweeper(ctx)
}

const (
	msgUnauthorized = "🔒 You have no namespaces you can query on any cluster. If that's unexpected, ask your cluster admin to grant access."
	msgBackendError = "⚙️ Authorization is temporarily unavailable. Please try again in a moment."
	msgAgentError   = "⚠️ I hit an error answering that. Please try again shortly."
	msgRateLimited  = "🐢 You're sending requests too fast. Give me a few seconds and try again."
	msgEmptyAnswer  = "🤔 I couldn't put together an answer for that. Try rephrasing, or narrow it to a specific namespace/cluster."
	msgTooLong      = "✂️ That message is too long for me to process. Please shorten it and ask again."
)

// maxTranscriptRunes caps the stored per-thread memory transcript.
const maxTranscriptRunes = 6000

// OnPost handles one incoming Mattermost post.
func (s *Service) OnPost(ctx context.Context, p mattermost.Post) error {
	// Answer every direct message; in channels, only when @-mentioned.
	//
	// The mention is detected two ways because the event's mention list is a
	// NOTIFICATION artefact: Mattermost does not populate it for posts made by
	// other bots and integrations, so a bot that @-mentions us would otherwise
	// be dropped here without a trace. Matching the text is independent of that.
	if !p.IsDirect() && s.requireMention && !p.Mentioned && !s.textMentionsBot(p.Message) {
		metrics.Messages.WithLabelValues("ignored").Inc()
		s.log.Debug("ignored channel message: bot not mentioned",
			"channel", p.ChannelID, "user", p.UserID, "post", p.ID)
		return nil
	}
	query := strings.TrimSpace(s.stripMention(p.Message))
	if query == "" {
		return nil
	}
	if len([]rune(query)) > s.maxQueryRunes {
		s.replyTo(ctx, p, msgTooLong)
		metrics.Messages.WithLabelValues("too_long").Inc()
		return nil
	}
	// reqID correlates every log line for this message (identity, auth, agent).
	reqID := newReqID()
	lg := s.log.With("req", reqID)

	// Metrics: record terminal outcome + latency for every message we act on.
	outcome := "answered"
	start := time.Now()
	metrics.InFlight.Inc()
	defer func() {
		metrics.InFlight.Dec()
		metrics.Messages.WithLabelValues(outcome).Inc()
		metrics.MessageDuration.Observe(time.Since(start).Seconds())
	}()

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
		// Bot and integration accounts often carry no email, so there is no SSO
		// identity to authorize. Log it: otherwise this looks like the bot
		// silently ignoring a mention.
		outcome = "unauthorized"
		lg.Info("no identity for sender", "post", p.ID, "channel", p.ChannelID,
			"user", p.UserID, "reason", "account has no email; map it via mattermost.identityMap")
		s.replyTo(ctx, p, msgUnauthorized)
		return nil
	}

	// Per-user rate limit: protects the bot, LLM budget, and downstream MCP
	// servers from one client flooding requests.
	if !s.limiter.Allow(identity) {
		outcome = "rate_limited"
		s.replyTo(ctx, p, msgRateLimited)
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
			outcome = "backend_error"
			s.replyTo(ctx, p, msgBackendError)
			return nil
		}
		outcome = "refreshed"
		lg.Info("access refreshed", "user", identity, "clusters", scope.Clusters())
		s.replyTo(ctx, p, refreshSummary(scope))
		return nil
	}

	// Schedule commands are handled before the agent: they manage saved queries
	// rather than asking one.
	if s.sched != nil {
		if handled, reply := s.scheduleCommand(identity, p, query); handled {
			outcome = "schedule_command"
			s.replyTo(ctx, p, reply)
			return nil
		}
	}

	// 2. Authorize across all regions (via the per-region mcp-authz APIs).
	scope, err := s.resolver.Resolve(ctx, identity)
	if err != nil {
		outcome = "backend_error"
		lg.Error("authorize failed", "user", identity, "err", err)
		s.replyTo(ctx, p, msgBackendError)
		return nil
	}
	if scope.Empty() {
		outcome = "denied"
		lg.Info("denied", "user", identity, "reason", "no namespaces on any cluster")
		s.replyTo(ctx, p, msgUnauthorized)
		return nil
	}

	// 3. Run the in-bot agent over the user's authorized clusters, with this
	// thread/DM's transcript for memory. The agent drives the MCP servers and
	// enforces namespace scope on every result.
	convKey := convKeyFor(identity, p)
	history := s.conv.get(convKey)
	lg.Info("request", "user", identity, "clusters", scope.Clusters(), "thread", history != "")

	answer, aerr := s.brain.Answer(ctx, scope, identity, query, history, reqID)
	if aerr != nil {
		outcome = "agent_error"
		lg.Error("agent failed", "user", identity, "err", aerr)
		s.replyTo(ctx, p, msgAgentError)
		return nil
	}
	clean := sanitize(answer)
	if clean == "" {
		// Model produced no usable text (empty, or all reasoning/markup that
		// sanitize stripped). Never post a blank message; give a clear fallback.
		outcome = "empty_answer"
		lg.Warn("empty answer after sanitize", "user", identity, "rawLen", len(answer))
		s.replyTo(ctx, p, msgEmptyAnswer)
		return nil
	}
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
var (
	thinkRe = regexp.MustCompile(`(?is)<think(?:ing)?>.*?</think(?:ing)?>`)
	// Some models leak tool-call / function-call markup or special control
	// tokens into the visible text; strip them so the user never sees them.
	toolCallRe = regexp.MustCompile(`(?is)<(tool_call|function_call|tool_use|antml:[^>]+)>.*?</(tool_call|function_call|tool_use|antml:[^>]+)>`)
	ctrlTokRe  = regexp.MustCompile(`<\|[^|>]*\|>`)
	// A whole answer wrapped in a single fenced block (```...```), which renders
	// as a code box instead of readable text.
	wholeFenceRe = regexp.MustCompile("(?s)^```[a-zA-Z0-9_-]*\n(.*)\n```$")
)

func sanitize(s string) string {
	s = thinkRe.ReplaceAllString(s, "")
	s = toolCallRe.ReplaceAllString(s, "")
	s = ctrlTokRe.ReplaceAllString(s, "")
	s = strings.TrimSpace(s)
	// Unwrap an answer that is entirely one code fence (not real code) so it's
	// human-readable markdown, not a monospace block.
	if m := wholeFenceRe.FindStringSubmatch(s); m != nil {
		inner := strings.TrimSpace(m[1])
		// Only unwrap when the content clearly isn't code (has prose sentences).
		if !looksLikeCode(inner) {
			s = inner
		}
	}
	return strings.TrimSpace(s)
}

// codeSignal matches tokens that indicate the fenced content is real code /
// commands / logs (so its fence must be kept), not prose the model wrongly
// wrapped. Conservative: unwrap ONLY when none of these appear.
var codeSignal = regexp.MustCompile(`(?m)(^\s*[-$#>]|[;{}$|=]|--[a-z]|\b(oc|kubectl|cilium|curl|helm|sudo|apiVersion|kind)\b)`)

// looksLikeCode reports whether fenced content should keep its code fence.
func looksLikeCode(s string) bool {
	if strings.Contains(s, "```") {
		return true // nested fences → leave as-is
	}
	return codeSignal.MatchString(s)
}

// textMentionsBot reports whether the message text @-mentions this bot. Used as
// a fallback for senders whose posts carry no mention metadata (other bots,
// webhooks, integrations). Case-insensitive, and the mention must be followed by
// a word boundary so "@cloud-bot-staging" does not match "@cloud-bot".
func (s *Service) textMentionsBot(msg string) bool {
	if s.botUsername == "" {
		return false
	}
	lower := strings.ToLower(msg)
	at := "@" + strings.ToLower(s.botUsername)
	for i := 0; ; {
		idx := strings.Index(lower[i:], at)
		if idx < 0 {
			return false
		}
		end := i + idx + len(at)
		if end == len(lower) || !isMentionChar(lower[end]) {
			return true
		}
		i = end
	}
}

// isMentionChar reports whether b can be part of a Mattermost username, which
// may contain letters, digits, and . - _ (so a longer username is not matched).
func isMentionChar(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= '0' && b <= '9':
		return true
	case b == '.' || b == '-' || b == '_':
		return true
	}
	return false
}

func (s *Service) stripMention(msg string) string {
	if s.botUsername == "" {
		return msg
	}
	// Case-insensitive: other clients may not preserve the exact casing.
	re := regexp.MustCompile(`(?i)@` + regexp.QuoteMeta(s.botUsername))
	return re.ReplaceAllString(msg, "")
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
		if strings.TrimSpace(part) == "" {
			continue
		}
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

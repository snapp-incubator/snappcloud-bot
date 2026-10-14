// Package bot handles Mattermost messages: resolve identity and scope, run the
// agent if authorized, post the answer.
package bot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"github.com/snapp-incubator/snappcloud-bot/internal/answer"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/snapp-incubator/snappcloud-bot/internal/alerts"
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
	// ResolveCluster maps a cluster name as alerts or people write it to the
	// configured cluster, if any is recognised.
	ResolveCluster(label string) (string, bool)
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
	alertChannels  *alerts.Channels
	alertAgg       *alerts.Aggregator
	maxQueryRunes  int
	// trigger starts a manual run of a stored schedule on the scheduler's own
	// worker pool. Nil when schedules are disabled.
	trigger func(schedule.Entry) error
	log     *slog.Logger
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
	// AlertChannels and AlertAggregator enable investigating alerts posted in
	// marked channels. Both nil disables the feature.
	AlertChannels   *alerts.Channels
	AlertAggregator *alerts.Aggregator
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
		alertChannels:  o.AlertChannels,
		alertAgg:       o.AlertAggregator,
		maxQueryRunes:  maxQ,
		log:            log,
	}
}

// SetScheduleTrigger supplies the function that starts a manual run of a stored
// schedule. It is set after construction because the scheduler needs the service
// (to answer) and the service needs the scheduler (to run on its worker pool).
// Until it is set, "run <id>" says schedules are not running rather than
// starting work outside the pool.
func (s *Service) SetScheduleTrigger(f func(schedule.Entry) error) { s.trigger = f }

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
	msgTooLongFmt   = "✂️ That message is %d characters over the %d I can take. Shorten it, or split it into two questions."
)

// maxTranscriptRunes caps the stored per-thread memory transcript.
const maxTranscriptRunes = 6000

// OnPost handles one incoming Mattermost post.
func (s *Service) OnPost(ctx context.Context, p mattermost.Post) error {
	// An unaddressed post from an integration in an alert channel is an alert.
	if !p.IsDirect() && !p.Mentioned && !s.textMentionsBot(p.Message) && s.isAlertChannel(p.ChannelID) {
		if s.postIsFromAlertSource(ctx, p) {
			if s.ingestAlert(p) {
				metrics.Messages.WithLabelValues("alert").Inc()
				return nil
			}
		}
	}

	// Answer DMs; in channels only when mentioned. Text is checked too, since
	// Mattermost omits mentions for posts made by bots.
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
		s.replyTo(ctx, p, fmt.Sprintf(msgTooLongFmt, len([]rune(query))-s.maxQueryRunes, s.maxQueryRunes))
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

	// Typing indicator for the whole turn, in the existing thread if any.
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
	// Commands are answered before the agent: they manage the bot rather than
	// ask it anything, and none of them needs cluster access.
	if handled, cmdOutcome := s.runCommand(ctx, p, identity, query, lg); handled {
		outcome = cmdOutcome
		return nil
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

// sanitize cleans a model answer for posting.
func sanitize(s string) string { return answer.Clean(s) }

// textMentionsBot reports whether the message text @-mentions this bot. Used as
// a fallback for senders whose posts carry no mention metadata (other bots,
// webhooks, integrations). Case-insensitive, and the mention must be followed by
// a word boundary so "@cloud-bot-staging" does not match "@cloud-bot".
// isAlertChannel reports whether the channel is marked for alert investigation.
func (s *Service) isAlertChannel(channelID string) bool {
	if s.alertChannels == nil {
		return false
	}
	_, ok := s.alertChannels.Get(channelID)
	return ok
}

// postIsFromAlertSource: the post's props decide it (webhooks post under their
// creator's account); the author is only a fallback.
func (s *Service) postIsFromAlertSource(ctx context.Context, p mattermost.Post) bool {
	if p.FromIntegration() {
		return true
	}
	user, err := s.mm.GetUser(ctx, p.UserID)
	if err != nil {
		// Some integrations post under an id the API will not resolve. A lookup
		// failure means there is no identity here, which is the same conclusion.
		s.log.Debug("alert sender lookup failed; treating as an integration",
			"channel", p.ChannelID, "user", p.UserID, "err", err)
		return true
	}
	return s.resolveIdentity(user.Email) == ""
}

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

// runCommand answers the bot's own commands, which need no cluster
// authorization. handled is false for an ordinary question.
func (s *Service) runCommand(ctx context.Context, p mattermost.Post, identity, query string, lg *slog.Logger) (handled bool, outcome string) {
	if isRefreshCommand(query) {
		if inv, ok := s.resolver.(authzclient.Invalidator); ok {
			inv.Invalidate(identity)
		}
		scope, err := s.resolver.Resolve(ctx, identity)
		if err != nil {
			s.replyTo(ctx, p, msgBackendError)
			return true, "backend_error"
		}
		lg.Info("access refreshed", "user", identity, "clusters", scope.Clusters())
		s.replyTo(ctx, p, refreshSummary(scope))
		return true, "refreshed"
	}

	// help is answered without authorization: a user with no access still needs
	// to know how to ask, and it costs nothing to produce.
	if helpVerbs[normalizeCommand(query)] {
		s.replyTo(ctx, p, helpText(s.sched != nil, s.alertChannels != nil))
		return true, "help"
	}

	if handled, reply := s.scheduleCommand(identity, p, query); handled {
		s.replyTo(ctx, p, reply)
		return true, "schedule_command"
	}

	if handled, reply := s.alertCommand(identity, p, query); handled {
		s.replyTo(ctx, p, reply)
		return true, "alert_command"
	}

	if handled, reply := s.diagnosticsCommand(ctx, identity, p, query); handled {
		s.replyTo(ctx, p, reply)
		return true, "diagnostics"
	}
	return false, ""
}

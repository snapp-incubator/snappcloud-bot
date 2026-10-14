// Package api serves the agent over HTTP. Callers present their own OpenShift
// token, verified via mcp-authz TokenReview, and are scoped exactly like chat users.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"github.com/snapp-incubator/snappcloud-bot/internal/answer"
	"github.com/snapp-incubator/snappcloud-bot/internal/brain"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/snapp-incubator/snappcloud-bot/internal/authzclient"
	"github.com/snapp-incubator/snappcloud-bot/internal/metrics"
)

// answerer runs the enforced agent loop (implemented by brain.Brain).
type answerer interface {
	Answer(ctx context.Context, scope authzclient.Scope, user, query, history, reqID string) (string, error)
}

// authenticator turns a caller-supplied token into an identity.
type authenticator interface {
	Authenticate(ctx context.Context, token string) (authzclient.Identity, error)
	ResolveWithGroups(ctx context.Context, user string, groups []string) (authzclient.Scope, error)
}

// limiter throttles callers (per identity).
type limiter interface{ Allow(identity string) bool }

// Handler serves the query API.
type Handler struct {
	brain         answerer
	authn         authenticator
	limit         limiter
	maxQueryRunes int
	timeout       time.Duration
	log           *slog.Logger
}

// Options configures the handler.
type Options struct {
	// MaxQueryRunes rejects overly long queries (default 4000).
	MaxQueryRunes int
	// Timeout bounds one query end to end (default 5m).
	Timeout time.Duration
}

// New builds the API handler. limit may be nil (no rate limiting).
func New(brain answerer, authn authenticator, limit limiter, o Options, log *slog.Logger) *Handler {
	if o.MaxQueryRunes <= 0 {
		o.MaxQueryRunes = 4000
	}
	if o.Timeout <= 0 {
		o.Timeout = 5 * time.Minute
	}
	return &Handler{
		brain: brain, authn: authn, limit: limit,
		maxQueryRunes: o.MaxQueryRunes, timeout: o.Timeout, log: log,
	}
}

// Routes registers the endpoints on a mux.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/query", h.query)
	mux.HandleFunc("GET /v1/whoami", h.whoami)
	mux.HandleFunc("POST /mcp", h.mcp)
	mux.HandleFunc("/mcp", mcpMethodNotAllowed)
}

type queryRequest struct {
	Query string `json:"query"`
	// History is prior context for follow-ups; the bot keeps none for API callers.
	History string `json:"history,omitempty"`
	// Review re-checks the answer against its evidence before returning (slower).
	Review bool `json:"review,omitempty"`
}

type queryResponse struct {
	Answer string `json:"answer"`
	User   string `json:"user"`
	ReqID  string `json:"requestId"`
}

type whoamiResponse struct {
	User     string              `json:"user"`
	Region   string              `json:"authenticatedBy"`
	Clusters map[string][]string `json:"clusters"`
}

// bearer extracts the caller's token. It is never logged.
func bearer(r *http.Request) string {
	v := r.Header.Get("Authorization")
	if len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
		return strings.TrimSpace(v[7:])
	}
	return ""
}

// authenticate verifies the caller's token and resolves their scope. On
// failure it returns an HTTP status and a message for the caller.
func (h *Handler) authenticate(ctx context.Context, token string, lg *slog.Logger) (authzclient.Identity, authzclient.Scope, int, string) {
	if token == "" {
		return authzclient.Identity{}, nil, http.StatusUnauthorized,
			"missing bearer token: send your OpenShift user or ServiceAccount token"
	}
	id, err := h.authn.Authenticate(ctx, token)
	if err != nil {
		lg.Info("api authentication failed", "err", err)
		return authzclient.Identity{}, nil, http.StatusUnauthorized, "token could not be authenticated on any known cluster"
	}
	scope, err := h.authn.ResolveWithGroups(ctx, id.User, id.Groups)
	if err != nil {
		lg.Error("api authorize failed", "user", id.User, "err", err)
		return authzclient.Identity{}, nil, http.StatusServiceUnavailable, "authorization is temporarily unavailable"
	}
	if scope.Empty() {
		lg.Info("api denied", "user", id.User, "reason", "no namespaces on any cluster")
		return authzclient.Identity{}, nil, http.StatusForbidden, "you have no namespaces you can query on any cluster"
	}
	return id, scope, 0, ""
}

// whoami reports the caller's identity and access without running the agent —
// the cheap way to verify a token and see the scope it grants.
func (h *Handler) whoami(w http.ResponseWriter, r *http.Request) {
	reqID := newReqID()
	lg := h.log.With("req", reqID)
	id, scope, status, msg := h.authenticate(r.Context(), bearer(r), lg)
	if status != 0 {
		writeErr(w, status, msg, reqID)
		return
	}
	writeJSON(w, http.StatusOK, whoamiResponse{
		User: id.User, Region: id.Region, Clusters: clusterMap(scope),
	})
}

// query is the REST form of ask.
func (h *Handler) query(w http.ResponseWriter, r *http.Request) {
	reqID := newReqID()
	var req queryRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		metrics.APIRequests.WithLabelValues("bad_request").Inc()
		writeErr(w, http.StatusBadRequest, "invalid JSON body", reqID)
		return
	}
	res := h.ask(r.Context(), bearer(r), req, reqID)
	if res.status != 0 {
		writeErr(w, res.status, res.err, reqID)
		return
	}
	writeJSON(w, http.StatusOK, queryResponse{Answer: res.answer, User: res.user, ReqID: reqID})
}

type askResult struct {
	answer, user, err string
	status            int // HTTP status of a failure; 0 on success
}

// ask authenticates, validates, rate-limits and runs one enforced agent turn.
// Shared by REST and MCP so both get identical scoping, limits and cleaning.
func (h *Handler) ask(ctx context.Context, token string, req queryRequest, reqID string) askResult {
	lg := h.log.With("req", reqID, "src", "api")
	start := time.Now()
	outcome := "answered"
	metrics.InFlight.Inc()
	defer func() {
		metrics.InFlight.Dec()
		metrics.APIRequests.WithLabelValues(outcome).Inc()
		metrics.MessageDuration.Observe(time.Since(start).Seconds())
	}()
	fail := func(o string, status int, msg string) askResult {
		outcome = o
		return askResult{status: status, err: msg}
	}

	query := strings.TrimSpace(req.Query)
	if query == "" {
		return fail("bad_request", http.StatusBadRequest, "query is required")
	}
	if len([]rune(query)) > h.maxQueryRunes {
		return fail("too_long", http.StatusRequestEntityTooLarge, "query is too long")
	}
	id, scope, status, msg := h.authenticate(ctx, token, lg)
	if status != 0 {
		return fail("unauthorized", status, msg)
	}
	if h.limit != nil && !h.limit.Allow(id.User) {
		return fail("rate_limited", http.StatusTooManyRequests, "too many requests; slow down")
	}

	lg.Info("api request", "user", id.User, "clusters", scope.Clusters(), "review", req.Review)
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	if req.Review {
		ctx = brain.WithReview(ctx)
	}
	out, err := h.brain.Answer(ctx, scope, id.User, query, req.History, reqID)
	if err != nil {
		lg.Error("api agent failed", "user", id.User, "err", err)
		return fail("agent_error", http.StatusBadGateway, "failed to answer the query")
	}
	return askResult{answer: answer.Clean(out), user: id.User}
}

func clusterMap(s authzclient.Scope) map[string][]string {
	out := make(map[string][]string, len(s))
	for _, c := range s.Clusters() {
		out[c] = s[c].Namespaces
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg, reqID string) {
	writeJSON(w, status, map[string]string{"error": msg, "requestId": reqID})
}

func newReqID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "api"
	}
	return hex.EncodeToString(b[:])
}

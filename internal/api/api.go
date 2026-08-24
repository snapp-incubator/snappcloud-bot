// Package api is the bot's HTTP query interface: the same enforced agent loop
// Mattermost uses, reachable programmatically.
//
// The caller presents their OWN OpenShift token (a user token or a
// ServiceAccount token) as a bearer. The bot holds no cluster credentials, so
// the token is verified by mcp-authz via TokenReview; the identity that comes
// back is scoped exactly like a Mattermost user's, and every tool result is
// filtered against it. A caller can therefore never see more through this API
// than they can see with `oc`.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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
}

type queryRequest struct {
	Query string `json:"query"`
	// History is optional prior context for multi-turn use. The caller owns its
	// own conversation state; the bot keeps none for API callers.
	History string `json:"history,omitempty"`
}

type queryResponse struct {
	Answer   string              `json:"answer"`
	User     string              `json:"user"`
	Clusters map[string][]string `json:"clusters"`
	ReqID    string              `json:"requestId"`
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

// identify authenticates the caller and resolves their scope. Returns false
// when a response has already been written.
func (h *Handler) identify(w http.ResponseWriter, r *http.Request, lg *slog.Logger) (authzclient.Identity, authzclient.Scope, bool) {
	token := bearer(r)
	if token == "" {
		writeErr(w, http.StatusUnauthorized, "missing bearer token: send your OpenShift user or ServiceAccount token")
		return authzclient.Identity{}, nil, false
	}
	id, err := h.authn.Authenticate(r.Context(), token)
	if err != nil {
		lg.Info("api authentication failed", "err", err)
		writeErr(w, http.StatusUnauthorized, "token could not be authenticated on any known cluster")
		return authzclient.Identity{}, nil, false
	}
	scope, err := h.authn.ResolveWithGroups(r.Context(), id.User, id.Groups)
	if err != nil {
		lg.Error("api authorize failed", "user", id.User, "err", err)
		writeErr(w, http.StatusServiceUnavailable, "authorization is temporarily unavailable")
		return authzclient.Identity{}, nil, false
	}
	if scope.Empty() {
		lg.Info("api denied", "user", id.User, "reason", "no namespaces on any cluster")
		writeErr(w, http.StatusForbidden, "you have no namespaces you can query on any cluster")
		return authzclient.Identity{}, nil, false
	}
	return id, scope, true
}

// whoami reports the caller's identity and access without running the agent —
// the cheap way to verify a token and see the scope it grants.
func (h *Handler) whoami(w http.ResponseWriter, r *http.Request) {
	lg := h.log.With("req", newReqID())
	id, scope, ok := h.identify(w, r, lg)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, whoamiResponse{
		User: id.User, Region: id.Region, Clusters: clusterMap(scope),
	})
}

// query runs one enforced agent turn for the authenticated caller.
func (h *Handler) query(w http.ResponseWriter, r *http.Request) {
	reqID := newReqID()
	lg := h.log.With("req", reqID, "src", "api")
	start := time.Now()
	outcome := "answered"
	metrics.InFlight.Inc()
	defer func() {
		metrics.InFlight.Dec()
		metrics.APIRequests.WithLabelValues(outcome).Inc()
		metrics.MessageDuration.Observe(time.Since(start).Seconds())
	}()

	var req queryRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		outcome = "bad_request"
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	query := strings.TrimSpace(req.Query)
	if query == "" {
		outcome = "bad_request"
		writeErr(w, http.StatusBadRequest, "query is required")
		return
	}
	if len([]rune(query)) > h.maxQueryRunes {
		outcome = "too_long"
		writeErr(w, http.StatusRequestEntityTooLarge, "query is too long")
		return
	}

	id, scope, ok := h.identify(w, r, lg)
	if !ok {
		outcome = "unauthorized"
		return
	}
	if h.limit != nil && !h.limit.Allow(id.User) {
		outcome = "rate_limited"
		writeErr(w, http.StatusTooManyRequests, "too many requests; slow down")
		return
	}

	lg.Info("api request", "user", id.User, "clusters", scope.Clusters())
	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	defer cancel()

	answer, err := h.brain.Answer(ctx, scope, id.User, query, req.History, reqID)
	if err != nil {
		outcome = "agent_error"
		lg.Error("api agent failed", "user", id.User, "err", err)
		writeErr(w, http.StatusBadGateway, "failed to answer the query")
		return
	}
	writeJSON(w, http.StatusOK, queryResponse{
		Answer: answer, User: id.User, Clusters: clusterMap(scope), ReqID: reqID,
	})
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

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func newReqID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "api"
	}
	return hex.EncodeToString(b[:])
}

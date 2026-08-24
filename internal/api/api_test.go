package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/snapp-incubator/snappcloud-bot/internal/authzclient"
)

// authnStub stands in for the mcp-authz client.
type authnStub struct {
	id      authzclient.Identity
	authErr error
	scope   authzclient.Scope
	scErr   error
	gotUser string
	gotGrps []string
}

func (a *authnStub) Authenticate(context.Context, string) (authzclient.Identity, error) {
	if a.authErr != nil {
		return authzclient.Identity{}, a.authErr
	}
	return a.id, nil
}

func (a *authnStub) ResolveWithGroups(_ context.Context, user string, groups []string) (authzclient.Scope, error) {
	a.gotUser, a.gotGrps = user, groups
	return a.scope, a.scErr
}

type fakeBrain struct {
	answer   string
	err      error
	gotUser  string
	gotScope authzclient.Scope
	calls    int
}

func (f *fakeBrain) Answer(_ context.Context, scope authzclient.Scope, user, _, _, _ string) (string, error) {
	f.calls++
	f.gotUser, f.gotScope = user, scope
	return f.answer, f.err
}

// denyLimiter always refuses.
type denyLimiter struct{}

func (denyLimiter) Allow(string) bool { return false }

func newSrv(t *testing.T, a *authnStub, b *fakeBrain, l limiter) *httptest.Server {
	t.Helper()
	h := New(b, a, l, Options{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux := http.NewServeMux()
	h.Routes(mux)
	return httptest.NewServer(mux)
}

func post(t *testing.T, srv *httptest.Server, path, token, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestQueryRequiresToken(t *testing.T) {
	a := &authnStub{}
	b := &fakeBrain{answer: "hi"}
	srv := newSrv(t, a, b, nil)
	defer srv.Close()

	code, _ := post(t, srv, "/v1/query", "", `{"query":"hello"}`)
	if code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", code)
	}
	if b.calls != 0 {
		t.Fatal("agent ran without a token")
	}
}

func TestQueryRejectsBadToken(t *testing.T) {
	a := &authnStub{authErr: errors.New("not authenticated")}
	b := &fakeBrain{answer: "hi"}
	srv := newSrv(t, a, b, nil)
	defer srv.Close()

	code, _ := post(t, srv, "/v1/query", "bogus", `{"query":"hello"}`)
	if code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", code)
	}
	if b.calls != 0 {
		t.Fatal("agent ran with an unauthenticated token")
	}
}

// A ServiceAccount token authenticates like any other identity and its scope is
// resolved WITH its groups (that is where SA access lives).
func TestQueryServiceAccountIdentityAndGroups(t *testing.T) {
	a := &authnStub{
		id: authzclient.Identity{
			User:   "system:serviceaccount:team-a:ci",
			Groups: []string{"system:serviceaccounts", "system:serviceaccounts:team-a"},
			Region: "okd4-teh-1",
		},
		scope: authzclient.Scope{"okd4-teh-1": {Namespaces: []string{"team-a"}}},
	}
	b := &fakeBrain{answer: "3 pods"}
	srv := newSrv(t, a, b, nil)
	defer srv.Close()

	code, out := post(t, srv, "/v1/query", "sa-token", `{"query":"list pods"}`)
	if code != http.StatusOK {
		t.Fatalf("status %d: %v", code, out)
	}
	if out["answer"] != "3 pods" || out["user"] != "system:serviceaccount:team-a:ci" {
		t.Fatalf("unexpected response: %v", out)
	}
	if b.gotUser != "system:serviceaccount:team-a:ci" {
		t.Fatalf("agent got user %q", b.gotUser)
	}
	if len(a.gotGrps) != 2 {
		t.Fatalf("scope must be resolved with the SA groups, got %v", a.gotGrps)
	}
}

func TestQueryDeniedWhenNoNamespaces(t *testing.T) {
	a := &authnStub{
		id:    authzclient.Identity{User: "u@x", Region: "r"},
		scope: authzclient.Scope{}, // empty
	}
	b := &fakeBrain{}
	srv := newSrv(t, a, b, nil)
	defer srv.Close()

	code, _ := post(t, srv, "/v1/query", "tok", `{"query":"hi"}`)
	if code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", code)
	}
	if b.calls != 0 {
		t.Fatal("agent ran for a user with no namespaces")
	}
}

func TestQueryRateLimited(t *testing.T) {
	a := &authnStub{
		id:    authzclient.Identity{User: "u@x", Region: "r"},
		scope: authzclient.Scope{"c": {Namespaces: []string{"ns"}}},
	}
	b := &fakeBrain{answer: "ok"}
	srv := newSrv(t, a, b, denyLimiter{})
	defer srv.Close()

	code, _ := post(t, srv, "/v1/query", "tok", `{"query":"hi"}`)
	if code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429", code)
	}
	if b.calls != 0 {
		t.Fatal("agent ran despite the rate limit")
	}
}

func TestQueryValidatesBody(t *testing.T) {
	a := &authnStub{
		id:    authzclient.Identity{User: "u@x", Region: "r"},
		scope: authzclient.Scope{"c": {Namespaces: []string{"ns"}}},
	}
	srv := newSrv(t, a, &fakeBrain{}, nil)
	defer srv.Close()

	if code, _ := post(t, srv, "/v1/query", "tok", `not json`); code != http.StatusBadRequest {
		t.Fatalf("bad JSON: status %d, want 400", code)
	}
	if code, _ := post(t, srv, "/v1/query", "tok", `{"query":"  "}`); code != http.StatusBadRequest {
		t.Fatalf("empty query: status %d, want 400", code)
	}
	long := `{"query":"` + strings.Repeat("x", 5000) + `"}`
	if code, _ := post(t, srv, "/v1/query", "tok", long); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("long query: status %d, want 413", code)
	}
}

func TestWhoamiReturnsScope(t *testing.T) {
	a := &authnStub{
		id:    authzclient.Identity{User: "u@x", Region: "okd4-teh-2"},
		scope: authzclient.Scope{"okd4-teh-2": {Namespaces: []string{"team-a", "team-b"}}},
	}
	srv := newSrv(t, a, &fakeBrain{}, nil)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/whoami", nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out whoamiResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.User != "u@x" || out.Region != "okd4-teh-2" || len(out.Clusters["okd4-teh-2"]) != 2 {
		t.Fatalf("unexpected whoami: %+v", out)
	}
}

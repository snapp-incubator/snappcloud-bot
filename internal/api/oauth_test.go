package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/snapp-incubator/snappcloud-bot/internal/authzclient"
	"github.com/snapp-incubator/snappcloud-bot/internal/botoauth"
)

func TestOAuthMCPRoutesUseDelegatedScope(t *testing.T) {
	const resource = "https://bot.example/oauth/mcp"
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"active": r.PostFormValue("token") == "user-grant", "iss": "https://issuer.example", "aud": resource, "token_type": "Bearer", "exp": time.Now().Add(time.Hour).Unix(), "scope": "mcp:tools", "sub": "u123", "email": "a@example.test", "email_verified": true})
	}))
	defer issuer.Close()
	a := &authnStub{authErr: errors.New("not an OpenShift token"), scope: authzclient.Scope{"test": {Namespaces: []string{"team-a"}}}}
	brain := &fakeBrain{answer: "answer"}
	h := New(brain, a, nil, Options{}, slog.Default())
	mux := http.NewServeMux()
	h.Routes(mux)
	h.OAuthRoutes(mux, botoauth.Verifier{Config: botoauth.Incoming{Resource: resource, Issuer: "https://issuer.example", Scope: "mcp:tools", ClientID: "bot", IntrospectionURL: issuer.URL}, Secret: "secret"}, map[string]string{"a@example.test": "user-a"})
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ask","arguments":{"question":"status?"}}}`
	call := func(path, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	if w := call("/oauth/mcp", ""); w.Code != 401 || !strings.Contains(w.Header().Get("WWW-Authenticate"), "oauth-protected-resource/oauth/mcp") {
		t.Fatal("missing discovery challenge")
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/.well-known/oauth-protected-resource/oauth/mcp", nil))
	if !strings.Contains(w.Body.String(), resource) {
		t.Fatal("missing resource metadata")
	}
	if w := call("/oauth/mcp", "other"); w.Code != 401 {
		t.Fatal("bad token accepted")
	}
	if w := call("/oauth/mcp", "user-grant"); w.Code != 200 || brain.calls != 1 || brain.gotUser != "user-a" || len(brain.gotScope["test"].Namespaces) != 1 {
		t.Fatal("lost caller scope")
	}
	a.scope = nil
	call("/oauth/mcp", "user-grant")
	if brain.calls != 1 {
		t.Fatal("empty scope reached agent")
	}
	call("/mcp", "user-grant")
	if brain.calls != 1 {
		t.Fatal("OAuth leaked into legacy endpoint")
	}
}

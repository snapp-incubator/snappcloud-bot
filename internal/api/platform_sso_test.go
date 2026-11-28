package api

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/snapp-incubator/snappcloud-bot/internal/authzclient"
	"github.com/snapp-incubator/snappcloud-bot/internal/botoauth"
)

func TestPlatformSSOContract(t *testing.T) {
	const issuer = "https://issuer.example/realms/staging"
	newKey := func() *rsa.PrivateKey {
		t.Helper()
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		return key
	}
	key, replacement := newKey(), newKey()
	var published atomic.Pointer[jose.JSONWebKey]
	publish := func(kid string, key *rsa.PrivateKey) {
		published.Store(&jose.JSONWebKey{Key: &key.PublicKey, KeyID: kid, Use: "sig", Algorithm: "RS256"})
	}
	publish("one", key)
	var requests atomic.Int32
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("user bearer forwarded to JWKS")
		}
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{*published.Load()}})
	}))
	defer keys.Close()
	cfg := botoauth.Incoming{Resource: "https://bot.example/oauth/mcp", Issuer: issuer, JWKSURL: keys.URL, Audience: "ai-staging", TrustedAuthorizedParties: []string{"ai"}, SubjectUsers: map[string]string{"sub-a": "alice"}}
	resolver := &authnStub{scope: authzclient.Scope{"region": {Namespaces: []string{"team-a"}}}}
	brain := &fakeBrain{answer: "ok"}
	var logs bytes.Buffer
	h := New(brain, resolver, nil, Options{}, slog.New(slog.NewTextHandler(&logs, nil)))
	mux := http.NewServeMux()
	h.OAuthRoutes(mux, botoauth.Verifier{Config: cfg}, nil)
	call := func(method, token, body string) int {
		r := httptest.NewRequest(method, "/oauth/mcp", strings.NewReader(body))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w.Code
	}
	sign := func(key *rsa.PrivateKey, kid string, alg jose.SignatureAlgorithm, overrides map[string]any) string {
		t.Helper()
		claims := map[string]any{"iss": issuer, "aud": "ai-staging", "azp": "ai-staging", "sub": "sub-a", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(), "email": "alice@example.test", "groups": []string{"/team/dev"}, "scope": "openid email profile owa.read"}
		for k, v := range overrides {
			if v == nil {
				delete(claims, k)
			} else {
				claims[k] = v
			}
		}
		signer, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", kid))
		if err != nil {
			t.Fatal(err)
		}
		raw, err := jwt.Signed(signer).Claims(claims).Serialize()
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	for _, method := range []string{"GET", "POST", "DELETE"} {
		if call(method, "", "bad JSON") != 401 {
			t.Errorf("%s reached MCP without credentials", method)
		}
	}
	const ask = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ask","arguments":{"question":"status?"}}}`
	valid := sign(key, "one", jose.RS256, nil)
	for _, method := range []string{"initialize", "tools/list", "ping"} {
		body := `{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":{}}`
		if call("POST", "", body) != http.StatusUnauthorized || call("POST", valid, body) != http.StatusOK {
			t.Errorf("unexpected authentication result for %s", method)
		}
	}
	if call("POST", valid, ask) != 200 || brain.calls != 1 || resolver.gotUser != "alice" || len(resolver.gotGrps) != 0 {
		t.Fatal("handbook token failed scoped dispatch")
	}
	if call("GET", valid, "") != 405 || requests.Load() != 1 {
		t.Fatal("cached verification failed")
	}
	for _, aud := range []any{nil, []string{"account", "grafana"}} {
		gateway := sign(key, "one", jose.RS256, map[string]any{"aud": aud, "azp": "ai"})
		before := brain.calls
		if call("POST", gateway, ask) != http.StatusOK || brain.calls != before+1 || resolver.gotUser != "alice" {
			t.Fatal("gateway token failed scoped dispatch")
		}
	}
	const privateMarker = "private-test-marker"
	rejected := sign(key, "one", jose.RS256, map[string]any{"aud": privateMarker, "azp": privateMarker, "sub": privateMarker})
	const initialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	for _, tc := range []struct{ token, reason string }{
		{"", "missing bearer token"},
		{privateMarker, "invalid token format"},
		{rejected, "audience and authorized party do not match"},
	} {
		logs.Reset()
		if status := call("POST", tc.token, initialize); status != http.StatusUnauthorized {
			t.Errorf("expected 401 for %s, got %d", tc.reason, status)
		}
		if !strings.Contains(logs.String(), tc.reason) || strings.Contains(logs.String(), privateMarker) || strings.Contains(logs.String(), rejected) {
			t.Errorf("missing or unsafe diagnostic for %s", tc.reason)
		}
	}
	for _, test := range []struct {
		name string
		raw  string
	}{
		{"malformed", "broken"},
		{"unknown client", sign(key, "one", jose.RS256, map[string]any{"aud": "account", "azp": "other"})},
		{"missing recipients", sign(key, "one", jose.RS256, map[string]any{"aud": nil, "azp": nil})},
		{"gateway ID token", sign(key, "one", jose.RS256, map[string]any{"aud": "account", "azp": "ai", "typ": "ID"})},
		{"gateway wrong issuer", sign(key, "one", jose.RS256, map[string]any{"aud": nil, "azp": "ai", "iss": "other"})},
		{"other signer", sign(replacement, "one", jose.RS256, nil)},
		{"other algorithm", sign(key, "one", jose.RS512, nil)},
		{"no kid", sign(key, "", jose.RS256, nil)},
		{"production audience", sign(key, "one", jose.RS256, map[string]any{"aud": "ai"})},
		{"other issuer", sign(key, "one", jose.RS256, map[string]any{"iss": "https://other.example"})},
		{"expired", sign(key, "one", jose.RS256, map[string]any{"exp": time.Now().Unix()})},
		{"missing exp", sign(key, "one", jose.RS256, map[string]any{"exp": nil})},
		{"future nbf", sign(key, "one", jose.RS256, map[string]any{"nbf": time.Now().Add(time.Hour).Unix()})},
		{"no subject", sign(key, "one", jose.RS256, map[string]any{"sub": nil})},
		{"refresh token", sign(key, "one", jose.RS256, map[string]any{"typ": "Refresh"})},
		{"reserved username", sign(key, "one", jose.RS256, map[string]any{"sub": "sub-b", "preferred_username": "system:admin"})},
	} {
		if status := call("POST", test.raw, ask); status != 401 {
			t.Errorf("%s returned %d", test.name, status)
		}
	}
	publish("two", replacement)
	bob := sign(replacement, "two", jose.RS256, map[string]any{"sub": "sub-b", "preferred_username": "bob", "aud": []string{"account", "ai-staging"}})
	if call("POST", bob, ask) != 200 || resolver.gotUser != "bob" || requests.Load() != 2 {
		t.Fatal("key rotation or signed username failed")
	}
	if call("POST", valid, ask) != 401 {
		t.Error("retired key accepted after rotation")
	}
	anonymousSubject := sign(replacement, "two", jose.RS256, map[string]any{"sub": "sub-c", "email": "alice@example.test"})
	resolver.scope = nil
	calls := brain.calls
	call("POST", anonymousSubject, ask)
	if resolver.gotUser != "sub-c" || brain.calls != calls {
		t.Error("email granted another subject access")
	}
}

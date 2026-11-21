package botoauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"golang.org/x/oauth2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestIntrospectionBindsResourceAndIdentity(t *testing.T) {
	const resource = "https://bot.example/oauth/mcp"
	valid := map[string]any{"active": true, "aud": []string{resource}, "iss": "https://id.example", "exp": time.Now().Add(time.Minute).Unix(), "token_type": "Bearer", "scope": "profile mcp:tools", "sub": "person", "email": "user@example.test", "email_verified": true}
	for _, test := range []struct {
		key   string
		value any
		valid bool
	}{{"aud", resource, true}, {"aud", []string{resource}, true}, {"aud", "other", false}, {"iss", "other", false}, {"exp", 0, false}, {"active", false, false}, {"sub", "", false}, {"email_verified", false, false}, {"scope", "other", false}, {"token_type", "refresh_token", false}} {
		claims := map[string]any{}
		for k, v := range valid {
			claims[k] = v
		}
		claims[test.key] = test.value
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, p, ok := r.BasicAuth()
			if !ok || u != "app" || p != "secret" || r.PostFormValue("token") != "opaque" {
				t.Error("incorrect introspection credentials")
			}
			_ = json.NewEncoder(w).Encode(claims)
		}))
		v := Verifier{Incoming{Resource: resource, Issuer: "https://id.example", Scope: "mcp:tools", ClientID: "app", IntrospectionURL: ts.URL}, "secret"}
		email, err := v.Email(context.Background(), "opaque")
		ts.Close()
		if (err == nil) != test.valid || test.valid && email != "user@example.test" {
			t.Fatalf("%s %v: unexpected decision", test.key, test.value)
		}
	}
}

func TestLinkLifecycle(t *testing.T) {
	var challenge string
	var refreshes, revocations int
	var secret string
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.PostFormValue("client_id") != "client" || r.PostFormValue("client_secret") != secret || r.Header.Get("Authorization") != "" {
			t.Error("Pardis client credentials missing from request form")
		}
		if r.URL.Path == "/revoke" {
			if r.PostFormValue("token") != "refresh" || r.PostFormValue("client_id") != "client" {
				t.Error("wrong revocation credential")
			}
			revocations++
			return
		}
		if r.PostFormValue("grant_type") == "refresh_token" {
			refreshes++
			if r.PostFormValue("refresh_token") != "refresh" {
				t.Error("wrong refresh credential")
			}
		} else {
			hashed := sha256.Sum256([]byte(r.PostFormValue("code_verifier")))
			if challenge != base64.RawURLEncoding.EncodeToString(hashed[:]) || r.PostFormValue("resource") != "https://pardis.example/mcp" {
				t.Error("PKCE/resource mismatch")
			}
		}
		_, _ = w.Write([]byte(`{"access_token":"test-access","token_type":"Bearer","refresh_token":"refresh","expires_in":3200}`))
	}))
	defer ts.Close()
	old := client
	client = ts.Client()
	defer func() { client = old }()
	cfg := Outgoing{"https://pardis.example/mcp", ts.URL + "/authorize", ts.URL + "/token", ts.URL + "/revoke", "https://bot.example/oauth/callback", "client", t.TempDir()}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	open := func() *Links {
		l, err := NewLinks(cfg, secret, base64.StdEncoding.EncodeToString(key), func(_ context.Context, token, email string) error {
			if email != "a@example.test" || token != "test-access" {
				return errors.New("mismatch")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	links := open()
	login := func(user, email string) (string, int) {
		u, _ := url.Parse(links.Start(user, email))
		q := u.Query()
		challenge = q.Get("code_challenge")
		if q.Get("code_challenge_method") != "S256" {
			t.Fatal("missing PKCE")
		}
		path := "/oauth/callback?code=code&state=" + q.Get("state")
		w := httptest.NewRecorder()
		links.Callback(w, httptest.NewRequest("GET", path, nil))
		return path, w.Code
	}
	if _, status := login("b", "b@example.test"); status != 403 {
		t.Fatal("identity mismatch accepted")
	}
	secret = "staging-client-secret"
	links = open()
	path, status := login("a", "a@example.test")
	if status != 200 {
		t.Fatalf("login failed %d", status)
	}
	w := httptest.NewRecorder()
	links.Callback(w, httptest.NewRequest("GET", path, nil))
	if w.Code != 400 {
		t.Fatal("state replay accepted")
	}
	links = open()
	raw, err := os.ReadFile(links.file("a"))
	if err != nil || strings.Contains(string(raw), "test-access") {
		t.Fatal("encrypted token storage failed")
	}
	if err = os.WriteFile(links.file("b"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = links.Token(context.Background(), "b"); err == nil {
		t.Fatal("token accepted under another user")
	}
	token, err := links.Token(context.Background(), "a")
	if err != nil || token != "test-access" {
		t.Fatal("restart lost grant")
	}
	expired := oauth2.Token{AccessToken: token, RefreshToken: "refresh", Expiry: time.Now().Add(-time.Minute)}
	if err = links.save("a", &expired); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err = links.Token(context.Background(), "a"); err != nil {
			t.Fatal(err)
		}
	}
	if refreshes != 1 {
		t.Fatalf("expected one refresh, got %d", refreshes)
	}
	if err = links.Disconnect(context.Background(), "a"); err != nil || revocations != 1 {
		t.Fatal(err)
	}
	if _, err = links.Token(context.Background(), "a"); !errors.Is(err, ErrUnlinked) {
		t.Fatal("disconnected grant survived")
	}
}

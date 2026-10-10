package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A server that refuses a request the bot sent with no Authorization header
// answers exactly like one that is down: "http 403" with an empty body. That
// read as an outage of five servers per production region in diagnostics while
// every one of them was healthy, because the env var naming the credential was
// absent from the Secret and os.Getenv returned "".
func TestRefusalDistinguishesAMissingCredentialFromARejectedOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Basic good" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
			return
		}
		// Empty body, exactly as the real servers answer.
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	t.Run("no header sent", func(t *testing.T) {
		_, err := New(srv.URL, "", false, time.Minute).ListTools(context.Background())
		if !errors.Is(err, ErrNoCredential) {
			t.Fatalf("want ErrNoCredential, got %v", err)
		}
		if errors.Is(err, ErrCredentialRefused) {
			t.Error("a request with no credential cannot have had one refused")
		}
	})

	t.Run("header sent and rejected", func(t *testing.T) {
		_, err := New(srv.URL, "Basic stale", false, time.Minute).ListTools(context.Background())
		if !errors.Is(err, ErrCredentialRefused) {
			t.Fatalf("want ErrCredentialRefused, got %v", err)
		}
		if errors.Is(err, ErrNoCredential) {
			t.Error("a credential was sent; it was not missing")
		}
	})

	t.Run("accepted", func(t *testing.T) {
		if _, err := New(srv.URL, "Basic good", false, time.Minute).ListTools(context.Background()); err != nil {
			t.Fatalf("a good credential must be accepted: %v", err)
		}
	})
}

// Only 401 and 403 are credential verdicts. A 404 is a route or a server that
// is not there, and must not be reported as a Secret problem.
func TestOtherStatusesAreNotCredentialVerdicts(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusBadGateway, http.StatusServiceUnavailable} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		_, err := New(srv.URL, "", false, time.Minute).ListTools(context.Background())
		if errors.Is(err, ErrNoCredential) || errors.Is(err, ErrCredentialRefused) {
			t.Errorf("http %d classified as a credential problem: %v", code, err)
		}
		srv.Close()
	}
}

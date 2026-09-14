package mcp

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A server may ship far more than the bot should offer. The Grafana MCP server
// can update dashboards, manage alert rules and expire silences; the bot is
// read-only, so the allow-list is what keeps those out of reach.
func TestAllowListGatesToolsBothWays(t *testing.T) {
	c := New("https://example/mcp", "", false, 0,
		"query_prometheus", "list_prometheus_metric_names")

	for _, ok := range []string{"query_prometheus", "list_prometheus_metric_names"} {
		if !c.Allowed(ok) {
			t.Errorf("%q should be allowed", ok)
		}
	}
	for _, blocked := range []string{"update_dashboard", "alerting_manage_silences", "delete_annotation", ""} {
		if c.Allowed(blocked) {
			t.Errorf("%q must not be reachable", blocked)
		}
	}

	// The call is refused even when the model names a tool it was never offered.
	if _, err := c.CallTool(t.Context(), "update_dashboard", nil); err == nil {
		t.Fatal("a tool outside the allow-list was called")
	}
}

// No allow-list means the server's own tool set, unchanged.
func TestEmptyAllowListPermitsEverything(t *testing.T) {
	c := New("https://example/mcp", "", false, 0)
	for _, name := range []string{"anything", "list_pods", "query_prometheus"} {
		if !c.Allowed(name) {
			t.Errorf("%q should be allowed when no allow-list is configured", name)
		}
	}
}

// An oversized response must be refused at the read, not after it is resident:
// the bot fans out across clusters in a small container, and each response is
// parsed again downstream at several times the size of its text.
func TestOversizedResponseIsRefusedAtTheRead(t *testing.T) {
	// A body larger than the cap, served as plain JSON.
	big := `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"` +
		strings.Repeat("x", maxResponseBytes+1024) + `"}]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, big)
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if _, err := decodeResponse(resp, 1); err == nil {
		t.Fatal("an oversized response was accepted")
	} else if !strings.Contains(err.Error(), "narrow the query") {
		t.Errorf("error should tell the model what to do, got: %v", err)
	}
}

// A normal response is unaffected by the cap.
func TestNormalResponseStillDecodes(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":7,"result":{"content":[{"type":"text","text":"ok"}]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	rpc, err := decodeResponse(resp, 7)
	if err != nil {
		t.Fatalf("a normal response was refused: %v", err)
	}
	if rpc.Error != nil || len(rpc.Result) == 0 {
		t.Errorf("result not decoded: %+v", rpc)
	}
}

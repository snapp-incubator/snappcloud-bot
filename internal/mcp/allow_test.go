package mcp

import "testing"

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

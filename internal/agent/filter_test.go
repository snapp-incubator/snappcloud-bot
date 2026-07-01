package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func allowedSet(ns ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range ns {
		m[n] = true
	}
	return m
}

// Hubble-style flow list: keep flows within authorized namespaces, drop others.
func TestFilterDropsUnauthorizedFlows(t *testing.T) {
	result := `{"flows":[
	  {"source":{"namespace":"team-a","pod":"web-0"},"destination":{"namespace":"team-a","pod":"db-0"},"verdict":"DROPPED"},
	  {"source":{"namespace":"team-a"},"destination":{"namespace":"kube-system"},"verdict":"FORWARDED"},
	  {"source":{"namespace":"other-team"},"destination":{"namespace":"other-team"}}
	]}`
	out, removed, blocked := FilterResult(result, allowedSet("team-a"), nil)
	if blocked {
		t.Fatal("array result should filter, not block")
	}
	if removed != 2 {
		t.Fatalf("expected 2 flows removed, got %d", removed)
	}
	if strings.Contains(out, "kube-system") || strings.Contains(out, "other-team") {
		t.Fatalf("unauthorized namespace leaked: %s", out)
	}
	if !strings.Contains(out, "team-a") {
		t.Fatal("authorized flow was dropped")
	}
}

func TestFilterTopLevelArray(t *testing.T) {
	result := `[{"namespace":"team-a"},{"namespace":"secret-ns"}]`
	out, removed, _ := FilterResult(result, allowedSet("team-a"), nil)
	if removed != 1 || strings.Contains(out, "secret-ns") {
		t.Fatalf("top-level array not filtered: removed=%d out=%s", removed, out)
	}
}

func TestFilterBlocksNonArrayDocWithUnauthorizedNS(t *testing.T) {
	result := `{"summary":"status","namespace":"kube-system"}`
	_, _, blocked := FilterResult(result, allowedSet("team-a"), nil)
	if !blocked {
		t.Fatal("non-array doc naming an unauthorized namespace must be blocked")
	}
}

func TestFilterKeepsNamespacelessAndNonJSON(t *testing.T) {
	if _, _, blocked := FilterResult(`{"status":"ok","count":3}`, allowedSet("team-a"), nil); blocked {
		t.Fatal("namespace-less doc should pass")
	}
	if out, _, blocked := FilterResult("plain text answer", allowedSet("team-a"), nil); blocked || out != "plain text answer" {
		t.Fatal("non-JSON should pass through unchanged")
	}
}

func TestFilterResolvesIPToNamespace(t *testing.T) {
	// A record naming only an IP is gated via the resolved map.
	result := `[{"src_ip":"10.0.0.5","bytes":100},{"src_ip":"10.0.0.9","bytes":50}]`
	resolved := map[string][]string{"10.0.0.5": {"team-a"}, "10.0.0.9": {"kube-system"}}
	out, removed, _ := FilterResult(result, allowedSet("team-a"), resolved)
	if removed != 1 || strings.Contains(out, "10.0.0.9") {
		t.Fatalf("IP-based record not gated: removed=%d out=%s", removed, out)
	}
}

func TestExtractRefs(t *testing.T) {
	ns, ips := ExtractRefs(`{"source":{"namespace":"team-a"},"ip":"10.1.2.3"}`)
	if len(ns) != 1 || ns[0] != "team-a" {
		t.Fatalf("namespaces: %v", ns)
	}
	if len(ips) != 1 || ips[0] != "10.1.2.3" {
		t.Fatalf("ips: %v", ips)
	}
}

func TestFilteredArrayStaysValidJSON(t *testing.T) {
	result := `{"flows":[{"namespace":"team-a"},{"namespace":"x"}]}`
	out, _, _ := FilterResult(result, allowedSet("team-a"), nil)
	var v any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("filtered result is not valid JSON: %v", err)
	}
}

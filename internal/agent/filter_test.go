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

// Hubble-style flow list. A flow is kept when the caller owns EITHER side —
// their own egress always names a peer they do not own — and dropped when they
// are not party to it at all.
func TestFilterDropsFlowsTheCallerIsNotPartyTo(t *testing.T) {
	result := `{"flows":[
	  {"source":{"namespace":"team-a","pod":"web-0"},"destination":{"namespace":"team-a","pod":"db-0"},"verdict":"DROPPED"},
	  {"source":{"namespace":"team-a"},"destination":{"namespace":"kube-system"},"verdict":"FORWARDED"},
	  {"source":{"namespace":"other-team"},"destination":{"namespace":"other-team"}}
	]}`
	out, removed, blocked := FilterResult(result, allowedSet("team-a"), nil)
	if blocked {
		t.Fatal("array result should filter, not block")
	}
	if removed != 1 {
		t.Fatalf("expected 1 flow removed (other-team to other-team), got %d", removed)
	}
	if strings.Contains(out, "other-team") {
		t.Fatalf("a flow the caller is not party to leaked: %s", out)
	}
	// The caller's own egress to CoreDNS is theirs to see.
	if !strings.Contains(out, "kube-system") {
		t.Fatalf("the caller's own DNS traffic was hidden: %s", out)
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

// Listing namespaces must return only the caller's own namespaces. A Namespace
// object's identity is its name (no "namespace" field), so gate on the name.
func TestFilterGatesNamespaceObjectsByName(t *testing.T) {
	result := `{"items":[
	  {"kind":"Namespace","name":"team-a","namespace":""},
	  {"kind":"Namespace","name":"team-b","namespace":""},
	  {"kind":"Namespace","name":"kube-system","namespace":""}
	]}`
	out, removed, blocked := FilterResult(result, allowedSet("team-a"), nil)
	if blocked {
		t.Fatal("array doc should filter, not block whole")
	}
	if removed != 2 {
		t.Fatalf("removed=%d, want 2 (team-b, kube-system)", removed)
	}
	if strings.Contains(out, "team-b") || strings.Contains(out, "kube-system") {
		t.Fatalf("leaked unauthorized namespace: %s", out)
	}
	if !strings.Contains(out, "team-a") {
		t.Fatalf("dropped authorized namespace: %s", out)
	}
}

// Raw Kubernetes Namespace object (metadata.name form) is gated too.
func TestFilterGatesRawNamespaceObject(t *testing.T) {
	result := `{"kind":"Namespace","metadata":{"name":"kube-system"}}`
	_, _, blocked := FilterResult(result, allowedSet("team-a"), nil)
	if !blocked {
		t.Fatal("unauthorized namespace object must be blocked")
	}
}

// Hubble's flow_summary is an AGGREGATE: the namespace lives in the map key,
// not in a field, so per-record filtering never sees it. A tenant asking for a
// summary without a namespace must still only see their own workloads.
func TestFilterScopesNamespaceKeyedAggregates(t *testing.T) {
	summary := `{
		"total_flows": 500,
		"verdict_counts": {"FORWARDED": 480, "DROPPED": 20},
		"top_source_pods": {"team-a/web-0": 300, "team-b/api-0": 150, "team-a/worker-1": 50},
		"top_destination_pods": {"team-b/db-0": 200, "team-a/cache-0": 100},
		"top_services": {"team-a/web": 300, "team-b/api": 150},
		"top_fqdns": {"api.example.com": 42},
		"top_protocols": {"TCP": 400, "UDP": 100}
	}`
	allowed := map[string]bool{"team-a": true}

	out, removed, blocked := FilterResult(summary, allowed, nil)
	if blocked {
		t.Fatal("summary blocked wholesale; the caller's own data must survive")
	}
	if removed != 3 {
		t.Errorf("removed %d entries, want 3 (two team-b pods + one team-b service)", removed)
	}
	if strings.Contains(out, "team-b") {
		t.Errorf("another tenant's workloads leaked:\n%s", out)
	}
	// The caller's own aggregates, and the namespace-free ones, must remain.
	for _, want := range []string{"team-a/web-0", "team-a/worker-1", "team-a/cache-0", "FORWARDED", "TCP", "api.example.com"} {
		if !strings.Contains(out, want) {
			t.Errorf("%q was dropped; only unauthorized entries should be:\n%s", want, out)
		}
	}
}

// A cluster-admin (every namespace allowed) must see the whole summary.
func TestFilterKeepsAggregatesForFullScope(t *testing.T) {
	summary := `{"top_source_pods": {"team-a/web-0": 300, "team-b/api-0": 150}}`
	allowed := map[string]bool{"team-a": true, "team-b": true}

	out, removed, _ := FilterResult(summary, allowed, nil)
	if removed != 0 || !strings.Contains(out, "team-b/api-0") {
		t.Errorf("authorized aggregate entries were dropped: removed=%d out=%s", removed, out)
	}
}

// Keys that merely contain a slash are not namespace-qualified and must survive.
func TestFilterLeavesNonNamespaceKeysAlone(t *testing.T) {
	doc := `{"http_paths": {"/api/v1/users": 10, "/healthz": 99}, "top_fqdns": {"a.b.example.com": 5}}`
	out, removed, blocked := FilterResult(doc, map[string]bool{"team-a": true}, nil)
	if removed != 0 || blocked {
		t.Fatalf("non-namespace keys were filtered: removed=%d blocked=%v out=%s", removed, blocked, out)
	}
}

// A tenant must see their OWN traffic, including where it goes: DNS to
// kube-system and a denied call to another team are the two questions they ask
// most, and both were invisible under the all-sides rule.
func TestTenantSeesOwnFlowsIncludingPeers(t *testing.T) {
	flows := `[
	  {"source":{"namespace":"team-a","pod_name":"web-0"},"destination":{"namespace":"team-a","pod_name":"cache-0"},"verdict":"FORWARDED"},
	  {"source":{"namespace":"team-a","pod_name":"web-0"},"destination":{"namespace":"kube-system","pod_name":"coredns-1"},"verdict":"FORWARDED"},
	  {"source":{"namespace":"team-a","pod_name":"web-0"},"destination":{"namespace":"team-b","pod_name":"api-0"},"verdict":"DROPPED"},
	  {"source":{"namespace":"team-b","pod_name":"api-0"},"destination":{"namespace":"team-a","pod_name":"web-0"},"verdict":"FORWARDED"},
	  {"source":{"namespace":"team-c","pod_name":"other-0"},"destination":{"namespace":"team-d","pod_name":"x"},"verdict":"FORWARDED"}
	]`
	out, removed, _ := FilterResult(flows, map[string]bool{"team-a": true}, nil)

	if removed != 1 {
		t.Errorf("removed %d, want 1 (only the team-c -> team-d flow)", removed)
	}
	for _, want := range []string{"coredns-1", "team-b", "cache-0"} {
		if !strings.Contains(out, want) {
			t.Errorf("the caller's own traffic to %q was hidden:\n%s", want, out)
		}
	}
	if strings.Contains(out, "team-c") || strings.Contains(out, "team-d") {
		t.Errorf("a flow the caller is not party to leaked:\n%s", out)
	}
}

// The relaxation is only for peer-shaped records. An ordinary resource must not
// become visible because it mentions an authorized namespace somewhere.
func TestNonPeerRecordsStayStrict(t *testing.T) {
	records := `[
	  {"kind":"Pod","namespace":"team-b","name":"api-0","note":"mirrors team-a"},
	  {"kind":"Pod","namespace":"team-a","name":"web-0"}
	]`
	out, removed, _ := FilterResult(records, map[string]bool{"team-a": true}, nil)
	if removed != 1 || strings.Contains(out, "team-b") {
		t.Errorf("strict rule weakened for non-peer records: removed=%d out=%s", removed, out)
	}
}

// A flow naming only IPs is gated by what those IPs resolve to, per side.
func TestPeerRuleUsesResolvedIPs(t *testing.T) {
	flows := `[
	  {"source":{"ip":"10.0.0.1"},"destination":{"ip":"10.0.0.2"},"verdict":"FORWARDED"},
	  {"source":{"ip":"10.0.0.3"},"destination":{"ip":"10.0.0.4"},"verdict":"FORWARDED"}
	]`
	resolved := map[string][]string{
		"10.0.0.1": {"team-a"}, "10.0.0.2": {"team-b"},
		"10.0.0.3": {"team-c"}, "10.0.0.4": {"team-d"},
	}
	out, removed, _ := FilterResult(flows, map[string]bool{"team-a": true}, resolved)
	if removed != 1 || !strings.Contains(out, "10.0.0.2") || strings.Contains(out, "10.0.0.3") {
		t.Errorf("IP-only flows not gated per side: removed=%d out=%s", removed, out)
	}
}

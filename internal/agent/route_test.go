package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func quietAgent(llm LLM, rules map[string]ToolRule, maxTools int) *Agent {
	b := DefaultBudgets()
	if maxTools > 0 {
		b.MaxTools = maxTools
	}
	return New(llm, NewEnforcer(rules), nil, 6, b, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func toolByName(ts []Tool, name string) *Tool {
	for i := range ts {
		if ts[i].Name == name {
			return &ts[i]
		}
	}
	return nil
}

// A tool every named cluster has is offered once, with a cluster argument, and
// the call runs on the cluster it names without that argument.
func TestToolsAreMergedAcrossClusters(t *testing.T) {
	teh1 := &fakeMCP{tools: []string{"list_pods"}, output: `{"pods":[]}`}
	teh2 := &fakeMCP{tools: []string{"list_pods"}, output: `{"pods":[]}`}
	llm := &fakeLLM{turns: []Response{
		{Calls: []ToolCall{{ID: "1", Name: "list_pods", Args: map[string]any{"cluster": "teh-2", "namespace": "team-a"}}}},
		{Text: "done"},
	}}
	_, err := quietAgent(llm, nil, 0).Run(context.Background(), Input{Query: "q", Clusters: []ClusterTools{
		{Cluster: "okd4-teh-1", Alias: "teh1", Allowed: []string{"team-a"}, MCP: teh1},
		{Cluster: "okd4-teh-2", Alias: "teh2", Allowed: []string{"team-a"}, MCP: teh2},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(llm.seen[0].Tools); len(got) != 1 || got[0] != "list_pods" {
		t.Fatalf("offered %v, want one list_pods", got)
	}
	props := llm.seen[0].Tools[0].InputSchema["properties"].(map[string]any)
	if enum := props["cluster"].(map[string]any)["enum"].([]any); len(enum) != 2 {
		t.Fatalf("cluster enum %v", enum)
	}
	if len(teh1.called) != 0 || len(teh2.called) != 1 {
		t.Fatalf("routed wrong: teh1=%v teh2=%v", teh1.called, teh2.called)
	}
	if _, leaked := teh2.args[0]["cluster"]; leaked {
		t.Fatalf("cluster argument reached the server: %v", teh2.args[0])
	}
}

func TestMergedToolWithoutClusterIsRefused(t *testing.T) {
	a, b := &fakeMCP{tools: []string{"list_pods"}}, &fakeMCP{tools: []string{"list_pods"}}
	llm := &fakeLLM{turns: []Response{
		{Calls: []ToolCall{{ID: "1", Name: "list_pods", Args: map[string]any{"namespace": "team-a"}}}},
		{Text: "done"},
	}}
	_, err := quietAgent(llm, nil, 0).Run(context.Background(), Input{Query: "q", Clusters: []ClusterTools{
		{Cluster: "okd4-teh-1", Allowed: []string{"team-a"}, MCP: a},
		{Cluster: "okd4-teh-2", Allowed: []string{"team-a"}, MCP: b},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.called)+len(b.called) != 0 {
		t.Fatal("a call without a cluster must not run anywhere")
	}
	if msg := lastResult(llm); !strings.Contains(msg, "okd4-teh-1, okd4-teh-2") {
		t.Fatalf("refusal must name the clusters: %q", msg)
	}
}

// One cluster: no cluster argument at all — it would only cost tokens.
func TestSingleClusterHasNoClusterArgument(t *testing.T) {
	llm := &fakeLLM{turns: []Response{{Text: "ok"}}}
	_, err := quietAgent(llm, nil, 0).Run(context.Background(), Input{Query: "q", Clusters: []ClusterTools{
		{Cluster: "okd4-teh-1", Allowed: []string{"team-a"}, MCP: &fakeMCP{tools: []string{"list_pods"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if props, _ := llm.seen[0].Tools[0].InputSchema["properties"].(map[string]any); props["cluster"] != nil {
		t.Fatal("single-cluster tool must not take a cluster argument")
	}
}

// Admin on one cluster only: the admin tool is offered for that cluster alone,
// and a call aimed at the other still gets the cluster-admin denial.
func TestAdminToolOffersOnlyAdminClusters(t *testing.T) {
	admin, user := &fakeMCP{tools: []string{"bgp_peers"}}, &fakeMCP{tools: []string{"bgp_peers"}}
	llm := &fakeLLM{turns: []Response{
		{Calls: []ToolCall{{ID: "1", Name: "bgp_peers", Args: map[string]any{"cluster": "okd4-teh-2"}}}},
		{Text: "done"},
	}}
	_, err := quietAgent(llm, map[string]ToolRule{"bgp_peers": {ClusterAdminOnly: true}}, 0).Run(context.Background(),
		Input{Query: "q", Clusters: []ClusterTools{
			{Cluster: "okd4-teh-1", ClusterAdmin: true, MCP: admin},
			{Cluster: "okd4-teh-2", Allowed: []string{"team-a"}, MCP: user},
		}})
	if err != nil {
		t.Fatal(err)
	}
	enum := llm.seen[0].Tools[0].InputSchema["properties"].(map[string]any)["cluster"].(map[string]any)["enum"].([]any)
	if len(enum) != 1 || enum[0] != "okd4-teh-1" {
		t.Fatalf("enum %v, want only the admin cluster", enum)
	}
	if len(user.called) != 0 || !strings.Contains(lastResult(llm), "requires cluster-admin access on okd4-teh-2") {
		t.Fatalf("non-admin cluster must be denied: called=%v result=%q", user.called, lastResult(llm))
	}
}

// The case that left an investigation without logs or the HTTPProxy: two named
// clusters with production-sized servers. Per-cluster copies gave each server
// ~10 slots, alphabetically; merged, every tool those questions need is there.
func TestTwoClustersKeepEveryServersTools(t *testing.T) {
	servers := map[string][]string{
		"cilium":    seq("cilium_", 64),
		"hubble":    seq("hubble_", 11),
		"envoy":     append(seq("envoy_", 12), "get_contour_dag", "get_httpproxy", "list_httpproxies", "search_httpproxy_by_fqdn"),
		"openshift": append(seq("get_", 14), "list_pods", "pod_logs", "top_pods"),
		"argocd":    seq("argocd_", 6),
		"grafana":   seq("query_", 15),
	}
	cluster := func() *multiMCP { return &multiMCP{servers: servers} }
	llm := &fakeLLM{turns: []Response{{Text: "ok"}}}
	_, err := quietAgent(llm, nil, 128).Run(context.Background(), Input{Query: "q", Clusters: []ClusterTools{
		{Cluster: "okd4-teh-2", ClusterAdmin: true, Preferred: true, MCP: cluster()},
		{Cluster: "okd4-snappgroup", ClusterAdmin: true, Preferred: true, MCP: cluster()},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"pod_logs", "list_pods", "get_httpproxy", "list_httpproxies", "get_contour_dag", "hubble_10"} {
		if toolByName(llm.seen[0].Tools, want) == nil {
			t.Errorf("%s not offered", want)
		}
	}
}

// A call in the old "<alias>__<tool>" form, from a conversation that began
// before tools were merged, still reaches its cluster.
func TestLegacyQualifiedNameStillRoutes(t *testing.T) {
	a, b := &fakeMCP{tools: []string{"list_pods"}}, &fakeMCP{tools: []string{"list_pods"}}
	llm := &fakeLLM{turns: []Response{
		{Calls: []ToolCall{{ID: "1", Name: "teh2__list_pods", Args: map[string]any{"namespace": "team-a"}}}},
		{Text: "done"},
	}}
	_, err := quietAgent(llm, nil, 0).Run(context.Background(), Input{Query: "q", Clusters: []ClusterTools{
		{Cluster: "okd4-teh-1", Alias: "teh1", Allowed: []string{"team-a"}, MCP: a},
		{Cluster: "okd4-teh-2", Alias: "teh2", Allowed: []string{"team-a"}, MCP: b},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.called) != 0 || len(b.called) != 1 {
		t.Fatalf("legacy name routed wrong: teh1=%v teh2=%v", a.called, b.called)
	}
}

func seq(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%d", prefix, i)
	}
	return out
}

func lastResult(llm *fakeLLM) string {
	msgs := llm.seen[len(llm.seen)-1].Messages
	for i := len(msgs) - 1; i >= 0; i-- {
		if len(msgs[i].Results) > 0 {
			return msgs[i].Results[0].Content
		}
	}
	return ""
}

// multiMCP is a cluster with several servers, the way the mux presents one.
type multiMCP struct{ servers map[string][]string }

func (m *multiMCP) ListTools(context.Context) ([]Tool, error) {
	var ts []Tool
	for _, srv := range []string{"cilium", "hubble", "envoy", "openshift", "argocd", "grafana"} {
		for _, n := range m.servers[srv] {
			ts = append(ts, Tool{Name: n, Server: srv})
		}
	}
	return ts, nil
}
func (m *multiMCP) CallTool(context.Context, string, map[string]any) (string, error) { return "{}", nil }

package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
)

func TestEnforceDefaultNamespaceArg(t *testing.T) {
	e := NewEnforcer(nil)
	if err := e.Check("get_flows", map[string]any{"namespace": "team-a"}, []string{"team-a"}); err != nil {
		t.Fatalf("authorized call denied: %v", err)
	}
	if err := e.Check("get_flows", map[string]any{"namespace": "team-x"}, []string{"team-a"}); err == nil {
		t.Fatal("unauthorized namespace allowed")
	}
}

func TestEnforceSlashFormat(t *testing.T) {
	e := NewEnforcer(map[string]ToolRule{
		"get_pod": {NamespaceArgs: []string{"pod"}, Format: NSSlash},
	})
	if err := e.Check("get_pod", map[string]any{"pod": "team-a/web-0"}, []string{"team-a"}); err != nil {
		t.Fatalf("authorized slash call denied: %v", err)
	}
	if err := e.Check("get_pod", map[string]any{"pod": "kube-system/x"}, []string{"team-a"}); err == nil {
		t.Fatal("unauthorized slash namespace allowed")
	}
}

func TestEnforceMultipleNamespaces(t *testing.T) {
	e := NewEnforcer(map[string]ToolRule{
		"compare": {NamespaceArgs: []string{"namespaces"}, Format: NSPlain},
	})
	// one of the list is not allowed -> deny whole call
	err := e.Check("compare", map[string]any{"namespaces": []any{"team-a", "team-z"}}, []string{"team-a"})
	if err == nil {
		t.Fatal("call touching an unauthorized namespace was allowed")
	}
}

func TestEnforceNonNamespacedPassesUnlessDefaultDeny(t *testing.T) {
	e := NewEnforcer(nil)
	// no "namespace" arg -> non-namespaced -> allowed
	if err := e.Check("server_status", map[string]any{}, []string{"team-a"}); err != nil {
		t.Fatalf("non-namespaced call denied: %v", err)
	}
	e.DefaultDeny = true
	if err := e.Check("server_status", map[string]any{}, []string{"team-a"}); err == nil {
		t.Fatal("DefaultDeny should deny a tool with no explicit rule")
	}
}

func TestEnforceRequireNamespace(t *testing.T) {
	e := NewEnforcer(map[string]ToolRule{
		"list_all": {NamespaceArgs: []string{"namespace"}, RequireNamespace: true},
	})
	if err := e.Check("list_all", map[string]any{}, []string{"team-a"}); err == nil {
		t.Fatal("RequireNamespace should deny an unscoped call")
	}
}

// --- loop ---

type fakeLLM struct {
	turns []Response // scripted responses, one per Complete call
	seen  []Request
	i     int
}

func (f *fakeLLM) Complete(_ context.Context, req Request) (Response, error) {
	f.seen = append(f.seen, req)
	r := f.turns[f.i]
	f.i++
	return r, nil
}

type fakeMCP struct {
	tools  []string
	called []string
	output string
}

func (f *fakeMCP) ListTools(context.Context) ([]Tool, error) {
	ts := make([]Tool, 0, len(f.tools))
	for _, n := range f.tools {
		ts = append(ts, Tool{Name: n})
	}
	return ts, nil
}
func (f *fakeMCP) CallTool(_ context.Context, name string, _ map[string]any) (string, error) {
	f.called = append(f.called, name)
	if f.output != "" {
		return f.output, nil
	}
	return "flows: 3 dropped", nil
}

func newAgent(l LLM) *Agent {
	return New(l, NewEnforcer(nil), nil, 6, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestRunExecutesAuthorizedToolThenAnswers(t *testing.T) {
	llm := &fakeLLM{turns: []Response{
		{Calls: []ToolCall{{ID: "1", Name: "okd4-ts-3__get_flows", Args: map[string]any{"namespace": "team-a"}}}},
		{Text: "3 packets dropped in team-a"},
	}}
	mcp := &fakeMCP{tools: []string{"get_flows"}}
	out, err := newAgent(llm).Run(context.Background(), Input{
		Query: "drops?", Clusters: []ClusterTools{{Cluster: "okd4-ts-3", Allowed: []string{"team-a"}, MCP: mcp}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != "3 packets dropped in team-a" {
		t.Fatalf("answer: %q", out)
	}
	if !reflect.DeepEqual(mcp.called, []string{"get_flows"}) {
		t.Fatalf("tool not executed (real name): %v", mcp.called)
	}
}

func TestRunDeniesUnauthorizedToolAndNeverExecutes(t *testing.T) {
	llm := &fakeLLM{turns: []Response{
		{Calls: []ToolCall{{ID: "1", Name: "okd4-ts-3__get_flows", Args: map[string]any{"namespace": "kube-system"}}}},
		{Text: "I can't access that namespace."},
	}}
	mcp := &fakeMCP{tools: []string{"get_flows"}}
	_, err := newAgent(llm).Run(context.Background(), Input{
		Query: "drops in kube-system?", Clusters: []ClusterTools{{Cluster: "okd4-ts-3", Allowed: []string{"team-a"}, MCP: mcp}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(mcp.called) != 0 {
		t.Fatalf("unauthorized tool WAS executed: %v", mcp.called)
	}
	last := llm.seen[len(llm.seen)-1]
	res := last.Messages[len(last.Messages)-1].Results
	if len(res) != 1 || !res[0].IsError {
		t.Fatalf("denial not fed back as error result: %+v", res)
	}
}

func TestRunMultiClusterRoutesToCorrectCluster(t *testing.T) {
	// Two clusters, each with the same tool name; the model targets ts-3.
	ts2 := &fakeMCP{tools: []string{"get_flows"}}
	ts3 := &fakeMCP{tools: []string{"get_flows"}}
	llm := &fakeLLM{turns: []Response{
		{Calls: []ToolCall{{ID: "1", Name: "okd4-ts-3__get_flows", Args: map[string]any{"namespace": "team-a"}}}},
		{Text: "done"},
	}}
	_, err := newAgent(llm).Run(context.Background(), Input{
		Query: "ts-3 drops",
		Clusters: []ClusterTools{
			{Cluster: "okd4-ts-2", Allowed: []string{"team-a"}, MCP: ts2},
			{Cluster: "okd4-ts-3", Allowed: []string{"team-a"}, MCP: ts3},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ts2.called) != 0 {
		t.Fatalf("wrong cluster called: ts-2 got %v", ts2.called)
	}
	if len(ts3.called) != 1 {
		t.Fatalf("target cluster ts-3 not called: %v", ts3.called)
	}
}

type failingResolver struct{}

func (failingResolver) ResolveIPs(context.Context, string, []string) (map[string][]string, error) {
	return nil, errors.New("resolve endpoint unavailable")
}

// Cluster-admins get infra-tool output raw — even when it contains external IPs
// and the resolver is down (BGP peer data is infrastructure, not tenant data).
func TestClusterAdminGetsInfraToolUnfiltered(t *testing.T) {
	llm := &fakeLLM{turns: []Response{
		{Calls: []ToolCall{{ID: "1", Name: "okd4-ts-3__bgp_peers", Args: map[string]any{"node": "cilium-1"}}}},
		{Text: "done"},
	}}
	mcp := &fakeMCP{tools: []string{"bgp_peers"}}
	mcp.output = `{"peers":[{"peer-address":"10.15.10.10","session-state":"established"}]}`

	enforcer := NewEnforcer(map[string]ToolRule{"bgp_peers": {ClusterAdminOnly: true}})
	ag := New(llm, enforcer, failingResolver{}, 6, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err := ag.Run(context.Background(), Input{
		Query:    "bgp?",
		Clusters: []ClusterTools{{Cluster: "okd4-ts-3", Allowed: []string{"argocd"}, ClusterAdmin: true, MCP: mcp}},
	})
	if err != nil {
		t.Fatal(err)
	}
	res := llm.seen[len(llm.seen)-1].Messages[2].Results[0]
	if res.IsError {
		t.Fatalf("admin infra result was withheld: %s", res.Content)
	}
	if !strings.Contains(res.Content, "10.15.10.10") {
		t.Fatalf("raw BGP output lost: %s", res.Content)
	}
}

// Non-admins are denied infra tools outright — the call never executes.
func TestNonAdminDeniedInfraTool(t *testing.T) {
	llm := &fakeLLM{turns: []Response{
		{Calls: []ToolCall{{ID: "1", Name: "okd4-ts-3__bgp_peers", Args: map[string]any{"node": "cilium-1"}}}},
		{Text: "done"},
	}}
	mcp := &fakeMCP{tools: []string{"bgp_peers"}}

	enforcer := NewEnforcer(map[string]ToolRule{"bgp_peers": {ClusterAdminOnly: true}})
	ag := New(llm, enforcer, failingResolver{}, 6, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err := ag.Run(context.Background(), Input{
		Query:    "bgp?",
		Clusters: []ClusterTools{{Cluster: "okd4-ts-3", Allowed: []string{"argocd"}, MCP: mcp}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(mcp.called) != 0 {
		t.Fatalf("infra tool executed for non-admin: %v", mcp.called)
	}
	res := llm.seen[len(llm.seen)-1].Messages[2].Results[0]
	if !res.IsError || !strings.Contains(res.Content, "cluster-admin") {
		t.Fatalf("expected cluster-admin denial, got: %+v", res)
	}
}

// Non-exempt tools keep the fail-closed behavior when resolution is down.
func TestNormalToolStillWithheldOnResolverFailure(t *testing.T) {
	llm := &fakeLLM{turns: []Response{
		{Calls: []ToolCall{{ID: "1", Name: "okd4-ts-3__get_flows", Args: map[string]any{"namespace": "argocd"}}}},
		{Text: "done"},
	}}
	mcp := &fakeMCP{tools: []string{"get_flows"}}
	mcp.output = `[{"src_ip":"10.0.0.9","bytes":10}]`

	ag := New(llm, NewEnforcer(nil), failingResolver{}, 6, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err := ag.Run(context.Background(), Input{
		Query:    "flows?",
		Clusters: []ClusterTools{{Cluster: "okd4-ts-3", Allowed: []string{"argocd"}, MCP: mcp}},
	})
	if err != nil {
		t.Fatal(err)
	}
	res := llm.seen[len(llm.seen)-1].Messages[2].Results[0]
	if !res.IsError {
		t.Fatal("tenant-data tool must stay fail-closed when resolution is unavailable")
	}
}

package agent

import (
	"context"
	"strings"
	"testing"
)

func approvalRun(t *testing.T, ctx context.Context) (*fakeMCP, *fakeLLM) {
	t.Helper()
	srv := &fakeMCP{tools: []string{"debug_pod_start"}, selfAuthorized: true, output: "started"}
	llm := &fakeLLM{turns: []Response{
		{Calls: []ToolCall{{ID: "1", Name: "debug_pod_start", Args: map[string]any{"namespace": "team-a", "reason": "dns"}}}},
		{Text: "done"},
	}}
	rules := map[string]ToolRule{"debug_pod_start": {NamespaceArgs: []string{"namespace"}, RequiresApproval: true}}
	_, err := quietAgent(llm, rules, 0).Run(ctx, Input{Query: "q", User: "ali@snapp.cab", Clusters: []ClusterTools{
		{Cluster: "c", Allowed: []string{"team-a"}, MCP: srv},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return srv, llm
}

func TestApprovalToolProposesInsteadOfRunning(t *testing.T) {
	ap := NewApproval()
	srv, llm := approvalRun(t, WithApproval(context.Background(), ap))
	if len(srv.called) != 0 {
		t.Fatal("ran without approval")
	}
	got := ap.Proposed()
	if len(got) != 1 || got[0].Cluster != "c" || got[0].Namespace != "team-a" || got[0].Reason != "dns" {
		t.Fatalf("proposed %+v", got)
	}
	if !strings.Contains(lastResult(llm), "reply \"yes\"") {
		t.Fatalf("model not told to ask: %q", lastResult(llm))
	}
}

func TestApprovedToolRuns(t *testing.T) {
	ap := NewApproval(Proposal{Tool: "debug_pod_start", Cluster: "c", Namespace: "team-a"})
	srv, _ := approvalRun(t, WithApproval(context.Background(), ap))
	if len(srv.called) != 1 {
		t.Fatal("approved call did not run")
	}
}

// A grant is for one namespace: it does not carry over to another.
func TestApprovalIsForItsNamespace(t *testing.T) {
	ap := NewApproval(Proposal{Tool: "debug_pod_start", Cluster: "c", Namespace: "team-b"})
	srv, _ := approvalRun(t, WithApproval(context.Background(), ap))
	if len(srv.called) != 0 {
		t.Fatal("grant for team-b ran in team-a")
	}
}

func TestNoApproverNoRun(t *testing.T) {
	srv, llm := approvalRun(t, context.Background())
	if len(srv.called) != 0 || !strings.Contains(lastResult(llm), "nobody can approve") {
		t.Fatalf("called=%v result=%q", srv.called, lastResult(llm))
	}
}

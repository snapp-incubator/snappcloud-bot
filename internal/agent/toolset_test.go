package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func names(ts []Tool) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name
	}
	return out
}

func group(cluster string, n int) clusterTools {
	g := clusterTools{cluster: cluster}
	for i := 0; i < n; i++ {
		g.tools = append(g.tools, Tool{Name: cluster + "__t" + string(rune('a'+i))})
	}
	return g
}

// Under the cap nothing is reordered or lost.
func TestInterleaveKeepsEverythingUnderTheCap(t *testing.T) {
	got, dropped := interleave([]clusterTools{group("a", 2), group("b", 2)}, 10)
	if want := []string{"a__ta", "a__tb", "b__ta", "b__tb"}; strings.Join(names(got), ",") != strings.Join(want, ",") {
		t.Fatalf("got %v", names(got))
	}
	if len(dropped) != 0 {
		t.Fatalf("dropped %v", dropped)
	}
}

// The failure this exists for: one cluster with a huge server must not consume
// the whole budget and leave another cluster with nothing.
func TestInterleaveGivesEveryClusterAShare(t *testing.T) {
	got, dropped := interleave([]clusterTools{group("big", 20), group("small", 3)}, 8)
	if len(got) != 8 {
		t.Fatalf("want 8 tools, got %d", len(got))
	}
	var big, small int
	for _, n := range names(got) {
		if strings.HasPrefix(n, "big") {
			big++
		} else {
			small++
		}
	}
	if small != 3 {
		t.Fatalf("the small cluster lost tools while the big one had room: %v", names(got))
	}
	if big != 5 {
		t.Fatalf("big cluster share: %d (%v)", big, names(got))
	}
	if dropped["big"] != 15 || dropped["small"] != 0 {
		t.Fatalf("dropped = %v", dropped)
	}
}

func TestToolNoticeNamesPresentUnreachableAndTrimmed(t *testing.T) {
	n := toolNotice([]string{"okd4-box"}, []string{"okd4-teh-1"}, map[string]int{"okd4-box": 12})
	for _, want := range []string{
		"you have tools for okd4-box",
		"okd4-teh-1 did not respond this turn",
		"NOT a limit on the user's access",
		"okd4-box (12)",
	} {
		if !strings.Contains(n, want) {
			t.Fatalf("missing %q in %q", want, n)
		}
	}
	if toolNotice(nil, nil, nil) != "" {
		t.Fatal("no clusters at all should produce no notice")
	}
}

type listFailMCP struct{ fakeMCP }

func (*listFailMCP) ListTools(context.Context) ([]Tool, error) { return nil, errors.New("http 403") }

// A cluster whose servers are down must be named to the model as unreachable —
// the model reading a short tool list is exactly what produced a report saying
// the cluster had no tooling at all.
func TestRunTellsTheModelWhichClustersAreUnreachable(t *testing.T) {
	llm := &fakeLLM{turns: []Response{{Text: "done"}}}
	ag := New(llm, NewEnforcer(nil), nil, 3, DefaultBudgets(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err := ag.Run(context.Background(), Input{
		System: "SYS", Query: "report",
		Clusters: []ClusterTools{
			{Cluster: "okd4-box", Allowed: []string{"team-a"}, MCP: &fakeMCP{tools: []string{"list_pods"}}},
			{Cluster: "okd4-teh-1", Allowed: []string{"team-a"}, MCP: &listFailMCP{}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sys := llm.seen[0].System
	if !strings.Contains(sys, "okd4-teh-1 did not respond this turn") {
		t.Fatalf("unreachable cluster not stated to the model: %q", sys)
	}
	if !strings.HasPrefix(sys, "SYS") {
		t.Fatalf("notice must be appended to the caller's system prompt: %q", sys)
	}
}

// With the cap set low, both clusters still appear.
func TestRunCapsToolsFairlyAcrossClusters(t *testing.T) {
	llm := &fakeLLM{turns: []Response{{Text: "done"}}}
	b := DefaultBudgets()
	b.MaxTools = 3
	ag := New(llm, NewEnforcer(nil), nil, 3, b, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err := ag.Run(context.Background(), Input{
		Query: "q",
		Clusters: []ClusterTools{
			{Cluster: "okd4-box", Alias: "box", Allowed: []string{"team-a"}, MCP: &fakeMCP{tools: []string{"a", "b", "c", "d", "e"}}},
			{Cluster: "okd4-teh-1", Alias: "teh1", Allowed: []string{"team-a"}, MCP: &fakeMCP{tools: []string{"z"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := names(llm.seen[0].Tools)
	if len(got) != 3 {
		t.Fatalf("cap not applied: %v", got)
	}
	var hasTeh1 bool
	for _, n := range got {
		if strings.HasPrefix(n, "teh1__") {
			hasTeh1 = true
		}
	}
	if !hasTeh1 {
		t.Fatalf("the small cluster was crowded out: %v", got)
	}
}

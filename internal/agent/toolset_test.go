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
	n := toolNotice([]string{"okd4-box"}, []string{"okd4-teh-1"}, nil, map[string]int{"okd4-box": 12})
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
	if toolNotice(nil, nil, nil, nil) != "" {
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

// A question that names one cluster keeps that cluster's tools whole; the
// clusters it never mentioned share what is left.
func TestInterleaveServesTheNamedClusterFirst(t *testing.T) {
	named := group("teh1", 30)
	named.preferred = true
	got, dropped := interleave([]clusterTools{group("box", 40), named, group("ts3", 10)}, 40)
	var teh1 int
	for _, n := range names(got) {
		if strings.HasPrefix(n, "teh1") {
			teh1++
		}
	}
	if teh1 != 30 {
		t.Fatalf("named cluster lost tools: %d of 30 (%v)", teh1, names(got))
	}
	if dropped["teh1"] != 0 {
		t.Fatalf("named cluster reported as trimmed: %v", dropped)
	}
	if len(got) != 40 {
		t.Fatalf("budget not filled: %d", len(got))
	}
}

// Every server of a cluster is trimmed evenly: the server configured last —
// in practice the newest one, the metrics server — must not be the one that
// always disappears.
func TestInterleaveTrimsEveryServerNotJustTheLast(t *testing.T) {
	big := group("teh1", 30)
	small := group("teh1", 6) // same cluster, a second server
	got, _ := interleave([]clusterTools{big, small}, 12)
	var fromSmall int
	for _, tl := range got {
		for _, s := range small.tools {
			if tl.Name == s.Name {
				fromSmall++
			}
		}
	}
	if fromSmall < 5 {
		t.Fatalf("the second server kept only %d of 6 tools: %v", fromSmall, names(got))
	}
}

// A cluster the question named is never trimmed, even when its own servers
// together exceed the whole budget. A subset of the right cluster's tools is
// what produced answers that were confident and incomplete; an oversized list
// is at worst the endpoint's problem, and a logged one.
func TestInterleaveNeverTrimsANamedCluster(t *testing.T) {
	k8s := group("teh1", 60)
	k8s.preferred = true
	envoy := group("teh1", 40)
	envoy.preferred = true
	metrics := clusterTools{cluster: "teh1", preferred: true, tools: []Tool{
		{Name: "teh1__query_prometheus"},
		{Name: "teh1__list_datasources"},
	}}
	got, dropped := interleave([]clusterTools{k8s, envoy, metrics, group("box", 40)}, 60)

	var teh1 int
	have := map[string]bool{}
	for _, n := range names(got) {
		have[n] = true
		if strings.HasPrefix(n, "teh1") {
			teh1++
		}
	}
	if teh1 != 102 {
		t.Fatalf("the named cluster was trimmed: %d of 102 tools", teh1)
	}
	for _, want := range []string{"teh1__query_prometheus", "teh1__list_datasources"} {
		if !have[want] {
			t.Fatalf("%s missing; the report has no metrics", want)
		}
	}
	if dropped["teh1"] != 0 {
		t.Fatalf("named cluster reported as trimmed: %v", dropped)
	}
	if dropped["box"] != 40 {
		t.Fatalf("the unnamed cluster should yield entirely: %v", dropped)
	}
}

// The case that produced a report of empty tables: the cluster answered, but
// its metrics server did not. The tools are simply absent, which is exactly
// what a cluster that never had them looks like — so it has to be said.
func TestToolNoticeReportsAPartlyAnsweringCluster(t *testing.T) {
	n := toolNotice([]string{"okd4-teh-1"}, nil, []string{"okd4-teh-1 (okd4-teh-1-5)"}, nil)
	for _, want := range []string{
		"Part of a cluster is missing this turn",
		"okd4-teh-1 (okd4-teh-1-5)",
		"still HAVE those tools",
		"Do not say the cluster has no such tool",
	} {
		if !strings.Contains(n, want) {
			t.Fatalf("missing %q in %q", want, n)
		}
	}
}

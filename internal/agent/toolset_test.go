package agent

import (
	"context"
	"errors"
	"fmt"
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
	n := toolNotice([]string{"okd4-box"}, []string{"okd4-teh-1"}, nil, map[string]int{"okd4-box": 12}, nil)
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
	if toolNotice(nil, nil, nil, nil, nil) != "" {
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

// A question that names one cluster gets the larger share of the budget; the
// clusters it never mentioned divide what is left.
func TestInterleaveServesTheNamedClusterFirst(t *testing.T) {
	named := group("teh1", 30)
	named.preferred = true
	got, _ := interleave([]clusterTools{group("box", 40), named, group("ts3", 10)}, 40)
	count := map[string]int{}
	for _, n := range names(got) {
		count[strings.SplitN(n, "__", 2)[0]]++
	}
	if len(got) != 40 {
		t.Fatalf("budget not filled: %d", len(got))
	}
	if count["teh1"] <= count["box"] {
		t.Fatalf("the named cluster did not get the larger share: %v", count)
	}
	// Served first in every round, so it keeps a tool for each round the budget
	// allows — never a fraction of what an unnamed cluster of the same size got.
	if count["teh1"] < 14 {
		t.Fatalf("named cluster kept only %d of 30: %v", count["teh1"], count)
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

// The budget is absolute, including for a cluster the question named. A named
// cluster used to be appended whole, on the reasoning that an oversized list
// was the endpoint's problem and a loud one — it is not loud. The endpoint
// carries 128 definitions and drops the rest of the array silently, so going
// over does not buy tools, it loses whichever server is configured last. What
// the named cluster gets instead is priority within every round, which keeps
// each of its servers represented — the metrics server above all, since nothing
// else can answer a question about a rate or a peak.
func TestInterleaveKeepsEveryServerOfANamedClusterWithinTheBudget(t *testing.T) {
	k8s := group("teh1", 60)
	k8s.preferred = true
	envoy := group("teh1", 40)
	envoy.preferred = true
	metrics := clusterTools{cluster: "teh1", preferred: true, tools: []Tool{
		{Name: "teh1__query_prometheus"},
		{Name: "teh1__list_datasources"},
	}}
	got, dropped := interleave([]clusterTools{k8s, envoy, metrics, group("box", 40)}, 60)

	if len(got) != 60 {
		t.Fatalf("the budget is absolute: got %d tools for a budget of 60", len(got))
	}
	have := map[string]bool{}
	for _, n := range names(got) {
		have[n] = true
	}
	// The whole point: the smallest server of the named cluster survives a
	// budget its siblings could have eaten on their own.
	for _, want := range []string{"teh1__query_prometheus", "teh1__list_datasources"} {
		if !have[want] {
			t.Fatalf("%s missing; the report has no metrics", want)
		}
	}
	if dropped["teh1"] == 0 {
		t.Fatal("the named cluster was over budget and must be reported as trimmed")
	}
	// The unnamed cluster yields all but its one reserved slot: a question about
	// teh-1 must not make another cluster's server vanish from the list
	// entirely, because "no tools for box" and "box is down" read the same.
	if dropped["box"] != 39 {
		t.Fatalf("the unnamed cluster should yield all but its reserve: %v", dropped)
	}
}

// The case that produced a report of empty tables: the cluster answered, but
// its metrics server did not. The tools are simply absent, which is exactly
// what a cluster that never had them looks like — so it has to be said.
func TestToolNoticeReportsAPartlyAnsweringCluster(t *testing.T) {
	n := toolNotice([]string{"okd4-teh-1"}, nil, []string{"okd4-teh-1 (okd4-teh-1-5)"}, nil, nil)
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

// A model that decides from memory which tools it "should" have reported a
// cluster as having no Prometheus while holding the metrics server's tools —
// and named tools that do not exist anywhere. The notice states the servers
// that answered and how many tools each gave, which is not arguable.
func TestToolNoticeNamesTheServersThatAnswered(t *testing.T) {
	n := toolNotice([]string{"okd4-teh-1"}, nil, nil, nil, map[string][]string{
		"okd4-teh-1": {"openshift-mcp (17)", "cloud-grafana-mcp (15)"},
	})
	for _, want := range []string{
		"okd4-teh-1 answered with openshift-mcp (17), cloud-grafana-mcp (15).",
		"under the name it actually has",
		"not a tool you do not have",
	} {
		if !strings.Contains(n, want) {
			t.Fatalf("missing %q in %q", want, n)
		}
	}
}

// teh-1's real shape on 2026-10-03: six servers advertising 64, 11, 25, 17, 6
// and 15 tools, 138 in all, with grafana-mcp configured last. Under the old
// rule the named cluster went in whole and the endpoint dropped everything past
// its 128th definition — so the bot logged "okd4-teh-1-5 (15)" while the model
// was never sent a single metrics tool, and the daily report explained that the
// cluster had no Prometheus.
func TestInterleaveKeepsTheMetricsServerAtTehOnesRealShape(t *testing.T) {
	sizes := []int{64, 11, 25, 17, 6, 15}
	var groups []clusterTools
	for i, n := range sizes {
		g := clusterTools{cluster: "okd4-teh-1", preferred: true}
		for j := 0; j < n; j++ {
			g.tools = append(g.tools, Tool{Name: fmt.Sprintf("teh1__s%d_t%02d", i, j)})
		}
		groups = append(groups, g)
	}
	// The two docs servers, which no question names.
	groups = append(groups,
		clusterTools{cluster: "docs", tools: []Tool{{Name: "docs__search_docs"}}},
		clusterTools{cluster: "platform-docs", tools: []Tool{{Name: "pdocs__search_docs"}}})

	got, _ := interleave(groups, gatewayToolCeiling)
	if len(got) > gatewayToolCeiling {
		t.Fatalf("offered %d definitions; the endpoint carries %d", len(got), gatewayToolCeiling)
	}
	perServer := map[string]int{}
	for _, tl := range got {
		if i := strings.Index(tl.Name, "_t"); i > 0 {
			perServer[tl.Name[:i]]++
		}
	}
	// grafana-mcp is server 5, the last configured and the smallest but one.
	if perServer["teh1__s5"] != 15 {
		t.Errorf("grafana-mcp kept %d of 15 tools: %v", perServer["teh1__s5"], perServer)
	}
	for i, n := range sizes {
		if perServer[fmt.Sprintf("teh1__s%d", i)] == 0 {
			t.Errorf("server %d (%d tools) was emptied: %v", i, n, perServer)
		}
	}
	// Only the largest server pays for the budget.
	if perServer["teh1__s0"] >= 64 {
		t.Errorf("the budget came from nowhere: server 0 kept %d of 64", perServer["teh1__s0"])
	}
}

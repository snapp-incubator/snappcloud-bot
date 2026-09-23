package brain

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/snapp-incubator/snappcloud-bot/internal/agent"
	"github.com/snapp-incubator/snappcloud-bot/internal/authzclient"
	"github.com/snapp-incubator/snappcloud-bot/internal/mcp"
)

func TestSystemPromptIncludesGuidanceAndScope(t *testing.T) {
	b := &Brain{persona: "PERSONA", system: "SYSTEM", guidance: "TOOL_SKILLS"}
	out := b.systemPrompt(authzclient.Scope{"okd4-teh-1": {Namespaces: []string{"team-a", "team-b"}}}, "")
	if !strings.Contains(out, "PERSONA") || !strings.Contains(out, "SYSTEM") || !strings.Contains(out, "TOOL_SKILLS") {
		t.Fatal("persona + system + guidance must be present")
	}
	if !strings.Contains(out, "okd4-teh-1: team-a, team-b") {
		t.Fatalf("scope not listed: %s", out)
	}
}

// An admin-only global group must be invisible to non-admins: not denied at
// call time, absent from the tool list entirely.
func TestAdminOnlyGlobalGroupHiddenFromNonAdmins(t *testing.T) {
	b := &Brain{
		global:          map[string]agent.MCP{"docs": nil, "platform-docs": nil},
		globalAdminOnly: map[string]bool{"platform-docs": true},
	}

	if got := b.visibleGlobal(false); len(got) != 1 || got[0] != "docs" {
		t.Errorf("non-admin sees %v, want [docs]", got)
	}
	if got := b.visibleGlobal(true); len(got) != 2 {
		t.Errorf("admin sees %v, want both groups", got)
	}
}

func TestHasClusterWide(t *testing.T) {
	none := authzclient.Scope{"a": {Namespaces: []string{"x"}}, "b": {Namespaces: []string{"y"}}}
	if none.HasClusterWide() {
		t.Error("no cluster-wide grant reported as admin")
	}
	some := authzclient.Scope{"a": {Namespaces: []string{"x"}}, "b": {ClusterWide: true}}
	if !some.HasClusterWide() {
		t.Error("cluster-wide grant on one cluster not reported")
	}
}

func namedBrain() *Brain {
	return &Brain{clusters: map[string]*clusterMCP{
		"okd4-teh-1":      {alias: "teh1", names: clusterNames("okd4-teh-1", "teh1", nil)},
		"okd4-teh-2":      {alias: "teh2", names: clusterNames("okd4-teh-2", "teh2", nil)},
		"okd4-snappgroup": {alias: "snappgroup", names: clusterNames("okd4-snappgroup", "snappgroup", []string{"sg"})},
		"okd4-box":        {alias: "box", names: clusterNames("okd4-box", "box", nil)},
	}}
}

// The names alerts, datasources and people use for a cluster all land on the
// configured one — and "snappgroup-teh-1" lands on snappgroup, not teh-1,
// although it contains both.
func TestResolveClusterMapsOutsideNamesToConfiguredCluster(t *testing.T) {
	b := namedBrain()
	cases := map[string]string{
		"okd4-snappgroup":  "okd4-snappgroup",
		"snappgroup-teh-1": "okd4-snappgroup",
		"SnappGroup":       "okd4-snappgroup",
		"sg":               "okd4-snappgroup",
		"teh-1":            "okd4-teh-1",
		"teh1":             "okd4-teh-1",
		"okd4.teh-2":       "okd4-teh-2",
		"box-teh-2":        "okd4-box",
		"prod-box":         "okd4-box",
	}
	for in, want := range cases {
		got, ok := b.ResolveCluster(in)
		if !ok || got != want {
			t.Errorf("ResolveCluster(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	if got, ok := b.ResolveCluster("nowhere-1"); ok {
		t.Errorf("unknown label resolved to %q", got)
	}
	if _, ok := b.ResolveCluster(""); ok {
		t.Error("empty label resolved")
	}
}

// The prompt lists the cluster's other names beside it and says cluster-admins
// see everything — and it forbids the one sentence that was actually said.
func TestSystemPromptNamesAliasesAndAdminAndForbidsAccessVerdicts(t *testing.T) {
	b := namedBrain()
	b.persona, b.system = "P", "S"
	out := b.systemPrompt(authzclient.Scope{
		"okd4-snappgroup": {Namespaces: []string{"team-a"}, ClusterWide: true},
		"okd4-teh-1":      {Namespaces: []string{"team-a"}},
	}, "")
	for _, want := range []string{
		"- okd4-snappgroup (also called sg, snappgroup) — CLUSTER-ADMIN",
		"- okd4-teh-1 (also called teh-1, teh1): team-a",
		"Never tell the user they lack access to a cluster",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in prompt:\n%s", want, out)
		}
	}
}

func TestClustersNamedInQuery(t *testing.T) {
	b := namedBrain()
	cases := map[string][]string{
		"Daily platform report for cluster okd4-teh-1 only": {"okd4-teh-1"},
		"compare snappgroup-teh-1 and box":                  {"okd4-snappgroup", "okd4-box"},
		"why is my pod restarting in team-a":                nil,
		"the box is full":                                   {"okd4-box"}, // a real word that is also a cluster name
		"check teh1 and TEH-2":                              {"okd4-teh-1", "okd4-teh-2"},
		"sandbox namespaces are noisy":                      nil, // substring must not match
	}
	for q, want := range cases {
		got := b.clustersNamedIn(q)
		if len(got) != len(want) {
			t.Errorf("clustersNamedIn(%q) = %v, want %v", q, got, want)
			continue
		}
		for _, w := range want {
			if !got[w] {
				t.Errorf("clustersNamedIn(%q) = %v, missing %s", q, got, w)
			}
		}
	}
}

// The cluster an alert names must be the preferred one, so an investigation
// keeps that cluster's tools when the list is trimmed. The alert prompt writes
// the cluster under its configured name, which is what must match.
func TestAlertScopeLineNamesAPreferredCluster(t *testing.T) {
	b := namedBrain()
	q := "Scope: namespaces team-a; clusters okd4-snappgroup (the alert's label for it is \"snappgroup-teh-1\")"
	got := b.clustersNamedIn(q)
	if !got["okd4-snappgroup"] || len(got) != 1 {
		t.Fatalf("alert scope line did not resolve to one cluster: %v", got)
	}
}

// The built-in system prompt is what runs in production — helm overrides the
// persona and the tool guidance, not this — so the rules that keep an answer
// an answer have to be in it.
func TestDefaultSystemForbidsNarrationAndHandCounting(t *testing.T) {
	for _, want := range []string{
		"The reply is the result.",
		"Counting and totals come from a query, never from reading a list.",
		"Never output your chain-of-thought",
	} {
		if !strings.Contains(defaultSystem, want) {
			t.Errorf("default system prompt is missing: %q", want)
		}
	}
}

type stubMCP struct{ tools []string }

func (s stubMCP) ListTools(context.Context) ([]agent.Tool, error) {
	out := make([]agent.Tool, len(s.tools))
	for i, n := range s.tools {
		out[i] = agent.Tool{Name: n}
	}
	return out, nil
}
func (stubMCP) CallTool(context.Context, string, map[string]any) (string, error) { return "", nil }

type recordingAgent struct{ in agent.Input }

func (r *recordingAgent) Run(_ context.Context, in agent.Input) (string, error) {
	r.in = in
	return "ok", nil
}

// A question about one cluster must carry only that cluster's tools. Sending
// the other five alongside is what pushed the list past what the endpoint
// carries, and the tools that went missing were the ones the answer needed.
func TestAnswerOffersOnlyTheNamedClustersTools(t *testing.T) {
	b := namedBrain()
	for name, cm := range b.clusters {
		cm.mcp = stubMCP{tools: []string{"list_pods", "query_prometheus"}}
		b.clusters[name] = cm
	}
	rec := &recordingAgent{}
	b.agent = rec
	b.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	scope := authzclient.Scope{
		"okd4-teh-1":      {Namespaces: []string{"team-a"}},
		"okd4-teh-2":      {Namespaces: []string{"team-a"}},
		"okd4-box":        {Namespaces: []string{"team-a"}},
		"okd4-snappgroup": {Namespaces: []string{"team-a"}},
	}

	if _, err := b.Answer(context.Background(), scope, "u", "why is payyar failing on teh-1?", "", "r1"); err != nil {
		t.Fatal(err)
	}
	if len(rec.in.Clusters) != 1 || rec.in.Clusters[0].Cluster != "okd4-teh-1" {
		var got []string
		for _, c := range rec.in.Clusters {
			got = append(got, c.Cluster)
		}
		t.Fatalf("clusters offered = %v, want only okd4-teh-1", got)
	}

	// A question naming no cluster still reaches every cluster the user has.
	if _, err := b.Answer(context.Background(), scope, "u", "are any of my pods crashing?", "", "r2"); err != nil {
		t.Fatal(err)
	}
	if len(rec.in.Clusters) != 4 {
		t.Fatalf("a question naming no cluster must keep all of them, got %d", len(rec.in.Clusters))
	}
}

// The prompt must teach a method, not a catalogue of incidents: the next
// problem will not be one anybody wrote down in advance.
func TestDefaultSystemTeachesInvestigationNotCases(t *testing.T) {
	for _, want := range []string{
		"WHAT THE USER REPORTS IS EVIDENCE",
		"WHERE ELSE this symptom could be produced",
		"any of them can answer it",
		"Aggregate numbers hide the case you are chasing",
		"not from a list of known cases",
	} {
		if !strings.Contains(defaultSystem, want) {
			t.Errorf("default system prompt is missing: %q", want)
		}
	}
}

// The agent asks its tool source which of its servers failed. If the adapter
// stops forwarding that, a partly-answering cluster goes back to looking like
// a cluster that never had the tool — silently, and only in production.
func TestMuxAdapterReportsPerServerFailures(t *testing.T) {
	mux := mcp.NewMux(slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux.Add("teh-1-5", mcp.New("http://127.0.0.1:1/mcp", "", false, time.Second))
	a := muxAdapter{mux}

	var src interface{ ListFailures() []string } = a
	if _, err := a.ListTools(context.Background()); err == nil {
		t.Fatal("a mux whose only server is unreachable must return an error")
	}
	if got := src.ListFailures(); len(got) != 1 || got[0] != "teh-1-5" {
		t.Fatalf("per-server failure not forwarded: %v", got)
	}
}

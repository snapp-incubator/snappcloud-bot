package agent

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestAnnouncesMoreWork(t *testing.T) {
	unfinished := []string{
		"The Service has no selector and points at an external IP. Let me get the Envoy listener filter to see if there are also per-route limits, and check Hubble flows to confirm.",
		"I'll now query the restart counts for that namespace.",
		"Next, I will verify the certificate chain.",
		"That explains the drop. Let's look at the policy.",
		"I need to check the upstream cluster config.",
	}
	for _, s := range unfinished {
		if !announcesMoreWork(s) {
			t.Errorf("not detected as unfinished: %q", s)
		}
	}
	finished := []string{
		"The 413 comes from an nginx outside the cluster, reached through a selectorless Service. Raise client_max_body_size there.",
		"No packets are being dropped for that namespace. Let me know if you want the flows for a different pod.",
		"I checked the listener and the route; neither sets a body limit.",
		"",
		"Pod api-1 is OOMKilled every few minutes; its limit is 256Mi and its peak working set is 254Mi.",
	}
	for _, s := range finished {
		if announcesMoreWork(s) {
			t.Errorf("complete answer treated as unfinished: %q", s)
		}
	}
	// Narration in the middle of a real answer is not an announcement.
	long := "Let me check the routes first.\n\n" + strings.Repeat("The route has no body limit configured. ", 20) +
		"The cause is the external nginx."
	if announcesMoreWork(long) {
		t.Error("only the tail of an answer should be examined")
	}
}

// The turn that produced this: the model found the cause, said it would check
// two more things, called nothing, and the half-investigation was posted.
func TestRunSendsBackATurnThatAnnouncesWorkItDidNotDo(t *testing.T) {
	llm := &fakeLLM{turns: []Response{
		{Text: "The Service has no selector; its endpoint is an external IP. Let me get the Envoy listener filter to confirm."},
		{Calls: []ToolCall{{ID: "1", Name: "c__envoy_listeners", Args: map[string]any{}}}},
		{Text: "The 413 comes from an nginx outside the cluster. Raise client_max_body_size there."},
	}}
	m := &fakeMCP{tools: []string{"envoy_listeners"}}
	ag := New(llm, NewEnforcer(nil), nil, 6, DefaultBudgets(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	out, err := ag.Run(context.Background(), Input{
		Query:    "why 413?",
		Clusters: []ClusterTools{{Cluster: "c", Allowed: []string{"team-a"}, MCP: m}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "nginx outside the cluster") {
		t.Fatalf("the investigation was posted half-finished: %q", out)
	}
	if len(m.called) != 1 {
		t.Fatalf("the announced tool call was never made: %v", m.called)
	}
	sent := llm.seen[1].Messages[len(llm.seen[1].Messages)-1]
	if !strings.Contains(sent.Text, "either make the calls now or write the finished answer") {
		t.Fatalf("the model was not asked to continue: %q", sent.Text)
	}
}

// A model that keeps promising and never calls must not spin: two nudges, then
// the answer is demanded rather than its deliberation posted.
func TestRunStopsNudgingAfterTwoTries(t *testing.T) {
	llm := &fakeLLM{turns: []Response{
		{Text: "Let me check the routes."},
		{Text: "Let me check the listener."},
		{Text: "Let me check the endpoints."},
		{Text: "The listener caps bodies at 1 MiB.\n\nNot checked: Hubble flows."},
	}}
	ag := New(llm, NewEnforcer(nil), nil, 6, DefaultBudgets(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	out, err := ag.Run(context.Background(), Input{
		Query:    "why 413?",
		Clusters: []ClusterTools{{Cluster: "c", Allowed: []string{"team-a"}, MCP: &fakeMCP{tools: []string{"t"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1 MiB") {
		t.Fatalf("the demanded answer was not returned: %q", out)
	}
	// two nudged rounds, then one demand — four completions, no more.
	if len(llm.seen) != 4 {
		t.Fatalf("expected two nudges and one demand, got %d completions", len(llm.seen))
	}
}

// The wordings a model actually uses are unbounded, so the detector matches
// the subject — "let me", "I'll", "I need to" — rather than a list of verbs
// that is always one entry short. Each of these ended a real turn that called
// no tool and was posted as an answer.
func TestAnnouncesMoreWorkMatchesAnyIntent(t *testing.T) {
	for _, s := range []string{
		"The HTTPProxy data is enormous. Let me move on and complete the report based on what I have. Let me make a focused final pass to gather the rest with limited budget.",
		"I have enough to proceed. Next, I compile the tables.",
		"I'm going to pull the remaining sections together.",
		"I have to narrow this query first.",
		"I want to double-check the listener config.",
		"Let's wrap this up with the storage numbers.",
	} {
		if !announcesMoreWork(s) {
			t.Errorf("not detected: %q", s)
		}
	}
	for _, s := range []string{
		"The 413 comes from an nginx outside the cluster. Raise client_max_body_size there.",
		"No packets are dropped for that namespace. Let me know if you want another pod.",
		"Nothing is pending. Let us know if that changes.",
		"Pod api-1 is OOMKilled every few minutes; its limit is 256Mi.",
	} {
		if announcesMoreWork(s) {
			t.Errorf("false positive: %q", s)
		}
	}
}

// Narrating the work is no more an answer than announcing it. This turn was
// posted verbatim to a channel that had asked for a report.
func TestAnnouncesMoreWorkCatchesProgressNarration(t *testing.T) {
	for _, s := range []string{
		"Stop. I have spent my tool budget on tool exploration and gathered the structural data the user asked for. I'll write what I have, mark what I could not compute within budget, and stop.",
		"I have gathered the node data. The operational metrics need PromQL I do not have the tool to execute.",
		"The HTTPProxy data is enormous. Let me move on and complete the report based on what I have.",
	} {
		if !announcesMoreWork(s) {
			t.Errorf("not detected: %q", s)
		}
	}
	for _, s := range []string{
		"237 nodes are Ready, none NotReady. Three namespaces are above 90% of their CPU quota: team-a, team-b, team-c.",
		"No metrics tool answered on this cluster, so every measured cell is n/a. Let me know if you want the Kubernetes-side counts instead.",
	} {
		if announcesMoreWork(s) {
			t.Errorf("false positive: %q", s)
		}
	}
}

// After its nudges are spent the model must not have its deliberation posted:
// the answer is demanded once more, with no tools attached.
func TestRunDemandsTheAnswerWhenNudgesAreSpent(t *testing.T) {
	llm := &fakeLLM{turns: []Response{
		{Text: "Let me gather the rest."},
		{Text: "I have spent my tool budget. I'll write what I have."},
		{Text: "Let me make one more pass."},
		{Text: "## Daily report\n\n237 nodes Ready. Quotas above 90%: team-a.\n\nNot checked: ingress rates."},
	}}
	ag := New(llm, NewEnforcer(nil), nil, 8, DefaultBudgets(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	out, err := ag.Run(context.Background(), Input{
		Query:    "daily report",
		Clusters: []ClusterTools{{Cluster: "c", Allowed: []string{"team-a"}, MCP: &fakeMCP{tools: []string{"t"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "237 nodes Ready") {
		t.Fatalf("deliberation was posted instead of the report: %q", out)
	}
	last := llm.seen[len(llm.seen)-1]
	if len(last.Tools) != 0 {
		t.Fatalf("the demand must carry no tools, got %d", len(last.Tools))
	}
	if !strings.Contains(last.Messages[len(last.Messages)-1].Text, "Do not describe your own progress") {
		t.Fatalf("demand instruction missing: %q", last.Messages[len(last.Messages)-1].Text)
	}
}

func TestMostlyUnmeasured(t *testing.T) {
	empty := `## 1. Namespaces
| metric | value | query |
|---|---|---|
| in-scope | n/a | not run |
| zero pods | n/a | not run |
## 2. CPU
| namespace | usage | requests |
|---|---|---|
| n/a | n/a | n/a |
| n/a | n/a | n/a |
Expression: not run.`
	if !mostlyUnmeasured(empty) {
		t.Fatal("an all-unmeasured report was not recognised")
	}
	real := `## 1. Namespaces
| metric | value |
|---|---|
| in-scope | 280 |
| zero pods | 21 |
## 2. CPU
| namespace | usage | requests |
|---|---|---|
| nats-production | 16.7 | 24.0 |
| baly-ode-central | 9.1 | 12.0 |
PV usage is n/a: kubelet_volume_stats_used_bytes returned nothing.`
	if mostlyUnmeasured(real) {
		t.Fatal("a filled report with one honest gap was treated as empty")
	}
	if mostlyUnmeasured("No metrics tool answered. n/a for everything, n/a, n/a, n/a, n/a, n/a, n/a, n/a, n/a") {
		t.Fatal("prose is not a report")
	}
}

// The run that produced this: probe the datasource, then write every table as
// "not run" while thirty rounds were still available.
func TestRunSendsBackAReportWithNothingInIt(t *testing.T) {
	empty := "| a | n/a | not run |\n| b | n/a | not run |\n| c | n/a | not run |\n" +
		strings.Repeat("| x | n/a |\n", 8)
	llm := &fakeLLM{turns: []Response{
		{Text: empty},
		{Calls: []ToolCall{{ID: "1", Name: "c__query_prometheus", Args: map[string]any{}}}},
		{Text: "| ns | cpu |\n| nats-production | 16.7 |\n| baly-ode-central | 9.1 |"},
	}}
	ag := New(llm, NewEnforcer(nil), nil, 8, DefaultBudgets(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	out, err := ag.Run(context.Background(), Input{
		Query:    "daily report",
		Clusters: []ClusterTools{{Cluster: "c", Allowed: []string{"team-a"}, MCP: &fakeMCP{tools: []string{"query_prometheus"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "nats-production") {
		t.Fatalf("the empty report was posted: %q", out)
	}
	sent := llm.seen[1].Messages[len(llm.seen[1].Messages)-1].Text
	if !strings.Contains(sent, "report with nothing in it") {
		t.Fatalf("wrong instruction: %q", sent)
	}
}

func TestLooksLikePreamble(t *testing.T) {
	preambles := []struct {
		text  string
		calls int
	}{
		{"Now the main Prometheus batch (legacy datasource):", 6},
		{"Here are the results:", 4},
		{"**Datasource proved. Running the capacity queries:**", 3},
		{"Done.", 5},
	}
	for _, c := range preambles {
		if !looksLikePreamble(c.text, c.calls) {
			t.Errorf("not detected: %q", c.text)
		}
	}
	answers := []struct {
		text  string
		calls int
	}{
		{"No packets are dropped for that namespace.", 0},
		{"The 413 comes from an nginx outside the cluster. Raise client_max_body_size there; nothing in the cluster can change it.", 8},
		{"## Capacity\n\n| ns | cpu |\n|---|---|\n| nats-production | 103.0 |\n\nExpression: sum by (namespace) (rate(container_cpu_usage_seconds_total[5m]))", 9},
	}
	for _, c := range answers {
		if looksLikePreamble(c.text, c.calls) {
			t.Errorf("false positive: %q", c.text)
		}
	}
}

// The turn that produced this: a daily report posted as one line promising a
// batch of queries that never ran.
func TestRunSendsBackAPreamble(t *testing.T) {
	llm := &fakeLLM{turns: []Response{
		{Calls: []ToolCall{{ID: "1", Name: "c__query_prometheus", Args: map[string]any{}}}},
		{Text: "Now the main Prometheus batch (legacy datasource):"},
		{Text: "## Capacity\n\n| ns | cpu |\n|---|---|\n| nats-production | 103.0 |"},
	}}
	ag := New(llm, NewEnforcer(nil), nil, 8, DefaultBudgets(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	out, err := ag.Run(context.Background(), Input{
		Query:    "capacity report",
		Clusters: []ClusterTools{{Cluster: "c", Allowed: []string{"team-a"}, MCP: &fakeMCP{tools: []string{"query_prometheus"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "nats-production") {
		t.Fatalf("the preamble was posted: %q", out)
	}
	sent := llm.seen[2].Messages[len(llm.seen[2].Messages)-1].Text
	if !strings.Contains(sent, "the sentence before the work") {
		t.Fatalf("wrong instruction: %q", sent)
	}
}

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
	if !strings.Contains(sent.Text, "Either make those calls now") {
		t.Fatalf("the model was not asked to continue: %q", sent.Text)
	}
}

// A model that keeps promising and never calls must not spin.
func TestRunStopsNudgingAfterTwoTries(t *testing.T) {
	llm := &fakeLLM{turns: []Response{
		{Text: "Let me check the routes."},
		{Text: "Let me check the listener."},
		{Text: "Let me check the endpoints."},
	}}
	ag := New(llm, NewEnforcer(nil), nil, 6, DefaultBudgets(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	out, err := ag.Run(context.Background(), Input{
		Query:    "why 413?",
		Clusters: []ClusterTools{{Cluster: "c", Allowed: []string{"team-a"}, MCP: &fakeMCP{tools: []string{"t"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != "Let me check the endpoints." {
		t.Fatalf("expected the third answer after two nudges, got %q", out)
	}
	if len(llm.seen) != 3 {
		t.Fatalf("expected exactly two nudges, got %d completions", len(llm.seen))
	}
}

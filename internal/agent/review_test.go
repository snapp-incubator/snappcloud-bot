package agent

import (
	"context"
	"strings"
	"testing"
)

const draftReport = "## Daily report\n\n" +
	"Act today: ~1,200 invalid HTTPProxies.\n\n" +
	"| status | count |\n|---|---|\n| invalid | 142 |\n"

func reviewRun(t *testing.T, review bool, reviewReply Response) (string, *fakeLLM) {
	t.Helper()
	llm := &fakeLLM{turns: []Response{
		{Calls: []ToolCall{{ID: "1", Name: "okd4-ts-3__list_invalid_httpproxies", Args: map[string]any{"namespace": "team-a"}}}},
		{Text: draftReport},
		reviewReply,
	}}
	mcp := &fakeMCP{tools: []string{"list_invalid_httpproxies"}, output: `{"invalid":142}`}
	out, err := newAgent(llm).Run(context.Background(), Input{
		Query: "report", Review: review,
		Clusters: []ClusterTools{{Cluster: "okd4-ts-3", Allowed: []string{"team-a"}, MCP: mcp}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return out, llm
}

// The case it exists for: a summary that contradicts its own table, checked
// against the tool result that holds the real figure.
func TestReviewReplacesADraftWithItsCorrection(t *testing.T) {
	fixed := strings.Replace(draftReport, "~1,200", "142", 1)
	out, llm := reviewRun(t, true, Response{Text: fixed})
	if out != strings.TrimSpace(fixed) {
		t.Fatalf("the corrected report was not returned (%d calls):\n%s", len(llm.seen), out)
	}
	last := llm.seen[len(llm.seen)-1]
	if len(last.Tools) != 0 {
		t.Error("a review must not be able to call tools: it corrects, it does not investigate")
	}
	msgs := last.Messages
	if msgs[len(msgs)-1].Role != "user" || !strings.Contains(msgs[len(msgs)-1].Text, "the table's source decides") {
		t.Error("the review instruction is not the last turn")
	}
	if msgs[len(msgs)-2].Role != "assistant" || msgs[len(msgs)-2].Text != draftReport {
		t.Error("the draft is not what the review was asked to check")
	}
	// The evidence has to be in front of it, or there is nothing to check against.
	var sawResult bool
	for _, m := range msgs {
		for _, r := range m.Results {
			if strings.Contains(r.Content, `"invalid":142`) {
				sawResult = true
			}
		}
	}
	if !sawResult {
		t.Error("the review was not given the tool results")
	}
	// Turns must alternate: the draft must not follow the model's own final turn.
	for i := 1; i < len(msgs); i++ {
		if msgs[i].Role == msgs[i-1].Role {
			t.Fatalf("turns %d and %d are both %q", i-1, i, msgs[i].Role)
		}
	}
}

// Interactive answers are never reviewed: the asker is there, and waiting.
func TestNoReviewUnlessAsked(t *testing.T) {
	out, llm := reviewRun(t, false, Response{Text: "unused"})
	if out != draftReport || len(llm.seen) != 2 {
		t.Fatalf("an unreviewed run made %d calls and returned %q", len(llm.seen), out)
	}
}

// Every way a review can go wrong leaves the draft in place. A review is only
// ever allowed to make an answer better or leave it as it was.
func TestReviewNeverTradesTheDraftForSomethingWorse(t *testing.T) {
	for name, reply := range map[string]Response{
		"empty":          {Text: ""},
		"shrank":         {Text: "## Daily report\n\n| a |\n|---|\n"},
		"lost-structure": {Text: strings.Repeat("The report above is correct and needs no changes. ", 4)},
	} {
		if out, _ := reviewRun(t, true, reply); out != draftReport {
			t.Errorf("%s: the draft was replaced by %q", name, out)
		}
	}
}

package agent

import (
	"strings"
	"testing"
)

func runes(n int, c rune) string { return strings.Repeat(string(c), n) }

func convSize(msgs []Turn) int {
	n := 0
	for _, m := range msgs {
		n += len([]rune(m.Text))
		for _, r := range m.Results {
			n += len([]rune(r.Content))
		}
	}
	return n
}

// Several tools answering at once must not blow the round budget between them,
// and a small result must not be truncated to pay for a large one.
func TestCapRoundSharesTheBudget(t *testing.T) {
	results := []ToolResult{
		{CallID: "a", Content: runes(150_000, 'a')},
		{CallID: "b", Content: runes(150_000, 'b')},
		{CallID: "c", Content: "short"},
	}
	out := capRound(results)

	total := 0
	for _, r := range out {
		total += len([]rune(r.Content))
	}
	if total > maxRoundRunes+len([]rune(droppedNotice))*len(out) {
		t.Errorf("round still over budget: %d runes", total)
	}
	if out[2].Content != "short" {
		t.Errorf("a small result was truncated to pay for a large one: %q", out[2].Content)
	}
	for _, r := range out[:2] {
		if !strings.Contains(r.Content, "truncated") {
			t.Error("an oversized result was not marked as truncated")
		}
	}
}

// A round that fits must be left exactly alone.
func TestCapRoundLeavesSmallRoundsAlone(t *testing.T) {
	results := []ToolResult{{CallID: "a", Content: "one"}, {CallID: "b", Content: "two"}}
	out := capRound(results)
	if out[0].Content != "one" || out[1].Content != "two" {
		t.Errorf("a round within budget was modified: %+v", out)
	}
}

// The transcript is trimmed from the OLDEST end: the recent rounds are what the
// model is reasoning about. Structure must survive — a result without its
// CallID is the "invalid conversation shape" the API rejects outright.
func TestTrimConversationDropsOldestAndKeepsShape(t *testing.T) {
	msgs := []Turn{
		{Role: "user", Text: "why is it broken?"},
		{Role: "assistant", Calls: []ToolCall{{ID: "1"}}},
		{Role: "user", Results: []ToolResult{{CallID: "1", Content: runes(200_000, 'o')}}},
		{Role: "assistant", Calls: []ToolCall{{ID: "2"}}},
		{Role: "user", Results: []ToolResult{{CallID: "2", Content: runes(200_000, 'm')}}},
		{Role: "assistant", Calls: []ToolCall{{ID: "3"}}},
		{Role: "user", Results: []ToolResult{{CallID: "3", Content: runes(200_000, 'n')}}},
	}

	dropped := trimConversation(msgs)
	if dropped == 0 {
		t.Fatal("an oversized conversation was not trimmed")
	}
	if convSize(msgs) > maxConvRunes {
		t.Errorf("still over budget after trimming: %d runes", convSize(msgs))
	}
	if msgs[2].Results[0].Content != droppedNotice {
		t.Error("the OLDEST result should be the first dropped")
	}
	if msgs[6].Results[0].Content == droppedNotice {
		t.Error("the newest result was dropped; it is what the model is reasoning about")
	}
	for _, m := range msgs {
		for _, r := range m.Results {
			if r.CallID == "" {
				t.Fatal("a result lost its CallID; the API rejects that conversation shape")
			}
		}
	}
	if msgs[0].Text != "why is it broken?" {
		t.Error("the user's question must never be dropped")
	}
}

// A conversation within budget is untouched, so ordinary turns pay nothing.
func TestTrimConversationLeavesSmallOnesAlone(t *testing.T) {
	msgs := []Turn{
		{Role: "user", Text: "hello"},
		{Role: "user", Results: []ToolResult{{CallID: "1", Content: "a small result"}}},
	}
	if n := trimConversation(msgs); n != 0 {
		t.Errorf("trimmed %d results from a conversation within budget", n)
	}
	if msgs[1].Results[0].Content != "a small result" {
		t.Error("content changed")
	}
}

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
	out := capRound(results, DefaultBudgets().RoundRunes)

	total := 0
	for _, r := range out {
		total += len([]rune(r.Content))
	}
	if total > DefaultBudgets().RoundRunes+len([]rune(droppedNotice))*len(out) {
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
	out := capRound(results, DefaultBudgets().RoundRunes)
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

	dropped := trimConversation(msgs, DefaultBudgets().ConversationRunes)
	if dropped == 0 {
		t.Fatal("an oversized conversation was not trimmed")
	}
	if convSize(msgs) > DefaultBudgets().ConversationRunes {
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
	if n := trimConversation(msgs, DefaultBudgets().ConversationRunes); n != 0 {
		t.Errorf("trimmed %d results from a conversation within budget", n)
	}
	if msgs[1].Results[0].Content != "a small result" {
		t.Error("content changed")
	}
}

// A result too large to authorize is withheld, not truncated — because
// truncating JSON leaves something the filter cannot parse, and an unparseable
// result is passed through UNFILTERED. Truncating to save memory would turn a
// memory guard into a data leak, which is why the caller refuses instead.
func TestTruncatedJSONWouldBypassTheFilter(t *testing.T) {
	full := `[{"namespace":"team-b","note":"another tenant"},{"namespace":"team-a","note":"mine"}]`

	// Whole and valid: the other tenant's record is dropped.
	out, removed, _ := FilterResult(full, map[string]bool{"team-a": true}, nil)
	if removed != 1 || strings.Contains(out, "team-b") {
		t.Fatalf("valid JSON not filtered: removed=%d out=%s", removed, out)
	}

	// Truncated: unparseable, so it passes through with the other tenant intact.
	truncated := full[:len(full)/2]
	body, n, blocked := FilterResult(truncated, map[string]bool{"team-a": true}, nil)
	if n != 0 || blocked {
		t.Fatalf("expected pass-through for unparseable input, got removed=%d blocked=%v", n, blocked)
	}
	if !strings.Contains(body, "team-b") {
		t.Fatal("fixture no longer demonstrates the hazard")
	}
}

// Budgets are configuration, and a zero field must fall back rather than
// disabling the cap — a zero budget would truncate every result to nothing.
func TestBudgetsFallBackToDefaults(t *testing.T) {
	b := Budgets{ResultRunes: 250_000}
	b.applyDefaults()

	if b.ResultRunes != 250_000 {
		t.Errorf("configured value overwritten: %d", b.ResultRunes)
	}
	d := DefaultBudgets()
	if b.RoundRunes != d.RoundRunes || b.ConversationRunes != d.ConversationRunes || b.FilterBytes != d.FilterBytes {
		t.Errorf("unset fields did not take defaults: %+v", b)
	}

	var zero Budgets
	zero.applyDefaults()
	if zero != d {
		t.Errorf("an empty Budgets must equal the defaults, got %+v", zero)
	}
}

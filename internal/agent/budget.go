package agent

import "fmt"

// Three budgets keep the transcript under the model's context limit:
// ResultRunes (one result), RoundRunes (one round), ConversationRunes (the
// whole transcript, oldest results dropped first).
type Budgets struct {
	// ResultRunes caps one tool result fed back to the model.
	ResultRunes int
	// RoundRunes caps all results from one round, where several tools answer in
	// parallel.
	RoundRunes int
	// ConversationRunes caps the whole transcript. This one is bounded by the
	// MODEL rather than by memory: exceed its context window and the endpoint
	// answers HTTP 400 on every retry.
	ConversationRunes int
	// FilterBytes is the largest result the bot will authorize. Filtering parses
	// a result twice and JSON becomes Go values at several times the size of its
	// text, so this is a memory bound.
	FilterBytes int
	// MaxTools caps tool definitions per request; set it to what the LLM endpoint
	// carries, since an endpoint that cannot carry them all drops the tail silently.
	MaxTools int
}

// DefaultBudgets are sized for a 1Gi container and a large-context model.
func DefaultBudgets() Budgets {
	return Budgets{
		ResultRunes:       100_000,
		RoundRunes:        200_000,
		ConversationRunes: 400_000,
		FilterBytes:       4 << 20,
		MaxTools:          120,
	}
}

// ApplyDefaults fills in the zero values, so a caller can report the budgets
// that will actually be used rather than the ones it happened to configure.
func (b *Budgets) ApplyDefaults() { b.applyDefaults() }

func (b *Budgets) applyDefaults() {
	d := DefaultBudgets()
	if b.ResultRunes <= 0 {
		b.ResultRunes = d.ResultRunes
	}
	if b.RoundRunes <= 0 {
		b.RoundRunes = d.RoundRunes
	}
	if b.ConversationRunes <= 0 {
		b.ConversationRunes = d.ConversationRunes
	}
	if b.FilterBytes <= 0 {
		b.FilterBytes = d.FilterBytes
	}
	if b.MaxTools <= 0 {
		b.MaxTools = d.MaxTools
	}
}

const droppedNotice = "[earlier tool output dropped to fit the model's context budget — " +
	"re-run the tool if you still need it]"

// capRound shrinks a round's results to fit maxRoundRunes, taking from the
// largest first so one huge result cannot crowd out several small ones.
func capRound(results []ToolResult, maxRoundRunes int) []ToolResult {
	total := 0
	for _, r := range results {
		total += len([]rune(r.Content))
	}
	if total <= maxRoundRunes || len(results) == 0 {
		return results
	}
	// A fair share each, then hand back what the small ones did not use.
	share := maxRoundRunes / len(results)
	spare := 0
	for _, r := range results {
		if n := len([]rune(r.Content)); n < share {
			spare += share - n
		}
	}
	over := 0
	for _, r := range results {
		if len([]rune(r.Content)) > share {
			over++
		}
	}
	if over > 0 {
		share += spare / over
	}
	for i, r := range results {
		runes := []rune(r.Content)
		if len(runes) > share {
			results[i].Content = string(runes[:share]) +
				fmt.Sprintf("\n[truncated to %d characters: several tools answered at once; narrow the query]", share)
		}
	}
	return results
}

// trimConversation drops the oldest tool-result content until the transcript
// fits maxConvRunes, and reports how many results it emptied. Structure is
// preserved: every result keeps its CallID, because a tool result with no
// matching call is exactly the "invalid conversation shape" the API rejects.
func trimConversation(msgs []Turn, maxConvRunes int) int {
	total := 0
	for _, m := range msgs {
		total += len([]rune(m.Text))
		for _, r := range m.Results {
			total += len([]rune(r.Content))
		}
	}
	if total <= maxConvRunes {
		return 0
	}

	dropped := 0
	for i := range msgs { // oldest first
		for j := range msgs[i].Results {
			content := msgs[i].Results[j].Content
			if content == droppedNotice || content == "" {
				continue
			}
			total -= len([]rune(content)) - len([]rune(droppedNotice))
			msgs[i].Results[j].Content = droppedNotice
			dropped++
			if total <= maxConvRunes {
				return dropped
			}
		}
	}
	return dropped
}

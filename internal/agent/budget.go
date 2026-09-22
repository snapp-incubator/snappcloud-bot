package agent

import "fmt"

// The model rejects a conversation that is too large with HTTP 400 ("invalid
// parameters or conversation shape ... try a shorter prompt"), which is a dead
// end: it fails identically on every retry, so the alert or question is never
// answered. Capping ONE tool result is not enough — a loop of many rounds, each
// with several parallel tool calls, adds up to a transcript no per-result cap
// can bound.
//
// So there are three budgets, each protecting the next:
//
//	maxResultRunes  one result, so a single dump cannot dominate a round
//	maxRoundRunes   all results in one round, so parallel calls cannot either
//	maxConvRunes    the whole transcript, enforced by dropping the OLDEST
//	                results — the recent rounds are the ones being reasoned about
//
// Budgets bound what one answer may consume. They are configuration rather than
// constants because the right values depend on two things that differ per
// deployment: how much memory the container has, and how large a context the
// model accepts. Raising memory alone does not buy deeper investigations — the
// transcript still has to fit the model — so the two move together or not at all.
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
	// MaxTools caps how many tools are offered to the model in one request.
	// Every tool definition is sent on every round, and an endpoint that will
	// not carry them all drops the tail SILENTLY — the model then reports, in
	// perfect good faith, that a cluster has no tools. So the bot does the
	// trimming itself, fairly and visibly, rather than discovering it in an
	// answer.
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

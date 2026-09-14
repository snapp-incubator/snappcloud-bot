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
const (
	maxRoundRunes = 200_000
	maxConvRunes  = 400_000
)

const droppedNotice = "[earlier tool output dropped to fit the model's context budget — " +
	"re-run the tool if you still need it]"

// capRound shrinks a round's results to fit maxRoundRunes, taking from the
// largest first so one huge result cannot crowd out several small ones.
func capRound(results []ToolResult) []ToolResult {
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
func trimConversation(msgs []Turn) int {
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

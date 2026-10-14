package agent

import (
	"regexp"
	"strings"
)

// A turn that announces work and makes no call is unfinished: ask it to go on.

// announcement matches a self-directed statement of intent: the model telling
// itself what to do next. "Let me know" is excluded — that is addressed to the
// user and ends a turn legitimately.
var announcement = regexp.MustCompile(`(?i)\b(?:` +
	`let me|let'?s|lets|i'?ll|i will|i am going to|i'?m going to|` +
	`i need to|i should|i want to|i have to|next,? i|now,? i|` +
	// Talking about the work rather than doing it: a turn spent narrating its
	// own progress or budget is no more an answer than one announcing a call.
	`i have (?:spent|gathered|used|run|completed|done)|i (?:cannot|can'?t|do not|don'?t) have the tool|` +
	`tool budget|my budget|within budget|remaining calls|i'?ve spent` +
	`)\b`)

// addressedToTheUser matches the intent phrases that legitimately END a turn
// because they are aimed at the reader rather than at the model itself.
var addressedToTheUser = regexp.MustCompile(`(?i)\blet (?:me|us) know\b`)

// announcesMoreWork reports whether a tool-less response ends by announcing
// work the model did not do. Only the tail is examined: an answer may narrate
// what it did in the middle and still be complete, but a turn that ENDS on
// "let me check X" is a turn that expected to continue.
func announcesMoreWork(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return false
	}
	r := []rune(t)
	if len(r) > 400 {
		r = r[len(r)-400:]
	}
	tail := string(r)
	// Matching the subject rather than the verb: a model announces what it is
	// about to do in every imaginable wording, and a list of verbs is a list
	// that is always one entry short. "Let me make a focused final pass to
	// gather the rest" was the entry that was missing.
	return announcement.MatchString(tail) && !addressedToTheUser.MatchString(tail)
}

// maxNudges bounds how often one turn may be sent back for announcing work it
// did not do. A model that keeps promising and not calling is answered with
// what it has rather than spun on.
const maxNudges = 2

const nudge = "That message was about the work, not the work. You still have your tools and there are " +
	"tool calls left, so either make the calls now or write the finished answer. Never post your own " +
	"progress, your remaining budget, or what you were about to do: the reader sees only the message you " +
	"send, and a description of the work is not the work."

// unmeasuredCell matches the ways a model marks a cell it did not fill.
var unmeasuredCell = regexp.MustCompile(`(?i)\bn/a\b|\bnot run\b|\bnot measured\b|\bnot issued\b|\bqueries pending\b`)

// mostlyUnmeasured reports whether an answer is mostly empty cells. The
// threshold is high: a few honest gaps are fine.
func mostlyUnmeasured(text string) bool {
	if len(unmeasuredCell.FindAllString(text, -1)) < 8 {
		return false
	}
	// Only for answers shaped like a report; a prose answer that says "n/a"
	// a lot is not this.
	return strings.Count(text, "|") > 20
}

const runTheQueries = "That is a report with nothing in it: most of its cells say the measurement was not made, " +
	"and you still have tools and tool calls left. Go and run those queries now — issue them together in one " +
	"round, one per cell you left empty — and then write the report from what comes back. A cell may only stay " +
	"unmeasured if you ran its query and it returned nothing, in which case say what you ran."

// looksLikePreamble reports whether an answer only introduces the work: it ends
// in a colon, or is a fragment after a turn that made several calls.
func looksLikePreamble(text string, toolCalls int) bool {
	t := strings.TrimRight(strings.TrimSpace(text), "*_` ")
	if t == "" {
		return false
	}
	if strings.HasSuffix(t, ":") {
		return true
	}
	return toolCalls >= 3 && len([]rune(t)) < 120
}

const finishTheThought = "That was the sentence before the work, not the work: it reads as an introduction to " +
	"something that never followed. You still have your tools and tool calls left. Do the thing you were " +
	"about to do, then send the complete answer — every section the question asked for, filled from what the " +
	"tools returned."

package agent

import (
	"regexp"
	"strings"
)

// A model sometimes ends its turn saying what it is about to do — "Let me get
// the Envoy listener filter to see if there are also per-route limits, and
// check Hubble flows to confirm" — and makes no tool call. The loop has
// nothing to run, so it treats the text as the final answer and posts it. The
// reader gets an investigation that stops one sentence before its conclusion,
// with no sign that anything is missing.
//
// The model is not finished and says so. Ask it to go on.

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

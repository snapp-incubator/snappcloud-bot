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
	`(?:let me|let's|lets|i'll|i will|i am going to|i'm going to|next i'll|next i will|now i'll|now i will|` +
	`i need to|i should|i want to)\s+(?:now\s+|also\s+|first\s+|quickly\s+|just\s+)*` +
	`(?:check|look|query|fetch|get|run|verify|confirm|inspect|examine|pull|retrieve|see|find|trace|dig|grab|` +
	`compile|gather|collect|read|list|call|use|try|start|continue|do)\b` +
	`|(?:checking|querying|fetching|verifying|looking|inspecting|gathering)\s+(?:this|that|it|these|those|the)\b` +
	`)`)

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
	return announcement.MatchString(string(r))
}

// maxNudges bounds how often one turn may be sent back for announcing work it
// did not do. A model that keeps promising and not calling is answered with
// what it has rather than spun on.
const maxNudges = 2

const nudge = "You ended without calling any tool, but your last message says you are about to. " +
	"You still have your tools and there are tool calls left. Either make those calls now, or — if you " +
	"already have what you need — write the final answer. Do not describe what you are going to do next."

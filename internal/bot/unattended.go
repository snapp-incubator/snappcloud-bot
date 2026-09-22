package bot

import (
	"strings"
	"unicode/utf8"
)

// A scheduled report or an alert investigation runs with nobody at the other
// end. The first daily report ran for two turns, then stopped to ask the
// channel which of three smaller deliverables it preferred, and explained what
// the work was costing. Nothing about the model's behaviour was wrong for a
// conversation; everything about it was wrong for a post that nobody answers.
// So an unattended run is told what it is, up front, in terms it acts on.

// unattended frames a query as an unattended run: no questions, no options,
// no commentary on effort — a delivery, complete or explicitly partial.
func unattended(kind, query string) string {
	return "This is an unattended " + kind + ": it is posted to a channel and nobody will reply to it. " +
		"Do not ask a question, offer options, or wait for a decision — there is no one to answer. " +
		"Do not comment on the size, cost or effort of the task. " +
		"If everything asked for cannot be done within your tool-call budget, deliver the parts that can, " +
		"in the order they are asked for, mark each missing cell or section \"n/a\" with the reason, and " +
		"finish with one line listing what was cut. A partial report delivered is the job; a question is not. " +
		"Post the result only: no narration of your reasoning, no notes about which tools you can see, " +
		"no thinking out loud. If a tool you need is missing, that is one line in the report, not a discussion.\n\n" +
		query
}

// summarizeQuery shortens a long scheduled query for a post header: the first
// sentence or line, capped — the reader saw the whole text when it was
// scheduled, and a 4000-character question above every answer buries the
// answer.
func summarizeQuery(q string, max int) string {
	q = strings.TrimSpace(q)
	if i := strings.IndexAny(q, "\n"); i > 0 {
		q = q[:i]
	}
	if i := strings.Index(q, ". "); i > 0 {
		q = q[:i+1]
	}
	if utf8.RuneCountInString(q) <= max {
		return q
	}
	r := []rune(q)
	return strings.TrimSpace(string(r[:max-1])) + "…"
}

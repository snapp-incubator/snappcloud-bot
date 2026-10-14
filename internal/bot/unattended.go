package bot

import (
	"strings"
	"unicode/utf8"
)

// Unattended runs (schedules, alerts) have nobody to answer a question, so the
// model is told to deliver, not ask.

// unattended frames a query as an unattended run: no questions, no options,
// no commentary on effort — a delivery, complete or explicitly partial.
func unattended(kind, query string) string {
	return "This is an unattended " + kind + ": it is posted to a channel and nobody will reply to it. " +
		"Do not ask a question, offer options, or wait for a decision — there is no one to answer. " +
		"Do not comment on the size, cost or effort of the task. " +
		"If everything asked for cannot be done within your tool-call budget, deliver the parts that can, " +
		"in the order they are asked for, and finish with one line listing what was cut. " +
		// A missing measurement is a note under the table; a row below the
		// threshold is deleted.
		"Leave a cell you could not measure empty and give the reason once, under the table. Never write " +
		"\"n/a\" in a cell to mean a row did not meet a threshold — a row that does not meet the threshold " +
		"is deleted, not annotated. A partial report delivered is the job; a question is not. " +
		"Post the result only: no narration of your reasoning, no notes about which tools you can see, " +
		"no thinking out loud. If a tool you need is missing, that is one line in the report, not a discussion.\n\n" +
		query
}

// summarizeQuery shortens a scheduled query to a header line.
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

// tableCell makes a string safe to put in one cell of a markdown table: no
// newline can end the row early, and no pipe can open a column that is not
// there. Long text is cut at a word boundary — a schedule's question can be
// thousands of characters, and the listing exists to identify it, not to
// reproduce it.
func tableCell(s string, max int) string {
	s = strings.Join(strings.Fields(strings.ReplaceAll(s, "|", "\\|")), " ")
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)[:max]
	if i := strings.LastIndexByte(string(r), ' '); i > max/2 {
		return string(r[:i]) + " …"
	}
	return string(r) + "…"
}

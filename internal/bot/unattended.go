package bot

import (
	"regexp"
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

// narrationLine matches a line of the model talking about its own progress
// rather than reporting anything: "Excellent — I have very rich data.",
// "I have enough to deliver the report. Now let me produce the report."
var narrationLine = regexp.MustCompile(`(?i)^(?:\*\*|#+\s*)?(?:` +
	`excellent|great|perfect|good[.,!—-]|ok[.,!—-]|alright|right[.,!—-]|` +
	`i (?:have|now have|'ve) (?:enough|what i need|rich|plenty|all)|` +
	`(?:now )?(?:let me|i'?ll|i will|here is|here'?s) (?:produce|write|deliver|compile|put together|assemble|give)|` +
	`writing the (?:final )?report|producing the report|key findings` +
	`)\b`)

// productionSentence matches a sentence about producing the answer
// ("Let me build the report.") rather than one that is part of it.
var productionSentence = regexp.MustCompile(`(?i)` +
	`\b(?:let me|let's|i'll|i will|i'm going to|i am going to|i can)\s+(?:\w+\s+){0,2}` +
	`(?:build|compose|produce|write|compile|assemble|put together|deliver|present|draft|format|create|summari[sz]e|give you)\b` +
	`|\b(?:now )?i (?:now )?(?:have|'ve got|have got|'ve gathered|have gathered|'ve collected|have collected) ` +
	`(?:all|enough|everything|what i need|the (?:key |necessary |required )?(?:data|information|results))` +
	`|\bthe (?:data|results?|information) (?:is|are) (?:rich|complete|sufficient|enough|compact|in)\b` +
	`|^here(?:'s| is) (?:the|your) (?:full |final |complete |daily |weekly )?(?:report|summary|answer)\b`)

// structuralLine is where an answer's body starts: a heading, a table, a list
// item, a quote, or a bold title line.
var structuralLine = regexp.MustCompile(`^(?:#|\||[-*>]|\d+[.)]\s)`)

// dropLeadingNarration strips sentences about producing the answer from above
// its body. It acts only when a structured body follows, and never on a line
// with a figure in it.
func dropLeadingNarration(s string) string {
	lines := strings.Split(s, "\n")
	body := -1
	for i, ln := range lines {
		if structuralLine.MatchString(strings.TrimSpace(ln)) {
			body = i
			break
		}
	}
	if body < 0 {
		return s
	}
	cut := 0
	for i := 0; i < body; i++ {
		t := strings.TrimSpace(lines[i])
		if t == "" {
			if cut == i {
				cut = i + 1
			}
			continue
		}
		if !isNarration(t) {
			break
		}
		cut = i + 1
	}
	if cut == 0 {
		return s
	}
	return strings.TrimSpace(strings.Join(lines[cut:], "\n"))
}

// isNarration reports whether a whole line is talk about producing the answer.
// Every sentence on it has to be, so a line that narrates and then states a
// finding is kept.
func isNarration(line string) bool {
	if strings.ContainsAny(line, "0123456789") {
		return false
	}
	if narrationLine.MatchString(line) {
		return true
	}
	sentences := splitSentences(line)
	if len(sentences) == 0 {
		return false
	}
	for _, sn := range sentences {
		if !productionSentence.MatchString(sn) {
			return false
		}
	}
	return true
}

// splitSentences breaks a line after sentence-ending punctuation.
func splitSentences(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if (s[i] == '.' || s[i] == '!' || s[i] == '?') && (i+1 == len(s) || s[i+1] == ' ') {
			if t := strings.TrimSpace(s[start : i+1]); t != "" {
				out = append(out, t)
			}
			start = i + 1
		}
	}
	if t := strings.TrimSpace(s[start:]); t != "" {
		out = append(out, t)
	}
	return out
}

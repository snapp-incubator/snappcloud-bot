package bot

import (
	"regexp"
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

// dropLeadingNarration removes the model's throat-clearing from the top of an
// answer that does go on to contain the answer. The guards elsewhere catch a
// turn that is ONLY narration; this is the other half — a report preceded by
// four lines of "Excellent, I have very rich data… Now let me produce the
// report", which the reader sees before anything useful.
//
// Only the lines before the first heading, table or bullet are considered, and
// only while they keep matching, so prose answers are untouched.
func dropLeadingNarration(s string) string {
	lines := strings.Split(s, "\n")
	cut := 0
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "" {
			if cut == i {
				cut = i + 1
			}
			continue
		}
		// The answer proper has started: stop looking.
		if strings.HasPrefix(t, "#") || strings.HasPrefix(t, "|") ||
			strings.HasPrefix(t, "-") || strings.HasPrefix(t, "*") || strings.HasPrefix(t, ">") {
			break
		}
		if !narrationLine.MatchString(t) {
			break
		}
		cut = i + 1
	}
	if cut == 0 {
		return s
	}
	return strings.TrimSpace(strings.Join(lines[cut:], "\n"))
}

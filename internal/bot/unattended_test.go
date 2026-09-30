package bot

import (
	"strings"
	"testing"
)

func TestUnattendedForbidsQuestionsAndKeepsQuery(t *testing.T) {
	q := unattended("scheduled report", "how many pods?")
	for _, want := range []string{"unattended scheduled report", "nobody will reply", "Do not ask a question", "how many pods?"} {
		if !strings.Contains(q, want) {
			t.Fatalf("missing %q in %q", want, q)
		}
	}
	if !strings.HasSuffix(q, "how many pods?") {
		t.Fatalf("query must come last: %q", q)
	}
}

func TestSummarizeQueryFirstSentenceCapped(t *testing.T) {
	long := "Daily platform report for the production clusters okd4-teh-1, okd4-teh-2. One section per cluster.\n\nScope: ..."
	if got := summarizeQuery(long, 200); got != "Daily platform report for the production clusters okd4-teh-1, okd4-teh-2." {
		t.Fatalf("got %q", got)
	}
	if got := summarizeQuery(strings.Repeat("x", 300), 20); len([]rune(got)) != 20 || !strings.HasSuffix(got, "…") {
		t.Fatalf("cap not applied: %q", got)
	}
	if got := summarizeQuery("short", 200); got != "short" {
		t.Fatalf("got %q", got)
	}
}

// A schedule's question is a four-thousand-character report spec. Put it in a
// table cell whole and the table stops being a table: the first newline ends
// the row, and every later line renders as literal pipes.
func TestTableCellCannotBreakTheTable(t *testing.T) {
	q := "Daily capacity report for cluster okd4-teh-1 only; do not query any other cluster.\n\n" +
		"Pick the datasource once, in your first round: list the Prometheus datasources and send every candidate the same probe.\n\n" +
		"Scope: namespace!~\"default|kube-.*\" — a | inside the text must not open a column."
	got := tableCell(q, 90)
	if strings.ContainsAny(got, "\n\r") {
		t.Fatalf("a newline survived: %q", got)
	}
	if strings.Contains(got, "|") && !strings.Contains(got, "\\|") {
		t.Fatalf("an unescaped pipe survived: %q", got)
	}
	if n := len([]rune(got)); n > 92 {
		t.Fatalf("cell is %d runes, want <= 92: %q", n, got)
	}
	if !strings.HasPrefix(got, "Daily capacity report") {
		t.Fatalf("the identifying part was lost: %q", got)
	}
	if short := tableCell("every day at 09:00 are any pods failing?", 90); short != "every day at 09:00 are any pods failing?" {
		t.Fatalf("a short question must pass through unchanged: %q", short)
	}
}

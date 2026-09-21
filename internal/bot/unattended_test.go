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

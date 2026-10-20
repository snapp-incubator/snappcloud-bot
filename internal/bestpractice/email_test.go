package bestpractice

import (
	"strings"
	"testing"
)

// Titles and details carry names from cluster objects. The HTML part must
// escape them; only `code` spans become markup.
func TestAdminEmailEscapesHTML(t *testing.T) {
	f := Finding{Rule: RuleMaxUnavailableZero, Severity: High, Cluster: "teh-1", Namespace: "pay<x>",
		Title: "PDB `<script>alert(1)</script>` allows no evictions", Detail: "a & b, one ` unmatched", Fix: "Set `maxUnavailable: 1`."}
	e := AdminEmail([]TeamFindings{{Team: "pay", Findings: []Finding{f}}}, map[string]string{"pdb": "https://docs/pdb?a=1&b=2"}, false)

	if strings.Contains(e.HTML, "<script>") || strings.Contains(e.HTML, "pay<x>") {
		t.Fatalf("unescaped HTML:\n%s", e.HTML)
	}
	for _, want := range []string{"<code>&lt;script&gt;alert(1)&lt;/script&gt;</code>", "pay&lt;x&gt;", "a &amp; b, one ` unmatched",
		"<code>maxUnavailable: 1</code>", `href="https://docs/pdb?a=1&amp;b=2"`} {
		if !strings.Contains(e.HTML, want) {
			t.Errorf("html missing %q:\n%s", want, e.HTML)
		}
	}
	if e.Subject != "SnappCloud best-practice check: 1 finding for pay (1 high)" {
		t.Errorf("subject = %q", e.Subject)
	}
	if !strings.Contains(e.Text, "Docs: https://docs/pdb?a=1&b=2") || strings.Contains(e.Text, "reply to this email") {
		t.Errorf("text:\n%s", e.Text)
	}
}

// The ignore annotation is offered only for findings it can silence.
func TestAdminEmailOffersIgnoreOnlyWhereItWorks(t *testing.T) {
	blocker := Finding{Rule: RuleMinAvailableAll, Severity: High, Cluster: "teh-1", Namespace: "app", Title: "t", Detail: "d", Fix: "f"}
	quorum := Finding{Rule: RuleQuorumNoPDB, Severity: High, Cluster: "teh-1", Namespace: "app", Title: "t", Detail: "d", Fix: "f"}

	only := AdminEmail([]TeamFindings{{Team: "pay", Findings: []Finding{blocker}}}, nil, false)
	if strings.Contains(only.Text, AnnotationIgnore) || !strings.Contains(only.Text, "Rule: pdb-min-available-all (cannot be ignored)") {
		t.Errorf("blockers only:\n%s", only.Text)
	}
	mixed := AdminEmail([]TeamFindings{{Team: "pay", Findings: []Finding{blocker, quorum}}}, nil, false)
	for _, want := range []string{AnnotationIgnore, "cannot be ignored: they must be fixed", "Rule: quorum-no-pdb\n"} {
		if !strings.Contains(mixed.Text, want) {
			t.Errorf("mixed missing %q:\n%s", want, mixed.Text)
		}
	}
}

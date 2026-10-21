package bestpractice

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"
)

func init() { pdfCompress = false }

var day = time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)

func teamFinding(team, rule string, sev Severity) Finding {
	return Finding{Rule: rule, Severity: sev, Cluster: "teh-1", Namespace: team + "-prod", Team: team,
		Title: "PDB `api` allows no evictions", Detail: "d", Fix: "Set `maxUnavailable: 1`."}
}

func TestMailingsGroupTeamsAndSendTheRestToOthers(t *testing.T) {
	rt := Routing{
		Groups: [][]string{{"box", "box-ai"}, {"pay"}, {"quiet"}},
		Others: []string{"tech-ems@snapp.cab"},
		Cc:     []string{"cto@snapp.cab"},
	}
	fs := []Finding{
		teamFinding("pay", RuleQuorumNoPDB, High),
		teamFinding("box-ai", RuleQuorumNoPDB, Medium),
		teamFinding("box", RuleQuorumNoPDB, High),
		teamFinding("ride", RuleQuorumNoPDB, High),
		teamFinding("food", RuleQuorumNoPDB, Medium),
	}
	admins := map[string][]string{
		"box": {"a@snapp.cab", "b@snapp.cab"}, "box-ai": {"b@snapp.cab", "c@snapp.cab"}, "ride": {"r@snapp.cab"},
	}
	ms := rt.Mailings(fs, admins)
	if len(ms) != 3 {
		t.Fatalf("mailings = %+v", ms)
	}
	box, pay, others := ms[0], ms[1], ms[2]
	if box.Name != "box, box-ai" || !slices.Equal(box.Teams, []string{"box", "box-ai"}) ||
		!slices.Equal(box.To, []string{"a@snapp.cab", "b@snapp.cab", "c@snapp.cab"}) || len(box.Findings) != 2 {
		t.Errorf("box = %+v", box)
	}
	// pay's Team CR names no admin: the cc receive it alone, and the preview says why.
	if len(pay.To) != 0 || !slices.Equal(pay.NoAdmins, []string{"pay"}) || !pay.Recipients() {
		t.Errorf("pay = %+v", pay)
	}
	// Teams in no group go to Others, whatever their own admins.
	if others.Name != OthersName || !slices.Equal(others.Teams, []string{"food", "ride"}) ||
		!slices.Equal(others.To, rt.Others) || !slices.Equal(others.Cc, rt.Cc) {
		t.Errorf("others = %+v", others)
	}
	if m, ok := MailingOf(ms, "ride"); !ok || m.Name != OthersName {
		t.Errorf("MailingOf(ride) = %+v", m)
	}
}

func TestRoutingValidate(t *testing.T) {
	for _, rt := range []Routing{
		{Groups: [][]string{{"a"}, {"b", "a"}}},
		{Groups: [][]string{{}}},
		{Others: []string{"not an address"}},
		{Cc: []string{"x@snapp.cab\r\nBcc: y@evil.com"}},
	} {
		if rt.Validate() == nil {
			t.Errorf("%+v accepted", rt)
		}
	}
	if err := (Routing{Groups: [][]string{{"a", "b"}, {"c"}}, Cc: []string{"cto@snapp.cab"}}).Validate(); err != nil {
		t.Error(err)
	}
}

// Team names come from cluster objects. The HTML part must escape them.
func TestReportEmailEscapesHTML(t *testing.T) {
	m := Mailing{Name: "x", Teams: []string{"pay<x>"}, Findings: []Finding{teamFinding("pay<x>", RuleMaxUnavailableZero, High)}}
	e, err := ReportEmail(m, nil, false, day, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(e.HTML, "pay<x>") || !strings.Contains(e.HTML, "pay&lt;x&gt;") {
		t.Fatalf("unescaped HTML:\n%s", e.HTML)
	}
	if e.Subject != "SnappCloud best-practice check: 1 finding for pay<x> (1 high)" {
		t.Errorf("subject = %q", e.Subject)
	}
	if e.FileName != "snappcloud-best-practice-2026-10-24.pdf" || strings.Contains(e.Text, "reply to this email") {
		t.Errorf("file = %q, text:\n%s", e.FileName, e.Text)
	}
}

func TestReportEmailSubjectCountsManyTeams(t *testing.T) {
	m := Mailing{Teams: []string{"a", "b", "c", "d"}}
	for _, team := range m.Teams {
		m.Findings = append(m.Findings, teamFinding(team, RuleQuorumNoPDB, Medium))
	}
	e, err := ReportEmail(m, nil, true, day, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if e.Subject != "SnappCloud best-practice check: 4 findings for 4 teams" || !strings.Contains(e.Text, "reply to this email") {
		t.Errorf("subject = %q, text:\n%s", e.Subject, e.Text)
	}
}

// The PDF holds every finding with its fix, docs link and rule.
func TestReportPDFHoldsEveryFinding(t *testing.T) {
	m := Mailing{Teams: []string{"box", "pay"}, Findings: []Finding{
		teamFinding("box", RuleMinAvailableAll, High), teamFinding("pay", RuleQuorumNoPDB, Medium),
	}}
	e, err := ReportEmail(m, map[string]string{"pdb": "https://docs/pdb"}, false, day, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(e.PDF, []byte("%PDF-")) {
		t.Fatalf("not a PDF: %q", e.PDF[:min(20, len(e.PDF))])
	}
	for _, want := range []string{"(Team box)", "(Team pay)", "(teh-1 / box-prod)", "(maxUnavailable: 1)", "(pdb-min-available-all)",
		"( \\(cannot be ignored\\))", "(quorum-no-pdb)", "/URI (https://docs/pdb)", "page 1 of"} {
		if !bytes.Contains(e.PDF, []byte(want)) {
			t.Errorf("PDF missing %q", want)
		}
	}
}

// The ignore annotation is offered only for findings it can silence.
func TestEmailFooterOffersIgnoreOnlyWhereItWorks(t *testing.T) {
	blocker := teamFinding("pay", RuleMinAvailableAll, High)
	quorum := teamFinding("pay", RuleQuorumNoPDB, High)

	if only := emailFooter([]Finding{blocker}, false); strings.Contains(only, AnnotationIgnore) ||
		!strings.Contains(only, "cannot be ignored: they must be fixed") {
		t.Errorf("blockers only: %s", only)
	}
	mixed := emailFooter([]Finding{blocker, quorum}, false)
	if !strings.Contains(mixed, AnnotationIgnore) || !strings.Contains(mixed, "cannot be ignored: they must be fixed") {
		t.Errorf("mixed: %s", mixed)
	}
	if text := FindingsText([]Finding{blocker, quorum}, nil); !strings.Contains(text, "Rule: pdb-min-available-all (cannot be ignored)") ||
		!strings.Contains(text, "Rule: quorum-no-pdb\n") {
		t.Errorf("findings text:\n%s", text)
	}
}

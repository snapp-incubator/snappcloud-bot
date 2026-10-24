package bestpractice

import (
	"bytes"
	"fmt"
	"html"
	"html/template"
	"strings"
	"time"
)

// Email is one mailing rendered: a short body summing it up, and a PDF with
// every finding and how to fix it.
type Email struct {
	Subject  string
	Text     string
	HTML     string
	FileName string
	PDF      []byte
}

const emailIntro = "These can cause downtime, or block node maintenance, when a node is drained."

var severityLabel = map[Severity]string{High: "High", Medium: "Medium"}

// ruleChecks says in a line what each rule finds, for the email's "What we
// check". Every rule in Rules has one.
var ruleChecks = []struct{ Rule, Text string }{
	{RuleMaxUnavailableZero, "a PDB with `maxUnavailable: 0`, which lets no pod be evicted."},
	{RuleMinAvailableAll, "a PDB whose `minAvailable` is as many pods as there are, which lets no pod be evicted."},
	{RuleOverlap, "two PDBs selecting the same pods: eviction refuses a pod in more than one."},
	{RuleBlockedUnhealthy, "a PDB that allows no evictions because its pods are unhealthy, without `unhealthyPodEvictionPolicy: AlwaysAllow`."},
	{RuleBelowQuorum, "a quorum system (such as a RabbitMQ cluster or Redis Sentinel) whose PDB lets a drain take enough members to lose quorum."},
	{RuleQuorumNoPDB, "a quorum system with no PDB at all."},
	{RuleRabbitSingle, "RabbitMQ running 1 replica: every drain of its node takes it down."},
	{RuleRedisSingle, "Redis running 1 replica, or with replicas but no failover: every drain of its node takes it down."},
}

// checksScope says where the rules apply.
const checksScope = "PDBs that block node drains are checked in every namespace; the rest in production namespaces only."

// Checks lists the rules a report checks, those not disabled, as
// "`rule`: what it finds".
func Checks(disabled []string) []string {
	off := toSet(disabled)
	var out []string
	for _, c := range ruleChecks {
		if !off[c.Rule] {
			out = append(out, "`"+c.Rule+"`: "+c.Text)
		}
	}
	return out
}

// TeamCount is one row of an email's summary table.
type TeamCount struct {
	Team         string
	High, Medium int
}

// ReportEmail renders one mailing. disabled are the rules left out of the
// scan, so the email does not claim to check them; canReply says whether a
// reply reaches the cloud team (a Reply-To is configured); created dates the
// report.
func ReportEmail(m Mailing, docs map[string]string, disabled []string, canReply bool, created time.Time,
	loc *time.Location) (Email, error) {
	high, _ := counts(m.Findings)
	who := strings.Join(m.Teams, ", ")
	if len(m.Teams) > 3 {
		who = fmt.Sprintf("%d teams", len(m.Teams))
	}
	subject := fmt.Sprintf("SnappCloud best-practice check: %d finding%s for %s", len(m.Findings), plural(len(m.Findings)), who)
	if high > 0 {
		subject += fmt.Sprintf(" (%d high)", high)
	}
	if loc == nil {
		loc = time.Local
	}
	day := created.In(loc)
	footer := emailFooter(canReply)
	pdf, err := reportPDF(m, docs, footer, day)
	if err != nil {
		return Email{}, err
	}
	var rows []TeamCount
	byTeam := GroupByTeam(m.Findings)
	for _, t := range m.Teams {
		h, md := counts(byTeam[t])
		rows = append(rows, TeamCount{t, h, md})
	}
	lead := fmt.Sprintf("The attached PDF has the %d finding%s for these teams, and how to fix each one.",
		len(m.Findings), plural(len(m.Findings)))
	body := emailBody{Lead: lead, Hint: ignoreHint(m.Findings), Rows: rows, Checks: Checks(disabled), Footer: footer}
	return Email{
		Subject:  subject,
		Text:     emailText(day, body),
		HTML:     emailHTML(day, body),
		FileName: "snappcloud-best-practice-" + day.Format("2006-01-02") + ".pdf",
		PDF:      pdf,
	}, nil
}

// ignoreHint says how to silence a finding, where that works. The email shows
// it above the team table, so it is not missed.
func ignoreHint(fs []Finding) string {
	var ignorable, blockers bool
	for _, f := range fs {
		blockers = blockers || DrainBlocker(f.Rule)
		ignorable = ignorable || !DrainBlocker(f.Rule)
	}
	var hint string
	if ignorable {
		hint = fmt.Sprintf("Is a finding intentional? Exclude it by annotating its object with `%s: <rule>` (several rules go comma-separated); "+
			"the PDF gives the exact annotation for each finding that allows it.", AnnotationIgnore)
	}
	if blockers {
		hint += " PDBs that block node drains cannot be excluded: they must be fixed."
	}
	return strings.TrimSpace(hint)
}

// emailFooter says where to ask.
func emailFooter(canReply bool) string {
	if canReply {
		return "Questions? Reply to this email."
	}
	return ""
}

// emailBody is what the text and HTML parts both say.
type emailBody struct {
	Lead, Hint string
	Rows       []TeamCount
	Checks     []string
	Footer     string
}

func emailText(day time.Time, e emailBody) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Weekly SnappCloud best-practice check, %s\n\n%s\n%s\n\n", day.Format("2 Jan 2006"), emailIntro, e.Lead)
	if e.Hint != "" {
		b.WriteString("Note: " + e.Hint + "\n\n")
	}
	w := len("Team")
	for _, r := range e.Rows {
		w = max(w, len(r.Team))
	}
	fmt.Fprintf(&b, "%-*s  %4s  %6s\n", w, "Team", "High", "Medium")
	for _, r := range e.Rows {
		fmt.Fprintf(&b, "%-*s  %4d  %6d\n", w, r.Team, r.High, r.Medium)
	}
	if len(e.Checks) > 0 {
		b.WriteString("\nWhat we check:\n")
		for _, c := range e.Checks {
			b.WriteString("- " + c + "\n")
		}
		b.WriteString(checksScope + "\n")
	}
	if e.Footer != "" {
		b.WriteString("\n--\n" + e.Footer + "\n")
	}
	return b.String()
}

// FindingsText lists findings as the PDF does, for the admin channel.
func FindingsText(fs []Finding, docs map[string]string) string {
	var b strings.Builder
	for i, f := range fs {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "[%s] %s / %s — %s\n%s\nFix: %s\n", severityLabel[f.Severity], f.Cluster, f.Namespace,
			f.Title, f.Detail, f.Fix)
		if url := docs[Topic(f.Rule)]; url != "" {
			fmt.Fprintf(&b, "Docs: %s\n", url)
		}
		fmt.Fprintf(&b, "Rule: %s%s\n", f.Rule, ignoreNote(f.Rule))
	}
	return b.String()
}

// Team names come from cluster objects, so the HTML is built by html/template.
var emailTmpl = template.Must(template.New("email").Funcs(template.FuncMap{
	"code": codeSpans,
}).Parse(`<!doctype html>
<html><body style="font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;font-size:14px;line-height:1.5;color:#1f2328;max-width:720px">
<h2 style="font-size:18px;margin:0 0 4px">Weekly SnappCloud best-practice check</h2>
<p style="margin:0 0 4px;color:#59636e">{{.Day}}</p>
<p style="margin:12px 0">{{.Intro}}<br>{{.Lead}}</p>
{{if .Hint}}<p style="margin:12px 0 16px;padding:8px 12px;background:#fff8c5;border:1px solid #d4a72c;border-radius:6px">{{code .Hint}}</p>
{{end}}<table style="border-collapse:collapse;font-size:14px">
<tr><th style="text-align:left;padding:4px 16px 4px 0;border-bottom:1px solid #d1d9e0">Team</th><th style="text-align:right;padding:4px 0 4px 16px;border-bottom:1px solid #d1d9e0">High</th><th style="text-align:right;padding:4px 0 4px 16px;border-bottom:1px solid #d1d9e0">Medium</th></tr>
{{range .Rows}}<tr><td style="padding:4px 16px 4px 0"><code>{{.Team}}</code></td><td style="text-align:right;padding:4px 0 4px 16px;{{if .High}}color:#d1242f;font-weight:600{{end}}">{{.High}}</td><td style="text-align:right;padding:4px 0 4px 16px">{{.Medium}}</td></tr>
{{end}}</table>
{{if .Checks}}<h3 style="font-size:15px;margin:24px 0 4px">What we check</h3>
<ul style="margin:0 0 8px;padding-left:20px">
{{range .Checks}}<li>{{code .}}</li>
{{end}}</ul>
<p style="margin:0;color:#59636e">{{.Scope}}</p>
{{end}}{{if .Footer}}<hr style="border:0;border-top:1px solid #d1d9e0;margin:24px 0 8px">
<p style="color:#59636e;font-size:12px">{{code .Footer}}</p>
{{end}}</body></html>
`))

func emailHTML(day time.Time, e emailBody) string {
	var b bytes.Buffer
	err := emailTmpl.Execute(&b, struct {
		emailBody
		Day, Intro, Scope string
	}{e, day.Format("2 January 2006"), emailIntro, checksScope})
	if err != nil { // the template is fixed; only a bug gets here
		return "<pre>" + html.EscapeString(emailText(day, e)) + "</pre>"
	}
	return b.String()
}

// codeSpans escapes s and renders its `backtick` spans as <code>.
func codeSpans(s string) template.HTML {
	parts := strings.Split(s, "`")
	var b strings.Builder
	for i, p := range parts {
		p = html.EscapeString(p)
		if i%2 == 1 && i < len(parts)-1 {
			p = "<code>" + p + "</code>"
		} else if i%2 == 1 { // an unmatched backtick stays as written
			p = "`" + p
		}
		b.WriteString(p)
	}
	return template.HTML(b.String()) //nolint:gosec // every piece is escaped above
}

// ignoreNote marks a rule the ignore annotation does not silence.
func ignoreNote(rule string) string {
	if DrainBlocker(rule) {
		return " (cannot be ignored)"
	}
	return ""
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

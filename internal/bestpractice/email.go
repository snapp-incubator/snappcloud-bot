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

// TeamCount is one row of an email's summary table.
type TeamCount struct {
	Team         string
	High, Medium int
}

// ReportEmail renders one mailing. canReply says whether a reply reaches the
// cloud team (a Reply-To is configured); created dates the report.
func ReportEmail(m Mailing, docs map[string]string, canReply bool, created time.Time, loc *time.Location) (Email, error) {
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
	return Email{
		Subject:  subject,
		Text:     emailText(day, lead, ignoreHint(m.Findings), rows, footer),
		HTML:     emailHTML(day, lead, ignoreHint(m.Findings), rows, footer),
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

func emailText(day time.Time, lead, hint string, rows []TeamCount, footer string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Weekly SnappCloud best-practice check, %s\n\n%s\n%s\n\n", day.Format("2 Jan 2006"), emailIntro, lead)
	if hint != "" {
		b.WriteString("Note: " + hint + "\n\n")
	}
	w := len("Team")
	for _, r := range rows {
		w = max(w, len(r.Team))
	}
	fmt.Fprintf(&b, "%-*s  %4s  %6s\n", w, "Team", "High", "Medium")
	for _, r := range rows {
		fmt.Fprintf(&b, "%-*s  %4d  %6d\n", w, r.Team, r.High, r.Medium)
	}
	if footer != "" {
		b.WriteString("\n--\n" + footer + "\n")
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
{{if .Footer}}<hr style="border:0;border-top:1px solid #d1d9e0;margin:24px 0 8px">
<p style="color:#59636e;font-size:12px">{{code .Footer}}</p>
{{end}}</body></html>
`))

func emailHTML(day time.Time, lead, hint string, rows []TeamCount, footer string) string {
	var b bytes.Buffer
	err := emailTmpl.Execute(&b, struct {
		Day, Intro, Lead, Hint, Footer string
		Rows                           []TeamCount
	}{day.Format("2 January 2006"), emailIntro, lead, hint, footer, rows})
	if err != nil { // the template is fixed; only a bug gets here
		return "<pre>" + html.EscapeString(emailText(day, lead, hint, rows, footer)) + "</pre>"
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

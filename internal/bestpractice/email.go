package bestpractice

import (
	"bytes"
	"fmt"
	"html"
	"html/template"
	"strings"
)

// TeamFindings is one team's findings, as one of its admins may see them.
type TeamFindings struct {
	Team     string
	Findings []Finding
}

// Email is what one team admin receives: a section per team they administer.
type Email struct {
	Subject string
	Text    string
	HTML    string
}

const emailIntro = "These can cause downtime, or block node maintenance, when a node is drained."

var severityLabel = map[Severity]string{High: "High", Medium: "Medium"}

// AdminEmail renders one admin's email. canReply says whether a reply reaches
// the cloud team (a Reply-To is configured).
func AdminEmail(teams []TeamFindings, docs map[string]string, canReply bool) Email {
	var all []Finding
	names := make([]string, 0, len(teams))
	for _, t := range teams {
		all = append(all, t.Findings...)
		names = append(names, t.Team)
	}
	high, _ := counts(all)
	subject := fmt.Sprintf("SnappCloud best-practice check: %d finding%s for %s", len(all), plural(len(all)),
		strings.Join(names, ", "))
	if high > 0 {
		subject += fmt.Sprintf(" (%d high)", high)
	}
	footer := fmt.Sprintf("Intentional? Annotate the object with %s: <rule>, using the rule shown under the finding.",
		AnnotationIgnore)
	if canReply {
		footer += " Questions: reply to this email."
	}
	return Email{Subject: subject, Text: emailText(teams, docs, footer), HTML: emailHTML(teams, docs, footer)}
}

func emailText(teams []TeamFindings, docs map[string]string, footer string) string {
	var b strings.Builder
	b.WriteString("Weekly SnappCloud best-practice check\n\n" + emailIntro + "\n")
	for _, t := range teams {
		fmt.Fprintf(&b, "\n== Team %s ==\n", t.Team)
		for _, f := range t.Findings {
			fmt.Fprintf(&b, "\n[%s] %s / %s — %s\n%s\nFix: %s\n", severityLabel[f.Severity], f.Cluster, f.Namespace,
				f.Title, f.Detail, f.Fix)
			if url := docs[Topic(f.Rule)]; url != "" {
				fmt.Fprintf(&b, "Docs: %s\n", url)
			}
			fmt.Fprintf(&b, "Rule: %s\n", f.Rule)
		}
	}
	b.WriteString("\n--\n" + footer + "\n")
	return b.String()
}

// Everything in a finding comes from cluster objects that tenants name, so the
// HTML is built by html/template and only `code` spans become markup.
var emailTmpl = template.Must(template.New("email").Funcs(template.FuncMap{
	"code":  codeSpans,
	"mark":  func(s Severity) string { return severityMark[s] },
	"label": func(s Severity) string { return severityLabel[s] },
	"docs":  func(docs map[string]string, rule string) string { return docs[Topic(rule)] },
}).Parse(`<!doctype html>
<html><body style="font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;font-size:14px;line-height:1.5;color:#1f2328;max-width:720px">
<h2 style="font-size:18px;margin:0 0 4px">Weekly SnappCloud best-practice check</h2>
<p style="margin:0 0 16px;color:#59636e">{{.Intro}}</p>
{{range .Teams}}<h3 style="font-size:16px;margin:24px 0 8px">Team <code>{{.Team}}</code></h3>
{{range .Findings}}<div style="border-left:3px solid {{if eq .Severity "high"}}#d1242f{{else}}#bc4c00{{end}};padding:4px 12px;margin:0 0 12px">
<div><b>{{mark .Severity}} {{label .Severity}} · {{.Cluster}} / {{.Namespace}} — {{code .Title}}</b></div>
<div>{{code .Detail}}</div>
<div><b>Fix:</b> {{code .Fix}}{{with docs $.Docs .Rule}} <a href="{{.}}">Docs</a>{{end}}</div>
<div style="color:#59636e;font-size:12px">rule <code>{{.Rule}}</code></div>
</div>
{{end}}{{end}}<hr style="border:0;border-top:1px solid #d1d9e0;margin:24px 0 8px">
<p style="color:#59636e;font-size:12px">{{code .Footer}}</p>
</body></html>
`))

func emailHTML(teams []TeamFindings, docs map[string]string, footer string) string {
	var b bytes.Buffer
	err := emailTmpl.Execute(&b, struct {
		Intro, Footer string
		Teams         []TeamFindings
		Docs          map[string]string
	}{emailIntro, footer, teams, docs})
	if err != nil { // the template is fixed; only a bug gets here
		return "<pre>" + html.EscapeString(emailText(teams, docs, footer)) + "</pre>"
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

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

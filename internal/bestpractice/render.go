package bestpractice

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Everything a person reads about a report is rendered here from templates:
// nothing in it is written by a model.

// FormatTime renders a time in the report timezone, naming the zone.
func FormatTime(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.Local
	}
	return t.In(loc).Format("Mon 2 Jan 15:04") + " " + loc.String()
}

var severityMark = map[Severity]string{High: "🔴", Medium: "🟠"}

func counts(fs []Finding) (high, medium int) {
	for _, f := range fs {
		if f.Severity == High {
			high++
		} else {
			medium++
		}
	}
	return high, medium
}

// PreviewRoot is the short post that opens a report's thread in the admin
// channel.
func PreviewRoot(r Report, loc *time.Location) string {
	var b strings.Builder
	send := r.Sendable()
	high, _ := counts(send)
	fmt.Fprintf(&b, "**Best-practice report `%s`** — %s\n", r.ID, FormatTime(r.Created, loc))
	fmt.Fprintf(&b, "%d findings for %d teams (%d high)", len(send), len(r.Teams()), high)
	if n := len(r.Findings) - len(send); n > 0 {
		fmt.Fprintf(&b, ", plus %d with no owning team", n)
	}
	fmt.Fprintf(&b, " · %d clusters scanned", len(r.Scanned))
	if len(r.Failed) > 0 {
		fmt.Fprintf(&b, ", **%d failed: %s** (partial report)", len(r.Failed), strings.Join(sortedKeys(r.Failed), ", "))
	}
	fmt.Fprintf(&b, "\n`report approve %s` · `report reject %s` · `report preview %s <team>` · expires %s",
		r.ID, r.ID, r.ID, FormatTime(r.Expires, loc))
	return b.String()
}

// PreviewDetail is the thread under the root: per team, who would receive it,
// what has no owner, what was ignored, and what failed.
func PreviewDetail(r Report) string {
	var b strings.Builder
	byTeam := map[string][]Finding{}
	var unowned []Finding
	for _, f := range r.Findings {
		if f.Team == "" {
			unowned = append(unowned, f)
			continue
		}
		byTeam[f.Team] = append(byTeam[f.Team], f)
	}
	missing := toSet(r.Missing)

	if len(byTeam) == 0 {
		b.WriteString("No findings for any team.\n")
	} else {
		b.WriteString("**Per team**\n\n| team | clusters | high | medium | recipients |\n|---|---|---:|---:|---|\n")
		for _, team := range sortedKeys(byTeam) {
			fs := byTeam[team]
			high, medium := counts(fs)
			fmt.Fprintf(&b, "| %s%s | %s | %d | %d | %s |\n", team, excludedMark(r, team),
				strings.Join(clustersOf(fs), ", "), high, medium, recipientsCell(r.Admins[team], missing))
		}
	}

	if len(unowned) > 0 {
		b.WriteString("\n**No owning team** (namespace has no `snappcloud.io/team` label; not sent to anyone)\n\n")
		b.WriteString("| cluster | namespace | finding | rule |\n|---|---|---|---|\n")
		for _, f := range unowned {
			fmt.Fprintf(&b, "| %s | %s | %s | `%s` |\n", f.Cluster, f.Namespace, cell(f.Title), f.Rule)
		}
	}

	if len(r.Ignored) > 0 {
		parts := make([]string, 0, len(r.Ignored))
		for _, rule := range sortedKeys(r.Ignored) {
			parts = append(parts, fmt.Sprintf("`%s` %d", rule, r.Ignored[rule]))
		}
		fmt.Fprintf(&b, "\nIgnored by `%s`: %s\n", AnnotationIgnore, strings.Join(parts, " · "))
	}

	if r.Diff.Previous != "" {
		fmt.Fprintf(&b, "\nSince `%s`: +%d new · −%d fixed · %d open for 3+ weeks\n",
			r.Diff.Previous, r.Diff.New, r.Diff.Fixed, r.Diff.OpenLong)
	}

	if len(r.Missing) > 0 {
		fmt.Fprintf(&b, "\n**No Mattermost account** for %s: they will not receive it. Fix the email in the Team CR, "+
			"or ask them to sign in once.\n", strings.Join(r.Missing, ", "))
	}
	if len(r.Failed) > 0 {
		b.WriteString("\n**Clusters not scanned** — their findings are missing from this report:\n")
		for _, c := range sortedKeys(r.Failed) {
			fmt.Fprintf(&b, "- %s: %s\n", c, firstLine(r.Failed[c], 200))
		}
	}
	return b.String()
}

func excludedMark(r Report, team string) string {
	if contains(r.Excluded, team) {
		return " _(excluded)_"
	}
	return ""
}

func recipientsCell(admins []string, missing map[string]bool) string {
	if len(admins) == 0 {
		return "**none** (no admins in the Team CR)"
	}
	parts := make([]string, 0, len(admins))
	for _, a := range admins {
		if missing[a] {
			a = "~~" + a + "~~"
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, ", ")
}

// TeamSection is one team's findings, as its admins read them.
func TeamSection(team string, fs []Finding, docs map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "#### Team `%s`\n", team)
	for _, f := range fs {
		fmt.Fprintf(&b, "\n%s **%s / %s — %s**\n%s\n**Fix:** %s", severityMark[f.Severity], f.Cluster, f.Namespace,
			f.Title, f.Detail, f.Fix)
		if url := docs[Topic(f.Rule)]; url != "" {
			fmt.Fprintf(&b, " [Docs](%s)", url)
		}
		fmt.Fprintf(&b, " · `%s`\n", f.Rule)
	}
	return b.String()
}

// DM is the whole message one team admin receives: a section per team they
// administer.
func DM(sections []string) string {
	var b strings.Builder
	b.WriteString("**Weekly SnappCloud best-practice check**\n")
	b.WriteString("These can cause downtime, or block node maintenance, when a node is drained.\n\n")
	b.WriteString(strings.Join(sections, "\n"))
	fmt.Fprintf(&b, "\n---\nIntentional? Annotate the object with `%s: <rule>`, using the rule shown at the end "+
		"of the finding. Questions: reply here.", AnnotationIgnore)
	return b.String()
}

// GroupByTeam splits findings by team, in team order.
func GroupByTeam(fs []Finding) map[string][]Finding {
	out := map[string][]Finding{}
	for _, f := range fs {
		out[f.Team] = append(out[f.Team], f)
	}
	return out
}

// DeliverySummary is posted in the report thread once sending is done.
func DeliverySummary(r Report, loc *time.Location) string {
	d := r.Delivery
	if d == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "📬 Approved by %s at %s. Sent to **%d** team admins", r.DecidedBy, FormatTime(r.DecidedAt, loc), len(d.Sent))
	fmt.Fprintf(&b, " · %d resolved since the preview · %d new (held for next week)", d.Resolved, d.Held)
	if d.Unverified > 0 {
		fmt.Fprintf(&b, " · %d sent as previewed (their cluster could not be re-checked)", d.Unverified)
	}
	b.WriteString(".\n")
	if d.Interrupted {
		b.WriteString("\n**Sending was interrupted** (timeout or shutdown); the admins not listed as sent did not get it.\n")
	}
	line := func(label string, xs []string) {
		if len(xs) > 0 {
			fmt.Fprintf(&b, "\n- %s: %s", label, strings.Join(xs, ", "))
		}
	}
	line("No Mattermost account", d.NoAccount)
	line("Access check failed, nothing sent (fail-closed)", d.AuthzFailed)
	line("Post failed", d.PostFailed)
	line("Nothing they can see (every finding is in a namespace they cannot access)", d.NothingVisible)
	if d.ScopeDropped > 0 {
		fmt.Fprintf(&b, "\n- %d findings withheld from recipients without access to their namespace", d.ScopeDropped)
	}
	return b.String()
}

// StatusLine summarises a report for `report status`.
func StatusLine(r Report, loc *time.Location) string {
	s := fmt.Sprintf("Report `%s` from %s: **%s**", r.ID, FormatTime(r.Created, loc), r.Status)
	switch r.Status {
	case StatusPending:
		s += fmt.Sprintf(", waiting for approval until %s", FormatTime(r.Expires, loc))
	case StatusSent, StatusRejected, StatusSending:
		if r.DecidedBy != "" {
			s += fmt.Sprintf(" by %s at %s", r.DecidedBy, FormatTime(r.DecidedAt, loc))
		}
	case StatusExpired:
		if r.SupersededBy != "" {
			s += fmt.Sprintf(", superseded by `%s`", r.SupersededBy)
		}
	}
	return s + fmt.Sprintf(". %d findings for %d teams.", len(r.Sendable()), len(r.Teams()))
}

func clustersOf(fs []Finding) []string {
	seen := map[string]bool{}
	for _, f := range fs {
		seen[f.Cluster] = true
	}
	return sortedKeys(seen)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// cell keeps text inside one markdown table cell.
func cell(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "|", "\\|")), " ")
}

func firstLine(s string, max int) string {
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	if r := []rune(s); len(r) > max {
		s = string(r[:max-1]) + "…"
	}
	return s
}

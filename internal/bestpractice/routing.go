package bestpractice

import (
	"fmt"
	netmail "net/mail"
	"slices"
	"strings"
)

// Routing decides who receives which findings. Each group of teams gets one
// email, to the union of its teams' admins; the findings of every team in no
// group go in one email to Others. Cc is on every email.
type Routing struct {
	Groups [][]string
	Others []string
	Cc     []string
}

// Validate rejects a team in two groups, an empty group and a bad address.
func (rt Routing) Validate() error {
	in := map[string]int{}
	for i, g := range rt.Groups {
		if len(g) == 0 {
			return fmt.Errorf("group %d is empty", i+1)
		}
		for _, t := range g {
			if t = strings.TrimSpace(t); t == "" {
				return fmt.Errorf("group %d has an empty team name", i+1)
			}
			if j, ok := in[t]; ok {
				return fmt.Errorf("team %q is in group %d and group %d", t, j+1, i+1)
			}
			in[t] = i
		}
	}
	for _, a := range slices.Concat(rt.Others, rt.Cc) {
		if _, err := netmail.ParseAddress(a); err != nil {
			return fmt.Errorf("%q is not an email address", a)
		}
	}
	return nil
}

// OthersName labels the email that carries the teams in no group.
const OthersName = "other teams"

// Mailing is one email of a report.
type Mailing struct {
	// Name labels it in the admin channel: its group's teams, or OthersName.
	Name string
	// Teams are the teams it has findings for, sorted.
	Teams    []string
	Findings []Finding
	To, Cc   []string
	// NoAdmins are the teams in it whose Team CR names no admin.
	NoAdmins []string
}

// Mailings splits the findings into emails: the groups in configured order,
// then the other teams. A group without findings sends nothing. When a
// mailing has nobody in To, the Cc receive it alone.
func (rt Routing) Mailings(fs []Finding, admins map[string][]string) []Mailing {
	byTeam := GroupByTeam(fs)
	grouped := map[string]bool{}
	var out []Mailing
	for _, g := range rt.Groups {
		m := Mailing{Name: strings.Join(g, ", "), Cc: rt.Cc}
		for _, t := range g {
			grouped[t] = true
			if len(byTeam[t]) == 0 {
				continue
			}
			m.Teams = append(m.Teams, t)
			if len(admins[t]) == 0 {
				m.NoAdmins = append(m.NoAdmins, t)
			}
			for _, a := range admins[t] {
				if !slices.Contains(m.To, a) {
					m.To = append(m.To, a)
				}
			}
		}
		if len(m.Teams) > 0 {
			slices.Sort(m.Teams)
			m.Findings = teamFindings(byTeam, m.Teams)
			out = append(out, m)
		}
	}
	others := Mailing{Name: OthersName, To: rt.Others, Cc: rt.Cc}
	for _, t := range sortedKeys(byTeam) {
		if !grouped[t] {
			others.Teams = append(others.Teams, t)
		}
	}
	if len(others.Teams) > 0 {
		others.Findings = teamFindings(byTeam, others.Teams)
		out = append(out, others)
	}
	return out
}

// Recipients reports whether the mailing reaches anyone.
func (m Mailing) Recipients() bool { return len(m.To)+len(m.Cc) > 0 }

// MailingOf returns the mailing that carries a team's findings.
func MailingOf(ms []Mailing, team string) (Mailing, bool) {
	for _, m := range ms {
		if slices.Contains(m.Teams, team) {
			return m, true
		}
	}
	return Mailing{}, false
}

func teamFindings(byTeam map[string][]Finding, teams []string) []Finding {
	var out []Finding
	for _, t := range teams {
		out = append(out, byTeam[t]...)
	}
	return out
}

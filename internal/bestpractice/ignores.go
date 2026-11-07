package bestpractice

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
)

// The bot alone decides what is ignored: a team asks it in chat, and it records
// who asked, when and why. There is no annotation to set, so "who set this?"
// always has an answer. Records are never deleted, only marked revoked, so the
// history survives a change of mind.

// IgnoreTTL is how long an ignore lasts. It ends on its own, so the decision is
// made again rather than outliving the people who made it.
const IgnoreTTL = 180 * 24 * time.Hour

// maxReasonRunes keeps a reason readable in a table and in the PDF.
const maxReasonRunes = 300

// Ignore is one request to leave a finding out of the reports.
type Ignore struct {
	Cluster   string    `json:"cluster"`
	Namespace string    `json:"ns"`
	Kind      string    `json:"kind"`
	Name      string    `json:"name"`
	Rule      string    `json:"rule"`
	By        string    `json:"by"`
	Reason    string    `json:"reason"`
	At        time.Time `json:"at"`
	Expires   time.Time `json:"expires"`
	// RevokedBy is who ended it early, by removing or renewing it.
	RevokedBy string    `json:"revokedBy,omitempty"`
	RevokedAt time.Time `json:"revokedAt,omitempty"`
}

// Key is what an ignore matches a finding on, in lower case, because the
// command is typed by hand.
func (i Ignore) Key() string {
	return IgnoreKey(i.Cluster, i.Namespace, i.Kind, i.Name, i.Rule)
}

func (i Ignore) active(now time.Time) bool {
	return i.RevokedBy == "" && now.Before(i.Expires)
}

// IgnoreKey builds the key of one object's rule.
func IgnoreKey(cluster, namespace, kind, name, rule string) string {
	return strings.ToLower(cluster + "/" + namespace + "/" + kind + "/" + name + "/" + rule)
}

// IgnoredFinding is a finding left out of the report, and the request behind
// that. The team still sees it, so an ignore cannot go unnoticed.
type IgnoredFinding struct {
	Finding Finding `json:"finding"`
	Ignore  Ignore  `json:"ignore"`
}

// Target is the object a command names, as cluster/namespace/kind/name.
func (f Finding) Target() string {
	return f.Cluster + "/" + f.Namespace + "/" + f.Kind + "/" + f.Name
}

// Target is the object an ignore names, as a command gives it.
func (i Ignore) Target() string {
	return i.Cluster + "/" + i.Namespace + "/" + i.Kind + "/" + i.Name
}

// IgnoreCommand is what a team sends the bot to ignore a finding.
func IgnoreCommand(f Finding) string {
	return "ignore " + f.Target() + " " + f.Rule + " reason: <why>"
}

// IgnorableRules lists the rules a team can ignore.
func IgnorableRules() []string {
	var out []string
	for _, r := range Rules {
		if !DrainBlocker(r) {
			out = append(out, r)
		}
	}
	return out
}

// CleanReason checks a reason and makes it safe to show: on one line, and with
// nothing that would break a table or a code span in chat. Only Latin letters
// are accepted, because the PDF fonts cannot show other scripts and a reason
// that prints as dots is no record at all.
func CleanReason(s string) (string, error) {
	s = strings.Join(strings.Fields(s), " ")
	s = strings.NewReplacer("`", "'", "|", "/").Replace(s)
	if s == "" {
		return "", errors.New("a reason is required")
	}
	runes := []rune(s)
	if len(runes) > maxReasonRunes {
		return "", fmt.Errorf("the reason is too long: keep it under %d characters", maxReasonRunes)
	}
	for _, r := range runes {
		if r > unicode.MaxLatin1 || !unicode.IsPrint(r) {
			return "", errors.New("write the reason in English: the report PDF cannot show other scripts")
		}
	}
	return s, nil
}

// ErrNoIgnore means nothing active is ignored under that key.
var ErrNoIgnore = errors.New("no active ignore for that finding")

// AddIgnore records a request. One already active for the finding is ended by
// the new one (renewed reports that). When it cannot be saved nothing changes:
// an ignore without a record is what this exists to prevent.
func (s *Store) AddIgnore(i Ignore, now time.Time) (renewed bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := slices.Clone(s.st.Ignores)
	for n := range s.st.Ignores {
		if old := &s.st.Ignores[n]; old.Key() == i.Key() && old.active(now) {
			old.RevokedBy, old.RevokedAt = i.By, now
			renewed = true
		}
	}
	i.At, i.Expires = now, now.Add(IgnoreTTL)
	s.st.Ignores = append(s.st.Ignores, i)
	if err := s.flush(); err != nil {
		s.st.Ignores = before
		return false, err
	}
	return renewed, nil
}

// RevokeIgnore ends the active ignore under key, keeping its record.
func (s *Store) RevokeIgnore(key, by string, now time.Time) (Ignore, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := slices.Clone(s.st.Ignores)
	for n := range s.st.Ignores {
		i := &s.st.Ignores[n]
		if i.Key() != key || !i.active(now) {
			continue
		}
		i.RevokedBy, i.RevokedAt = by, now
		out := *i
		if err := s.flush(); err != nil {
			s.st.Ignores = before
			return Ignore{}, err
		}
		return out, nil
	}
	return Ignore{}, ErrNoIgnore
}

// ActiveIgnores returns what is ignored now, by key.
func (s *Store) ActiveIgnores(now time.Time) map[string]Ignore {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]Ignore{}
	for _, i := range s.st.Ignores {
		if i.active(now) {
			out[i.Key()] = i
		}
	}
	return out
}

// FindFinding looks a key up in the reports kept, newest first, including what
// they list as ignored. A request must name a real finding: a typo would
// otherwise be recorded, and the team would believe it had worked.
func (s *Store) FindFinding(key string) (Finding, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range slices.Backward(s.st.Reports) {
		for _, f := range r.Findings {
			if strings.ToLower(f.Key()) == key {
				return f, true
			}
		}
		for _, ig := range r.Ignored {
			if strings.ToLower(ig.Finding.Key()) == key {
				return ig.Finding, true
			}
		}
	}
	return Finding{}, false
}

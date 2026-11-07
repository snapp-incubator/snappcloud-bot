package bestpractice

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var ignoreNow = time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)

func redisIgnore(by string) Ignore {
	return Ignore{Cluster: "teh-1", Namespace: "pay", Kind: "Redis", Name: "cache", Rule: RuleRedisSingle, By: by, Reason: "cache only"}
}

// The record is the point: it must survive a restart, with the history of what
// ended, and an ignore must end by itself.
func TestIgnoreRecordsPersistAndEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := NewStore(path, 0)
	key := redisIgnore("").Key()
	if renewed, err := s.AddIgnore(redisIgnore("jane"), ignoreNow); err != nil || renewed {
		t.Fatalf("add: renewed %v err %v", renewed, err)
	}
	if key != "teh-1/pay/redis/cache/redis-single-instance" {
		t.Errorf("key = %s", key)
	}
	renewed, err := s.AddIgnore(redisIgnore("sam"), ignoreNow.Add(time.Hour))
	if err != nil || !renewed {
		t.Fatalf("renew: renewed %v err %v", renewed, err)
	}

	s = NewStore(path, 0)
	if got := s.ActiveIgnores(ignoreNow.Add(2 * time.Hour)); len(got) != 1 || got[key].By != "sam" || got[key].Reason != "cache only" {
		t.Fatalf("after restart: %+v", got)
	}
	if len(s.st.Ignores) != 2 || s.st.Ignores[0].By != "jane" || s.st.Ignores[0].RevokedBy != "sam" {
		t.Errorf("history must keep who asked first: %+v", s.st.Ignores)
	}
	if got := s.ActiveIgnores(ignoreNow.Add(IgnoreTTL + 2*time.Hour)); len(got) != 0 {
		t.Errorf("must expire: %+v", got)
	}

	if _, err := s.RevokeIgnore(key, "lee", ignoreNow.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := s.ActiveIgnores(ignoreNow.Add(2 * time.Hour)); len(got) != 0 {
		t.Errorf("revoked: %+v", got)
	}
	if _, err := s.RevokeIgnore(key, "lee", ignoreNow.Add(2*time.Hour)); !errors.Is(err, ErrNoIgnore) {
		t.Errorf("second revoke: %v", err)
	}
	if len(s.st.Ignores) != 2 {
		t.Errorf("a revoke must not delete the record: %+v", s.st.Ignores)
	}
}

// An ignore nobody can later prove must not take effect.
func TestIgnoreNotKeptWhenItCannotBeSaved(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	s := NewStore(filepath.Join(blocker, "x", "state.json"), 0)
	s.path = filepath.Join(blocker, "x", "state.json")
	if err := writeFile(blocker); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddIgnore(redisIgnore("jane"), ignoreNow); err == nil {
		t.Fatal("want a save error")
	}
	if got := s.ActiveIgnores(ignoreNow); len(got) != 0 {
		t.Errorf("unsaved ignore is active: %+v", got)
	}
}

func TestCleanReason(t *testing.T) {
	if got, err := CleanReason("  a `cache`\n | safe "); err != nil || got != "a 'cache' / safe" {
		t.Errorf("got %q, %v", got, err)
	}
	for _, bad := range []string{"", "   ", "دلیل", strings.Repeat("x", maxReasonRunes+1)} {
		if _, err := CleanReason(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

// A team whose only finding is ignored still gets an email, and it names who
// asked: that is how the team's admins come to know.
func TestEmailListsIgnoredFindings(t *testing.T) {
	old := pdfCompress
	pdfCompress = false
	defer func() { pdfCompress = old }()
	f := teamFinding("pay", RuleRedisSingle, High)
	ig := IgnoredFinding{Finding: f, Ignore: Ignore{Cluster: f.Cluster, Namespace: f.Namespace, Kind: f.Kind, Name: f.Name,
		Rule: f.Rule, By: "jane", Reason: "cache only", At: ignoreNow, Expires: ignoreNow.Add(IgnoreTTL)}}

	ms := Routing{Groups: [][]string{{"pay"}}}.Mailings(nil, []IgnoredFinding{ig}, map[string][]string{"pay": {"a@snapp.cab"}})
	if len(ms) != 1 || len(ms[0].Findings) != 0 || len(ms[0].Ignored) != 1 || ms[0].To[0] != "a@snapp.cab" {
		t.Fatalf("mailings = %+v", ms)
	}
	ms = Routing{}.Mailings(nil, []IgnoredFinding{ig}, nil)
	if len(ms) != 1 || ms[0].Name != OthersName || ms[0].Teams[0] != "pay" {
		t.Fatalf("others = %+v", ms)
	}

	e, err := ReportEmail(ms[0], nil, nil, false, ignoreNow, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.Subject, "(1 ignored)") || !strings.Contains(e.Text, "asked to ignore") {
		t.Errorf("subject %q text:\n%s", e.Subject, e.Text)
	}
	for _, want := range []string{"Ignored on request", "Asked by jane on 7 Nov 2026", "Reason: cache only", "unignore "} {
		if !bytes.Contains(e.PDF, []byte(want)) {
			t.Errorf("PDF missing %q", want)
		}
	}

	table := IgnoreTable([]Ignore{ig.Ignore})
	if !strings.Contains(table, "jane") || !strings.Contains(table, "`cache only`") {
		t.Errorf("table:\n%s", table)
	}
}

func writeFile(path string) error { return os.WriteFile(path, []byte("x"), 0o600) }

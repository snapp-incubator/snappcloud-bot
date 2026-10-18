package bestpractice

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func finding(cluster, team, name string) Finding {
	return Finding{Rule: RuleMinAvailableAll, Severity: High, Cluster: cluster, Namespace: team + "-prod",
		Kind: "PodDisruptionBudget", Name: name, Team: team}
}

func TestIntersect(t *testing.T) {
	approved := []Finding{finding("a", "pay", "fixed"), finding("a", "pay", "still"), finding("b", "pay", "unread")}
	current := []Finding{finding("a", "pay", "still"), finding("a", "pay", "new")}
	send, resolved, held, unverified := Intersect(approved, current, map[string]bool{"a": true})
	if len(send) != 2 || send[0].Name != "still" || send[1].Name != "unread" {
		t.Errorf("send = %+v", send)
	}
	if resolved != 1 || held != 1 || unverified != 1 {
		t.Errorf("resolved=%d held=%d unverified=%d, want 1 1 1", resolved, held, unverified)
	}
}

func TestStoreLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bp.json")
	s := NewStore(path, 48*time.Hour)
	t0 := time.Date(2026, 10, 17, 20, 30, 0, 0, time.UTC)

	first := &Report{Findings: []Finding{finding("a", "pay", "x"), finding("a", "pay", "y")}, Scanned: []string{"a", "b"}}
	if sup, err := s.Add(first, t0); err != nil || len(sup) != 0 {
		t.Fatalf("add: %v %v", sup, err)
	}
	if first.ID == "" || first.Status != StatusPending || !first.Expires.Equal(t0.Add(48*time.Hour)) {
		t.Fatalf("first = %+v", first)
	}

	// A day later the first is still pending: a new build supersedes it.
	t1 := t0.Add(24 * time.Hour)
	second := &Report{Findings: []Finding{finding("a", "pay", "y"), finding("a", "pay", "z")}, Scanned: []string{"a"}}
	sup, err := s.Add(second, t1)
	if err != nil {
		t.Fatal(err)
	}
	if len(sup) != 1 || sup[0].ID != first.ID || sup[0].SupersededBy != second.ID {
		t.Errorf("superseded = %+v", sup)
	}
	if d := second.Diff; d.Previous != first.ID || d.New != 1 || d.Fixed != 1 {
		t.Errorf("diff = %+v, want +1 -1 against %s", d, first.ID)
	}

	// Persisted: a restart sees the same state.
	s2 := NewStore(path, 48*time.Hour)
	got, err := s2.Get(second.ID, t1)
	if err != nil || got.Status != StatusPending || len(got.Findings) != 2 {
		t.Fatalf("reloaded = %+v, %v", got, err)
	}
	if old, _ := s2.Get(first.ID, t1); old.Status != StatusExpired {
		t.Errorf("superseded report status = %s", old.Status)
	}

	// Past its expiry, a pending report can no longer be approved.
	if late, _ := s2.Get(second.ID, t1.Add(49*time.Hour)); late.Status != StatusExpired {
		t.Errorf("status after expiry = %s", late.Status)
	}
}

func TestOpenLongCountsFindingsThreeWeeksOld(t *testing.T) {
	s := NewStore("", 0)
	t0 := time.Now()
	for week := 0; week < 4; week++ {
		r := &Report{Findings: []Finding{finding("a", "pay", "old")}, Scanned: []string{"a"}}
		if week == 3 {
			r.Findings = append(r.Findings, finding("a", "pay", "fresh"))
		}
		if _, err := s.Add(r, t0.Add(time.Duration(week)*7*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if week == 3 && r.Diff.OpenLong != 1 {
			t.Errorf("openLong = %d, want 1", r.Diff.OpenLong)
		}
	}
}

func TestUpdateAbortsOnError(t *testing.T) {
	s := NewStore("", 0)
	r := &Report{}
	now := time.Now()
	_, _ = s.Add(r, now)
	_, err := s.Update(r.ID, now, func(r *Report) error {
		r.Status = StatusSent
		return context.Canceled
	})
	if got, _ := s.Get(r.ID, now); err == nil || got.Status != StatusPending {
		t.Errorf("a failed update must not apply: %v %s", err, got.Status)
	}
}

func tehran(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Tehran")
	if err != nil {
		t.Skip("no tzdata")
	}
	return loc
}

func TestSlotsSaturdayMidnight(t *testing.T) {
	loc := tehran(t)
	sl, err := ParseSlots("every saturday at 00:00", loc)
	if err != nil {
		t.Fatal(err)
	}
	// Sunday 18 Oct 2026, 10:00 Tehran.
	now := time.Date(2026, 10, 18, 10, 0, 0, 0, loc)
	if got, want := sl.Last(now), time.Date(2026, 10, 17, 0, 0, 0, 0, loc); !got.Equal(want) {
		t.Errorf("last = %s, want %s", got, want)
	}
	if got, want := sl.Next(now), time.Date(2026, 10, 24, 0, 0, 0, 0, loc); !got.Equal(want) {
		t.Errorf("next = %s, want %s", got, want)
	}
	// Exactly at the slot, it is the last one.
	at := time.Date(2026, 10, 24, 0, 0, 0, 0, loc)
	if got := sl.Last(at); !got.Equal(at) {
		t.Errorf("last at the slot = %s", got)
	}

	for _, bad := range []string{"every 6h", "every saturday at 00:00 and more", "whenever"} {
		if _, err := ParseSlots(bad, loc); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
	if sl, err := ParseSlots("", loc); sl != nil || err != nil {
		t.Errorf("empty spec = no timer, got %v %v", sl, err)
	}
}

func newTestRunner(t *testing.T, now *time.Time, builds *atomic.Int32) (*Runner, *Store) {
	t.Helper()
	loc := tehran(t)
	sl, err := ParseSlots("every saturday at 00:00", loc)
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore("", 0)
	r := NewRunner(store, sl, func(context.Context) error { builds.Add(1); return nil },
		RunnerOptions{CatchUp: 12 * time.Hour}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.now = func() time.Time { return *now }
	r.base = context.Background()
	return r, store
}

func TestRunnerFiresOncePerSlot(t *testing.T) {
	loc := tehran(t)
	var builds atomic.Int32
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, loc) // Friday
	r, _ := newTestRunner(t, &now, &builds)

	// First start: the past slot is marked, not fired.
	r.check()
	r.wait()
	if builds.Load() != 0 {
		t.Fatal("first start must not fire a past slot")
	}

	now = time.Date(2026, 10, 17, 0, 0, 30, 0, loc) // Saturday 00:00:30
	r.check()
	r.wait()
	r.check()
	r.wait()
	if builds.Load() != 1 {
		t.Fatalf("builds = %d, want exactly 1 for the slot", builds.Load())
	}
}

func TestRunnerCatchUp(t *testing.T) {
	loc := tehran(t)
	var builds atomic.Int32
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, loc)
	r, store := newTestRunner(t, &now, &builds)
	r.check() // marks the previous Saturday

	// Down over midnight, back at 08:00: inside the window, so it fires.
	now = time.Date(2026, 10, 17, 8, 0, 0, 0, loc)
	r.check()
	r.wait()
	if builds.Load() != 1 {
		t.Fatalf("within catch-up: builds = %d", builds.Load())
	}

	// Down past the window next week: skipped and marked.
	now = time.Date(2026, 10, 24, 13, 0, 0, 0, loc)
	r.check()
	r.wait()
	if builds.Load() != 1 {
		t.Errorf("past catch-up must skip, builds = %d", builds.Load())
	}
	if got := store.LastSlot(); !got.Equal(time.Date(2026, 10, 24, 0, 0, 0, 0, loc)) {
		t.Errorf("skipped slot not marked: %s", got)
	}
}

func TestRunnerOneJobAtATime(t *testing.T) {
	var builds atomic.Int32
	now := time.Now()
	r, _ := newTestRunner(t, &now, &builds)
	release := make(chan struct{})
	if err := r.Do("approve", func(context.Context) error { <-release; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := r.Do("manual", func(context.Context) error { return nil }); err == nil {
		t.Error("a second job must be refused while one runs")
	}
	close(release)
	r.wait()
	if err := r.Do("manual", func(context.Context) error { return nil }); err != nil {
		t.Errorf("after the first finished: %v", err)
	}
	r.wait()
}

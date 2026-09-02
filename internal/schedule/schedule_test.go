package schedule

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func entry(user, q string, every time.Duration, next time.Time) *Entry {
	return &Entry{User: user, Query: q, Every: Duration(every), Next: next, ChannelID: "c"}
}

func TestAddEnforcesPerUserLimit(t *testing.T) {
	s := NewStore("", Limits{PerUser: 2, MinInterval: time.Hour})
	for i := 0; i < 2; i++ {
		if err := s.Add(entry("u", "q", time.Hour, time.Now())); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Add(entry("u", "q", time.Hour, time.Now())); !errors.Is(err, ErrTooManyForUser) {
		t.Fatalf("err = %v, want ErrTooManyForUser", err)
	}
	// A different user is unaffected.
	if err := s.Add(entry("other", "q", time.Hour, time.Now())); err != nil {
		t.Fatal(err)
	}
}

func TestAddEnforcesMinInterval(t *testing.T) {
	s := NewStore("", Limits{MinInterval: time.Hour})
	if err := s.Add(entry("u", "q", time.Minute, time.Now())); !errors.Is(err, ErrTooFrequent) {
		t.Fatalf("err = %v, want ErrTooFrequent", err)
	}
}

func TestAddEnforcesTotalLimit(t *testing.T) {
	s := NewStore("", Limits{PerUser: 10, Total: 1, MinInterval: time.Hour})
	_ = s.Add(entry("a", "q", time.Hour, time.Now()))
	if err := s.Add(entry("b", "q", time.Hour, time.Now())); !errors.Is(err, ErrTooManyTotal) {
		t.Fatalf("err = %v, want ErrTooManyTotal", err)
	}
}

// Due must advance the entry so a slow run cannot fire the same schedule twice.
func TestDueAdvancesAndDoesNotRepeat(t *testing.T) {
	s := NewStore("", Limits{MinInterval: time.Hour})
	now := time.Now()
	_ = s.Add(entry("u", "q", time.Hour, now.Add(-time.Minute)))

	if got := s.Due(now); len(got) != 1 {
		t.Fatalf("expected 1 due, got %d", len(got))
	}
	if got := s.Due(now); len(got) != 0 {
		t.Fatalf("schedule fired twice for the same instant: %d", len(got))
	}
}

func TestDeleteOnlyOwn(t *testing.T) {
	s := NewStore("", Limits{MinInterval: time.Hour})
	_ = s.Add(entry("owner", "q", time.Hour, time.Now()))
	id := s.List("owner")[0].ID

	if err := s.Delete("someone-else", id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a user must not delete another user's schedule (err=%v)", err)
	}
	if err := s.Delete("owner", id); err != nil {
		t.Fatal(err)
	}
	if len(s.List("owner")) != 0 {
		t.Fatal("schedule not deleted")
	}
}

func TestRepeatedFailuresDisableSchedule(t *testing.T) {
	s := NewStore("", Limits{MinInterval: time.Hour, MaxFailures: 3})
	_ = s.Add(entry("u", "q", time.Hour, time.Now()))
	id := s.List("u")[0].ID

	for i := 0; i < 2; i++ {
		if removed := s.RecordResult(id, errors.New("boom")); removed {
			t.Fatalf("removed too early at failure %d", i+1)
		}
	}
	if removed := s.RecordResult(id, errors.New("boom")); !removed {
		t.Fatal("schedule should be disabled after MaxFailures")
	}
	if len(s.List("u")) != 0 {
		t.Fatal("disabled schedule still listed")
	}
}

func TestSuccessResetsFailures(t *testing.T) {
	s := NewStore("", Limits{MinInterval: time.Hour, MaxFailures: 2})
	_ = s.Add(entry("u", "q", time.Hour, time.Now()))
	id := s.List("u")[0].ID
	s.RecordResult(id, errors.New("boom"))
	s.RecordResult(id, nil) // recovery
	if removed := s.RecordResult(id, errors.New("boom")); removed {
		t.Fatal("a success must reset the failure counter")
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedules.json")
	s := NewStore(path, Limits{MinInterval: time.Hour})
	_ = s.Add(entry("u", "check pods", time.Hour, time.Now().Add(time.Hour)))
	s.Flush()

	reloaded := NewStore(path, Limits{MinInterval: time.Hour})
	got := reloaded.List("u")
	if len(got) != 1 || got[0].Query != "check pods" {
		t.Fatalf("reloaded = %+v", got)
	}
	// IDs must not be reused after a reload.
	_ = reloaded.Add(entry("u", "second", time.Hour, time.Now()))
	all := reloaded.List("u")
	if all[0].ID == all[1].ID {
		t.Fatalf("duplicate ids after reload: %+v", all)
	}
}

type fakeAnswerer struct {
	mu   sync.Mutex
	runs []string
	err  error
	cur  int
	peak int
}

func (f *fakeAnswerer) RunScheduled(_ context.Context, e Entry) error {
	f.mu.Lock()
	f.cur++
	if f.cur > f.peak {
		f.peak = f.cur
	}
	f.runs = append(f.runs, e.ID)
	f.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	f.mu.Lock()
	f.cur--
	f.mu.Unlock()
	return f.err
}

func TestRunnerRespectsConcurrency(t *testing.T) {
	s := NewStore("", Limits{PerUser: 10, MinInterval: time.Hour})
	now := time.Now()
	for i := 0; i < 5; i++ {
		_ = s.Add(entry("u", "q", time.Hour, now.Add(-time.Minute)))
	}
	a := &fakeAnswerer{}
	r := NewRunner(s, a, RunnerOptions{Concurrency: 2}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.runDue(context.Background(), now)

	if len(a.runs) != 5 {
		t.Fatalf("ran %d schedules, want 5", len(a.runs))
	}
	if a.peak > 2 {
		t.Fatalf("concurrency cap breached: peak %d", a.peak)
	}
}

func TestStatsCountsDistinctOwners(t *testing.T) {
	s := NewStore("", Limits{PerUser: 10, MinInterval: time.Hour})
	now := time.Now()
	_ = s.Add(entry("alice", "a", time.Hour, now))
	_ = s.Add(entry("alice", "b", time.Hour, now))
	_ = s.Add(entry("bob", "c", time.Hour, now))

	total, owners := s.Stats()
	if total != 3 || owners != 2 {
		t.Fatalf("Stats() = (%d, %d), want (3, 2)", total, owners)
	}
}

// A skipped run means the owner lost access: retrying cannot help, so it must
// not consume the failure budget that eventually deletes the schedule.
func TestSkippedRunsDoNotDisableSchedule(t *testing.T) {
	s := NewStore("", Limits{PerUser: 10, MinInterval: time.Hour, MaxFailures: 2})
	now := time.Now()
	_ = s.Add(entry("u", "q", time.Hour, now.Add(-time.Minute)))

	a := &fakeAnswerer{err: ErrSkipped}
	r := NewRunner(s, a, RunnerOptions{Concurrency: 1}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for i := 0; i < 5; i++ {
		r.runDue(context.Background(), now.Add(time.Duration(i)*time.Hour))
	}

	if got := s.Count(); got != 1 {
		t.Fatalf("schedule removed after skips: count = %d, want 1", got)
	}
	if got := s.List("u")[0].Failures; got != 0 {
		t.Fatalf("skips counted as failures: %d, want 0", got)
	}
}

package schedule

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// blockingAnswerer holds every run open until released, so what is in flight at
// once can be observed.
type blockingAnswerer struct {
	mu      sync.Mutex
	cur     int
	peak    int
	started chan string
	release chan struct{}
}

func (b *blockingAnswerer) RunScheduled(_ context.Context, e Entry) error {
	b.mu.Lock()
	b.cur++
	if b.cur > b.peak {
		b.peak = b.cur
	}
	b.mu.Unlock()
	select {
	case b.started <- e.ID:
	default:
	}
	<-b.release
	b.mu.Lock()
	b.cur--
	b.mu.Unlock()
	return nil
}

func (b *blockingAnswerer) highWater() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.peak
}

func startedRunner(t *testing.T, a Answerer, concurrency int) (*Runner, func()) {
	t.Helper()
	s := NewStore("", Limits{PerUser: 20, Total: 20, MinInterval: time.Minute})
	r := NewRunner(s, a, RunnerOptions{Concurrency: concurrency, Timeout: time.Minute}, quietLog())
	ctx, cancel := context.WithCancel(context.Background())
	// Start() captures the process context; a test needs that without the tick
	// loop running schedules of its own.
	r.mu.Lock()
	r.base = ctx
	r.mu.Unlock()
	return r, cancel
}

// A manual run shares the worker pool with timed runs: "run <id>" must not be a
// way to start unbounded agent work from a chat message. The rate limiter the
// message already passed is no protection here — a token is cheap and a run is
// half an hour of agent loop.
func TestTriggerQueuesOnTheSamePool(t *testing.T) {
	a := &blockingAnswerer{started: make(chan string, 8), release: make(chan struct{})}
	r, cancel := startedRunner(t, a, 2)
	defer cancel()
	defer close(a.release)

	for i := 0; i < 6; i++ {
		if err := r.Trigger(Entry{ID: string(rune('a' + i)), User: "u@x", Query: "q"}); err != nil {
			t.Fatalf("trigger %d: %v", i, err)
		}
	}
	// Wait for the pool to be saturated, then confirm it was never exceeded.
	<-a.started
	<-a.started
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if a.highWater() > 2 {
			t.Fatalf("%d runs at once with concurrency 2", a.highWater())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Running the same schedule twice at once would post its report twice.
func TestTriggerRefusesASecondRunOfTheSameSchedule(t *testing.T) {
	a := &blockingAnswerer{started: make(chan string, 1), release: make(chan struct{})}
	r, cancel := startedRunner(t, a, 2)
	defer cancel()
	defer close(a.release)

	e := Entry{ID: "dup", User: "u@x", Query: "q"}
	if err := r.Trigger(e); err != nil {
		t.Fatalf("first trigger: %v", err)
	}
	<-a.started
	if err := r.Trigger(e); !errors.Is(err, ErrBusy) {
		t.Fatalf("second trigger returned %v, want ErrBusy", err)
	}
}

// Before Start there is no process context to attach a run to, and a run on a
// detached background context would outlive shutdown.
func TestTriggerRefusesBeforeStart(t *testing.T) {
	s := NewStore("", Limits{PerUser: 5, Total: 5, MinInterval: time.Minute})
	r := NewRunner(s, &blockingAnswerer{started: make(chan string, 1), release: make(chan struct{})},
		RunnerOptions{}, quietLog())
	if err := r.Trigger(Entry{ID: "x", User: "u@x"}); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("got %v, want ErrNotStarted", err)
	}
}

// A manual run is an extra run, not a replacement: tonight's still happens.
func TestTriggerDoesNotAdvanceTheSchedule(t *testing.T) {
	a := &blockingAnswerer{started: make(chan string, 1), release: make(chan struct{})}
	r, cancel := startedRunner(t, a, 1)
	defer cancel()
	defer close(a.release)

	e, _, err := Parse("every day at 09:00 are any pods failing?", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e.ID, e.User, e.Query = "keep", "u@x", "q"
	before := e.Next
	if err := r.Trigger(*e); err != nil {
		t.Fatal(err)
	}
	<-a.started
	if !e.Next.Equal(before) {
		t.Fatalf("next run moved from %s to %s", before, e.Next)
	}
}

// Two schedules due at the same minute run together, and the ticker is not one
// of the things waiting for them. runDue used to take a worker slot on the
// ticker goroutine and then wait for the whole batch: two 30-minute reports due
// at 15:40 stopped the scheduling loop until both finished, so a schedule due at
// 15:45 fired whenever the batch ahead of it ended.
func TestRunDueDoesNotBlockTheSchedulingLoop(t *testing.T) {
	a := &blockingAnswerer{started: make(chan string, 4), release: make(chan struct{})}
	s := NewStore("", Limits{PerUser: 10, Total: 10, MinInterval: time.Hour})
	now := time.Now()
	for i := 0; i < 2; i++ {
		e, _, err := Parse("every 1h anything failing?", now)
		if err != nil {
			t.Fatal(err)
		}
		e.User, e.Next = "u@x", now.Add(-time.Minute)
		if err := s.Add(e); err != nil {
			t.Fatal(err)
		}
	}
	r := NewRunner(s, a, RunnerOptions{Concurrency: 2, Timeout: time.Minute}, quietLog())

	returned := make(chan struct{})
	go func() { r.runDue(context.Background(), now); close(returned) }()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("runDue blocked while its runs were still working")
	}
	// Both are genuinely in flight, not serialized.
	for i := 0; i < 2; i++ {
		select {
		case <-a.started:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of 2 schedules due at the same time started", i)
		}
	}
	close(a.release)
	r.wait()
}

// A third run with two slots waits, and says so: a report that queued looks
// exactly like one that was slow, and the two want opposite fixes.
func TestQueuedRunIsReportedNotLost(t *testing.T) {
	a := &blockingAnswerer{started: make(chan string, 4), release: make(chan struct{})}
	r, cancel := startedRunner(t, a, 2)
	defer cancel()

	for i := 0; i < 3; i++ {
		if err := r.Trigger(Entry{ID: string(rune('a' + i)), User: "u@x", Query: "q"}); err != nil {
			t.Fatalf("trigger %d: %v", i, err)
		}
	}
	<-a.started
	<-a.started
	// The third is queued, not dropped: it runs once a slot frees.
	close(a.release)
	select {
	case <-a.started:
	case <-time.After(3 * time.Second):
		t.Fatal("the queued run never started")
	}
	r.wait()
}

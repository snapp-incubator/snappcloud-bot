package schedule

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/snapp-incubator/snappcloud-bot/internal/metrics"
)

// Answerer runs one query for an owner and delivers the result. Implemented by
// the bot service, which resolves the owner's CURRENT authorization before
// running anything — a stored schedule carries no authority of its own.
type Answerer interface {
	RunScheduled(ctx context.Context, e Entry) error
}

// Runner fires due schedules. It is deliberately single-threaded apart from a
// small worker pool: scheduled work must never crowd out interactive users.
type Runner struct {
	store       *Store
	answerer    Answerer
	tick        time.Duration
	concurrency int
	timeout     time.Duration
	log         *slog.Logger

	// sem bounds every run the process makes, timed or manual: a manual run is
	// the same 40-iteration investigation against the same MCP servers, so it
	// queues behind the timer's runs rather than beside them.
	sem chan struct{}
	// inFlight guards against running one schedule twice at once — a second
	// "run <id>" while the first is still working would post the report twice.
	mu       sync.Mutex
	inFlight map[string]bool
	// wg tracks runs in flight, so shutdown and tests can wait for them.
	wg sync.WaitGroup
	// base is the process context, captured by Start. A manual run must die on
	// shutdown like any other; it must NOT outlive the process on a detached
	// background context.
	base context.Context
}

// RunnerOptions configures the runner.
type RunnerOptions struct {
	// Tick is how often due schedules are looked for (default 30s).
	Tick time.Duration
	// Concurrency caps simultaneous scheduled runs (default 2).
	Concurrency int
	// Timeout bounds one scheduled run (default 5m).
	Timeout time.Duration
}

// NewRunner builds a runner over the store.
func NewRunner(store *Store, a Answerer, o RunnerOptions, log *slog.Logger) *Runner {
	if o.Tick <= 0 {
		o.Tick = 30 * time.Second
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 2
	}
	if o.Timeout <= 0 {
		o.Timeout = 5 * time.Minute
	}
	return &Runner{store: store, answerer: a, tick: o.Tick,
		concurrency: o.Concurrency, timeout: o.Timeout, log: log,
		sem: make(chan struct{}, o.Concurrency), inFlight: map[string]bool{}}
}

// Start runs the scheduling loop until ctx is cancelled, flushing on exit.
func (r *Runner) Start(ctx context.Context) {
	r.mu.Lock()
	r.base = ctx
	r.mu.Unlock()
	metrics.ScheduleLimit.Set(float64(r.store.Limits().Total))
	r.observe()

	t := time.NewTicker(r.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// The runs' own context is this one, so they are already unwinding;
			// waiting means the final flush includes their results.
			r.wait()
			r.store.Flush()
			return
		case now := <-t.C:
			r.runDue(ctx, now)
			r.store.Flush()
			// Refresh the gauges every tick: additions and deletions happen on
			// other goroutines, and failures remove entries here.
			r.observe()
		}
	}
}

// observe publishes the current schedule inventory.
func (r *Runner) observe() {
	total, owners := r.store.Stats()
	metrics.Schedules.Set(float64(total))
	metrics.ScheduleOwners.Set(float64(owners))
}

// runDue starts everything due at now and returns immediately. The pool bounds
// how many run at once; the loop must not be one of the things waiting for it.
//
// It used to take a slot on this goroutine and then wait for the whole batch to
// finish. With a 30-minute timeout and two slots, two long reports starting
// together stopped the ticker for as long as they ran — so a schedule due five
// minutes later did not fire five minutes later, it fired when the batch ahead
// of it was done. Nothing in the logs said so: the run simply started late.
func (r *Runner) runDue(ctx context.Context, now time.Time) {
	due := r.store.Due(now)
	if len(due) == 0 {
		return
	}
	r.log.Info("scheduled runs due", "count", len(due), "slots", cap(r.sem))

	for _, e := range due {
		e := e
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.acquire(ctx, e, "scheduled")
		}()
	}
}

// wait blocks until every run in flight has finished. Used on shutdown, so a
// report that was mid-flight is flushed rather than lost, and by tests, which
// need runDue's work to settle now that it no longer waits for it.
func (r *Runner) wait() { r.wg.Wait() }

// acquire waits for a slot and runs e, or gives up if the process shuts down
// first. Waiting is logged: a run that sat in the queue looks exactly like a
// run that was slow, and the two want opposite fixes — more slots, or a
// narrower question.
func (r *Runner) acquire(ctx context.Context, e Entry, kind string) {
	start := time.Now()
	select {
	case r.sem <- struct{}{}:
	default:
		r.log.Info("schedule run waiting for a slot", "id", e.ID, "kind", kind,
			"slots", cap(r.sem), "inFlight", len(r.sem))
		select {
		case r.sem <- struct{}{}:
		case <-ctx.Done():
			r.log.Warn("schedule run abandoned while queued", "id", e.ID, "kind", kind,
				"waited", time.Since(start).Round(time.Second))
			return
		}
	}
	defer func() { <-r.sem }()
	if waited := time.Since(start); waited > time.Second {
		r.log.Info("schedule run started after a wait", "id", e.ID, "kind", kind,
			"waited", waited.Round(time.Second))
		metrics.ScheduleQueueWait.Observe(waited.Seconds())
	}
	r.run(ctx, e)
}

// run executes one schedule with everything a scheduled run gets: a bounded
// context, panic recovery, metrics, and the failure accounting that eventually
// disables a schedule that never works. Both the timer and "run <id>" go
// through here, so a manual run is the same run rather than one that resembles
// it.
func (r *Runner) run(ctx context.Context, e Entry) {
	// A panic in one run must not take the process down.
	defer func() {
		if p := recover(); p != nil {
			r.log.Error("scheduled run panicked", "id", e.ID, "user", e.User, "panic", p)
		}
	}()
	rctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	metrics.ScheduleRunsInFlight.Inc()
	start := time.Now()
	err := r.answerer.RunScheduled(rctx, e)
	metrics.ScheduleRunsInFlight.Dec()
	metrics.ScheduleRunDuration.Observe(time.Since(start).Seconds())

	// A skip is not a failure: the owner lost access, and retrying will not
	// help, so it must not burn the failure budget.
	skipped := errors.Is(err, ErrSkipped)
	switch {
	case skipped:
		metrics.ScheduleRuns.WithLabelValues("skipped").Inc()
	case err != nil:
		metrics.ScheduleRuns.WithLabelValues("error").Inc()
	default:
		metrics.ScheduleRuns.WithLabelValues("ok").Inc()
	}

	result := err
	if skipped {
		result = nil
	}
	if removed := r.store.RecordResult(e.ID, result); removed {
		metrics.ScheduleDisabled.Inc()
		r.log.Warn("schedule disabled after repeated failures",
			"id", e.ID, "user", e.User, "query", e.Query)
	}
	if err != nil && !skipped {
		r.log.Warn("scheduled run failed", "id", e.ID, "user", e.User, "err", err)
	}
}

// ErrBusy reports that the schedule is already running.
var ErrBusy = errors.New("that schedule is already running")

// ErrNotStarted reports that the runner is not accepting work yet.
var ErrNotStarted = errors.New("the scheduler is not running")

// Trigger runs one schedule now, off the caller's goroutine, on the same worker
// pool and with the same timeout as a timed run. It returns once the run is
// ACCEPTED, not once it finishes — a report takes minutes, and the caller is a
// chat message.
//
// It does not advance the schedule's next run time: a manual run is an extra
// run, not a replacement for tonight's.
func (r *Runner) Trigger(e Entry) error {
	r.mu.Lock()
	base, busy := r.base, r.inFlight[e.ID]
	if base != nil && !busy {
		r.inFlight[e.ID] = true
	}
	r.mu.Unlock()
	if base == nil {
		return ErrNotStarted
	}
	if busy {
		return ErrBusy
	}

	// Queue, never block the caller: if the pool is full the run waits on its
	// own goroutine, not in the message handler.
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer func() {
			r.mu.Lock()
			delete(r.inFlight, e.ID)
			r.mu.Unlock()
		}()
		r.log.Info("manual schedule run", "id", e.ID, "user", e.User)
		r.acquire(base, e, "manual")
	}()
	return nil
}

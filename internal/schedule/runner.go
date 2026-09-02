package schedule

import (
	"context"
	"log/slog"
	"time"
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
		concurrency: o.Concurrency, timeout: o.Timeout, log: log}
}

// Start runs the scheduling loop until ctx is cancelled, flushing on exit.
func (r *Runner) Start(ctx context.Context) {
	t := time.NewTicker(r.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			r.store.Flush()
			return
		case now := <-t.C:
			r.runDue(ctx, now)
			r.store.Flush()
		}
	}
}

// runDue executes everything due at now, bounded by the worker pool.
func (r *Runner) runDue(ctx context.Context, now time.Time) {
	due := r.store.Due(now)
	if len(due) == 0 {
		return
	}
	r.log.Info("scheduled runs due", "count", len(due))

	sem := make(chan struct{}, r.concurrency)
	done := make(chan struct{}, len(due))
	for _, e := range due {
		e := e
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; done <- struct{}{} }()
			// A panic in one scheduled run must not take the process down.
			defer func() {
				if p := recover(); p != nil {
					r.log.Error("scheduled run panicked", "id", e.ID, "user", e.User, "panic", p)
				}
			}()
			rctx, cancel := context.WithTimeout(ctx, r.timeout)
			defer cancel()
			err := r.answerer.RunScheduled(rctx, e)
			if removed := r.store.RecordResult(e.ID, err); removed {
				r.log.Warn("schedule disabled after repeated failures",
					"id", e.ID, "user", e.User, "query", e.Query)
			}
			if err != nil {
				r.log.Warn("scheduled run failed", "id", e.ID, "user", e.User, "err", err)
			}
		}()
	}
	for range due {
		<-done
	}
}

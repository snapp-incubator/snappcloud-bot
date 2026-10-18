package bestpractice

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/snapp-incubator/snappcloud-bot/internal/schedule"
)

// Slots turns a schedule spec into its timer slots. Only anchored cadences are
// allowed ("every saturday at 00:00", "every day at 09:00"): an interval such
// as "every 6h" counts from whenever it was parsed, so its slots would move on
// every restart.
type Slots struct {
	spec  string
	every time.Duration
	loc   *time.Location
}

// ParseSlots validates spec in loc. An empty spec means no timer.
func ParseSlots(spec string, loc *time.Location) (*Slots, error) {
	if spec == "" {
		return nil, nil
	}
	if loc == nil {
		loc = time.Local
	}
	e, rest, err := schedule.Parse(spec, time.Now().In(loc))
	if err != nil {
		return nil, fmt.Errorf("schedule %q: %w", spec, err)
	}
	if rest != "" || e.At == "" {
		return nil, fmt.Errorf("schedule %q: use a fixed time, e.g. \"every saturday at 00:00\"", spec)
	}
	return &Slots{spec: spec, every: time.Duration(e.Every), loc: loc}, nil
}

// Next is the first slot strictly after now.
func (s *Slots) Next(now time.Time) time.Time {
	e, _, _ := schedule.Parse(s.spec, now.In(s.loc))
	return e.Next
}

// Last is the latest slot at or before now.
func (s *Slots) Last(now time.Time) time.Time { return s.Next(now.Add(-s.every)) }

// Spec is the cadence as configured.
func (s *Slots) Spec() string { return s.spec }

// ErrBusy means a report job is already running: a build, or an approval
// sending DMs. They share one slot, so two cannot interleave.
var ErrBusy = errors.New("a report job is already running")

// ErrNotStarted means the runner is not accepting work yet.
var ErrNotStarted = errors.New("the report runner is not running")

// Runner fires the weekly build and runs every report job — timed or asked
// for in chat — one at a time, on the process context, with a timeout.
type Runner struct {
	store   *Store
	slots   *Slots
	catchUp time.Duration
	timeout time.Duration
	tick    time.Duration
	build   func(context.Context) error
	log     *slog.Logger
	now     func() time.Time

	mu   sync.Mutex
	base context.Context
	busy string
	wg   sync.WaitGroup
}

// RunnerOptions configures the runner.
type RunnerOptions struct {
	// CatchUp is how late a missed slot may still fire after downtime
	// (default 12h). Later than that, the week is skipped.
	CatchUp time.Duration
	// Timeout bounds one job (default 10m).
	Timeout time.Duration
	// Tick is how often the timer looks (default 30s).
	Tick time.Duration
}

// NewRunner builds a runner. slots may be nil: then only jobs asked for in
// chat run.
func NewRunner(store *Store, slots *Slots, build func(context.Context) error, o RunnerOptions, log *slog.Logger) *Runner {
	if o.CatchUp <= 0 {
		o.CatchUp = 12 * time.Hour
	}
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Minute
	}
	if o.Tick <= 0 {
		o.Tick = 30 * time.Second
	}
	return &Runner{store: store, slots: slots, catchUp: o.CatchUp, timeout: o.Timeout, tick: o.Tick,
		build: build, log: log, now: time.Now}
}

// Start runs the timer until ctx is cancelled, then waits for the job in
// flight so its result is recorded.
func (r *Runner) Start(ctx context.Context) {
	r.mu.Lock()
	r.base = ctx
	r.mu.Unlock()
	t := time.NewTicker(r.tick)
	defer t.Stop()
	r.check()
	for {
		select {
		case <-ctx.Done():
			r.wg.Wait()
			return
		case <-t.C:
			r.check()
		}
	}
}

// check fires the latest slot if it has not been handled. On the very first
// start there is no record of past slots, so the latest one is marked handled
// rather than fired: deploying on a Saturday morning must not send a report
// nobody scheduled.
func (r *Runner) check() {
	if r.slots == nil {
		return
	}
	now := r.now()
	slot := r.slots.Last(now)
	last := r.store.LastSlot()
	switch {
	case last.IsZero():
		r.mark(slot)
		return
	case !slot.After(last):
		return
	case now.Sub(slot) > r.catchUp:
		r.log.Warn("best-practice report skipped: the bot was down past the catch-up window",
			"slot", slot, "catchUp", r.catchUp)
		r.mark(slot)
		return
	}
	switch err := r.Do("scheduled", r.build); {
	case errors.Is(err, ErrBusy):
		// Not marked: it fires on a later tick, once the running job is done.
		r.log.Info("best-practice report waiting for a running job", "slot", slot)
	case err != nil:
		r.log.Warn("best-practice report not started", "slot", slot, "err", err)
	default:
		// Marked once accepted, so a crash mid-run does not send it twice.
		r.mark(slot)
	}
}

func (r *Runner) mark(slot time.Time) {
	if err := r.store.SetLastSlot(slot); err != nil {
		r.log.Error("persist best-practice slot", "err", err)
	}
}

// Next is when the timer fires next; false when there is no timer.
func (r *Runner) Next() (time.Time, bool) {
	if r.slots == nil {
		return time.Time{}, false
	}
	return r.slots.Next(r.now()), true
}

// Do runs fn in the background, returning once it is accepted. Only one job
// runs at a time.
func (r *Runner) Do(kind string, fn func(context.Context) error) error {
	r.mu.Lock()
	base, busy := r.base, r.busy
	if base != nil && busy == "" {
		r.busy = kind
	}
	r.mu.Unlock()
	if base == nil {
		return ErrNotStarted
	}
	if busy != "" {
		return fmt.Errorf("%w (%s)", ErrBusy, busy)
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer func() {
			r.mu.Lock()
			r.busy = ""
			r.mu.Unlock()
		}()
		defer func() {
			if p := recover(); p != nil {
				r.log.Error("best-practice job panicked", "kind", kind, "panic", p)
			}
		}()
		ctx, cancel := context.WithTimeout(base, r.timeout)
		defer cancel()
		start := time.Now()
		if err := fn(ctx); err != nil {
			r.log.Warn("best-practice job failed", "kind", kind, "err", err, "took", time.Since(start).Round(time.Second))
			return
		}
		r.log.Info("best-practice job done", "kind", kind, "took", time.Since(start).Round(time.Second))
	}()
	return nil
}

// wait blocks until the job in flight is done (tests).
func (r *Runner) wait() { r.wg.Wait() }

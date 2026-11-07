package bestpractice

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/snapp-incubator/snappcloud-bot/internal/schedule"
)

// Slots turns a schedule spec into its timer slots. Only anchored cadences are
// allowed ("every saturday at 00:00", "every day at 09:00", "biweekly on
// saturdays at 06:00"): an interval such as "every 6h" counts from whenever it
// was parsed, so its slots would move on every restart.
//
// "biweekly on <weekday>s" uses every second week, counted from the last slot
// the timer handled (the store's LastSlot: fired, skipped, or marked on first
// start), so the rhythm survives restarts and a switch from a weekly schedule
// carries on from its last slot. `report run` does not move it.
type Slots struct {
	spec   string
	weekly string // the spec as one slot a week, of which every weeks-th is used
	weeks  int
	every  time.Duration
	loc    *time.Location
}

// ParseSlots validates spec in loc. An empty spec means no timer.
func ParseSlots(spec string, loc *time.Location) (*Slots, error) {
	if spec == "" {
		return nil, nil
	}
	if loc == nil {
		loc = time.Local
	}
	weekly, weeks := spec, 1
	if m := biweeklyRe.FindStringSubmatch(spec); m != nil {
		weekly, weeks = "every "+m[1]+" "+m[2], 2
	}
	if weeks == 1 && strings.HasPrefix(strings.ToLower(strings.TrimSpace(spec)), "biweekly") {
		return nil, fmt.Errorf("schedule %q: name a weekday and a time, e.g. \"biweekly on saturdays at 06:00\"", spec)
	}
	e, rest, err := schedule.Parse(weekly, time.Now().In(loc))
	if err != nil {
		return nil, fmt.Errorf("schedule %q: %w", spec, err)
	}
	if rest != "" || e.At == "" {
		return nil, fmt.Errorf("schedule %q: use a fixed time, e.g. \"every saturday at 00:00\"", spec)
	}
	return &Slots{spec: spec, weekly: weekly, weeks: weeks, every: time.Duration(weeks) * time.Duration(e.Every), loc: loc}, nil
}

// biweeklyRe matches "biweekly on saturdays at 06:00" (or "saturday", "at"
// optional). "biweekly" alone, or with no weekday or time, is rejected below.
var biweeklyRe = regexp.MustCompile(`(?i)^\s*biweekly\s+on\s+(monday|tuesday|wednesday|thursday|friday|saturday|sunday)s?\s+((?:at\s+)?\d{1,2}:\d{2})\s*$`)

// Next is the first slot strictly after now. prev is the last slot the timer
// handled: for "biweekly", a slot an odd number of weeks from it is passed over.
// Zero prev (none handled yet) accepts any weekly slot.
func (s *Slots) Next(now, prev time.Time) time.Time {
	e, _, _ := schedule.Parse(s.weekly, now.In(s.loc))
	t := e.Next
	if s.weeks > 1 && !prev.IsZero() {
		for i := 0; i < s.weeks && weeksApart(prev, t, s.loc)%s.weeks != 0; i++ {
			t = t.AddDate(0, 0, 7)
		}
	}
	return t
}

// Last is the latest slot at or before now; prev as for Next. For "biweekly"
// with zero prev, it is instead the weekly slot one to two weeks back, so that
// marking it leaves the next weekly slot as the first to fire.
func (s *Slots) Last(now, prev time.Time) time.Time { return s.Next(now.Add(-s.every), prev) }

// weeksApart is how many whole weeks lie between the calendar days of a and b
// in loc, rounded, and never negative. Counted in days, not hours, so a
// daylight-saving change in between does not shift it.
func weeksApart(a, b time.Time, loc *time.Location) int {
	day := func(t time.Time) time.Time {
		y, m, d := t.In(loc).Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	}
	days := int(math.Round(day(b).Sub(day(a)).Hours() / 24))
	if days < 0 {
		days = -days
	}
	return (days + 3) / 7
}

// Spec is the cadence as configured.
func (s *Slots) Spec() string { return s.spec }

// ErrBusy means a report job is already running: a build, or an approval
// sending DMs. They share one slot, so two cannot interleave.
var ErrBusy = errors.New("a report job is already running")

// ErrNotStarted means the runner is not accepting work yet.
var ErrNotStarted = errors.New("the report runner is not running")

// Runner fires the scheduled build and runs every report job — timed or asked
// for in chat — one at a time, on the process context, with a timeout.
type Runner struct {
	store   *Store
	slots   *Slots
	catchUp time.Duration
	timeout time.Duration
	tick    time.Duration
	onSkip  func(slot, next time.Time)
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
	// (default 12h). Later than that, the slot is skipped.
	CatchUp time.Duration
	// Timeout bounds one job (default 10m).
	Timeout time.Duration
	// Tick is how often the timer looks (default 30s).
	Tick time.Duration
	// OnSkip, when set, is told about a slot given up past CatchUp, and when the
	// timer fires next. It runs on the timer's goroutine, so it must not block
	// for long.
	OnSkip func(slot, next time.Time)
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
		onSkip: o.OnSkip, build: build, log: log, now: time.Now}
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
// start there is no record of past slots, so a past one is marked handled
// rather than fired: deploying on a Saturday morning must not send a report
// nobody scheduled.
func (r *Runner) check() {
	if r.slots == nil {
		return
	}
	now := r.now()
	last := r.store.LastSlot()
	slot := r.slots.Last(now, last)
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
		if r.onSkip != nil {
			r.onSkip(slot, r.slots.Next(now, slot))
		}
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
	return r.slots.Next(r.now(), r.store.LastSlot()), true
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

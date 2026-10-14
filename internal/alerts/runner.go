package alerts

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/snapp-incubator/snappcloud-bot/internal/metrics"
)

// Investigator investigates one batch of alerts for a channel. Implemented by
// the bot service, which resolves the channel owner's CURRENT authorization
// before running anything.
type Investigator interface {
	Investigate(ctx context.Context, ch Channel, b Batch) error
}

// Runner closes alert windows and hands the batches to the investigator.
type Runner struct {
	agg         *Aggregator
	channels    *Channels
	inv         Investigator
	tick        time.Duration
	concurrency int
	timeout     time.Duration
	log         *slog.Logger

	sem chan struct{} // shared worker slots
	wg  sync.WaitGroup
}

// RunnerOptions configures the runner.
type RunnerOptions struct {
	// Tick is how often closed windows are looked for (default 10s). It must be
	// well under the window, or batches wait longer than they should.
	Tick time.Duration
	// Concurrency caps simultaneous investigations (default 2), so an alert
	// storm cannot crowd out people asking questions.
	Concurrency int
	// Timeout bounds one investigation (default 5m).
	Timeout time.Duration
}

// NewRunner builds a runner over an aggregator.
func NewRunner(agg *Aggregator, ch *Channels, inv Investigator, o RunnerOptions, log *slog.Logger) *Runner {
	if o.Tick <= 0 {
		o.Tick = 10 * time.Second
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 2
	}
	if o.Timeout <= 0 {
		o.Timeout = 5 * time.Minute
	}
	return &Runner{agg: agg, channels: ch, inv: inv, tick: o.Tick,
		concurrency: o.Concurrency, timeout: o.Timeout, log: log,
		sem: make(chan struct{}, o.Concurrency)}
}

// Start runs until ctx is cancelled, flushing channel marks on exit.
func (r *Runner) Start(ctx context.Context) {
	t := time.NewTicker(r.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			r.wg.Wait()
			r.channels.Flush()
			return
		case now := <-t.C:
			r.runDue(ctx, now)
			r.channels.Flush()
			metrics.AlertsPending.Set(float64(r.agg.Pending()))
		}
	}
}

func (r *Runner) runDue(ctx context.Context, now time.Time) {
	for _, b := range r.agg.Due(now) {
		ch, ok := r.channels.Get(b.ChannelID)
		if !ok {
			continue // unmarked while the window was open
		}
		b := b
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.investigate(ctx, ch, b)
		}()
	}
}

// investigate waits for a worker slot, then runs one batch. Waiting happens
// here, never on the scheduling loop, so batches for several clusters queue
// without delaying the next window.
func (r *Runner) investigate(ctx context.Context, ch Channel, b Batch) {
	select {
	case r.sem <- struct{}{}:
	case <-ctx.Done():
		r.agg.Failed(b)
		return
	}
	defer func() { <-r.sem }()
	defer func() {
		if p := recover(); p != nil {
			r.log.Error("alert investigation panicked", "channel", b.ChannelID, "cluster", b.Cluster, "panic", p)
		}
	}()
	ictx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	if err := r.inv.Investigate(ictx, ch, b); err != nil {
		// Nothing was answered: release the cooldown so the next window retries.
		r.agg.Failed(b)
		r.log.Warn("alert investigation failed; will retry on the next window",
			"channel", b.ChannelID, "cluster", b.Cluster, "alerts", len(b.Investigate), "err", err)
	}
}

// wait blocks until every investigation in flight has finished.
func (r *Runner) wait() { r.wg.Wait() }

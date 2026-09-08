package alerts

import (
	"context"
	"log/slog"
	"time"
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
		concurrency: o.Concurrency, timeout: o.Timeout, log: log}
}

// Start runs until ctx is cancelled, flushing channel marks on exit.
func (r *Runner) Start(ctx context.Context) {
	t := time.NewTicker(r.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			r.channels.Flush()
			return
		case now := <-t.C:
			r.runDue(ctx, now)
			r.channels.Flush()
		}
	}
}

func (r *Runner) runDue(ctx context.Context, now time.Time) {
	batches := r.agg.Due(now)
	if len(batches) == 0 {
		return
	}

	sem := make(chan struct{}, r.concurrency)
	done := make(chan struct{}, len(batches))
	for _, b := range batches {
		ch, ok := r.channels.Get(b.ChannelID)
		if !ok {
			// Unmarked while the window was open: drop the batch rather than
			// posting into a channel that opted out.
			done <- struct{}{}
			continue
		}
		b := b
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; done <- struct{}{} }()
			defer func() {
				if p := recover(); p != nil {
					r.log.Error("alert investigation panicked", "channel", b.ChannelID, "panic", p)
				}
			}()
			ictx, cancel := context.WithTimeout(ctx, r.timeout)
			defer cancel()
			if err := r.inv.Investigate(ictx, ch, b); err != nil {
				r.log.Warn("alert investigation failed", "channel", b.ChannelID,
					"alerts", len(b.Investigate), "err", err)
			}
		}()
	}
	for range batches {
		<-done
	}
}

package llm

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/snapp-incubator/snappcloud-bot/internal/agent"
	"github.com/snapp-incubator/snappcloud-bot/internal/metrics"
)

// Failover serves requests from a primary model and falls back to a backup when
// the primary is failing, then returns to the primary on its own.
//
// It is a circuit breaker, not a per-request retry: the underlying client
// already retries transient errors, so reaching Failover means those retries
// were exhausted. After FailureThreshold consecutive primary failures the
// breaker OPENS and every request goes to the backup for CooldownPeriod. When
// the cooldown expires the next request PROBES the primary once (half-open): if
// it succeeds the breaker closes and normal service resumes; if it fails the
// cooldown restarts. A single success on the primary always resets the counter,
// so isolated errors never trip it.
type Failover struct {
	primary   agent.LLM
	backup    agent.LLM
	threshold int
	cooldown  time.Duration
	log       *slog.Logger

	mu        sync.Mutex
	failures  int       // consecutive primary failures (closed state)
	openUntil time.Time // when set in the future, the breaker is open
}

// FailoverOptions configures the breaker.
type FailoverOptions struct {
	// FailureThreshold is the number of consecutive primary failures that opens
	// the breaker (default 3).
	FailureThreshold int
	// CooldownPeriod is how long the backup serves before the primary is probed
	// again (default 5m).
	CooldownPeriod time.Duration
}

// NewFailover wraps primary with backup. A nil backup returns primary unchanged,
// so failover is strictly opt-in.
func NewFailover(primary, backup agent.LLM, o FailoverOptions, log *slog.Logger) agent.LLM {
	if backup == nil {
		return primary
	}
	if o.FailureThreshold <= 0 {
		o.FailureThreshold = 3
	}
	if o.CooldownPeriod <= 0 {
		o.CooldownPeriod = 5 * time.Minute
	}
	return &Failover{
		primary:   primary,
		backup:    backup,
		threshold: o.FailureThreshold,
		cooldown:  o.CooldownPeriod,
		log:       log,
	}
}

// usePrimary reports whether this request should go to the primary. When the
// cooldown has expired it returns true as a half-open probe.
func (f *Failover) usePrimary() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.openUntil.IsZero() || time.Now().After(f.openUntil) {
		return true
	}
	return false
}

// onPrimaryResult records the outcome of a primary call and moves the breaker.
func (f *Failover) onPrimaryResult(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		if !f.openUntil.IsZero() {
			f.log.Info("llm primary recovered; failing back", "model", "primary")
			metrics.LLMFailover.WithLabelValues("closed").Inc()
		}
		f.failures = 0
		f.openUntil = time.Time{}
		return
	}
	f.failures++
	if f.failures >= f.threshold {
		f.openUntil = time.Now().Add(f.cooldown)
		f.log.Warn("llm primary failing; switching to backup",
			"failures", f.failures, "cooldown", f.cooldown, "err", err)
		metrics.LLMFailover.WithLabelValues("open").Inc()
	}
}

// Complete implements agent.LLM.
func (f *Failover) Complete(ctx context.Context, req agent.Request) (agent.Response, error) {
	if f.usePrimary() {
		resp, err := f.primary.Complete(ctx, req)
		f.onPrimaryResult(err)
		if err == nil {
			metrics.LLMByModel.WithLabelValues("primary", "ok").Inc()
			return resp, nil
		}
		metrics.LLMByModel.WithLabelValues("primary", "error").Inc()
		// The caller's context being done is not the model's fault; do not paper
		// over it with a backup call that will fail the same way.
		if ctx.Err() != nil {
			return agent.Response{}, err
		}
		// Immediate fallback: this request still gets an answer.
		f.log.Warn("llm primary failed; trying backup for this request", "err", err)
		resp, berr := f.backup.Complete(ctx, req)
		if berr != nil {
			metrics.LLMByModel.WithLabelValues("backup", "error").Inc()
			return agent.Response{}, berr
		}
		metrics.LLMByModel.WithLabelValues("backup", "ok").Inc()
		return resp, nil
	}

	// Breaker open: serve from the backup without touching the primary.
	resp, err := f.backup.Complete(ctx, req)
	if err != nil {
		metrics.LLMByModel.WithLabelValues("backup", "error").Inc()
		return agent.Response{}, err
	}
	metrics.LLMByModel.WithLabelValues("backup", "ok").Inc()
	return resp, nil
}

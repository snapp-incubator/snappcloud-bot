package alerts

import (
	"sort"
	"sync"
	"time"
)

// Limits bound what a channel's alert traffic can cost. Each investigation is
// an LLM run plus MCP calls, and an alert storm is exactly when the cluster is
// least able to absorb extra load.
type Limits struct {
	// Window is how long alerts are buffered before processing, so a burst
	// becomes one batch (default 1m).
	Window time.Duration
	// Cooldown suppresses re-investigating the SAME alert (by fingerprint) for
	// this long. Alertmanager re-sends a firing alert every repeat_interval;
	// without this, every repeat is a fresh investigation (default 30m).
	Cooldown time.Duration
	// MaxPerWindow caps how many alerts go into one investigation. Alerts in a
	// window are usually one incident, so they are investigated TOGETHER; this
	// bounds the prompt, not the number of investigations (default 10).
	MaxPerWindow int
	// IgnoredAlerts are alert names never investigated (Watchdog and friends,
	// which fire forever by design).
	IgnoredAlerts []string
	// MinSeverity is the least severe level worth investigating: "critical",
	// "warning", "info", or "" for everything (default "warning").
	MinSeverity string
}

func (l *Limits) applyDefaults() {
	if l.Window <= 0 {
		l.Window = time.Minute
	}
	if l.Cooldown <= 0 {
		l.Cooldown = 30 * time.Minute
	}
	if l.MaxPerWindow <= 0 {
		l.MaxPerWindow = 10
	}
	if len(l.IgnoredAlerts) == 0 {
		l.IgnoredAlerts = []string{"Watchdog", "InfoInhibitor", "DeadMansSwitch"}
	}
	if l.MinSeverity == "" {
		l.MinSeverity = "warning"
	}
}

// Batch is one window's worth of alerts, already collapsed. It is investigated
// as a UNIT: alerts that fire together are usually one incident (a node goes
// down, and pods, endpoints, quota and ingress all alert about it), and looking
// at them separately produces several partial answers to one question — plus
// one message each, which is the noise the window exists to prevent.
type Batch struct {
	ChannelID string
	// Investigate are the alerts this batch is about, worst first.
	Investigate []Alert
	// Context are alerts that fired but are still inside their cooldown. They
	// are NOT the subject of the investigation, but they are handed to it
	// anyway: knowing what else is firing is often what identifies the incident.
	Context []Alert
	// Skipped are alerts left out entirely (over the per-batch cap), so the
	// channel can be told rather than left wondering.
	Skipped []Skipped
}

// Alerts returns every alert in the batch, subjects first, for rendering.
func (b Batch) Alerts() []Alert {
	out := make([]Alert, 0, len(b.Investigate)+len(b.Context))
	out = append(out, b.Investigate...)
	out = append(out, b.Context...)
	return out
}

// Root returns the thread to reply in: the alert's own thread when the batch is
// a single alert, and "" (a new channel post) when it covers several, since a
// finding about five alerts does not belong under one of them.
func (b Batch) Root() string {
	if len(b.Investigate) == 1 && len(b.Context) == 0 {
		return b.Investigate[0].PostID
	}
	return ""
}

// Skipped is an alert that was not investigated, and why.
type Skipped struct {
	Alert  Alert
	Reason string // duplicate, cooldown, budget, ignored, resolved, low_severity
}

// Aggregator buffers alerts per channel and emits a Batch per window.
type Aggregator struct {
	limits Limits
	mu     sync.Mutex
	// pending is the current window's alerts per channel, keyed by fingerprint
	// so a repeat inside one window collapses into the first.
	pending map[string]map[string]*pendingAlert
	opened  map[string]time.Time // channel -> when its window opened
	// lastRun is the last time each fingerprint was INVESTIGATED, which is what
	// the cooldown measures. Survives windows; pruned as it is read.
	lastRun map[string]time.Time
}

type pendingAlert struct {
	alert  Alert
	repeat int
}

// NewAggregator builds an aggregator with the given limits.
func NewAggregator(l Limits) *Aggregator {
	l.applyDefaults()
	return &Aggregator{
		limits:  l,
		pending: map[string]map[string]*pendingAlert{},
		opened:  map[string]time.Time{},
		lastRun: map[string]time.Time{},
	}
}

// Limits returns the configured bounds.
func (a *Aggregator) Limits() Limits { return a.limits }

// Add buffers an alert. It reports whether the alert was accepted; a rejected
// alert (resolved, ignored, below MinSeverity) is dropped immediately rather
// than occupying the window.
func (a *Aggregator) Add(al Alert, now time.Time) (accepted bool, reason string) {
	if al.Status == Resolved {
		return false, "resolved"
	}
	for _, name := range a.limits.IgnoredAlerts {
		if equalFold(name, al.Name) {
			return false, "ignored"
		}
	}
	if severityRank(al.Severity) > severityRank(a.limits.MinSeverity) {
		return false, "low_severity"
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	ch := a.pending[al.ChannelID]
	if ch == nil {
		ch = map[string]*pendingAlert{}
		a.pending[al.ChannelID] = ch
		a.opened[al.ChannelID] = now
	}
	fp := al.Fingerprint()
	if p, ok := ch[fp]; ok {
		// Same alert twice inside one window: one investigation, and remember
		// how loud it was.
		p.repeat++
		return true, "duplicate"
	}
	ch[fp] = &pendingAlert{alert: al, repeat: 1}
	return true, ""
}

// Due returns the batches whose window has closed. Calling it advances state:
// investigated fingerprints start their cooldown here.
func (a *Aggregator) Due(now time.Time) []Batch {
	a.mu.Lock()
	defer a.mu.Unlock()

	var out []Batch
	for channel, alerts := range a.pending {
		if now.Sub(a.opened[channel]) < a.limits.Window {
			continue
		}
		delete(a.pending, channel)
		delete(a.opened, channel)

		batch := Batch{ChannelID: channel}
		ordered := make([]*pendingAlert, 0, len(alerts))
		for _, p := range alerts {
			ordered = append(ordered, p)
		}
		// Worst first, then loudest, then oldest — so the budget is spent on
		// what matters, deterministically.
		sort.Slice(ordered, func(i, j int) bool {
			ri, rj := severityRank(ordered[i].alert.Severity), severityRank(ordered[j].alert.Severity)
			if ri != rj {
				return ri < rj
			}
			if ordered[i].repeat != ordered[j].repeat {
				return ordered[i].repeat > ordered[j].repeat
			}
			return ordered[i].alert.Seen.Before(ordered[j].alert.Seen)
		})

		for _, p := range ordered {
			fp := p.alert.Fingerprint()
			if len(batch.Investigate)+len(batch.Context) >= a.limits.MaxPerWindow {
				batch.Skipped = append(batch.Skipped, Skipped{Alert: p.alert, Reason: "budget"})
				continue
			}
			// Inside its cooldown: not worth investigating again on its own, but
			// worth knowing about while investigating whatever else is firing.
			if last, ok := a.lastRun[fp]; ok && now.Sub(last) < a.limits.Cooldown {
				batch.Context = append(batch.Context, p.alert)
				continue
			}
			a.lastRun[fp] = now
			batch.Investigate = append(batch.Investigate, p.alert)
		}
		// A window with nothing new is not worth a message: every alert in it has
		// been investigated recently and the channel already has that answer.
		if len(batch.Investigate) > 0 {
			out = append(out, batch)
		}
	}

	// Drop cooldown entries that can no longer suppress anything, so the map
	// does not grow with every distinct alert the cluster ever fires.
	for fp, t := range a.lastRun {
		if now.Sub(t) > a.limits.Cooldown {
			delete(a.lastRun, fp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ChannelID < out[j].ChannelID })
	return out
}

// Pending reports how many alerts are buffered, for metrics.
func (a *Aggregator) Pending() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, ch := range a.pending {
		n += len(ch)
	}
	return n
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

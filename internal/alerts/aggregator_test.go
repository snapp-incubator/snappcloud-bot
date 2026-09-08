package alerts

import (
	"fmt"
	"testing"
	"time"
)

func alert(name, sev, instance, channel string, seen time.Time) Alert {
	return Alert{
		Name: name, Status: Firing, Severity: sev,
		Labels:    map[string]string{"instance": instance, "severity": sev},
		ChannelID: channel, Seen: seen,
	}
}

// A storm of the SAME alert inside one window is one investigation.
func TestDuplicatesCollapseWithinWindow(t *testing.T) {
	now := time.Now()
	a := NewAggregator(Limits{Window: time.Minute})
	for i := 0; i < 25; i++ {
		a.Add(alert("CiliumHighBpfMapPressure", "critical", "cilium-d88nh", "ch1", now), now)
	}
	batches := a.Due(now.Add(time.Minute))
	if len(batches) != 1 || len(batches[0].Investigate) != 1 {
		t.Fatalf("25 copies produced %d investigations", len(batches[0].Investigate))
	}
}

// Alertmanager re-sends a firing alert every repeat_interval. The second window
// must not investigate it again — and with nothing else firing, must not post at
// all: the channel already has that answer.
func TestCooldownSuppressesRepeatsAcrossWindows(t *testing.T) {
	now := time.Now()
	a := NewAggregator(Limits{Window: time.Minute, Cooldown: 30 * time.Minute})

	a.Add(alert("KubePodCrashLooping", "critical", "web-0", "ch1", now), now)
	if b := a.Due(now.Add(time.Minute)); len(b[0].Investigate) != 1 {
		t.Fatal("first sighting must be investigated")
	}

	later := now.Add(5 * time.Minute)
	a.Add(alert("KubePodCrashLooping", "critical", "web-0", "ch1", later), later)
	if b := a.Due(later.Add(time.Minute)); len(b) != 0 {
		t.Errorf("a window with nothing new produced a batch: %+v", b)
	}

	// Once the cooldown expires it is worth looking again.
	after := now.Add(31 * time.Minute)
	a.Add(alert("KubePodCrashLooping", "critical", "web-0", "ch1", after), after)
	if b := a.Due(after.Add(time.Minute)); len(b) != 1 || len(b[0].Investigate) != 1 {
		t.Error("alert still suppressed after the cooldown expired")
	}
}

// An alert inside its cooldown still travels with a batch that has something
// new: what else is firing is often what identifies the incident.
func TestCooledAlertsRideAlongAsContext(t *testing.T) {
	now := time.Now()
	a := NewAggregator(Limits{Window: time.Minute, Cooldown: 30 * time.Minute})

	a.Add(alert("NodeNotReady", "critical", "node-3", "ch1", now), now)
	a.Due(now.Add(time.Minute)) // investigated, now cooling

	later := now.Add(2 * time.Minute)
	a.Add(alert("NodeNotReady", "critical", "node-3", "ch1", later), later)
	a.Add(alert("KubePodPending", "warning", "web-0", "ch1", later), later)

	b := a.Due(later.Add(time.Minute))
	if len(b) != 1 {
		t.Fatalf("expected one batch, got %d", len(b))
	}
	if len(b[0].Investigate) != 1 || b[0].Investigate[0].Name != "KubePodPending" {
		t.Errorf("subjects = %+v, want only the new alert", b[0].Investigate)
	}
	if len(b[0].Context) != 1 || b[0].Context[0].Name != "NodeNotReady" {
		t.Errorf("context = %+v, want the cooling alert carried along", b[0].Context)
	}
}

// One incident fires many DIFFERENT alerts. They belong in ONE investigation,
// worst first, with the cap bounding the prompt rather than the answer count.
func TestBatchKeepsAlertsTogetherWorstFirst(t *testing.T) {
	now := time.Now()
	a := NewAggregator(Limits{Window: time.Minute, MaxPerWindow: 3, MinSeverity: "info"})
	a.Add(alert("LowPriorityThing", "info", "x", "ch1", now), now)
	a.Add(alert("WarnThing", "warning", "y", "ch1", now), now)
	a.Add(alert("BadThing", "critical", "z", "ch1", now), now)
	a.Add(alert("AlsoBad", "critical", "w", "ch1", now), now)

	batches := a.Due(now.Add(time.Minute))
	if len(batches) != 1 {
		t.Fatalf("one window must produce ONE batch, got %d", len(batches))
	}
	b := batches[0]
	if len(b.Investigate) != 3 {
		t.Fatalf("cap breached or under-filled: %d alerts", len(b.Investigate))
	}
	if b.Investigate[0].Severity != "critical" || b.Investigate[1].Severity != "critical" {
		t.Errorf("not worst-first: %+v", b.Investigate)
	}
	if len(b.Skipped) != 1 || b.Skipped[0].Reason != "budget" {
		t.Errorf("over-cap alert not reported: %+v", b.Skipped)
	}
}

// A single alert answers in its own thread; a batch about several does not
// belong under any one of them.
func TestBatchRootPicksThreadOnlyForASingleAlert(t *testing.T) {
	one := Batch{Investigate: []Alert{{Name: "A", PostID: "post-1"}}}
	if one.Root() != "post-1" {
		t.Errorf("single-alert batch must reply in its thread, got %q", one.Root())
	}
	many := Batch{Investigate: []Alert{{Name: "A", PostID: "post-1"}, {Name: "B", PostID: "post-2"}}}
	if many.Root() != "" {
		t.Errorf("multi-alert batch must post at channel level, got %q", many.Root())
	}
	withCtx := Batch{Investigate: []Alert{{Name: "A", PostID: "post-1"}}, Context: []Alert{{Name: "B"}}}
	if withCtx.Root() != "" {
		t.Errorf("a batch carrying context is not a single-alert reply, got %q", withCtx.Root())
	}
}

// Watchdog fires forever by design; resolved and info-level alerts are noise.
func TestRejectsNoiseBeforeBuffering(t *testing.T) {
	now := time.Now()
	a := NewAggregator(Limits{Window: time.Minute, MinSeverity: "warning"})

	if ok, reason := a.Add(alert("Watchdog", "none", "x", "ch1", now), now); ok || reason != "ignored" {
		t.Errorf("Watchdog accepted: ok=%v reason=%s", ok, reason)
	}
	resolved := alert("KubePodCrashLooping", "critical", "web-0", "ch1", now)
	resolved.Status = Resolved
	if ok, reason := a.Add(resolved, now); ok || reason != "resolved" {
		t.Errorf("resolved alert accepted: ok=%v reason=%s", ok, reason)
	}
	if ok, reason := a.Add(alert("ChattyInfo", "info", "x", "ch1", now), now); ok || reason != "low_severity" {
		t.Errorf("info alert accepted under MinSeverity=warning: ok=%v reason=%s", ok, reason)
	}
	if a.Pending() != 0 {
		t.Errorf("noise occupied the window: %d pending", a.Pending())
	}
}

// The window is per channel: one busy channel must not delay another.
func TestWindowsArePerChannel(t *testing.T) {
	now := time.Now()
	a := NewAggregator(Limits{Window: time.Minute})
	a.Add(alert("A", "critical", "1", "ch1", now), now)
	later := now.Add(50 * time.Second)
	a.Add(alert("B", "critical", "2", "ch2", later), later)

	b := a.Due(now.Add(time.Minute + time.Second))
	if len(b) != 1 || b[0].ChannelID != "ch1" {
		t.Fatalf("expected only ch1 due, got %+v", b)
	}
	if b2 := a.Due(later.Add(time.Minute + time.Second)); len(b2) != 1 || b2[0].ChannelID != "ch2" {
		t.Errorf("ch2 window did not close on its own schedule: %+v", b2)
	}
}

// The cooldown map must not grow without bound on a cluster that fires many
// distinct alerts over time.
func TestCooldownStatePrunes(t *testing.T) {
	now := time.Now()
	a := NewAggregator(Limits{Window: time.Minute, Cooldown: 10 * time.Minute})
	for i := 0; i < 100; i++ {
		al := alert("Alert", "critical", fmt.Sprintf("host-%d", i), "ch1", now)
		a.Add(al, now)
	}
	a.Due(now.Add(time.Minute))
	a.Due(now.Add(2 * time.Hour)) // long past every cooldown
	if n := len(a.lastRun); n != 0 {
		t.Errorf("cooldown state retained %d expired entries", n)
	}
}

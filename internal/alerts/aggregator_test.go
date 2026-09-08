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
// must not investigate it again.
func TestCooldownSuppressesRepeatsAcrossWindows(t *testing.T) {
	now := time.Now()
	a := NewAggregator(Limits{Window: time.Minute, Cooldown: 30 * time.Minute})

	a.Add(alert("KubePodCrashLooping", "critical", "web-0", "ch1", now), now)
	if b := a.Due(now.Add(time.Minute)); len(b[0].Investigate) != 1 {
		t.Fatal("first sighting must be investigated")
	}

	later := now.Add(5 * time.Minute)
	a.Add(alert("KubePodCrashLooping", "critical", "web-0", "ch1", later), later)
	b := a.Due(later.Add(time.Minute))
	if len(b[0].Investigate) != 0 {
		t.Error("a repeat inside the cooldown was investigated again")
	}
	if len(b[0].Skipped) != 1 || b[0].Skipped[0].Reason != "cooldown" {
		t.Errorf("repeat not reported as cooldown: %+v", b[0].Skipped)
	}

	// Once the cooldown expires it is worth looking again.
	after := now.Add(31 * time.Minute)
	a.Add(alert("KubePodCrashLooping", "critical", "web-0", "ch1", after), after)
	if b := a.Due(after.Add(time.Minute)); len(b[0].Investigate) != 1 {
		t.Error("alert still suppressed after the cooldown expired")
	}
}

// One incident fires many DIFFERENT alerts. Investigate the worst few; list the rest.
func TestBudgetInvestigatesWorstFirst(t *testing.T) {
	now := time.Now()
	a := NewAggregator(Limits{Window: time.Minute, MaxPerWindow: 2})
	a.Add(alert("LowPriorityThing", "info", "x", "ch1", now), now)
	a.Add(alert("WarnThing", "warning", "y", "ch1", now), now)
	a.Add(alert("BadThing", "critical", "z", "ch1", now), now)
	a.Add(alert("AlsoBad", "critical", "w", "ch1", now), now)

	b := a.Due(now.Add(time.Minute))[0]
	if len(b.Investigate) != 2 {
		t.Fatalf("budget breached: %d investigations", len(b.Investigate))
	}
	for _, inv := range b.Investigate {
		if inv.Severity != "critical" {
			t.Errorf("budget spent on %s/%s instead of the criticals", inv.Name, inv.Severity)
		}
	}
	if len(b.Skipped) == 0 {
		t.Error("over-budget alerts must be reported, not dropped silently")
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

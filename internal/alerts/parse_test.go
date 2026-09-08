package alerts

import (
	"testing"
	"time"
)

// The real thing, as it arrives in the channel.
const realAlert = `CiliumHighBpfMapPressure
⚠️ Severity: 🔴 critical
🌍 Region: okd4-teh-1
🖥 Instance: cilium-d88nh
📋 Summary: Cilium BPF map ct_any4_global on node okd4-worker-ls-13 has reached a critical level, operating at over 95% of its configured size and is nearing full capacity. Remove its pod with oc delete po -n kube-system cilium-d88nh to trigger the garbage collection
💬 Message: Cilium BPF map ct_any4_global on node okd4-worker-ls-13 has reached a critical level`

func TestParseRealAlert(t *testing.T) {
	a, ok := Parse(realAlert, time.Now())
	if !ok {
		t.Fatal("real alert not recognised")
	}
	if a.Name != "CiliumHighBpfMapPressure" {
		t.Errorf("name = %q", a.Name)
	}
	if a.Severity != "critical" {
		t.Errorf("severity = %q, want critical (emoji must be stripped)", a.Severity)
	}
	if a.Labels["region"] != "okd4-teh-1" {
		t.Errorf("region = %q", a.Labels["region"])
	}
	if a.Labels["instance"] != "cilium-d88nh" {
		t.Errorf("instance = %q", a.Labels["instance"])
	}
	if a.Status != Firing {
		t.Errorf("status = %q", a.Status)
	}
	if a.Summary == "" {
		t.Error("summary lost")
	}
}

func TestParseFiringAndResolvedHeaders(t *testing.T) {
	firing, ok := Parse("[FIRING:2] KubePodCrashLooping (critical)\nnamespace: team-a\npod: web-0", time.Now())
	if !ok || firing.Name != "KubePodCrashLooping" || firing.Status != Firing {
		t.Errorf("firing header: %+v ok=%v", firing, ok)
	}
	resolved, ok := Parse("[RESOLVED] KubePodCrashLooping\nnamespace: team-a\npod: web-0", time.Now())
	if !ok || resolved.Status != Resolved {
		t.Errorf("resolved header: %+v ok=%v", resolved, ok)
	}
}

// Parse deliberately accepts anything non-empty: alert templates vary too much
// between teams to reject on shape, and rejecting an unrecognised format would
// silently drop that team's alerts. What separates an alert from chatter is the
// AUTHOR — a webhook or bot account with no SSO identity — which the caller
// checks. The only thing Parse refuses is an empty post.
func TestParseAcceptsAnyNonEmptyText(t *testing.T) {
	if _, ok := Parse("   \n\t ", time.Now()); ok {
		t.Error("empty post parsed as an alert")
	}
	if _, ok := Parse("Database replica lag is high on shard 3", time.Now()); !ok {
		t.Error("an unstructured alert from a webhook must still be investigable")
	}
}

// The same alert re-sent must fingerprint identically; a different node must not.
func TestFingerprintIdentifiesTheAlertNotTheNotification(t *testing.T) {
	a, _ := Parse(realAlert, time.Now())
	b, _ := Parse(realAlert, time.Now().Add(time.Hour))
	if a.Fingerprint() != b.Fingerprint() {
		t.Error("a repeat of the same alert must share a fingerprint")
	}
	other, _ := Parse("CiliumHighBpfMapPressure\nSeverity: critical\nRegion: okd4-teh-1\nInstance: cilium-xxxxx", time.Now())
	if a.Fingerprint() == other.Fingerprint() {
		t.Error("a different instance must be a different alert")
	}
}

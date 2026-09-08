package alerts

import (
	"testing"
	"time"
)

// Teams template alerts differently. Every one of these must yield something
// investigable, and repeats of each must collapse.
func TestParsesForeignFormats(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name     string
		text     string
		wantName string
		wantSev  string
		wantStat Status
	}{
		{
			name: "alertmanager mattermost template",
			text: "#### [FIRING:1] KubePodCrashLooping (team-a critical)\n" +
				"**Alert:** pod is restarting — `critical`\n" +
				"**Description:** web-0 restarted 12 times\n" +
				"**Details:**\n • **alertname:** KubePodCrashLooping\n • **namespace:** team-a",
			wantName: "KubePodCrashLooping", wantStat: Firing,
		},
		{
			name:     "grafana notification",
			text:     "[Alerting] Postgres connections high\nseverity = warning\nnamespace = team-b",
			wantName: "Postgres connections high", wantSev: "warning", wantStat: Firing,
		},
		{
			name:     "grafana recovery",
			text:     "[OK] Postgres connections high\nseverity = warning",
			wantName: "Postgres connections high", wantSev: "warning", wantStat: Resolved,
		},
		{
			name: "raw alertmanager webhook json",
			text: `{"status":"firing","alerts":[{"status":"firing","labels":{"alertname":"TargetDown","severity":"critical","namespace":"team-c"},` +
				`"annotations":{"summary":"1 target down"}}]}`,
			wantName: "TargetDown", wantSev: "critical", wantStat: Firing,
		},
		{
			name:     "no severity anywhere",
			text:     "DiskWillFillIn4Hours\ninstance: worker-9\nsummary: /var is filling",
			wantName: "DiskWillFillIn4Hours", wantSev: "", wantStat: Firing,
		},
		{
			name:     "prose only, no structure at all",
			text:     "Database replica lag is above 5 minutes on shard 3",
			wantName: "Database replica lag is above 5 minutes on shard 3", wantStat: Firing,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, ok := Parse(tc.text, now)
			if !ok {
				t.Fatal("format not parsed at all")
			}
			if a.Name != tc.wantName {
				t.Errorf("name = %q, want %q", a.Name, tc.wantName)
			}
			if a.Severity != tc.wantSev {
				t.Errorf("severity = %q, want %q", a.Severity, tc.wantSev)
			}
			if a.Status != tc.wantStat {
				t.Errorf("status = %q, want %q", a.Status, tc.wantStat)
			}
		})
	}
}

// A template with no severity must still be investigated: filtering it out
// would look like the bot ignoring that team.
func TestUnknownSeverityIsInvestigated(t *testing.T) {
	now := time.Now()
	agg := NewAggregator(Limits{Window: time.Minute, MinSeverity: "warning"})
	for _, sev := range []string{"", "P1", "blocker", "sev3", "urgent"} {
		a, _ := Parse("SomethingBroke\nseverity: "+sev+"\ninstance: host-1", now)
		a.ChannelID = "ch1"
		if ok, reason := agg.Add(a, now); !ok {
			t.Errorf("severity %q rejected as %q", sev, reason)
		}
	}
}

// Repeats of an unstructured alert must still collapse, or an unrecognised
// format would be investigated on every repeat_interval.
func TestUnstructuredRepeatsShareAFingerprint(t *testing.T) {
	now := time.Now()
	first, _ := Parse("Database replica lag is 6 minutes on shard 3", now)
	// Same alert, later, with a different measured value.
	second, _ := Parse("Database replica lag is 11 minutes on shard 3", now.Add(time.Hour))
	if first.Fingerprint() != second.Fingerprint() {
		t.Error("repeats of the same unstructured alert must share a fingerprint")
	}
	other, _ := Parse("Database replica lag is 6 minutes on shard 9", now)
	if first.Fingerprint() == other.Fingerprint() {
		t.Error("a different shard is a different alert")
	}
}

// Package alerts turns Alertmanager notifications posted in a marked channel
// into investigations, run with the marking user's access. Alerts are collapsed
// by duplicate window, cooldown and a per-window budget to bound noise.
package alerts

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"time"
)

// Status is the alert lifecycle state Alertmanager reports.
type Status string

const (
	Firing   Status = "firing"
	Resolved Status = "resolved"
)

// Alert is one parsed notification.
type Alert struct {
	Name      string            // alertname, e.g. CiliumHighBpfMapPressure
	Status    Status            // firing or resolved
	Severity  string            // critical, warning, info, ...
	Labels    map[string]string // every "Key: value" line, lowercased keys
	Summary   string            // the human summary/message, if present
	Raw       string            // the original post text, for the investigation prompt
	PostID    string            // the post to reply under
	ChannelID string
	Seen      time.Time
}

// identityLabels are the labels that make one alert distinct from another of
// the same name. Order is fixed so the fingerprint is stable.
var identityLabels = []string{"cluster", "region", "namespace", "node", "pod", "instance", "service", "map"}

// Fingerprint identifies the alert, not the notification, so repeats collapse.
// Without identity labels it falls back to the normalised message text.
func (a Alert) Fingerprint() string {
	h := sha256.New()
	h.Write([]byte(strings.ToLower(a.Name)))
	identified := false
	for _, k := range identityLabels {
		if v, ok := a.Labels[k]; ok && v != "" {
			h.Write([]byte("\x00" + k + "=" + strings.ToLower(v)))
			identified = true
		}
	}
	if !identified {
		// The name itself may carry the measured value ("... lag is 6 minutes"),
		// so hash its skeleton rather than the literal text.
		h.Reset()
		h.Write([]byte("skeleton=" + skeleton(a.Name+"\n"+a.Raw)))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// severityRank orders alerts for the per-window budget. Unknown severities
// rank with warning so they are never silently filtered out.
func severityRank(s string) int {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "critical", "crit", "page", "emergency", "fatal", "p1", "sev1", "high":
		return 0
	case "warning", "warn", "p2", "sev2", "medium":
		return 1
	case "info", "information", "informational", "notice", "low", "p4", "sev4", "debug", "none":
		return 3
	default:
		// Includes "" (no severity in the template) and anything unknown.
		return 1
	}
}

// Scope returns the namespace this alert is about, if it names one. Used to
// tell the investigation where to look.
func (a Alert) Scope() (namespace, cluster string) {
	ns := a.Labels["namespace"]
	c := a.Labels["cluster"]
	if c == "" {
		c = a.Labels["region"]
	}
	return ns, c
}

// Describe renders the alert for the investigation prompt: the identity of the
// alert plus whatever context the notification carried.
func (a Alert) Describe() string {
	var b strings.Builder
	b.WriteString(a.Name)
	if a.Severity != "" {
		b.WriteString(" (severity " + a.Severity + ")")
	}
	keys := make([]string, 0, len(a.Labels))
	for k := range a.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k == "severity" || k == "summary" || k == "message" {
			continue
		}
		b.WriteString("\n" + k + ": " + a.Labels[k])
	}
	if a.Summary != "" {
		b.WriteString("\nsummary: " + a.Summary)
	}
	return b.String()
}

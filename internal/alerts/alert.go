// Package alerts turns Alertmanager notifications posted in a Mattermost
// channel into investigations.
//
// A channel is marked as an alert channel by a user; every subsequent post in
// it (normally from a webhook account, which has no identity of its own) is
// parsed as an alert, grouped, and investigated with the MARKING user's
// authorization, resolved fresh at investigation time.
//
// The hard problem is noise, not parsing. Alertmanager repeats a firing alert
// every repeat_interval, and one incident can fire dozens of distinct alerts at
// once. Investigating each notification would bury the on-call team under more
// text than the alerts themselves, so alerts are collapsed on three axes:
// duplicates within a window, the same alert across windows (cooldown), and a
// budget per window with the rest merely listed.
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

// Fingerprint identifies the ALERT, not the notification: the same alert
// re-sent every repeat_interval has the same fingerprint, which is what the
// cooldown suppresses.
//
// When a template carries none of the identity labels — many do not — the
// fingerprint falls back to the message skeleton (the text with digits and
// whitespace normalised away), so repeats of an unrecognised format still
// collapse instead of being investigated over and over.
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

// severityRank orders alerts for the per-window budget: the worst get
// investigated when there are more alerts than budget.
//
// An UNRECOGNISED severity ranks with warning, not below info. A template that
// omits severity, or names it something we do not know, must not have its
// alerts silently filtered out — that would look exactly like the bot ignoring
// a team, which is the failure mode hardest to notice.
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

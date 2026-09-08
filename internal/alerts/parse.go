package alerts

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

// Every team templates its alerts differently — Alertmanager's Mattermost
// template, a Grafana notification, a hand-rolled webhook, raw JSON — so
// parsing is best-effort and DEGRADES rather than rejects. The investigation
// itself only needs the text; structure is what makes dedupe, severity budgets
// and resolved-detection possible, and each of those falls back safely:
//
//   - no alert name        -> the first line is the title, and the fingerprint
//                             comes from the message skeleton, so repeats still
//                             collapse
//   - no severity          -> INVESTIGATED. An unknown severity must never be
//                             filtered out, or a team whose template omits it
//                             would silently get nothing
//   - no firing/resolved   -> assumed firing
//
// What makes something an alert is not its shape: it is being posted by a
// webhook or bot account in a channel someone marked. That decision lives in
// the caller, not here.

// Parse turns post text into an alert. ok is false only when there is nothing
// to work with at all.
func Parse(text string, seen time.Time) (Alert, bool) {
	raw := strings.TrimSpace(text)
	if raw == "" {
		return Alert{}, false
	}
	if a, ok := parseJSON(raw, seen); ok {
		return a, true
	}
	return parseText(raw, seen), true
}

// parseJSON handles webhooks that post their payload verbatim: an Alertmanager
// notification, or a single alert object. This is the best case — real labels.
func parseJSON(raw string, seen time.Time) (Alert, bool) {
	body := strings.TrimSpace(strings.Trim(raw, "`"))
	if i := strings.IndexAny(body, "{"); i > 0 {
		body = body[i:]
	}
	if !strings.HasPrefix(body, "{") {
		return Alert{}, false
	}
	var payload struct {
		Status string `json:"status"`
		Alerts []struct {
			Status      string            `json:"status"`
			Labels      map[string]string `json:"labels"`
			Annotations map[string]string `json:"annotations"`
		} `json:"alerts"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	}
	if json.Unmarshal([]byte(body), &payload) != nil {
		return Alert{}, false
	}

	labels, annotations, status := payload.Labels, payload.Annotations, payload.Status
	if len(payload.Alerts) > 0 {
		labels, annotations = payload.Alerts[0].Labels, payload.Alerts[0].Annotations
		if payload.Alerts[0].Status != "" {
			status = payload.Alerts[0].Status
		}
	}
	if len(labels) == 0 && len(annotations) == 0 {
		return Alert{}, false
	}

	a := Alert{Status: Firing, Labels: map[string]string{}, Raw: raw, Seen: seen}
	for k, v := range labels {
		a.Labels[strings.ToLower(k)] = v
	}
	for _, key := range []string{"summary", "description", "message"} {
		if v := annotations[key]; v != "" && len(v) > len(a.Summary) {
			a.Summary = v
		}
	}
	a.Name = a.Labels["alertname"]
	a.Severity = a.Labels["severity"]
	if strings.EqualFold(status, string(Resolved)) {
		a.Status = Resolved
	}
	if a.Name == "" {
		a.Name = firstLine(raw)
	}
	return a, true
}

// parseText reads the common templated shapes: a status header, an alert name,
// and "Key: value" lines, in any order and with any decoration around them.
func parseText(raw string, seen time.Time) Alert {
	a := Alert{Status: Firing, Labels: map[string]string{}, Raw: raw, Seen: seen}

	for _, line := range strings.Split(stripFormatting(raw), "\n") {
		line = strings.TrimSpace(stripLeadingSymbols(line))
		if line == "" {
			continue
		}
		if st, name, ok := parseStatusHeader(line); ok {
			a.Status = st
			if name != "" && a.Name == "" {
				a.Name = name
			}
			continue
		}
		if k, v, ok := parseLabelLine(line); ok {
			if _, seenKey := a.Labels[k]; !seenKey {
				a.Labels[k] = v
			}
			switch k {
			case "severity", "priority", "level":
				if a.Severity == "" {
					a.Severity = v
				}
			case "summary", "message", "description", "text":
				if len(v) > len(a.Summary) {
					a.Summary = v
				}
			case "alertname", "alert", "rule", "name":
				if a.Name == "" {
					a.Name = v
				}
			case "status", "state":
				if isResolvedWord(v) {
					a.Status = Resolved
				}
			}
			continue
		}
		if a.Name == "" {
			a.Name = strings.TrimSpace(line)
		}
	}

	if a.Name == "" {
		a.Name = firstLine(raw)
	}
	// A long first line is a sentence, not a name: keep it readable in the
	// channel and let the fingerprint carry the identity.
	if len([]rune(a.Name)) > 80 {
		a.Name = string([]rune(a.Name)[:77]) + "..."
	}
	return a
}

var (
	// "[FIRING:2] Name", "[RESOLVED] Name", "[Alerting] Name" (Grafana),
	// "[OK] Name", "RESOLVED - Name".
	statusHeaderRe = regexp.MustCompile(`(?i)^\[?\s*(firing|resolved|alerting|ok|recovered|resolved)\b(?::\s*\d+)?\s*\]?\s*[:\-–]?\s*(.*)$`)
	// "Severity: critical", "**Region**: teh-1", "namespace = team-a".
	labelLineRe = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9 _.\-/]{0,40}?)\s*[:=]\s+(.+)$`)
	// Emoji, bullets and markdown noise templates prepend.
	leadingSymbolsRe = regexp.MustCompile(`^[\p{S}\p{So}\p{Cf}\p{Zs}\x{FE0F}\*\->#•\|]+`)
	formattingRe     = regexp.MustCompile("[*_`]{1,3}")
	// What differs between two notifications of the SAME alert is the MEASURED
	// value and the time — "6 minutes", "95%", "12 restarts", a timestamp — not
	// the identifiers in it. Bare integers are left alone on purpose: "shard 3"
	// and "shard 9" are different alerts, and collapsing them would hide one
	// behind the other's cooldown.
	timestampRe = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}(:\d{2})?(\.\d+)?(Z|[+-]\d{2}:?\d{2})?`)
	measuredRe  = regexp.MustCompile(`(?i)\d+(\.\d+)?\s*(%|ms|µs|ns|s|sec|secs|seconds?|m|min|mins|minutes?|h|hr|hrs|hours?|d|days?|w|weeks?|[kmgt]i?b|restarts?|times?|errors?|requests?)\b`)
	decimalRe   = regexp.MustCompile(`\d+\.\d+`)
)

func parseStatusHeader(line string) (Status, string, bool) {
	m := statusHeaderRe.FindStringSubmatch(line)
	if m == nil {
		return "", "", false
	}
	st := Firing
	if isResolvedWord(m[1]) {
		st = Resolved
	}
	name := strings.TrimSpace(m[2])
	if i := strings.Index(name, "("); i > 0 {
		name = strings.TrimSpace(name[:i])
	}
	// A header line whose remainder is itself a label ("Status: firing") should
	// not become the alert name.
	if _, _, isLabel := parseLabelLine(name); isLabel {
		name = ""
	}
	return st, name, true
}

func isResolvedWord(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "resolved", "ok", "recovered", "normal", "cleared":
		return true
	}
	return false
}

func parseLabelLine(line string) (key, value string, ok bool) {
	m := labelLineRe.FindStringSubmatch(line)
	if m == nil {
		return "", "", false
	}
	k := strings.ToLower(strings.TrimSpace(m[1]))
	k = strings.ReplaceAll(k, " ", "_")
	v := strings.TrimSpace(stripLeadingSymbols(strings.TrimSpace(m[2])))
	if k == "" || v == "" {
		return "", "", false
	}
	// "https://host/path" splits on the scheme; not a label.
	if strings.HasPrefix(v, "//") {
		return "", "", false
	}
	return k, v, true
}

func firstLine(s string) string {
	line := strings.TrimSpace(stripLeadingSymbols(stripFormatting(s)))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	if len([]rune(line)) > 80 {
		line = string([]rune(line)[:77]) + "..."
	}
	return line
}

func stripFormatting(s string) string { return formattingRe.ReplaceAllString(s, "") }

func stripLeadingSymbols(s string) string { return leadingSymbolsRe.ReplaceAllString(s, "") }

// skeleton reduces a notification to what is stable across repeats of the same
// alert: no digits (timestamps, counts, percentages), no case, no whitespace
// runs. It backs the fingerprint when a template carries no usable labels.
func skeleton(s string) string {
	t := strings.ToLower(stripFormatting(s))
	t = timestampRe.ReplaceAllString(t, "#t")
	t = measuredRe.ReplaceAllString(t, "#v")
	t = decimalRe.ReplaceAllString(t, "#n")
	t = strings.Join(strings.Fields(t), " ")
	if len(t) > 512 {
		t = t[:512]
	}
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])[:16]
}

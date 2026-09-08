package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/snapp-incubator/snappcloud-bot/internal/alerts"
	"github.com/snapp-incubator/snappcloud-bot/internal/mattermost"
	"github.com/snapp-incubator/snappcloud-bot/internal/metrics"
)

// alertCommand handles the alert-channel sub-commands, which a human runs by
// mentioning the bot. Returns handled=false for anything else.
func (s *Service) alertCommand(identity string, p mattermost.Post, query string) (bool, string) {
	if s.alertChannels == nil {
		return false, ""
	}
	low := strings.ToLower(strings.TrimSpace(query))
	switch low {
	case "alerts on", "watch alerts", "mark alert channel":
		return true, s.markAlertChannel(identity, p)
	case "alerts off", "unwatch alerts", "unmark alert channel":
		if err := s.alertChannels.Unmark(p.ChannelID); err != nil {
			return true, "This channel is not marked for alerts."
		}
		metrics.AlertChannels.Set(float64(s.alertChannels.Count()))
		return true, "🔕 Stopped investigating alerts in this channel."
	case "alerts", "alerts status":
		return true, s.alertStatus(p)
	}
	return false, ""
}

func (s *Service) markAlertChannel(identity string, p mattermost.Post) string {
	if p.IsDirect() {
		return "Alert watching only makes sense in a channel — run this where the alerts arrive."
	}
	ch := s.alertChannels.Mark(p.ChannelID, "", identity)
	metrics.AlertChannels.Set(float64(s.alertChannels.Count()))
	lim := s.alertAgg.Limits()

	// Say plainly whose access is used and who can see the result. Everyone in
	// this channel is about to read answers produced with the marker's
	// authorization, and that is not obvious from the outside.
	return fmt.Sprintf(
		"🔔 Watching this channel for alerts.\n\n"+
			"- Investigations run with **%s**'s access, re-checked on every batch — so **everyone in this channel "+
			"will see what %s can see**. Turn it off with `alerts off` if that is not right for this channel.\n"+
			"- Alerts are collected for %s, then the %d most severe are investigated; repeats of the same alert are "+
			"skipped for %s.\n"+
			"- Answers are posted as replies in the alert's own thread.",
		ch.Owner, ch.Owner, human(lim.Window), lim.MaxPerWindow, human(lim.Cooldown))
}

func (s *Service) alertStatus(p mattermost.Post) string {
	ch, ok := s.alertChannels.Get(p.ChannelID)
	if !ok {
		return "This channel is not watched. Say `alerts on` to have me investigate alerts posted here."
	}
	lim := s.alertAgg.Limits()
	return fmt.Sprintf("🔔 Watched since %s, running with **%s**'s access.\n"+
		"Window %s · at most %d investigations per window · same alert skipped for %s · ignoring %s.",
		ch.Marked.Format("2006-01-02"), ch.Owner, human(lim.Window), lim.MaxPerWindow,
		human(lim.Cooldown), strings.Join(lim.IgnoredAlerts, ", "))
}

// ingestAlert buffers a post from a marked channel. It is called for posts by
// accounts with no SSO identity — webhooks and integrations — which is what
// separates an alert from someone talking in the same channel.
func (s *Service) ingestAlert(p mattermost.Post) bool {
	if s.alertChannels == nil || s.alertAgg == nil {
		return false
	}
	if _, marked := s.alertChannels.Get(p.ChannelID); !marked {
		return false
	}
	text := p.AlertText()
	a, ok := alerts.Parse(text, time.Now())
	if !ok {
		return false
	}
	a.PostID = p.ThreadRoot()
	a.ChannelID = p.ChannelID

	accepted, reason := s.alertAgg.Add(a, time.Now())
	metrics.AlertsReceived.WithLabelValues(outcomeLabel(accepted, reason)).Inc()
	metrics.AlertsPending.Set(float64(s.alertAgg.Pending()))
	s.log.Debug("alert ingested", "channel", p.ChannelID, "alert", a.Name,
		"severity", a.Severity, "accepted", accepted, "reason", reason)
	return true
}

func outcomeLabel(accepted bool, reason string) string {
	if !accepted {
		return reason
	}
	if reason == "duplicate" {
		return "duplicate"
	}
	return "queued"
}

// Investigate runs one batch of alerts and posts the findings. It implements
// alerts.Investigator.
//
// Authorization is resolved HERE, at investigation time, from the identity that
// marked the channel — never stored with the mark. If that person's access was
// revoked or narrowed, the investigation is scoped to what they can see now, or
// refused outright.
func (s *Service) Investigate(ctx context.Context, ch alerts.Channel, b alerts.Batch) error {
	reqID := newReqID()
	lg := s.log.With("req", reqID, "src", "alert", "channel", b.ChannelID)

	scope, err := s.resolver.Resolve(ctx, ch.Owner)
	if err != nil {
		return fmt.Errorf("authorize %s: %w", ch.Owner, err)
	}
	if scope.Empty() {
		lg.Info("alert batch skipped: owner has no access", "owner", ch.Owner)
		s.post(ctx, b.ChannelID, "",
			fmt.Sprintf("⚠️ I can no longer investigate alerts here: **%s**, who enabled this, has no cluster access. "+
				"Someone with access can take it over by saying `alerts on`.", ch.Owner))
		metrics.AlertInvestigations.WithLabelValues("unauthorized").Inc()
		return nil
	}

	for _, a := range b.Investigate {
		start := time.Now()
		answer, aerr := s.brain.Answer(ctx, scope, ch.Owner, alertQuery(a), "", reqID)
		metrics.AlertInvestigationDuration.Observe(time.Since(start).Seconds())
		if aerr != nil {
			metrics.AlertInvestigations.WithLabelValues("error").Inc()
			lg.Warn("alert investigation failed", "alert", a.Name, "err", aerr)
			continue
		}
		clean := sanitize(answer)
		if clean == "" {
			metrics.AlertInvestigations.WithLabelValues("empty").Inc()
			lg.Warn("alert investigation produced nothing", "alert", a.Name)
			continue
		}
		metrics.AlertInvestigations.WithLabelValues("ok").Inc()
		// Reply in the alert's own thread: the channel stays readable, and the
		// finding sits with the alert it explains.
		s.post(ctx, b.ChannelID, a.PostID, "🔎 **"+a.Name+"**\n\n"+clean)
	}

	if note := skippedNote(b.Skipped); note != "" {
		s.post(ctx, b.ChannelID, rootOf(b), note)
	}
	return nil
}

// alertQuery turns an alert into the question the agent answers. It names the
// job explicitly, because an alert's own text usually suggests a remedy that
// only clears the symptom.
func alertQuery(a alerts.Alert) string {
	var b strings.Builder
	b.WriteString("This alert just fired. Investigate it on the cluster and report: what is actually happening " +
		"(with the evidence you found), the root cause, and how to fix it. If the alert text suggests a remedy, " +
		"say whether it addresses the cause or only clears the symptom. If you cannot determine the cause, say what " +
		"you checked and what you would need.\n\n")
	if ns, cluster := a.Scope(); ns != "" || cluster != "" {
		b.WriteString("Scope: ")
		if ns != "" {
			b.WriteString("namespace " + ns + " ")
		}
		if cluster != "" {
			b.WriteString("on " + cluster)
		}
		b.WriteString("\n\n")
	}
	b.WriteString("Alert:\n" + a.Describe())
	if a.Raw != "" && a.Raw != a.Describe() {
		b.WriteString("\n\nAs posted:\n" + a.Raw)
	}
	return b.String()
}

// skippedNote summarises what fired but was not investigated, so the channel is
// never left wondering whether the bot saw an alert.
func skippedNote(skipped []alerts.Skipped) string {
	if len(skipped) == 0 {
		return ""
	}
	byReason := map[string][]string{}
	for _, s := range skipped {
		byReason[s.Reason] = append(byReason[s.Reason], s.Alert.Name)
	}
	var parts []string
	for _, reason := range []string{"cooldown", "budget"} {
		names := byReason[reason]
		if len(names) == 0 {
			continue
		}
		switch reason {
		case "cooldown":
			parts = append(parts, fmt.Sprintf("%s (already investigated recently)", strings.Join(uniq(names), ", ")))
		case "budget":
			parts = append(parts, fmt.Sprintf("%s (too many at once)", strings.Join(uniq(names), ", ")))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "ℹ️ Also firing, not investigated: " + strings.Join(parts, "; ") + "."
}

func rootOf(b alerts.Batch) string {
	if len(b.Investigate) > 0 {
		return b.Investigate[0].PostID
	}
	if len(b.Skipped) > 0 {
		return b.Skipped[0].Alert.PostID
	}
	return ""
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func human(d time.Duration) string {
	return strings.TrimSuffix(strings.TrimSuffix(d.String(), "0s"), "0m")
}

var errNoAlertSupport = errors.New("alert watching is not enabled")

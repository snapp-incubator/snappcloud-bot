package bot

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/snapp-incubator/snappcloud-bot/internal/alerts"
	"github.com/snapp-incubator/snappcloud-bot/internal/humanize"
	"github.com/snapp-incubator/snappcloud-bot/internal/mattermost"
	"github.com/snapp-incubator/snappcloud-bot/internal/metrics"
)

// alertCommand handles the alert-channel sub-commands, which a human runs by
// mentioning the bot. Returns handled=false for anything else.
func (s *Service) alertCommand(identity string, p mattermost.Post, query string) (bool, string) {
	if s.alertChannels == nil {
		return false, ""
	}
	verb, ok := alertVerb(normalizeCommand(query))
	if !ok {
		return false, ""
	}
	switch verb {
	case "on":
		return true, s.markAlertChannel(identity, p)
	case "off":
		if err := s.alertChannels.Unmark(p.ChannelID); err != nil {
			return true, "This channel is not marked for alerts."
		}
		metrics.AlertChannels.Set(float64(s.alertChannels.Count()))
		return true, "🔕 Stopped investigating alerts in this channel."
	default:
		return true, s.alertStatus(p)
	}
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
		ch.Owner, ch.Owner, humanize.Duration(lim.Window), lim.MaxPerWindow, humanize.Duration(lim.Cooldown))
}

func (s *Service) alertStatus(p mattermost.Post) string {
	ch, ok := s.alertChannels.Get(p.ChannelID)
	if !ok {
		return "This channel is not watched. Say `alerts on` to have me investigate alerts posted here."
	}
	lim := s.alertAgg.Limits()
	return fmt.Sprintf("🔔 Watched since %s, running with **%s**'s access.\n"+
		"Window %s · at most %d investigations per window · same alert skipped for %s · ignoring %s.",
		ch.Marked.Format("2006-01-02"), ch.Owner, humanize.Duration(lim.Window), lim.MaxPerWindow,
		humanize.Duration(lim.Cooldown), strings.Join(lim.IgnoredAlerts, ", "))
}

// ingestAlert buffers a post from a marked channel. It is called for posts
// produced by an integration, which is what separates an alert from someone
// talking in the same channel.
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
		"severity", a.Severity, "source", p.IntegrationName(),
		"accepted", accepted, "reason", reason)
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

	// One investigation for the whole batch. Alerts that fire together are
	// usually one incident, and asking about them separately gets several
	// partial answers — each blind to the others — plus a message each.
	start := time.Now()
	answer, aerr := s.brain.Answer(ctx, scope, ch.Owner, batchQuery(b), "", reqID)
	metrics.AlertInvestigationDuration.Observe(time.Since(start).Seconds())
	if aerr != nil {
		metrics.AlertInvestigations.WithLabelValues("error").Inc()
		return fmt.Errorf("agent: %w", aerr)
	}
	clean := sanitize(answer)
	if clean == "" {
		metrics.AlertInvestigations.WithLabelValues("empty").Inc()
		lg.Warn("alert investigation produced nothing", "alerts", len(b.Investigate))
		return nil
	}
	metrics.AlertInvestigations.WithLabelValues("ok").Inc()
	lg.Info("alert batch investigated", "subjects", len(b.Investigate),
		"context", len(b.Context), "skipped", len(b.Skipped))
	s.post(ctx, b.ChannelID, b.Root(), batchHeader(b)+clean)

	if note := skippedNote(b.Skipped); note != "" {
		s.post(ctx, b.ChannelID, b.Root(), note)
	}
	return nil
}

// batchHeader names what the finding covers, so a single message about five
// alerts is not mistaken for a comment on one of them.
func batchHeader(b alerts.Batch) string {
	if len(b.Investigate) == 1 && len(b.Context) == 0 {
		return "🔎 **" + b.Investigate[0].Name + "**\n\n"
	}
	names := make([]string, 0, len(b.Investigate))
	for _, a := range b.Investigate {
		names = append(names, a.Name)
	}
	header := fmt.Sprintf("🔎 **%d alerts fired together** — %s\n\n",
		len(b.Investigate), strings.Join(uniq(names), ", "))
	if len(b.Context) > 0 {
		ctxNames := make([]string, 0, len(b.Context))
		for _, a := range b.Context {
			ctxNames = append(ctxNames, a.Name)
		}
		header += "_Also firing, investigated recently: " + strings.Join(uniq(ctxNames), ", ") + "._\n\n"
	}
	return header
}

// batchQuery turns a window of alerts into ONE question. The framing matters:
// the model is told they may be one incident and asked to say which, because
// the alternative — treating each alert as its own problem — is exactly what
// buries an on-call engineer in symptoms of a single cause.
func batchQuery(b alerts.Batch) string {
	var q strings.Builder
	if len(b.Investigate) == 1 {
		q.WriteString("This alert just fired. Investigate it on the cluster and report: what is actually " +
			"happening (with the evidence you found), the root cause, and how to fix it.")
	} else {
		fmt.Fprintf(&q, "These %d alerts fired within the same minute. Alerts that fire together are "+
			"usually symptoms of ONE incident, so investigate them as a whole and report: what is actually "+
			"happening (with the evidence you found); whether this is one incident or several, mapping each alert "+
			"to its cause; the root cause; and how to fix it. If some alerts are unrelated to the rest, say so "+
			"and treat them separately rather than forcing one story.", len(b.Investigate))
	}
	q.WriteString(" If an alert's text suggests a remedy, say whether it addresses the cause or only clears the " +
		"symptom. If you cannot determine the cause, say what you checked and what you would need.\n\n")

	if scopes := batchScope(b); scopes != "" {
		q.WriteString("Scope: " + scopes + "\n\n")
	}

	q.WriteString("Alerts:\n")
	for i, a := range b.Investigate {
		fmt.Fprintf(&q, "\n%d. %s\n", i+1, a.Describe())
	}
	if len(b.Context) > 0 {
		q.WriteString("\nAlso firing right now, already investigated recently — context only, do not re-diagnose " +
			"unless it explains the above:\n")
		for _, a := range b.Context {
			q.WriteString("- " + a.Name + "\n")
		}
	}
	return q.String()
}

// batchScope collects the namespaces and clusters the alerts name, so the
// investigation starts where the incident is.
func batchScope(b alerts.Batch) string {
	nsSet, clusterSet := map[string]bool{}, map[string]bool{}
	for _, a := range b.Alerts() {
		ns, cluster := a.Scope()
		if ns != "" {
			nsSet[ns] = true
		}
		if cluster != "" {
			clusterSet[cluster] = true
		}
	}
	var parts []string
	if len(nsSet) > 0 {
		parts = append(parts, "namespaces "+strings.Join(sortedKeys(nsSet), ", "))
	}
	if len(clusterSet) > 0 {
		parts = append(parts, "clusters "+strings.Join(sortedKeys(clusterSet), ", "))
	}
	return strings.Join(parts, "; ")
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// skippedNote covers only what was left out entirely — alerts over the
// per-batch cap. Cooldown alerts are not mentioned here: they are handed to the
// investigation as context, so they are already accounted for.
func skippedNote(skipped []alerts.Skipped) string {
	if len(skipped) == 0 {
		return ""
	}
	names := make([]string, 0, len(skipped))
	for _, s := range skipped {
		names = append(names, s.Alert.Name)
	}
	return fmt.Sprintf("ℹ️ %d more alert(s) fired in the same minute than I investigate at once: %s. "+
		"Ask me about any of them directly.", len(skipped), strings.Join(uniq(names), ", "))
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

package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/snapp-incubator/snappcloud-bot/internal/mattermost"
	"github.com/snapp-incubator/snappcloud-bot/internal/metrics"
	"github.com/snapp-incubator/snappcloud-bot/internal/schedule"
)

// scheduleCommand handles the schedule sub-commands. It returns handled=false
// when the message is an ordinary question, so normal flow continues.
func (s *Service) scheduleCommand(identity string, p mattermost.Post, query string) (bool, string) {
	low := strings.ToLower(strings.TrimSpace(query))

	switch {
	case low == "schedules" || low == "list schedules" || low == "my schedules":
		return true, s.renderSchedules(identity)

	case strings.HasPrefix(low, "unschedule ") || strings.HasPrefix(low, "delete schedule "):
		id := strings.TrimSpace(query[strings.LastIndex(low, " ")+1:])
		if err := s.sched.Delete(identity, id); err != nil {
			return true, fmt.Sprintf("❔ %s. Say `schedules` to see yours.", err.Error())
		}
		s.observeSchedules()
		return true, fmt.Sprintf("🗑️ Removed schedule `%s`.", id)

	case strings.HasPrefix(low, "schedule "):
		return true, s.addSchedule(identity, p, strings.TrimSpace(query[len("schedule "):]))
	}
	return false, ""
}

// addSchedule parses "<cadence> <question>" and stores it.
func (s *Service) addSchedule(identity string, p mattermost.Post, rest string) string {
	// Parse in the schedule timezone: a time the user types means that time
	// where they are, not where the pod runs.
	e, q, err := schedule.Parse(rest, s.sched.Now())
	if err != nil {
		return "❔ I could not read that schedule. Try:\n" +
			"```text\n" +
			"schedule every day at 09:00 are any pods failing in my-namespace?\n" +
			"schedule every 6h is my-namespace over quota?\n" +
			"schedule every 4h starting at 16:10 any pods restarting in my-namespace?\n" +
			"schedule every monday at 08:30 summarise last week's drops\n" +
			"```"
	}
	if strings.TrimSpace(q) == "" {
		return "❔ That schedule has no question. Put the question after the time, e.g. " +
			"`schedule every day at 09:00 are any pods failing?`"
	}
	if len([]rune(q)) > s.maxQueryRunes {
		return msgTooLong
	}

	e.User = identity
	e.ChannelID = p.ChannelID
	// Answers land in the thread the schedule was created in (or the DM).
	if !p.IsDirect() {
		e.RootID = p.ThreadRoot()
	}
	e.Query = q

	if err := s.sched.Add(e); err != nil {
		return "🚫 " + err.Error() + "."
	}
	s.observeSchedules()
	return fmt.Sprintf("⏰ Scheduled **%s**: %q\nFirst run %s. Say `schedules` to list, `unschedule %s` to remove.",
		e.Spec, q, s.sched.FormatWhen(e.Next), e.ID)
}

// observeSchedules republishes the inventory gauges after a user-driven change.
// The runner refreshes them on its own tick too; this keeps them current between
// ticks.
func (s *Service) observeSchedules() {
	total, owners := s.sched.Stats()
	metrics.Schedules.Set(float64(total))
	metrics.ScheduleOwners.Set(float64(owners))
}

func (s *Service) renderSchedules(identity string) string {
	list := s.sched.List(identity)
	if len(list) == 0 {
		return "You have no schedules. Create one with " +
			"`schedule every day at 09:00 <your question>`."
	}
	var b strings.Builder
	b.WriteString("**Your schedules**\n\n| id | when | next | question |\n| --- | --- | --- | --- |\n")
	for _, e := range list {
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n",
			e.ID, e.Spec, s.sched.FormatWhen(e.Next), e.Query)
	}
	lim := s.sched.Limits()
	fmt.Fprintf(&b, "\nRemove one with `unschedule <id>`. Limits: %d per user, no more often than every %s.",
		lim.PerUser, lim.MinInterval)
	return b.String()
}

// RunScheduled executes one saved query and posts the answer. It implements
// schedule.Answerer.
//
// The owner's authorization is resolved HERE, at run time, never stored with the
// schedule: if their access was revoked or narrowed since the schedule was
// created, the run is scoped to what they can see now (or refused outright).
func (s *Service) RunScheduled(ctx context.Context, e schedule.Entry) error {
	reqID := newReqID()
	lg := s.log.With("req", reqID, "src", "schedule", "id", e.ID)

	scope, err := s.resolver.Resolve(ctx, e.User)
	if err != nil {
		return fmt.Errorf("authorize %s: %w", e.User, err)
	}
	if scope.Empty() {
		// Not an error worth retrying: the user genuinely has no access now.
		lg.Info("scheduled run skipped: owner has no access", "user", e.User)
		s.post(ctx, e.ChannelID, e.RootID,
			fmt.Sprintf("⏰ Schedule `%s` did not run: you no longer have access to any cluster.", e.ID))
		return schedule.ErrSkipped
	}

	lg.Info("scheduled run", "user", e.User, "clusters", scope.Clusters())
	answer, aerr := s.brain.Answer(ctx, scope, e.User, e.Query, "", reqID)
	if aerr != nil {
		return fmt.Errorf("agent: %w", aerr)
	}
	clean := sanitize(answer)
	if clean == "" {
		return errors.New("empty answer")
	}
	s.post(ctx, e.ChannelID, e.RootID,
		fmt.Sprintf("⏰ **%s** — %s\n\n%s", e.Spec, e.Query, clean))
	metrics.Messages.WithLabelValues("scheduled").Inc()
	return nil
}

// post writes a message to a channel/thread, splitting long answers.
func (s *Service) post(ctx context.Context, channelID, rootID, msg string) {
	parts := splitMessage(msg, maxPostRunes)
	if len(parts) > maxPostParts {
		parts = parts[:maxPostParts]
	}
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			continue
		}
		if err := s.mm.CreatePost(ctx, channelID, part, rootID); err != nil {
			s.log.Error("post scheduled answer", "channel", channelID, "err", err)
			return
		}
	}
}

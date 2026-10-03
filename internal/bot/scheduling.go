package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/snapp-incubator/snappcloud-bot/internal/humanize"
	"github.com/snapp-incubator/snappcloud-bot/internal/mattermost"
	"github.com/snapp-incubator/snappcloud-bot/internal/metrics"
	"github.com/snapp-incubator/snappcloud-bot/internal/schedule"
)

// scheduleCommand handles the schedule sub-commands. It returns handled=false
// when the message is an ordinary question, so normal flow continues.
func (s *Service) scheduleCommand(identity string, p mattermost.Post, query string) (bool, string) {
	// Guarded at the call site too, but not relying on that keeps the two
	// command handlers symmetric and a nil store from panicking a message.
	if s.sched == nil {
		return false, ""
	}
	low := normalizeCommand(query)

	switch {
	case low == "schedules" || low == "list schedules" || low == "my schedules" ||
		low == "schedules list" || low == "schedule list":
		return true, s.renderSchedules(identity)

	case strings.HasPrefix(low, "schedules remove ") || strings.HasPrefix(low, "schedule remove "):
		id := strings.TrimSpace(low[strings.LastIndex(low, " ")+1:])
		if err := s.sched.Delete(identity, id); err != nil {
			return true, fmt.Sprintf("❔ %s. Say `schedules` to see yours.", err.Error())
		}
		s.observeSchedules()
		return true, fmt.Sprintf("🗑️ Removed schedule `%s`.", id)

	case strings.HasPrefix(low, "unschedule ") || strings.HasPrefix(low, "delete schedule "):
		id := strings.TrimSpace(query[strings.LastIndex(low, " ")+1:])
		if err := s.sched.Delete(identity, id); err != nil {
			return true, fmt.Sprintf("❔ %s. Say `schedules` to see yours.", err.Error())
		}
		s.observeSchedules()
		return true, fmt.Sprintf("🗑️ Removed schedule `%s`.", id)

	// "run <id>" executes a stored schedule now, through the same path the timer
	// uses. Pasting the question into the channel instead is NOT the same run:
	// an interactive message carries the thread's history and no unattended
	// framing, and it is bounded by the chat timeout rather than the schedule
	// one. Comparing a hand-run report with a scheduled one meant comparing two
	// different requests, which is no way to tell whether a fix worked.
	case strings.HasPrefix(low, "run schedule ") || strings.HasPrefix(low, "run "):
		id := strings.TrimSpace(query[strings.LastIndex(low, " ")+1:])
		return true, s.runNow(identity, id)

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
		return fmt.Sprintf(msgTooLongFmt, len([]rune(q))-s.maxQueryRunes, s.maxQueryRunes)
	}

	e.User = identity
	e.ChannelID = p.ChannelID
	// Answers land in the thread the schedule was created in (or the DM).
	if !p.IsDirect() {
		e.RootID = p.ThreadRoot()
	}
	e.Query = q

	if err := s.sched.Add(e); err != nil {
		return "🚫 " + capitalize(err.Error()) + "."
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
	// A scheduled question can be four thousand characters of report spec.
	// Putting that in a table cell destroys the table — its newlines end the
	// row — and buries the two things this listing is for: which schedule is
	// which, and the id to remove it by. One line each, and the full text is
	// still the user's own message further up the channel.
	var b strings.Builder
	b.WriteString("**Your schedules**\n\n| ID | When | Next run | Question |\n| --- | --- | --- | --- |\n")
	for _, e := range list {
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n",
			e.ID, e.Spec, s.sched.FormatWhen(e.Next), tableCell(e.Query, 90))
	}
	lim := s.sched.Limits()
	fmt.Fprintf(&b, "\nRemove one with `unschedule <id>`. Limits: %d per user, no more often than every %s.",
		lim.PerUser, humanize.Duration(lim.MinInterval))
	return b.String()
}

// RunScheduled executes one saved query and posts the answer. It implements
// schedule.Answerer.
//
// runNow runs one of the caller's own schedules immediately, by the same code
// path as the timer: same unattended framing, same empty history, same timeout,
// and the same worker pool, so a manual run queues behind scheduled work
// instead of competing with it. The per-user rate limit has already been spent
// to get here, but that is not the protection that matters — a token is cheap
// and a run is half an hour of agent loop — so the pool and the one-at-a-time
// guard are what bound it.
//
// Another user's id reads as not found rather than refused: whether an id
// exists is itself something only its owner should learn.
func (s *Service) runNow(identity, id string) string {
	e, err := s.sched.Owned(identity, id)
	if err != nil {
		return fmt.Sprintf("❔ No schedule `%s` of yours. Say `schedules` to see them.", id)
	}
	if s.trigger == nil {
		return "🚫 Schedules are not running in this instance, so there is nothing to run."
	}
	switch err := s.trigger(e); {
	case errors.Is(err, schedule.ErrBusy):
		return fmt.Sprintf("⏳ Schedule `%s` is already running. Its answer will post when it finishes.", e.ID)
	case err != nil:
		return fmt.Sprintf("🚫 Could not start schedule `%s`: %s.", e.ID, err.Error())
	}
	return fmt.Sprintf("▶️ Running schedule `%s` now — the answer posts where the schedule does, "+
		"and this is the same run the timer makes, so it takes as long as one. "+
		"Tonight's run still happens.", e.ID)
}

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
		_ = s.post(ctx, e.ChannelID, e.RootID,
			fmt.Sprintf("⏰ Schedule `%s` did not run: you no longer have access to any cluster.", e.ID))
		return schedule.ErrSkipped
	}

	lg.Info("scheduled run", "user", e.User, "clusters", scope.Clusters())
	answer, aerr := s.brain.Answer(ctx, scope, e.User, unattended("scheduled report", e.Query), "", reqID)
	if aerr != nil {
		return fmt.Errorf("agent: %w", aerr)
	}
	clean := sanitize(answer)
	if clean == "" {
		return errors.New("empty answer")
	}
	if err := s.post(ctx, e.ChannelID, e.RootID,
		fmt.Sprintf("⏰ **%s** — %s\n\n%s", e.Spec, summarizeQuery(e.Query, 200), clean)); err != nil {
		return fmt.Errorf("deliver answer: %w", err)
	}
	metrics.Messages.WithLabelValues("scheduled").Inc()
	return nil
}

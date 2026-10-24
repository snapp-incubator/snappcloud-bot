package bot

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/snapp-incubator/snappcloud-bot/internal/bestpractice"
	"github.com/snapp-incubator/snappcloud-bot/internal/mail"
	"github.com/snapp-incubator/snappcloud-bot/internal/mattermost"
	"github.com/snapp-incubator/snappcloud-bot/internal/metrics"
)

// The best-practice report is the one feature that reads clusters without a
// caller: it audits every cluster on the bot's own authority. What it reads is
// released only once a cluster-admin approves the preview in the cloud-admin
// channel: then each email the routing names goes out, a group of teams to
// their admins, with its findings in a PDF. The admin channel stays the
// control room.

// reportMessenger is what the report needs from Mattermost beyond posting.
type reportMessenger interface {
	CreatePostID(ctx context.Context, channelID, message, rootID string) (string, error)
}

// reportMailer sends the report's emails (mail.Sender).
type reportMailer interface {
	Send(ctx context.Context, m mail.Message) (rejected []string, err error)
	CanReply() bool
}

// reportRunner runs report jobs one at a time (bestpractice.Runner).
type reportRunner interface {
	Do(kind string, fn func(context.Context) error) error
	Next() (time.Time, bool)
}

// ReportOptions enables the weekly best-practice report.
type ReportOptions struct {
	Store *bestpractice.Store
	// Collect audits every cluster with the bot's own authority.
	Collect func(context.Context) bestpractice.Inventory
	Rules   bestpractice.Options
	// AdminChannel is where previews are posted and approved.
	AdminChannel string
	Messenger    reportMessenger
	Mailer       reportMailer
	// Routing decides which email each team's findings go in, and to whom.
	Routing  bestpractice.Routing
	Location *time.Location
	// Docs maps a rule topic (pdb, quorum, rabbitmq, redis) to a docs page.
	Docs map[string]string
	// Pace spaces the emails out so they do not trip the mail server's rate
	// limit (default 200ms).
	Pace time.Duration
}

type reportFeature struct {
	ReportOptions
	runner reportRunner
	now    func() time.Time
}

func newReportFeature(o *ReportOptions) *reportFeature {
	if o == nil {
		return nil
	}
	if o.Pace <= 0 {
		o.Pace = 200 * time.Millisecond
	}
	if o.Location == nil {
		o.Location = time.Local
	}
	return &reportFeature{ReportOptions: *o, now: time.Now}
}

// SetReportRunner supplies the runner the report commands start jobs on. Set
// after construction because the runner needs the service to build reports.
func (s *Service) SetReportRunner(r reportRunner) {
	if s.report != nil {
		s.report.runner = r
	}
}

// BuildReport audits the clusters, stores the report as pending and posts its
// preview to the admin channel. The timer and `report run` both come here.
func (s *Service) BuildReport(ctx context.Context) error {
	rf := s.report
	inv := rf.Collect(ctx)
	for c, err := range inv.Failed {
		metrics.ReportCollectFailures.WithLabelValues(c).Inc()
		s.log.Warn("best-practice report: cluster not scanned", "cluster", c, "err", err)
	}
	if len(inv.Clusters) == 0 {
		metrics.ReportRuns.WithLabelValues("error").Inc()
		var b strings.Builder
		b.WriteString("⚠️ The best-practice report could not run: no cluster could be read.\n")
		for _, c := range sortedKeys(inv.Failed) {
			fmt.Fprintf(&b, "- %s: %s\n", c, firstLine(inv.Failed[c], 200))
		}
		_ = s.post(ctx, rf.AdminChannel, "", b.String())
		return errors.New("no cluster could be read")
	}

	res := bestpractice.Evaluate(inv, rf.Rules)
	r := &bestpractice.Report{
		Findings: res.Findings,
		Admins:   inv.TeamAdmins(),
		Scanned:  inv.Scanned(),
		Ignored:  res.Ignored,
	}
	if len(inv.Failed) > 0 {
		r.Failed = inv.Failed
	}

	superseded, err := rf.Store.Add(r, rf.now())
	if err != nil {
		// The report still works until a restart; say so rather than drop it.
		s.log.Error("persist best-practice report", "err", err)
	}
	rep, err := rf.Store.Get(r.ID, rf.now())
	if err != nil {
		return err
	}
	observeFindings(rep.Findings)
	for _, old := range superseded {
		if old.RootID != "" {
			_ = s.post(ctx, rf.AdminChannel, old.RootID,
				fmt.Sprintf("⏭️ Superseded by report `%s` before anyone approved it; nothing was sent.", rep.ID))
		}
	}

	root, err := rf.Messenger.CreatePostID(ctx, rf.AdminChannel, bestpractice.PreviewRoot(rep, rf.Routing, rf.Location), "")
	if err != nil {
		metrics.ReportRuns.WithLabelValues("error").Inc()
		return fmt.Errorf("post preview: %w", err)
	}
	if _, err := rf.Store.Update(rep.ID, rf.now(), func(r *bestpractice.Report) error {
		r.RootID = root
		return nil
	}); err != nil {
		s.log.Error("record preview post", "report", rep.ID, "err", err)
	}
	if err := s.post(ctx, rf.AdminChannel, root, bestpractice.PreviewDetail(rep, rf.Routing)); err != nil {
		return fmt.Errorf("post preview detail: %w", err)
	}
	outcome := "ok"
	if len(inv.Failed) > 0 {
		outcome = "partial"
	}
	metrics.ReportRuns.WithLabelValues(outcome).Inc()
	s.log.Info("best-practice report built", "report", rep.ID, "findings", len(rep.Findings),
		"teams", len(rep.Teams()), "failed", len(inv.Failed))
	return nil
}

func observeFindings(fs []bestpractice.Finding) {
	metrics.ReportFindings.Reset()
	for _, rule := range bestpractice.Rules {
		for _, sev := range []bestpractice.Severity{bestpractice.High, bestpractice.Medium} {
			metrics.ReportFindings.WithLabelValues(rule, string(sev))
		}
	}
	for _, f := range fs {
		metrics.ReportFindings.WithLabelValues(f.Rule, string(f.Severity)).Inc()
	}
}

// reportSubcommands are the words after "report" this handler owns. Anything
// else is a question that happens to start with "report", and goes to the agent.
var reportSubcommands = map[string]bool{
	"status": true, "run": true, "approve": true, "reject": true, "exclude": true, "preview": true,
}

// reportCommand handles `report …`. Cluster-admins only, and only in the
// admin channel or a DM: a preview names every team's problems.
func (s *Service) reportCommand(ctx context.Context, identity string, p mattermost.Post, query string) (bool, string) {
	if s.report == nil {
		return false, ""
	}
	fields := strings.Fields(normalizeCommand(query))
	if len(fields) == 0 || (fields[0] != "report" && fields[0] != "reports") {
		return false, ""
	}
	sub := "status"
	if len(fields) > 1 {
		sub = fields[1]
	}
	if !reportSubcommands[sub] {
		return false, ""
	}
	args := fields[min(len(fields), 2):]

	if !p.IsDirect() && p.ChannelID != s.report.AdminChannel {
		return true, "Report commands work in the cloud-admin report channel, or in a direct message with me."
	}
	scope, err := s.resolver.Resolve(ctx, identity)
	if err != nil {
		return true, msgBackendError
	}
	if !scope.HasClusterWide() {
		return true, "The best-practice report is for cluster-admins: a preview lists every team's findings."
	}

	switch sub {
	case "status":
		return true, s.reportStatus()
	case "run":
		return true, s.reportRun(p)
	}
	if len(args) == 0 {
		return true, fmt.Sprintf("Which report? `report %s <id>` — `report status` shows the latest.", sub)
	}
	id := args[0]
	switch sub {
	case "approve":
		return true, s.reportApprove(identity, id)
	case "reject":
		return true, s.reportReject(ctx, identity, id)
	case "exclude":
		return true, s.reportExclude(id, splitTeams(args[1:]))
	default: // preview
		if len(args) < 2 {
			return true, fmt.Sprintf("Which team? `report preview %s <team>`.", id)
		}
		return true, s.reportPreview(id, args[1])
	}
}

func splitTeams(args []string) []string {
	var out []string
	for _, a := range args {
		for _, t := range strings.Split(a, ",") {
			if t = strings.TrimSpace(t); t != "" {
				out = append(out, t)
			}
		}
	}
	return out
}

func (s *Service) reportStatus() string {
	rf := s.report
	var b strings.Builder
	if r, ok := rf.Store.Latest(rf.now()); ok {
		b.WriteString(bestpractice.StatusLine(r, rf.Location))
	} else {
		b.WriteString("No best-practice report has been built yet.")
	}
	if rf.runner != nil {
		if next, ok := rf.runner.Next(); ok {
			fmt.Fprintf(&b, "\nNext scheduled run: %s.", bestpractice.FormatTime(next, rf.Location))
		} else {
			b.WriteString("\nNo schedule is configured; `report run` builds one.")
		}
	}
	return b.String()
}

func (s *Service) reportRun(p mattermost.Post) string {
	if err := s.reportDo("manual", s.BuildReport); err != "" {
		return err
	}
	where := "in the report channel"
	if p.ChannelID == s.report.AdminChannel {
		where = "in this channel"
	}
	return fmt.Sprintf("🔎 Scanning every cluster now. The preview posts %s when it is ready; "+
		"nothing goes to any team until someone approves it.", where)
}

// reportDo starts a job, returning a message for the user if it could not.
func (s *Service) reportDo(kind string, fn func(context.Context) error) string {
	if s.report.runner == nil {
		return "🚫 The report runner is not running in this instance."
	}
	switch err := s.report.runner.Do(kind, fn); {
	case errors.Is(err, bestpractice.ErrBusy):
		return "⏳ Another report job is running (" + err.Error() + "). Try again when it finishes."
	case err != nil:
		return "🚫 Could not start: " + err.Error() + "."
	}
	return ""
}

// notPendingReason explains why a report cannot be acted on.
func (s *Service) notPendingReason(r bestpractice.Report) string {
	loc := s.report.Location
	switch r.Status {
	case bestpractice.StatusSending:
		return fmt.Sprintf("Report `%s` is being sent now (approved by %s).", r.ID, r.DecidedBy)
	case bestpractice.StatusSent:
		return fmt.Sprintf("Report `%s` was already sent, approved by %s at %s.", r.ID, r.DecidedBy,
			bestpractice.FormatTime(r.DecidedAt, loc))
	case bestpractice.StatusRejected:
		return fmt.Sprintf("Report `%s` was rejected by %s at %s.", r.ID, r.DecidedBy,
			bestpractice.FormatTime(r.DecidedAt, loc))
	case bestpractice.StatusExpired:
		if r.SupersededBy != "" {
			return fmt.Sprintf("Report `%s` expired: report `%s` replaced it.", r.ID, r.SupersededBy)
		}
		return fmt.Sprintf("Report `%s` expired at %s: its data is too old to send. `report run` builds a fresh one.",
			r.ID, bestpractice.FormatTime(r.Expires, loc))
	}
	return ""
}

func (s *Service) pendingReport(id string) (bestpractice.Report, string) {
	r, err := s.report.Store.Get(id, s.report.now())
	if err != nil {
		return r, fmt.Sprintf("❔ No report `%s`. `report status` shows the latest.", id)
	}
	if r.Status != bestpractice.StatusPending {
		return r, s.notPendingReason(r)
	}
	return r, ""
}

func (s *Service) reportApprove(identity, id string) string {
	r, why := s.pendingReport(id)
	if why != "" {
		return why
	}
	if len(r.Sendable()) == 0 {
		return fmt.Sprintf("Report `%s` has nothing to send to any team. `report reject %s` closes it.", id, id)
	}
	if msg := s.reportDo("approve", func(ctx context.Context) error {
		return s.deliverReport(ctx, id, identity)
	}); msg != "" {
		return msg
	}
	return fmt.Sprintf("✅ Approved `%s`. Re-checking the clusters, then sending %d emails for %d teams; "+
		"the result posts in the report thread.", id, len(s.report.Routing.Mailings(r.Sendable(), r.Admins)), len(r.Teams()))
}

func (s *Service) reportReject(ctx context.Context, identity, id string) string {
	rf := s.report
	now := rf.now()
	r, err := rf.Store.Update(id, now, func(r *bestpractice.Report) error {
		if r.Status != bestpractice.StatusPending {
			return errNotPending
		}
		r.Status, r.DecidedBy, r.DecidedAt = bestpractice.StatusRejected, identity, now
		return nil
	})
	switch {
	case errors.Is(err, bestpractice.ErrNotFound):
		return fmt.Sprintf("❔ No report `%s`. `report status` shows the latest.", id)
	case errors.Is(err, errNotPending):
		return s.notPendingReason(r)
	case err != nil:
		return "🚫 " + err.Error()
	}
	if r.RootID != "" {
		_ = s.post(ctx, rf.AdminChannel, r.RootID, fmt.Sprintf("❌ Rejected by %s. Nothing was sent.", identity))
	}
	return fmt.Sprintf("❌ Rejected `%s`. Nothing was sent.", id)
}

var errNotPending = errors.New("report is not pending")

func (s *Service) reportExclude(id string, teams []string) string {
	if len(teams) == 0 {
		return fmt.Sprintf("Which teams? `report exclude %s <team>[,<team>]`.", id)
	}
	var unknown []string
	r, err := s.report.Store.Update(id, s.report.now(), func(r *bestpractice.Report) error {
		if r.Status != bestpractice.StatusPending {
			return errNotPending
		}
		has := map[string]bool{}
		for _, f := range r.Findings {
			has[f.Team] = true
		}
		for _, t := range teams {
			switch {
			case !has[t] || t == "":
				unknown = append(unknown, t)
			case !contains(r.Excluded, t):
				r.Excluded = append(r.Excluded, t)
			}
		}
		if len(unknown) > 0 {
			return errUnknownTeam
		}
		sort.Strings(r.Excluded)
		return nil
	})
	switch {
	case errors.Is(err, bestpractice.ErrNotFound):
		return fmt.Sprintf("❔ No report `%s`. `report status` shows the latest.", id)
	case errors.Is(err, errNotPending):
		return s.notPendingReason(r)
	case errors.Is(err, errUnknownTeam):
		return fmt.Sprintf("❔ Report `%s` has no findings for %s; nothing was changed.", id, strings.Join(unknown, ", "))
	case err != nil:
		return "🚫 " + err.Error()
	}
	return fmt.Sprintf("Excluded %s from `%s`. It now goes to %d teams with %d findings.",
		strings.Join(teams, ", "), id, len(r.Teams()), len(r.Sendable()))
}

var errUnknownTeam = errors.New("unknown team")

// reportPreview shows the email that carries a team's findings, and those
// findings as its PDF lists them.
func (s *Service) reportPreview(id, team string) string {
	rf := s.report
	r, err := rf.Store.Get(id, rf.now())
	if err != nil {
		return fmt.Sprintf("❔ No report `%s`. `report status` shows the latest.", id)
	}
	var fs []bestpractice.Finding
	for _, f := range r.Findings {
		if f.Team == team {
			fs = append(fs, f)
		}
	}
	if len(fs) == 0 {
		return fmt.Sprintf("Report `%s` has no findings for team `%s`.", id, team)
	}
	if contains(r.Excluded, team) {
		return fmt.Sprintf("Team `%s` is **excluded** from report `%s`, so nothing is sent for it. Its findings:\n```text\n%s```",
			team, id, bestpractice.FindingsText(fs, rf.Docs))
	}
	m, _ := bestpractice.MailingOf(rf.Routing.Mailings(r.Sendable(), r.Admins), team)
	e, err := bestpractice.ReportEmail(m, rf.Docs, rf.Rules.DisabledRules, rf.Mailer.CanReply(), r.Created, rf.Location)
	if err != nil {
		return "🚫 " + err.Error()
	}
	return fmt.Sprintf("_The email that carries `%s`'s findings (%s), and what its PDF lists for `%s`._\n\n"+
		"**To:** %s\n**Cc:** %s\n**Subject:** %s\n**Attached:** %s (%d KB)\n```text\n%s```\n**In the PDF, for `%s`:**\n```text\n%s```",
		team, m.Name, team, orNone(m.To), orNone(m.Cc), e.Subject, e.FileName, (len(e.PDF)+1023)/1024, e.Text,
		team, bestpractice.FindingsText(fs, rf.Docs))
}

func orNone(xs []string) string {
	if len(xs) == 0 {
		return "nobody"
	}
	return strings.Join(xs, ", ")
}

// deliverReport re-checks the clusters and sends the approved findings that
// still hold, one email per team admin.
func (s *Service) deliverReport(ctx context.Context, id, by string) error {
	rf := s.report
	now := rf.now()
	r, err := rf.Store.Update(id, now, func(r *bestpractice.Report) error {
		if r.Status != bestpractice.StatusPending {
			return errNotPending
		}
		r.Status, r.DecidedBy, r.DecidedAt = bestpractice.StatusSending, by, now
		return nil
	})
	if err != nil {
		return err
	}

	// Re-check: only what the admin approved, and only what is still true.
	inv := rf.Collect(ctx)
	cur := bestpractice.Evaluate(inv, rf.Rules)
	var current []bestpractice.Finding
	for _, f := range cur.Findings {
		if f.Team != "" && !contains(r.Excluded, f.Team) {
			current = append(current, f)
		}
	}
	send, resolved, held, unverified := bestpractice.Intersect(r.Sendable(), current, toSet(inv.Scanned()))
	d := &bestpractice.Delivery{Resolved: resolved, Held: held, Unverified: unverified}

	if len(inv.Failed) > 0 {
		d.RecheckFailed = inv.Failed
		for c, err := range inv.Failed {
			metrics.ReportCollectFailures.WithLabelValues(c).Inc()
			s.log.Warn("best-practice report: re-check failed; sending as previewed", "report", id, "cluster", c, "err", err)
		}
	}

	for i, m := range rf.Routing.Mailings(send, r.Admins) {
		if ctx.Err() != nil {
			d.Interrupted = true
			break
		}
		if i > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(rf.Pace):
			}
		}
		outcome := s.sendReportEmail(ctx, r, m, d)
		metrics.ReportDeliveries.WithLabelValues(outcome).Inc()
	}

	r, err = rf.Store.Update(id, rf.now(), func(r *bestpractice.Report) error {
		r.Status, r.Delivery = bestpractice.StatusSent, d
		return nil
	})
	if err != nil {
		s.log.Error("record best-practice delivery", "report", id, "err", err)
	}
	s.log.Info("best-practice report sent", "report", id, "by", by, "sent", len(d.Sent),
		"sendFailed", len(d.SendFailed), "unrouted", len(d.Unrouted), "rejected", len(d.Rejected),
		"interrupted", d.Interrupted)
	if r.RootID != "" {
		// post detaches from ctx, so this lands even when the job timed out.
		_ = s.post(ctx, rf.AdminChannel, r.RootID, bestpractice.DeliverySummary(r, rf.Location))
	}
	return nil
}

// sendReportEmail sends one mailing and records the outcome.
func (s *Service) sendReportEmail(ctx context.Context, r bestpractice.Report, m bestpractice.Mailing,
	d *bestpractice.Delivery) string {
	rf := s.report
	if !m.Recipients() {
		s.log.Warn("best-practice report: email has nobody to go to", "email", m.Name)
		d.Unrouted = append(d.Unrouted, m.Name)
		return "unrouted"
	}
	e, err := bestpractice.ReportEmail(m, rf.Docs, rf.Rules.DisabledRules, rf.Mailer.CanReply(), r.Created, rf.Location)
	if err != nil {
		s.log.Error("best-practice report: render email", "email", m.Name, "err", err)
		d.SendFailed = append(d.SendFailed, m.Name)
		return "send_error"
	}
	rejected, err := rf.Mailer.Send(ctx, mail.Message{
		To: m.To, Cc: m.Cc, Subject: e.Subject, Text: e.Text, HTML: e.HTML,
		Attachments: []mail.Attachment{{Name: e.FileName, ContentType: "application/pdf", Data: e.PDF}},
	})
	if len(rejected) > 0 {
		s.log.Warn("best-practice report: addresses skipped", "email", m.Name, "addresses", rejected)
		d.Rejected = append(d.Rejected, rejected...)
	}
	if err != nil {
		s.log.Warn("best-practice report: email failed", "email", m.Name, "err", err)
		d.SendFailed = append(d.SendFailed, m.Name)
		if errors.Is(err, mail.ErrRejected) {
			return "rejected"
		}
		return "send_error"
	}
	d.Sent = append(d.Sent, m.Name)
	return "sent"
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func toSet(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

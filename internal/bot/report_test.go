package bot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/snapp-incubator/snappcloud-bot/internal/authzclient"
	"github.com/snapp-incubator/snappcloud-bot/internal/bestpractice"
	"github.com/snapp-incubator/snappcloud-bot/internal/mail"
	"github.com/snapp-incubator/snappcloud-bot/internal/mattermost"
)

const adminChannel = "admins"

type sentPost struct{ channel, root, msg string }

// reportMM is Mattermost for the report: every post with its channel.
type reportMM struct {
	mu      sync.Mutex
	email   string // the sender of command posts
	posts   []sentPost
	nextRef int
}

func (m *reportMM) GetUser(context.Context, string) (mattermost.User, error) {
	return mattermost.User{Email: m.email}, nil
}
func (m *reportMM) CreatePost(ctx context.Context, ch, msg, root string) error {
	_, err := m.CreatePostID(ctx, ch, msg, root)
	return err
}
func (m *reportMM) CreatePostID(ctx context.Context, ch, msg, root string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.posts = append(m.posts, sentPost{ch, root, msg})
	m.nextRef++
	return fmt.Sprintf("post-%d", m.nextRef), nil
}
func (m *reportMM) Typing(context.Context, string, string) {}

// fakeMailer records every email; the server rejects nobody@.
type fakeMailer struct {
	mu   sync.Mutex
	sent []mail.Message
}

func (m *fakeMailer) Send(_ context.Context, msg mail.Message) ([]string, error) {
	var rejected []string
	for _, a := range slices.Concat(msg.To, msg.Cc) {
		if strings.HasPrefix(a, "nobody@") {
			rejected = append(rejected, a)
		}
	}
	if len(rejected) == len(msg.To)+len(msg.Cc) {
		return rejected, fmt.Errorf("%w: 550 no such user", mail.ErrRejected)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, msg)
	return rejected, nil
}
func (m *fakeMailer) CanReply() bool { return true }

// to returns the emails whose To holds addr.
func (m *fakeMailer) to(addr string) []mail.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []mail.Message
	for _, msg := range m.sent {
		if slices.Contains(msg.To, addr) {
			out = append(out, msg)
		}
	}
	return out
}

func (m *reportMM) in(channel string) []sentPost {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []sentPost
	for _, p := range m.posts {
		if p.channel == channel {
			out = append(out, p)
		}
	}
	return out
}

// scopeResolver gives each identity its own scope; a missing one fails.
type scopeResolver map[string]authzclient.Scope

func (r scopeResolver) Resolve(_ context.Context, user string) (authzclient.Scope, error) {
	s, ok := r[user]
	if !ok {
		return nil, errors.New("mcp-authz unreachable")
	}
	return s, nil
}

// syncRunner runs jobs inline, so a test sees their effect at once.
type syncRunner struct{}

func (syncRunner) Do(_ string, fn func(context.Context) error) error { return fn(context.Background()) }
func (syncRunner) Next() (time.Time, bool)                           { return time.Time{}, false }

// reportInventory: team pay has a drain blocker in pay-prod and one in
// pay-secret; team ops has one in ops-prod; one namespace has no team.
func reportInventory() bestpractice.Inventory {
	one := 1
	block := func(ns, name string) bestpractice.PDB {
		return bestpractice.PDB{Namespace: ns, Name: name, MinAvailable: &bestpractice.IntOrPercent{Value: 1},
			ExpectedPods: 1, CurrentHealthy: 1, DesiredHealthy: 1,
			Owners: []bestpractice.Owner{{Kind: "Deployment", Name: name, Replicas: &one}}}
	}
	return bestpractice.Inventory{Clusters: []bestpractice.ClusterInventory{{
		Cluster: "teh-1",
		Audit: bestpractice.Audit{
			PDBs: []bestpractice.PDB{block("pay-prod", "api"), block("pay-secret", "vault"),
				block("ops-prod", "tool"), block("orphan", "x")},
			Namespaces: map[string]bestpractice.NSOwner{
				"pay-prod": {Team: "pay"}, "pay-secret": {Team: "pay"}, "ops-prod": {Team: "ops"}, "orphan": {},
			},
		},
		Teams: []bestpractice.Team{
			{Team: "pay", Admins: []string{"alice@snapp.cab", "nobody@snapp.cab"}},
			{Team: "ops", Admins: []string{"bob@snapp.cab"}},
		},
	}}}
}

type reportFixture struct {
	svc   *Service
	mm    *reportMM
	mail  *fakeMailer
	store *bestpractice.Store
	inv   bestpractice.Inventory
}

// reportRouting: pay is a group of its own; ops is in no group.
var reportRouting = bestpractice.Routing{
	Groups: [][]string{{"pay"}},
	Others: []string{"tech-ems@snapp.cab"},
	Cc:     []string{"cto@snapp.cab"},
}

func newReportFixture(t *testing.T) *reportFixture {
	t.Helper()
	f := &reportFixture{
		mm:    &reportMM{email: "admin@snapp.cab"},
		mail:  &fakeMailer{},
		store: bestpractice.NewStore("", 0),
		inv:   reportInventory(),
	}
	resolver := scopeResolver{
		"admin@snapp.cab":  {"teh-1": {ClusterWide: true, Namespaces: []string{"pay-prod"}}},
		"tenant@snapp.cab": {"teh-1": {Namespaces: []string{"pay-prod"}}},
	}
	f.svc = New(f.mm, &fakeBrain{answer: "agent answer"}, resolver, Options{
		ConversationTTL: time.Hour, BotUsername: "snappbot", RequireMention: true,
		Reports: &ReportOptions{
			Store:        f.store,
			Collect:      func(context.Context) bestpractice.Inventory { return f.inv },
			AdminChannel: adminChannel,
			Messenger:    f.mm,
			Mailer:       f.mail,
			Routing:      reportRouting,
			Location:     time.UTC,
			Pace:         time.Nanosecond,
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	f.svc.SetReportRunner(syncRunner{})
	return f
}

// say posts a command as the given sender in the admin channel and returns
// the bot's reply.
func (f *reportFixture) say(t *testing.T, sender, channel, msg string) string {
	t.Helper()
	f.mm.email = sender
	before := len(f.mm.in(channel))
	p := mattermost.Post{ID: "cmd", UserID: "u", ChannelID: channel, Message: "@snappbot " + msg, Mentioned: true}
	if err := f.svc.OnPost(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	posts := f.mm.in(channel)
	if len(posts) == before {
		return ""
	}
	return posts[len(posts)-1].msg
}

func (f *reportFixture) latest(t *testing.T) bestpractice.Report {
	t.Helper()
	r, ok := f.store.Latest(time.Now())
	if !ok {
		t.Fatal("no report")
	}
	return r
}

func TestReportCommandsAreForClusterAdmins(t *testing.T) {
	f := newReportFixture(t)
	if got := f.say(t, "tenant@snapp.cab", adminChannel, "report run"); !strings.Contains(got, "for cluster-admins") {
		t.Errorf("tenant got %q", got)
	}
	if got := f.say(t, "admin@snapp.cab", "random", "report run"); !strings.Contains(got, "report channel") {
		t.Errorf("wrong channel got %q", got)
	}
	if _, ok := f.store.Latest(time.Now()); ok {
		t.Error("a refused command must not build a report")
	}
}

func TestReportQuestionGoesToTheAgent(t *testing.T) {
	f := newReportFixture(t)
	if got := f.say(t, "tenant@snapp.cab", "random", "report the failing pods in pay-prod"); got != "agent answer" {
		t.Errorf("a question starting with \"report\" must reach the agent, got %q", got)
	}
}

func TestReportBuildPostsPreview(t *testing.T) {
	f := newReportFixture(t)
	f.say(t, "admin@snapp.cab", adminChannel, "report run")
	r := f.latest(t)
	if r.Status != bestpractice.StatusPending || len(r.Findings) != 4 || len(r.Sendable()) != 3 {
		t.Fatalf("report = %+v", r)
	}
	var root, detail *sentPost
	for _, p := range f.mm.in(adminChannel) {
		p := p
		if strings.Contains(p.msg, "Best-practice report `"+r.ID+"`") {
			root = &p
		}
		if strings.Contains(p.msg, "**Per team**") {
			detail = &p
		}
	}
	if root == nil || root.root != "" {
		t.Fatalf("no root preview post: %+v", f.mm.posts)
	}
	if detail == nil || detail.root != r.RootID || r.RootID == "" {
		t.Fatalf("detail must be threaded under the root (%s): %+v", r.RootID, detail)
	}
	for _, want := range []string{"**Emails**", "cc on every one: cto@snapp.cab",
		"| pay | pay | 2 | 0 | alice@snapp.cab, nobody@snapp.cab |",
		"| other teams | 1 teams in no group | 1 | 0 | tech-ems@snapp.cab |", "No owning team", "orphan"} {
		if !strings.Contains(detail.msg, want) {
			t.Errorf("preview detail missing %q:\n%s", want, detail.msg)
		}
	}
	if len(f.mail.sent) != 0 {
		t.Error("nothing may be sent before approval")
	}
}

func TestReportApproveSendsWhatStillHolds(t *testing.T) {
	f := newReportFixture(t)
	f.say(t, "admin@snapp.cab", adminChannel, "report run")
	id := f.latest(t).ID

	// pay-prod is fixed before approval.
	f.inv.Clusters[0].Audit.PDBs[0].MinAvailable = nil
	f.inv.Clusters[0].Audit.PDBs[0].MaxUnavailable = &bestpractice.IntOrPercent{Value: 1}
	f.inv.Clusters[0].Audit.PDBs[0].DesiredHealthy = 0

	if got := f.say(t, "admin@snapp.cab", adminChannel, "report approve "+id); !strings.Contains(got, "sending 2 emails for 2 teams") {
		t.Fatalf("approve reply = %q", got)
	}
	r := f.latest(t)
	if r.Status != bestpractice.StatusSent || r.DecidedBy != "admin@snapp.cab" || r.Delivery == nil {
		t.Fatalf("report = %+v", r)
	}
	d := r.Delivery
	if d.Resolved != 1 || !slices.Equal(d.Sent, []string{"pay", bestpractice.OthersName}) ||
		!slices.Equal(d.Rejected, []string{"nobody@snapp.cab"}) {
		t.Errorf("delivery = %+v", d)
	}
	pay := f.mail.to("alice@snapp.cab")
	if len(pay) != 1 || !strings.Contains(pay[0].Subject, "1 finding for pay (1 high)") {
		t.Fatalf("pay emails = %+v", pay)
	}
	threaded := false
	for _, p := range f.mm.in(adminChannel) {
		threaded = threaded || (p.root == r.RootID && strings.Contains(p.msg, "Approved by admin@snapp.cab") &&
			strings.Contains(p.msg, "Sent **2** emails") && strings.Contains(p.msg, "nobody@snapp.cab"))
	}
	if !threaded {
		t.Errorf("delivery summary not posted in the report thread: %+v", f.mm.in(adminChannel))
	}

	if got := f.say(t, "admin@snapp.cab", adminChannel, "report approve "+id); !strings.Contains(got, "already sent") {
		t.Errorf("second approve = %q", got)
	}
}

// A group's email goes to its teams' admins, the rest to Others, both with
// the cc, and each carries only its own teams in a PDF.
func TestReportEmailContent(t *testing.T) {
	f := newReportFixture(t)
	f.say(t, "admin@snapp.cab", adminChannel, "report run")
	f.say(t, "admin@snapp.cab", adminChannel, "report approve "+f.latest(t).ID)

	pay, others := f.mail.to("alice@snapp.cab"), f.mail.to("tech-ems@snapp.cab")
	if len(pay) != 1 || len(others) != 1 || len(f.mail.to("bob@snapp.cab")) != 0 {
		t.Fatalf("pay = %d, others = %d, bob = %d emails", len(pay), len(others), len(f.mail.to("bob@snapp.cab")))
	}
	for _, c := range []struct {
		e          mail.Message
		has, hasnt string
		to         []string
	}{
		{pay[0], "pay", "ops", []string{"alice@snapp.cab", "nobody@snapp.cab"}},
		{others[0], "ops", "pay", []string{"tech-ems@snapp.cab"}},
	} {
		if !slices.Equal(c.e.To, c.to) || !slices.Equal(c.e.Cc, []string{"cto@snapp.cab"}) {
			t.Errorf("%s: to %v cc %v", c.has, c.e.To, c.e.Cc)
		}
		if len(c.e.Attachments) != 1 || c.e.Attachments[0].ContentType != "application/pdf" ||
			!strings.HasPrefix(string(c.e.Attachments[0].Data), "%PDF-") ||
			!strings.HasPrefix(c.e.Attachments[0].Name, "snappcloud-best-practice-") {
			t.Errorf("%s: attachments = %+v", c.has, c.e.Attachments)
		}
		for _, part := range []string{c.e.Text, c.e.HTML} {
			if !strings.Contains(part, c.has) || strings.Contains(part, c.hasnt) || strings.Contains(part, "orphan") {
				t.Errorf("%s email must name %s only:\n%s", c.has, c.has, part)
			}
		}
	}
	// Every finding is a drain blocker: offering the ignore annotation would
	// invite something that does not work.
	if strings.Contains(pay[0].Text, bestpractice.AnnotationIgnore) || !strings.Contains(pay[0].Text, "cannot be excluded") {
		t.Errorf("footer:\n%s", pay[0].Text)
	}
}

// An email nobody is configured to receive is not sent, and the summary says so.
func TestReportWithoutRecipientsIsNotSent(t *testing.T) {
	f := newReportFixture(t)
	f.svc.report.Routing = bestpractice.Routing{Groups: [][]string{{"pay"}}}
	f.say(t, "admin@snapp.cab", adminChannel, "report run")
	f.say(t, "admin@snapp.cab", adminChannel, "report approve "+f.latest(t).ID)

	d := f.latest(t).Delivery
	if !slices.Equal(d.Sent, []string{"pay"}) || !slices.Equal(d.Unrouted, []string{bestpractice.OthersName}) {
		t.Errorf("delivery = %+v", d)
	}
	if len(f.mail.sent) != 1 {
		t.Errorf("sent %d emails", len(f.mail.sent))
	}
	var summary string
	for _, p := range f.mm.in(adminChannel) {
		if strings.Contains(p.msg, "Approved by") {
			summary = p.msg
		}
	}
	if !strings.Contains(summary, "nobody to send to: other teams") {
		t.Errorf("summary:\n%s", summary)
	}
}

func TestReportPreviewShowsTheTeamsEmail(t *testing.T) {
	f := newReportFixture(t)
	f.say(t, "admin@snapp.cab", adminChannel, "report run")
	got := f.say(t, "admin@snapp.cab", adminChannel, "report preview "+f.latest(t).ID+" ops")
	for _, want := range []string{"**To:** tech-ems@snapp.cab", "**Cc:** cto@snapp.cab", "**Attached:** snappcloud-best-practice-",
		"(other teams)", "teh-1 / ops-prod", "Rule: pdb-min-available-all (cannot be ignored)"} {
		if !strings.Contains(got, want) {
			t.Errorf("preview missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "pay-prod") {
		t.Errorf("preview of ops shows pay's findings:\n%s", got)
	}
}

func TestReportRejectAndExclude(t *testing.T) {
	f := newReportFixture(t)
	f.say(t, "admin@snapp.cab", adminChannel, "report run")
	id := f.latest(t).ID

	if got := f.say(t, "admin@snapp.cab", adminChannel, "report exclude "+id+" nope"); !strings.Contains(got, "no findings for nope") {
		t.Errorf("unknown team = %q", got)
	}
	if got := f.say(t, "admin@snapp.cab", adminChannel, "report exclude "+id+" ops"); !strings.Contains(got, "goes to 1 teams") {
		t.Errorf("exclude = %q", got)
	}
	if got := f.say(t, "admin@snapp.cab", adminChannel, "report preview "+id+" ops"); !strings.Contains(got, "excluded") {
		t.Errorf("preview of an excluded team = %q", got)
	}
	if got := f.say(t, "admin@snapp.cab", adminChannel, "report reject "+id); !strings.Contains(got, "Rejected") {
		t.Errorf("reject = %q", got)
	}
	if got := f.say(t, "admin@snapp.cab", adminChannel, "report approve "+id); !strings.Contains(got, "rejected by admin@snapp.cab") {
		t.Errorf("approve after reject = %q", got)
	}
	if got := f.say(t, "admin@snapp.cab", adminChannel, "report status"); !strings.Contains(got, "rejected") {
		t.Errorf("status = %q", got)
	}
}

// A cluster the approval cannot scan again is named in the summary, and the
// summary posts even when the job's time ran out.
func TestReportSummaryNamesRecheckFailuresAndSurvivesTimeout(t *testing.T) {
	f := newReportFixture(t)
	f.say(t, "admin@snapp.cab", adminChannel, "report run")
	id := f.latest(t).ID

	f.svc.report.Collect = func(context.Context) bestpractice.Inventory {
		return bestpractice.Inventory{Failed: map[string]string{"teh-1": "disruption_audit: context deadline exceeded"}}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.svc.deliverReport(ctx, id, "admin@snapp.cab"); err != nil {
		t.Fatal(err)
	}
	r := f.latest(t)
	if !r.Delivery.Interrupted || r.Delivery.RecheckFailed["teh-1"] == "" {
		t.Errorf("delivery = %+v", r.Delivery)
	}
	var summary string
	for _, p := range f.mm.in(adminChannel) {
		if p.root == r.RootID && strings.Contains(p.msg, "Approved by") {
			summary = p.msg
		}
	}
	for _, want := range []string{"Re-check failed", "teh-1: disruption_audit: context deadline exceeded", "interrupted"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q:\n%s", want, summary)
		}
	}
}

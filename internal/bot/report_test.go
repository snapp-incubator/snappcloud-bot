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

func (m *fakeMailer) Send(_ context.Context, msg mail.Message) error {
	if strings.HasPrefix(msg.To, "nobody@") {
		return fmt.Errorf("%w: 550 no such user", mail.ErrRejected)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, msg)
	return nil
}
func (m *fakeMailer) CanReply() bool { return true }

func (m *fakeMailer) to(addr string) []mail.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []mail.Message
	for _, msg := range m.sent {
		if msg.To == addr {
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

// accessFake checks recipients against a scope table, for the named regions
// only, and records what it was asked. A missing identity, or a region in
// down, fails.
type accessFake struct {
	scopes scopeResolver
	down   map[string]bool
	delay  time.Duration

	mu        sync.Mutex
	asked     map[string][]string
	cur, peak int
}

func (a *accessFake) ResolveRegions(_ context.Context, user string, regions []string) (authzclient.Scope, map[string]error) {
	a.mu.Lock()
	a.cur++
	a.peak = max(a.peak, a.cur)
	a.mu.Unlock()
	time.Sleep(a.delay)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cur--
	if a.asked == nil {
		a.asked = map[string][]string{}
	}
	a.asked[user] = append(a.asked[user], regions...)
	s, ok := a.scopes[user]
	scope, failed := authzclient.Scope{}, map[string]error{}
	for _, region := range regions {
		switch cs, has := s[region]; {
		case !ok || a.down[region]:
			failed[region] = errors.New("mcp-authz unreachable")
		case has:
			scope[region] = cs
		}
	}
	return scope, failed
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
	svc    *Service
	mm     *reportMM
	mail   *fakeMailer
	access *accessFake
	store  *bestpractice.Store
	inv    bestpractice.Inventory
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
		// alice cannot see pay-secret: that finding must not reach her.
		"alice@snapp.cab": {"teh-1": {Namespaces: []string{"pay-prod"}}},
		// bob is missing: his access check fails, so he gets nothing.
	}
	f.access = &accessFake{scopes: resolver}
	f.svc = New(f.mm, &fakeBrain{answer: "agent answer"}, resolver, Options{
		ConversationTTL: time.Hour, BotUsername: "snappbot", RequireMention: true,
		Reports: &ReportOptions{
			Store:        f.store,
			Collect:      func(context.Context) bestpractice.Inventory { return f.inv },
			AdminChannel: adminChannel,
			Messenger:    f.mm,
			Mailer:       f.mail,
			Access:       f.access,
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
	for _, want := range []string{"No owning team", "orphan", "nobody@snapp.cab"} {
		if !strings.Contains(detail.msg, want) {
			t.Errorf("preview detail missing %q:\n%s", want, detail.msg)
		}
	}
	if len(f.mail.sent) != 0 {
		t.Error("nothing may be sent before approval")
	}
}

func TestReportApproveSendsOnlyWhatEachAdminCanSee(t *testing.T) {
	f := newReportFixture(t)
	f.say(t, "admin@snapp.cab", adminChannel, "report run")
	id := f.latest(t).ID

	// pay-prod is fixed before approval; a new problem appears meanwhile.
	f.inv.Clusters[0].Audit.PDBs[0].MinAvailable = nil
	f.inv.Clusters[0].Audit.PDBs[0].MaxUnavailable = &bestpractice.IntOrPercent{Value: 1}
	f.inv.Clusters[0].Audit.PDBs[0].DesiredHealthy = 0

	if got := f.say(t, "admin@snapp.cab", adminChannel, "report approve "+id); !strings.Contains(got, "Approved") {
		t.Fatalf("approve reply = %q", got)
	}
	r := f.latest(t)
	if r.Status != bestpractice.StatusSent || r.DecidedBy != "admin@snapp.cab" || r.Delivery == nil {
		t.Fatalf("report = %+v", r)
	}
	d := r.Delivery
	// pay: api resolved, vault still there but alice cannot see pay-secret.
	if d.Resolved != 1 || len(d.NothingVisible) != 1 || d.NothingVisible[0] != "alice@snapp.cab" || d.ScopeDropped != 1 {
		t.Errorf("delivery = %+v", d)
	}
	if len(f.mail.to("alice@snapp.cab")) != 0 {
		t.Error("alice must receive nothing: her only remaining finding is in a namespace she cannot access")
	}
	// bob's access check fails: fail-closed. nobody's too, so the mail
	// server is never asked about the address.
	if len(d.AuthzFailed) != 2 || len(f.mail.to("bob@snapp.cab")) != 0 || len(d.Rejected) != 0 {
		t.Errorf("delivery = %+v, bob emails = %d", d, len(f.mail.to("bob@snapp.cab")))
	}
	threaded := false
	for _, p := range f.mm.in(adminChannel) {
		threaded = threaded || (p.root == r.RootID && strings.Contains(p.msg, "Approved by admin@snapp.cab"))
	}
	if !threaded {
		t.Errorf("delivery summary not posted in the report thread: %+v", f.mm.in(adminChannel))
	}

	if got := f.say(t, "admin@snapp.cab", adminChannel, "report approve "+id); !strings.Contains(got, "already sent") {
		t.Errorf("second approve = %q", got)
	}
}

func TestReportEmailContent(t *testing.T) {
	f := newReportFixture(t)
	f.access.scopes = scopeResolver{
		"alice@snapp.cab":  {"teh-1": {Namespaces: []string{"pay-prod", "pay-secret"}}},
		"bob@snapp.cab":    {"teh-1": {Namespaces: []string{"ops-prod"}}},
		"nobody@snapp.cab": {"teh-1": {Namespaces: []string{"pay-prod"}}},
	}
	f.say(t, "admin@snapp.cab", adminChannel, "report run")
	id := f.latest(t).ID
	f.say(t, "admin@snapp.cab", adminChannel, "report approve "+id)

	got := f.mail.to("alice@snapp.cab")
	if len(got) != 1 {
		t.Fatalf("alice emails = %d", len(got))
	}
	e := got[0]
	if !strings.Contains(e.Subject, "2 findings for pay (2 high)") {
		t.Errorf("subject = %q", e.Subject)
	}
	for _, part := range []struct{ name, body string }{{"text", e.Text}, {"html", e.HTML}} {
		for _, want := range []string{"Team ", "pay", "api", "vault", "pdb-min-available-all", bestpractice.AnnotationIgnore,
			"reply to this email"} {
			if !strings.Contains(part.body, want) {
				t.Errorf("%s part missing %q:\n%s", part.name, want, part.body)
			}
		}
		if strings.Contains(part.body, "ops-prod") || strings.Contains(part.body, "orphan") {
			t.Errorf("%s part leaks another team's findings:\n%s", part.name, part.body)
		}
	}
	if !strings.Contains(e.HTML, "PDB <code>api</code>") {
		t.Errorf("html does not render code spans:\n%s", e.HTML)
	}
	d := f.latest(t).Delivery
	if len(d.Sent) != 2 || len(d.Rejected) != 1 || d.Rejected[0] != "nobody@snapp.cab" {
		t.Errorf("delivery = %+v", d)
	}
	var summary string
	for _, p := range f.mm.in(adminChannel) {
		if strings.Contains(p.msg, "Emailed **2**") {
			summary = p.msg
		}
	}
	if !strings.Contains(summary, "Address rejected by the mail server") || !strings.Contains(summary, "nobody@snapp.cab") {
		t.Errorf("summary must name the rejected address:\n%s", summary)
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

// Each recipient is checked only on the clusters holding their findings, and
// a cluster that cannot be checked withholds only its own findings.
func TestReportChecksOnlyTheRecipientsClusters(t *testing.T) {
	f := newReportFixture(t)
	one := 1
	f.inv.Clusters = append(f.inv.Clusters, bestpractice.ClusterInventory{
		Cluster: "teh-2",
		Audit: bestpractice.Audit{
			PDBs: []bestpractice.PDB{{Namespace: "pay-prod", Name: "worker", MinAvailable: &bestpractice.IntOrPercent{Value: 1},
				ExpectedPods: 1, CurrentHealthy: 1, DesiredHealthy: 1,
				Owners: []bestpractice.Owner{{Kind: "Deployment", Name: "worker", Replicas: &one}}}},
			Namespaces: map[string]bestpractice.NSOwner{"pay-prod": {Team: "pay"}},
		},
		Teams: []bestpractice.Team{{Team: "pay", Admins: []string{"alice@snapp.cab"}}},
	})
	f.access.scopes = scopeResolver{
		"alice@snapp.cab": {"teh-1": {Namespaces: []string{"pay-prod"}}, "teh-2": {Namespaces: []string{"pay-prod"}}},
		"bob@snapp.cab":   {"teh-1": {Namespaces: []string{"ops-prod"}}},
	}
	f.access.down = map[string]bool{"teh-2": true}
	f.say(t, "admin@snapp.cab", adminChannel, "report run")
	f.say(t, "admin@snapp.cab", adminChannel, "report approve "+f.latest(t).ID)

	if got := f.access.asked["bob@snapp.cab"]; !slices.Equal(got, []string{"teh-1"}) {
		t.Errorf("bob's findings are all on teh-1, yet asked %v", got)
	}
	if got := f.access.asked["alice@snapp.cab"]; !slices.Equal(got, []string{"teh-1", "teh-2"}) {
		t.Errorf("alice asked %v", got)
	}
	got := f.mail.to("alice@snapp.cab")
	if len(got) != 1 || !strings.Contains(got[0].Text, "teh-1 / pay-prod") || strings.Contains(got[0].Text, "teh-2") {
		t.Fatalf("alice must get teh-1's finding and not teh-2's, whose check failed: %+v", got)
	}
	// Unchecked: alice's teh-2 finding, and all 3 of pay's for nobody@, whose
	// identity mcp-authz does not know.
	d := f.latest(t).Delivery
	if d.Unchecked != 4 || !slices.Contains(d.AuthzFailed, "alice@snapp.cab") || !slices.Contains(d.Sent, "alice@snapp.cab") {
		t.Errorf("delivery = %+v", d)
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

// Recipients are checked several at once: one slow mcp-authz answer per
// admin, one after another, is what made an approval take minutes.
func TestReportChecksRecipientsConcurrently(t *testing.T) {
	f := newReportFixture(t)
	f.access.delay = 50 * time.Millisecond
	f.say(t, "admin@snapp.cab", adminChannel, "report run")
	f.say(t, "admin@snapp.cab", adminChannel, "report approve "+f.latest(t).ID)
	if f.access.peak < 2 {
		t.Errorf("recipients checked one at a time (peak %d)", f.access.peak)
	}
	if f.access.peak > accessConcurrency {
		t.Errorf("peak %d exceeds the bound %d", f.access.peak, accessConcurrency)
	}
}

package bot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/snapp-incubator/snappcloud-bot/internal/authzclient"
	"github.com/snapp-incubator/snappcloud-bot/internal/bestpractice"
	"github.com/snapp-incubator/snappcloud-bot/internal/mattermost"
)

const adminChannel = "admins"

type sentPost struct{ channel, root, msg string }

// reportMM is Mattermost for the report: every post with its channel, a user
// directory by email, and DM channels named after their user.
type reportMM struct {
	mu      sync.Mutex
	email   string // the sender of command posts
	posts   []sentPost
	users   map[string]string // email -> user id
	nextRef int
}

func (m *reportMM) GetUser(context.Context, string) (mattermost.User, error) {
	return mattermost.User{Email: m.email}, nil
}
func (m *reportMM) CreatePost(ctx context.Context, ch, msg, root string) error {
	_, err := m.CreatePostID(ctx, ch, msg, root)
	return err
}
func (m *reportMM) CreatePostID(_ context.Context, ch, msg, root string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.posts = append(m.posts, sentPost{ch, root, msg})
	m.nextRef++
	return fmt.Sprintf("post-%d", m.nextRef), nil
}
func (m *reportMM) Typing(context.Context, string, string) {}
func (m *reportMM) UserByEmail(_ context.Context, email string) (mattermost.User, error) {
	id, ok := m.users[email]
	if !ok {
		return mattermost.User{}, fmt.Errorf("%w: no such user", mattermost.ErrNotFound)
	}
	return mattermost.User{ID: id, Email: email}, nil
}
func (m *reportMM) DirectChannel(_ context.Context, _, user string) (string, error) {
	return "dm-" + user, nil
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
	store *bestpractice.Store
	inv   bestpractice.Inventory
}

func newReportFixture(t *testing.T) *reportFixture {
	t.Helper()
	f := &reportFixture{
		mm:    &reportMM{email: "admin@snapp.cab", users: map[string]string{"alice@snapp.cab": "alice", "bob@snapp.cab": "bob"}},
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
	f.svc = New(f.mm, &fakeBrain{answer: "agent answer"}, resolver, Options{
		ConversationTTL: time.Hour, BotUsername: "snappbot", RequireMention: true,
		Reports: &ReportOptions{
			Store:        f.store,
			Collect:      func(context.Context) bestpractice.Inventory { return f.inv },
			AdminChannel: adminChannel,
			BotUserID:    "bot",
			Messenger:    f.mm,
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
	for _, want := range []string{"No owning team", "orphan", "No Mattermost account", "nobody@snapp.cab"} {
		if !strings.Contains(detail.msg, want) {
			t.Errorf("preview detail missing %q:\n%s", want, detail.msg)
		}
	}
	if len(f.mm.in("dm-alice")) != 0 {
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
	if len(f.mm.in("dm-alice")) != 0 {
		t.Error("alice must receive nothing: her only remaining finding is in a namespace she cannot access")
	}
	// bob's access check fails: fail-closed. nobody has no account, but has
	// nothing visible either (no scope), so is never looked up.
	if len(d.AuthzFailed) != 2 || len(f.mm.in("dm-bob")) != 0 {
		t.Errorf("authzFailed = %v, bob DMs = %d", d.AuthzFailed, len(f.mm.in("dm-bob")))
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

func TestReportDMContent(t *testing.T) {
	f := newReportFixture(t)
	f.svc.resolver = scopeResolver{
		"admin@snapp.cab":  {"teh-1": {ClusterWide: true}},
		"alice@snapp.cab":  {"teh-1": {Namespaces: []string{"pay-prod", "pay-secret"}}},
		"bob@snapp.cab":    {"teh-1": {Namespaces: []string{"ops-prod"}}},
		"nobody@snapp.cab": {"teh-1": {Namespaces: []string{"pay-prod"}}},
	}
	f.say(t, "admin@snapp.cab", adminChannel, "report run")
	id := f.latest(t).ID
	f.say(t, "admin@snapp.cab", adminChannel, "report approve "+id)

	dm := f.mm.in("dm-alice")
	if len(dm) != 1 {
		t.Fatalf("alice DMs = %d", len(dm))
	}
	for _, want := range []string{"Team `pay`", "PDB `api`", "PDB `vault`", "pdb-min-available-all", bestpractice.AnnotationIgnore} {
		if !strings.Contains(dm[0].msg, want) {
			t.Errorf("DM missing %q:\n%s", want, dm[0].msg)
		}
	}
	if strings.Contains(dm[0].msg, "ops-prod") || strings.Contains(dm[0].msg, "orphan") {
		t.Errorf("DM leaks another team's findings:\n%s", dm[0].msg)
	}
	d := f.latest(t).Delivery
	if len(d.Sent) != 2 || len(d.NoAccount) != 1 || d.NoAccount[0] != "nobody@snapp.cab" {
		t.Errorf("delivery = %+v", d)
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

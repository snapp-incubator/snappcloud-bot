package bot

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/snapp-incubator/snappcloud-bot/internal/authzclient"
	"github.com/snapp-incubator/snappcloud-bot/internal/dify"
	"github.com/snapp-incubator/snappcloud-bot/internal/mattermost"
)

type fakeMM struct {
	email    string
	posted   []string
	lastRoot string
}

func (f *fakeMM) GetUser(_ context.Context, _ string) (mattermost.User, error) {
	return mattermost.User{Email: f.email}, nil
}
func (f *fakeMM) CreatePost(_ context.Context, _, msg, rootID string) error {
	f.posted = append(f.posted, msg)
	f.lastRoot = rootID
	return nil
}
func (f *fakeMM) Typing(_ context.Context, _, _ string) {}

type fakeDify struct {
	called   bool
	gotNS    any
	gotQuery string
	gotConv  string
	answer   string
}

func (f *fakeDify) Chat(_ context.Context, _, query, conversationID string, inputs map[string]any) (dify.Reply, error) {
	f.called = true
	f.gotNS = inputs["allowed_namespaces"]
	f.gotQuery = query
	f.gotConv = conversationID
	return dify.Reply{Answer: f.answer, ConversationID: "conv-1"}, nil
}

type fakeResolver struct {
	scope authzclient.Scope
	err   error
}

func (f *fakeResolver) Resolve(_ context.Context, _ string) (authzclient.Scope, error) {
	return f.scope, f.err
}

func newSvc(mm *fakeMM, d *fakeDify, r *fakeResolver) *Service {
	return New(mm, d, r, Options{
		ConversationTTL: time.Hour,
		BotUsername:     "snappbot",
		RequireMention:  true,
		ScopeSecret:     "test-secret",
		ScopeTokenTTL:   time.Hour,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func post() mattermost.Post {
	return mattermost.Post{UserID: "u1", ChannelID: "c1", Message: "show dropped flows", ChannelType: "D"}
}

func TestUnauthorizedNeverReachesDify(t *testing.T) {
	mm := &fakeMM{email: "nobody@snapp.cab"}
	d := &fakeDify{}
	svc := newSvc(mm, d, &fakeResolver{scope: authzclient.Scope{}})

	if err := svc.OnPost(context.Background(), post()); err != nil {
		t.Fatalf("OnPost: %v", err)
	}
	if d.called {
		t.Fatal("Dify was called for an unauthorized user")
	}
	if len(mm.posted) != 1 || mm.posted[0] != msgUnauthorized {
		t.Fatalf("expected unauthorized reply, got %v", mm.posted)
	}
}

func TestAuthorizedForwardsClusterScopedToDify(t *testing.T) {
	mm := &fakeMM{email: "saman@snapp.cab"}
	d := &fakeDify{answer: "here are the flows"}
	svc := newSvc(mm, d, &fakeResolver{scope: authzclient.Scope{
		"okd4-teh-1": {"team-b", "team-a"},
		"okd4-ts-2":  {"team-c"},
	}})

	if err := svc.OnPost(context.Background(), post()); err != nil {
		t.Fatalf("OnPost: %v", err)
	}
	if !d.called {
		t.Fatal("Dify was not called for an authorized user")
	}
	want := "okd4-teh-1: team-a, team-b\nokd4-ts-2: team-c"
	if d.gotNS != want {
		t.Fatalf("scope not passed correctly:\n got %q\nwant %q", d.gotNS, want)
	}
	if !strings.Contains(d.gotQuery, scopeOpen) || !strings.Contains(d.gotQuery, scopeClose) {
		t.Fatalf("scope token not embedded in query: %q", d.gotQuery)
	}
	if !strings.HasPrefix(d.gotQuery, "show dropped flows") {
		t.Fatalf("user query must lead, token block after: %q", d.gotQuery)
	}
}

func TestChannelMentionRepliesInThread(t *testing.T) {
	mm := &fakeMM{email: "saman@snapp.cab"}
	d := &fakeDify{answer: "ok"}
	svc := newSvc(mm, d, &fakeResolver{scope: authzclient.Scope{"c": {"team-a"}}})

	p := post()
	p.ID = "post123"
	p.ChannelType = "O"
	p.Mentioned = true
	if err := svc.OnPost(context.Background(), p); err != nil {
		t.Fatalf("OnPost: %v", err)
	}
	if mm.lastRoot != "post123" {
		t.Fatalf("channel reply not threaded: root=%q", mm.lastRoot)
	}
}

func TestDirectMessageNotThreaded(t *testing.T) {
	mm := &fakeMM{email: "saman@snapp.cab"}
	d := &fakeDify{answer: "ok"}
	svc := newSvc(mm, d, &fakeResolver{scope: authzclient.Scope{"c": {"team-a"}}})

	p := post() // ChannelType "D"
	p.ID = "post123"
	if err := svc.OnPost(context.Background(), p); err != nil {
		t.Fatalf("OnPost: %v", err)
	}
	if mm.lastRoot != "" {
		t.Fatalf("DM should not be threaded: root=%q", mm.lastRoot)
	}
}

func TestConversationMemoryReusedInThread(t *testing.T) {
	mm := &fakeMM{email: "saman@snapp.cab"}
	d := &fakeDify{answer: "ok"}
	svc := newSvc(mm, d, &fakeResolver{scope: authzclient.Scope{"c": {"team-a"}}})

	p := post() // DM, stable channel id "c1"
	// First message: no prior conversation.
	if err := svc.OnPost(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if d.gotConv != "" {
		t.Fatalf("first call should have no conversation id, got %q", d.gotConv)
	}
	// Second message in the same DM: must continue conv-1 from the first reply.
	if err := svc.OnPost(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if d.gotConv != "conv-1" {
		t.Fatalf("second call should reuse conversation id, got %q", d.gotConv)
	}
}

func TestChannelWithoutMentionIgnored(t *testing.T) {
	mm := &fakeMM{email: "saman@snapp.cab"}
	d := &fakeDify{}
	svc := newSvc(mm, d, &fakeResolver{scope: authzclient.Scope{"c": {"team-a"}}})

	p := post()
	p.ChannelType = "O"
	p.Mentioned = false
	if err := svc.OnPost(context.Background(), p); err != nil {
		t.Fatalf("OnPost: %v", err)
	}
	if d.called || len(mm.posted) != 0 {
		t.Fatal("bot acted on an unmentioned channel message")
	}
}

func TestBackendErrorFailsClosed(t *testing.T) {
	mm := &fakeMM{email: "saman@snapp.cab"}
	d := &fakeDify{}
	svc := newSvc(mm, d, &fakeResolver{err: errors.New("all regions down")})

	if err := svc.OnPost(context.Background(), post()); err != nil {
		t.Fatalf("OnPost: %v", err)
	}
	if d.called {
		t.Fatal("Dify was called despite an authorization backend error")
	}
	if len(mm.posted) != 1 || mm.posted[0] != msgBackendError {
		t.Fatalf("expected backend-error reply, got %v", mm.posted)
	}
}

func TestSplitMessageRespectsLimitAndKeepsAll(t *testing.T) {
	long := strings.Repeat("line of text\n", 5000) // ~60k chars
	parts := splitMessage(long, 16000)
	if len(parts) < 2 {
		t.Fatalf("expected split, got %d", len(parts))
	}
	total := 0
	for i, p := range parts {
		if len([]rune(p)) > 16000 {
			t.Fatalf("part %d over limit: %d runes", i, len([]rune(p)))
		}
		total += len([]rune(p))
	}
	// No truncation notice; nothing dropped (allow for stripped trailing newlines).
	if strings.Contains(strings.Join(parts, ""), "truncated") {
		t.Fatal("user-facing truncation notice must not appear")
	}
	if total < len([]rune(strings.TrimRight(long, "\n")))-len(parts) {
		t.Fatalf("content lost: total=%d", total)
	}
}

func TestSplitMessageShortUnchanged(t *testing.T) {
	if got := splitMessage("hello", 16000); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("short message altered: %v", got)
	}
}

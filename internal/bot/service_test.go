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

type fakeBrain struct {
	called     bool
	gotScope   authzclient.Scope
	gotQuery   string
	gotHistory string
	answer     string
	err        error
}

func (f *fakeBrain) Answer(_ context.Context, scope authzclient.Scope, query, history string) (string, error) {
	f.called = true
	f.gotScope = scope
	f.gotQuery = query
	f.gotHistory = history
	return f.answer, f.err
}

type fakeResolver struct {
	scope authzclient.Scope
	err   error
}

func (f *fakeResolver) Resolve(_ context.Context, _ string) (authzclient.Scope, error) {
	return f.scope, f.err
}

func newSvc(mm *fakeMM, b *fakeBrain, r *fakeResolver) *Service {
	return New(mm, b, r, Options{
		ConversationTTL: time.Hour,
		BotUsername:     "snappbot",
		RequireMention:  true,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func post() mattermost.Post {
	return mattermost.Post{UserID: "u1", ChannelID: "c1", Message: "show dropped flows", ChannelType: "D"}
}

func TestUnauthorizedNeverReachesAgent(t *testing.T) {
	mm := &fakeMM{email: "nobody@snapp.cab"}
	b := &fakeBrain{}
	svc := newSvc(mm, b, &fakeResolver{scope: authzclient.Scope{}})

	if err := svc.OnPost(context.Background(), post()); err != nil {
		t.Fatalf("OnPost: %v", err)
	}
	if b.called {
		t.Fatal("agent was called for an unauthorized user")
	}
	if len(mm.posted) != 1 || mm.posted[0] != msgUnauthorized {
		t.Fatalf("expected unauthorized reply, got %v", mm.posted)
	}
}

func TestAuthorizedPassesScopeToAgent(t *testing.T) {
	mm := &fakeMM{email: "saman@snapp.cab"}
	b := &fakeBrain{answer: "here are the flows"}
	svc := newSvc(mm, b, &fakeResolver{scope: authzclient.Scope{
		"okd4-teh-1": {Namespaces: []string{"team-b", "team-a"}},
		"okd4-ts-2":  {Namespaces: []string{"team-c"}},
	}})

	if err := svc.OnPost(context.Background(), post()); err != nil {
		t.Fatalf("OnPost: %v", err)
	}
	if !b.called {
		t.Fatal("agent was not called for an authorized user")
	}
	if len(b.gotScope["okd4-teh-1"].Namespaces) != 2 || len(b.gotScope["okd4-ts-2"].Namespaces) != 1 {
		t.Fatalf("scope not passed correctly: %v", b.gotScope)
	}
	if len(mm.posted) != 1 || mm.posted[0] != "here are the flows" {
		t.Fatalf("answer not posted: %v", mm.posted)
	}
}

func TestChannelMentionRepliesInThread(t *testing.T) {
	mm := &fakeMM{email: "saman@snapp.cab"}
	b := &fakeBrain{answer: "ok"}
	svc := newSvc(mm, b, &fakeResolver{scope: authzclient.Scope{"c": {Namespaces: []string{"team-a"}}}})

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
	b := &fakeBrain{answer: "ok"}
	svc := newSvc(mm, b, &fakeResolver{scope: authzclient.Scope{"c": {Namespaces: []string{"team-a"}}}})

	p := post() // ChannelType "D"
	p.ID = "post123"
	if err := svc.OnPost(context.Background(), p); err != nil {
		t.Fatalf("OnPost: %v", err)
	}
	if mm.lastRoot != "" {
		t.Fatalf("DM should not be threaded: root=%q", mm.lastRoot)
	}
}

func TestConversationMemoryCarriesTranscript(t *testing.T) {
	mm := &fakeMM{email: "saman@snapp.cab"}
	b := &fakeBrain{answer: "first answer"}
	svc := newSvc(mm, b, &fakeResolver{scope: authzclient.Scope{"c": {Namespaces: []string{"team-a"}}}})

	p := post() // DM, stable channel id "c1"
	if err := svc.OnPost(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if b.gotHistory != "" {
		t.Fatalf("first call should have no history, got %q", b.gotHistory)
	}
	// Second message in the same DM: history must carry the first Q/A.
	b.answer = "second answer"
	if err := svc.OnPost(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.gotHistory, "first answer") || !strings.Contains(b.gotHistory, "show dropped flows") {
		t.Fatalf("second call missing prior transcript: %q", b.gotHistory)
	}
}

func TestChannelWithoutMentionIgnored(t *testing.T) {
	mm := &fakeMM{email: "saman@snapp.cab"}
	b := &fakeBrain{}
	svc := newSvc(mm, b, &fakeResolver{scope: authzclient.Scope{"c": {Namespaces: []string{"team-a"}}}})

	p := post()
	p.ChannelType = "O"
	p.Mentioned = false
	if err := svc.OnPost(context.Background(), p); err != nil {
		t.Fatalf("OnPost: %v", err)
	}
	if b.called || len(mm.posted) != 0 {
		t.Fatal("bot acted on an unmentioned channel message")
	}
}

func TestBackendErrorFailsClosed(t *testing.T) {
	mm := &fakeMM{email: "saman@snapp.cab"}
	b := &fakeBrain{}
	svc := newSvc(mm, b, &fakeResolver{err: errors.New("all regions down")})

	if err := svc.OnPost(context.Background(), post()); err != nil {
		t.Fatalf("OnPost: %v", err)
	}
	if b.called {
		t.Fatal("agent was called despite an authorization backend error")
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

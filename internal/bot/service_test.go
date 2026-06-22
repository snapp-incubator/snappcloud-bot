package bot

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/snapp-incubator/snappcloud-bot/internal/authzclient"
	"github.com/snapp-incubator/snappcloud-bot/internal/mattermost"
)

type fakeMM struct {
	email  string
	posted []string
}

func (f *fakeMM) GetUser(_ context.Context, _ string) (mattermost.User, error) {
	return mattermost.User{Email: f.email}, nil
}
func (f *fakeMM) CreatePost(_ context.Context, _, msg string) error {
	f.posted = append(f.posted, msg)
	return nil
}

type fakeDify struct {
	called bool
	gotNS  any
	answer string
}

func (f *fakeDify) Chat(_ context.Context, _, _ string, inputs map[string]any) (string, error) {
	f.called = true
	f.gotNS = inputs["allowed_namespaces"]
	return f.answer, nil
}

type fakeResolver struct {
	scope authzclient.Scope
	err   error
}

func (f *fakeResolver) Resolve(_ context.Context, _ string) (authzclient.Scope, error) {
	return f.scope, f.err
}

func newSvc(mm *fakeMM, d *fakeDify, r *fakeResolver) *Service {
	return New(mm, d, r, nil, "snappbot", true, slog.New(slog.NewTextHandler(io.Discard, nil)))
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

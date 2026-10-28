package bot

import (
	"context"
	"strings"
	"testing"

	"github.com/snapp-incubator/snappcloud-bot/internal/agent"
	"github.com/snapp-incubator/snappcloud-bot/internal/authzclient"
)

// proposingBrain proposes a debug pod on its first answer and records what
// the next turn was granted.
type proposingBrain struct {
	fakeBrain
	allowed bool
}

func (b *proposingBrain) Answer(ctx context.Context, scope authzclient.Scope, user, query, history, id string) (string, error) {
	p := agent.Proposal{Tool: "debug_pod_start", Cluster: "c", Namespace: "team-a"}
	ap := agent.ApprovalFrom(ctx)
	if b.calls == 0 {
		ap.Propose(p)
	} else {
		b.allowed = ap.Allows(p)
	}
	return b.fakeBrain.Answer(ctx, scope, user, query, history, id)
}

func TestYesApprovesTheThreadsProposal(t *testing.T) {
	mm := &fakeMM{email: "saman@snapp.cab"}
	b := &proposingBrain{fakeBrain: fakeBrain{answer: "Reply yes to start a debug pod in team-a."}}
	svc := New(mm, b, &fakeResolver{scope: authzclient.Scope{"c": {Namespaces: []string{"team-a"}}}},
		Options{}, newSvc(mm, &b.fakeBrain, nil).log)
	ctx := context.Background()
	if err := svc.OnPost(ctx, post()); err != nil {
		t.Fatal(err)
	}
	reply := post()
	reply.Message = "Yes!"
	if err := svc.OnPost(ctx, reply); err != nil {
		t.Fatal(err)
	}
	if !b.allowed {
		t.Fatal("the approving reply did not grant the proposal")
	}
	if !strings.Contains(b.gotQuery, "debug_pod_start on cluster c, namespace team-a") {
		t.Fatalf("approved turn was asked %q", b.gotQuery)
	}
	// Single use: a second yes is just a message.
	if err := svc.OnPost(ctx, reply); err != nil {
		t.Fatal(err)
	}
	if b.allowed || b.gotQuery != "Yes!" {
		t.Fatalf("approval was reused: allowed=%v query=%q", b.allowed, b.gotQuery)
	}
}

func TestNoDeclinesTheProposal(t *testing.T) {
	mm := &fakeMM{email: "saman@snapp.cab"}
	b := &proposingBrain{fakeBrain: fakeBrain{answer: "Reply yes to start a debug pod."}}
	svc := New(mm, b, &fakeResolver{scope: authzclient.Scope{"c": {Namespaces: []string{"team-a"}}}},
		Options{}, newSvc(mm, &b.fakeBrain, nil).log)
	_ = svc.OnPost(context.Background(), post())
	reply := post()
	reply.Message = "no"
	_ = svc.OnPost(context.Background(), reply)
	if b.calls != 1 || mm.posted[len(mm.posted)-1] != "OK, I won't run it." {
		t.Fatalf("calls=%d posted=%v", b.calls, mm.posted)
	}
}

func TestOnlyABareReplyApproves(t *testing.T) {
	for text, want := range map[string]bool{"yes": true, " OK. ": true, "go ahead": true, "بله": true,
		"yes, but check the service first": false, "what would it run?": false} {
		if got, _ := replyKind(text); got != want {
			t.Errorf("%q: approve=%v", text, got)
		}
	}
}

package bot

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/snapp-incubator/snappcloud-bot/internal/alerts"
	"github.com/snapp-incubator/snappcloud-bot/internal/authzclient"
	"github.com/snapp-incubator/snappcloud-bot/internal/mattermost"
)

func alertSvc(mm *fakeMM, b *fakeBrain, r *fakeResolver) (*Service, *alerts.Channels, *alerts.Aggregator) {
	ch := alerts.NewChannels("")
	agg := alerts.NewAggregator(alerts.Limits{Window: time.Minute})
	svc := New(mm, b, r, Options{
		ConversationTTL: time.Hour,
		BotUsername:     "snappbot",
		RequireMention:  true,
		AlertChannels:   ch,
		AlertAggregator: agg,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return svc, ch, agg
}

const alertPost = `CiliumHighBpfMapPressure
⚠️ Severity: 🔴 critical
🌍 Region: okd4-teh-1
🖥 Instance: cilium-d88nh
📋 Summary: BPF map ct_any4_global on node okd4-worker-ls-13 is over 95% full`

// A webhook post in a marked channel is an alert: no mention needed, and it is
// buffered rather than answered inline.
func TestWebhookPostInMarkedChannelIsIngested(t *testing.T) {
	mm := &fakeMM{email: ""} // a webhook account: no email, so no SSO identity
	b := &fakeBrain{answer: "ok"}
	svc, ch, agg := alertSvc(mm, b, &fakeResolver{scope: authzclient.Scope{"c": {Namespaces: []string{"team-a"}}}})
	ch.Mark("c1", "", "sre@snapp.cab")

	p := mattermost.Post{UserID: "hook", ChannelID: "c1", Message: alertPost, ChannelType: "O"}
	if err := svc.OnPost(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if agg.Pending() != 1 {
		t.Fatalf("alert not buffered: %d pending", agg.Pending())
	}
	if b.called {
		t.Error("the agent ran inline; alerts must be batched, not answered on arrival")
	}
	if len(mm.posted) != 0 {
		t.Errorf("posted before the window closed: %v", mm.posted)
	}
}

// A HUMAN talking in the same channel is not an alert. This is the whole
// discriminator: it holds no matter what shape another team's alerts take.
func TestHumanChatterInMarkedChannelIsNotAnAlert(t *testing.T) {
	mm := &fakeMM{email: "person@snapp.cab"}
	b := &fakeBrain{answer: "ok"}
	svc, ch, agg := alertSvc(mm, b, &fakeResolver{scope: authzclient.Scope{"c": {Namespaces: []string{"team-a"}}}})
	ch.Mark("c1", "", "sre@snapp.cab")

	p := mattermost.Post{UserID: "u1", ChannelID: "c1", Message: "anyone looking at this?", ChannelType: "O"}
	if err := svc.OnPost(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if agg.Pending() != 0 {
		t.Errorf("human chatter was ingested as an alert: %d pending", agg.Pending())
	}
}

// An unmarked channel ignores webhook posts entirely.
func TestUnmarkedChannelIgnoresAlerts(t *testing.T) {
	mm := &fakeMM{email: ""}
	svc, _, agg := alertSvc(mm, &fakeBrain{}, &fakeResolver{scope: authzclient.Scope{"c": {Namespaces: []string{"team-a"}}}})

	p := mattermost.Post{UserID: "hook", ChannelID: "c1", Message: alertPost, ChannelType: "O"}
	_ = svc.OnPost(context.Background(), p)
	if agg.Pending() != 0 {
		t.Errorf("alert ingested from an unmarked channel: %d pending", agg.Pending())
	}
}

// The investigation runs as the MARKING user, with their access resolved now.
func TestInvestigationRunsAsTheMarkingUser(t *testing.T) {
	mm := &fakeMM{email: ""}
	b := &fakeBrain{answer: "the cause is pod X"}
	scope := authzclient.Scope{"okd4-teh-1": {Namespaces: []string{"team-a"}, ClusterWide: true}}
	svc, ch, _ := alertSvc(mm, b, &fakeResolver{scope: scope})
	marked := ch.Mark("c1", "", "sre@snapp.cab")

	a, _ := alerts.Parse(alertPost, time.Now())
	a.ChannelID, a.PostID = "c1", "post-1"
	batch := alerts.Batch{ChannelID: "c1", Investigate: []alerts.Alert{a}}

	if err := svc.Investigate(context.Background(), marked, batch); err != nil {
		t.Fatal(err)
	}
	if b.gotUser != "sre@snapp.cab" {
		t.Errorf("investigation ran as %q, want the marking user", b.gotUser)
	}
	if len(b.gotScope) != 1 {
		t.Errorf("scope not the marking user's: %+v", b.gotScope)
	}
	if !strings.Contains(b.gotQuery, "CiliumHighBpfMapPressure") {
		t.Errorf("the alert was not put to the agent: %q", b.gotQuery)
	}
	if !strings.Contains(b.gotQuery, "root cause") {
		t.Error("the investigation must ask for the cause, not a restatement")
	}
	if len(mm.posted) != 1 || !strings.Contains(mm.posted[0], "the cause is pod X") {
		t.Errorf("finding not posted: %v", mm.posted)
	}
	if mm.lastRoot != "post-1" {
		t.Errorf("reply root = %q, want the alert's own thread", mm.lastRoot)
	}
}

// If the marking user loses access, investigations stop and the channel is told
// — a silent stop in an alert channel is the worst outcome.
func TestOwnerLosingAccessIsReported(t *testing.T) {
	mm := &fakeMM{email: ""}
	b := &fakeBrain{}
	svc, ch, _ := alertSvc(mm, b, &fakeResolver{scope: authzclient.Scope{}})
	marked := ch.Mark("c1", "", "sre@snapp.cab")

	a, _ := alerts.Parse(alertPost, time.Now())
	a.ChannelID = "c1"
	if err := svc.Investigate(context.Background(), marked, alerts.Batch{ChannelID: "c1", Investigate: []alerts.Alert{a}}); err != nil {
		t.Fatal(err)
	}
	if b.called {
		t.Error("investigated with no authorization")
	}
	if len(mm.posted) != 1 || !strings.Contains(mm.posted[0], "no cluster access") {
		t.Errorf("the channel was not told: %v", mm.posted)
	}
}

// Marking states whose access is used and that the channel can see it.
func TestMarkingIsExplicitAboutBorrowedAccess(t *testing.T) {
	mm := &fakeMM{email: "sre@snapp.cab"}
	svc, _, _ := alertSvc(mm, &fakeBrain{}, &fakeResolver{scope: authzclient.Scope{"c": {Namespaces: []string{"team-a"}}}})

	handled, reply := svc.alertCommand("sre@snapp.cab", mattermost.Post{ChannelID: "c1", ChannelType: "O"}, "alerts on")
	if !handled {
		t.Fatal("alerts on not handled")
	}
	for _, want := range []string{"sre@snapp.cab", "everyone in this channel", "alerts off"} {
		if !strings.Contains(reply, want) {
			t.Errorf("marking reply omits %q: %s", want, reply)
		}
	}
}

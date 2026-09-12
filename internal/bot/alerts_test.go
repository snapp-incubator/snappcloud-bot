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

// Alerts that fire together are usually one incident: one investigation, one
// message. Investigating each separately would give several partial answers to
// the same question and post one reply each — the noise the window exists to
// prevent.
func TestBatchIsOneInvestigationAndOneMessage(t *testing.T) {
	mm := &fakeMM{email: ""}
	b := &fakeBrain{answer: "node-3 lost its kubelet; everything else follows from that"}
	scope := authzclient.Scope{"okd4-teh-1": {Namespaces: []string{"team-a"}, ClusterWide: true}}
	svc, ch, _ := alertSvc(mm, b, &fakeResolver{scope: scope})
	marked := ch.Mark("c1", "", "sre@snapp.cab")

	mk := func(name, sev, post string) alerts.Alert {
		a, _ := alerts.Parse(name+"\nseverity: "+sev+"\nnamespace: team-a\ncluster: okd4-teh-1", time.Now())
		a.ChannelID, a.PostID = "c1", post
		return a
	}
	batch := alerts.Batch{
		ChannelID: "c1",
		Investigate: []alerts.Alert{
			mk("NodeNotReady", "critical", "post-1"),
			mk("KubePodPending", "warning", "post-2"),
			mk("TargetDown", "warning", "post-3"),
		},
		Context: []alerts.Alert{mk("KubeletDown", "critical", "post-0")},
	}

	if err := svc.Investigate(context.Background(), marked, batch); err != nil {
		t.Fatal(err)
	}

	if b.calls != 1 {
		t.Errorf("ran %d investigations for one batch, want 1", b.calls)
	}
	if len(mm.posted) != 1 {
		t.Fatalf("posted %d messages for one batch, want 1: %v", len(mm.posted), mm.posted)
	}

	// Every alert must reach the prompt, and the context alert must be marked as
	// context rather than presented as a thing to diagnose.
	for _, want := range []string{"NodeNotReady", "KubePodPending", "TargetDown", "KubeletDown"} {
		if !strings.Contains(b.gotQuery, want) {
			t.Errorf("alert %q missing from the investigation", want)
		}
	}
	if !strings.Contains(b.gotQuery, "one incident") {
		t.Error("the prompt must ask whether these are one incident")
	}
	if !strings.Contains(b.gotQuery, "context only") {
		t.Error("the cooling alert must be marked as context, not as a subject")
	}
	if !strings.Contains(b.gotQuery, "team-a") || !strings.Contains(b.gotQuery, "okd4-teh-1") {
		t.Errorf("scope not carried into the investigation: %q", b.gotQuery)
	}

	// A finding about four alerts does not belong under one of their threads.
	if mm.lastRoot != "" {
		t.Errorf("multi-alert finding posted in thread %q, want a channel post", mm.lastRoot)
	}
	if !strings.Contains(mm.posted[0], "3 alerts fired together") {
		t.Errorf("message does not say what it covers: %s", mm.posted[0])
	}
	if !strings.Contains(mm.posted[0], "KubeletDown") {
		t.Errorf("context alerts not named in the message: %s", mm.posted[0])
	}
}

// A lone alert keeps the nicer behaviour: answered in its own thread.
func TestSingleAlertStillRepliesInItsThread(t *testing.T) {
	mm := &fakeMM{email: ""}
	b := &fakeBrain{answer: "pod X is filling the conntrack map"}
	svc, ch, _ := alertSvc(mm, b, &fakeResolver{scope: authzclient.Scope{"c": {Namespaces: []string{"team-a"}}}})
	marked := ch.Mark("c1", "", "sre@snapp.cab")

	a, _ := alerts.Parse(alertPost, time.Now())
	a.ChannelID, a.PostID = "c1", "post-1"
	if err := svc.Investigate(context.Background(), marked, alerts.Batch{ChannelID: "c1", Investigate: []alerts.Alert{a}}); err != nil {
		t.Fatal(err)
	}
	if mm.lastRoot != "post-1" {
		t.Errorf("single alert answered at %q, want its own thread", mm.lastRoot)
	}
	if strings.Contains(mm.posted[0], "fired together") {
		t.Errorf("single alert described as a batch: %s", mm.posted[0])
	}
}

// The real case: Alertmanager posts through an INCOMING WEBHOOK created by an
// engineer, so the post carries that engineer's user id and a working SSO
// email. Judging by the author would file every alert as "a person talking" and
// ignore it. Mattermost marks the post itself, which is what decides.
func TestIntegrationPostFromARealUsersAccountIsAnAlert(t *testing.T) {
	mm := &fakeMM{email: "mohamad.shirkhodaei@snapp.cab"} // a real, resolvable identity
	b := &fakeBrain{answer: "ok"}
	svc, ch, agg := alertSvc(mm, b, &fakeResolver{scope: authzclient.Scope{"c": {Namespaces: []string{"team-a"}}}})
	ch.Mark("c1", "", "sre@snapp.cab")

	p := mattermost.Post{
		UserID: "u-mohamad", ChannelID: "c1", ChannelType: "O", Message: alertPost,
		Props: map[string]any{
			"from_webhook":      "true",
			"override_username": "Alertmanager",
		},
	}
	if err := svc.OnPost(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if agg.Pending() != 1 {
		t.Fatalf("integration post was not ingested as an alert: %d pending", agg.Pending())
	}
	if b.called {
		t.Error("answered inline instead of batching")
	}
}

// A bot account posting alerts is the same story.
func TestBotAccountPostIsAnAlert(t *testing.T) {
	mm := &fakeMM{email: "someone@snapp.cab"}
	svc, ch, agg := alertSvc(mm, &fakeBrain{}, &fakeResolver{scope: authzclient.Scope{"c": {Namespaces: []string{"team-a"}}}})
	ch.Mark("c1", "", "sre@snapp.cab")

	p := mattermost.Post{
		UserID: "u-bot", ChannelID: "c1", ChannelType: "O", Message: alertPost,
		Props: map[string]any{"from_bot": true}, // real bool, not the webhook's string
	}
	_ = svc.OnPost(context.Background(), p)
	if agg.Pending() != 1 {
		t.Errorf("bot post not ingested: %d pending", agg.Pending())
	}
}

// The same engineer TYPING in that channel must still be treated as a person —
// their messages carry none of those props.
func TestTheIntegrationOwnerTypingIsStillAPerson(t *testing.T) {
	mm := &fakeMM{email: "mohamad.shirkhodaei@snapp.cab"}
	svc, ch, agg := alertSvc(mm, &fakeBrain{}, &fakeResolver{scope: authzclient.Scope{"c": {Namespaces: []string{"team-a"}}}})
	ch.Mark("c1", "", "sre@snapp.cab")

	p := mattermost.Post{UserID: "u-mohamad", ChannelID: "c1", ChannelType: "O",
		Message: "I restarted the agent, watching it now"}
	_ = svc.OnPost(context.Background(), p)
	if agg.Pending() != 0 {
		t.Errorf("a person's message was ingested as an alert: %d pending", agg.Pending())
	}
}

// The marking reply quotes the limits back to the user. It said "skipped for 3"
// because the duration was rendered by trimming "0m" off "30m0s".
func TestMarkingReplyRendersDurationsReadably(t *testing.T) {
	mm := &fakeMM{email: "sre@snapp.cab"}
	ch := alerts.NewChannels("")
	agg := alerts.NewAggregator(alerts.Limits{Window: time.Minute, Cooldown: 30 * time.Minute, MaxPerWindow: 10})
	svc := New(mm, &fakeBrain{}, &fakeResolver{scope: authzclient.Scope{"c": {Namespaces: []string{"team-a"}}}},
		Options{ConversationTTL: time.Hour, BotUsername: "snappbot", RequireMention: true,
			AlertChannels: ch, AlertAggregator: agg},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	_, reply := svc.alertCommand("sre@snapp.cab", mattermost.Post{ChannelID: "c1", ChannelType: "O"}, "alerts on")
	if !strings.Contains(reply, "30m") {
		t.Errorf("cooldown not rendered as 30m: %s", reply)
	}
	if strings.Contains(reply, "for 3.") || strings.Contains(reply, "for 3 ") {
		t.Errorf("duration lost a digit: %s", reply)
	}
	if !strings.Contains(reply, "1m") {
		t.Errorf("window not rendered as 1m: %s", reply)
	}
}

// The status line described the old behaviour ("at most N investigations per
// window") after a window became ONE investigation. Wrong copy about what the
// bot will do is worse than terse copy.
func TestAlertTextsDescribeBatchedBehaviour(t *testing.T) {
	mm := &fakeMM{email: "sre@snapp.cab"}
	ch := alerts.NewChannels("")
	agg := alerts.NewAggregator(alerts.Limits{Window: time.Minute, Cooldown: 30 * time.Minute,
		MaxPerWindow: 10, MinSeverity: "warning"})
	svc := New(mm, &fakeBrain{}, &fakeResolver{scope: authzclient.Scope{"c": {Namespaces: []string{"team-a"}}}},
		Options{ConversationTTL: time.Hour, BotUsername: "snappbot", RequireMention: true,
			AlertChannels: ch, AlertAggregator: agg},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	post := mattermost.Post{ChannelID: "c1", ChannelType: "O"}

	_, on := svc.alertCommand("sre@snapp.cab", post, "alerts on")
	_, status := svc.alertCommand("sre@snapp.cab", post, "alerts status")

	for _, s := range []string{on, status} {
		if strings.Contains(s, "investigations per window") {
			t.Errorf("still claims several investigations per window: %s", s)
		}
		if strings.Contains(s, "0s") || strings.Contains(s, "0m0s") {
			t.Errorf("raw Go duration leaked into user text: %s", s)
		}
	}
	if !strings.Contains(on, "together") || !strings.Contains(on, "one message") {
		t.Errorf("marking reply does not say alerts are investigated together: %s", on)
	}
	if !strings.Contains(status, "One investigation per 1m") {
		t.Errorf("status does not state the batching: %s", status)
	}
	// The limits quoted must be the configured ones, not hard-coded prose.
	for _, want := range []string{"10", "30m", "warning", "Watchdog"} {
		if !strings.Contains(status, want) {
			t.Errorf("status omits %q: %s", want, status)
		}
	}
}

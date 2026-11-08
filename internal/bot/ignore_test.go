package bot

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/snapp-incubator/snappcloud-bot/internal/bestpractice"
)

const (
	redisTarget = "teh-1/pay-prod/Redis/cache"
	redisRule   = bestpractice.RuleRedisSingle
	ignoreRedis = "ignore " + redisTarget + " " + redisRule + " reason: cache only, safe to lose"
)

// ignoreFixture has team pay with one finding, a single-instance Redis, and a
// report that lists it.
func ignoreFixture(t *testing.T) *reportFixture {
	t.Helper()
	f := newReportFixture(t)
	f.inv = bestpractice.Inventory{Clusters: []bestpractice.ClusterInventory{{
		Cluster: "teh-1",
		Audit: bestpractice.Audit{
			Instances: []bestpractice.Instance{{Namespace: "pay-prod", Name: "cache", Engine: "redis", Kind: "Redis",
				Replicas: 1, Source: "operator"}},
			Namespaces: map[string]bestpractice.NSOwner{"pay-prod": {Team: "pay"}},
		},
		Teams: []bestpractice.Team{{Team: "pay", Admins: []string{"alice@snapp.cab"}}},
	}}}
	if err := f.svc.BuildReport(context.Background()); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestIgnoreRecordsWhoAskedAndShowsItEverywhere(t *testing.T) {
	f := ignoreFixture(t)
	reply := f.say(t, "tenant@snapp.cab", "tenant-channel", ignoreRedis)
	for _, want := range []string{"tenant", "The risk you are accepting", "unignore " + redisTarget} {
		if !strings.Contains(reply, want) {
			t.Errorf("reply omits %q:\n%s", want, reply)
		}
	}
	got := f.store.ActiveIgnores(time.Now())
	rec, ok := got[bestpractice.IgnoreKey("teh-1", "pay-prod", "Redis", "cache", redisRule)]
	if !ok || !strings.Contains(rec.By, "tenant") || rec.Reason != "cache only, safe to lose" {
		t.Fatalf("record = %+v", got)
	}
	posts := f.mm.in(adminChannel)
	if audit := posts[len(posts)-1].msg; !strings.Contains(audit, "tenant") || !strings.Contains(audit, "cache only") {
		t.Errorf("admin channel must get a copy: %s", audit)
	}

	// The next report leaves it out of the findings but names it.
	if err := f.svc.BuildReport(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := f.latest(t)
	if len(r.Findings) != 0 || len(r.Ignored) != 1 {
		t.Fatalf("findings %v ignored %v", r.Findings, r.Ignored)
	}
	posts = f.mm.in(adminChannel)
	if detail := posts[len(posts)-1].msg; !strings.Contains(detail, "Ignored on request") || !strings.Contains(detail, "tenant") {
		t.Errorf("preview must name who asked:\n%s", detail)
	}

	// The team has nothing else, and its admins are still told.
	f.say(t, "admin@snapp.cab", adminChannel, "report approve "+r.ID)
	mails := f.mail.to("alice@snapp.cab")
	if len(mails) != 1 || !strings.Contains(mails[0].Subject, "(1 ignored)") {
		t.Fatalf("mails = %+v", mails)
	}

	// Asking again works from the report that lists it as ignored, and renews.
	if reply := f.say(t, "tenant@snapp.cab", "tenant-channel", ignoreRedis); !strings.Contains(reply, "renewed") {
		t.Errorf("renew: %s", reply)
	}

	if reply := f.say(t, "tenant@snapp.cab", "tenant-channel", "ignores"); !strings.Contains(reply, "cache only") {
		t.Errorf("list: %s", reply)
	}
	if reply := f.say(t, "tenant@snapp.cab", "tenant-channel", "unignore "+redisTarget+" "+redisRule); !strings.Contains(reply, "no longer ignored") {
		t.Errorf("unignore: %s", reply)
	}
	if err := f.svc.BuildReport(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := f.latest(t); len(r.Findings) != 1 || len(r.Ignored) != 0 {
		t.Errorf("after unignore: findings %v ignored %v", r.Findings, r.Ignored)
	}
}

func TestIgnoreRefusals(t *testing.T) {
	f := ignoreFixture(t)
	for _, c := range []struct{ name, sender, msg, want string }{
		{"no access", "stranger@snapp.cab", ignoreRedis, "no access"},
		{"other namespace", "tenant@snapp.cab", "ignore teh-1/ops-prod/Redis/cache " + redisRule + " reason: x", "no access"},
		{"no reason", "tenant@snapp.cab", "ignore " + redisTarget + " " + redisRule, "reason is required"},
		{"text but not a reason", "tenant@snapp.cab", "ignore " + redisTarget + " " + redisRule + " because", "reason is required"},
		{"not English", "tenant@snapp.cab", "ignore " + redisTarget + " " + redisRule + " reason: فقط کش است", "in English"},
		{"unknown rule", "tenant@snapp.cab", "ignore " + redisTarget + " nope reason: x", "no rule"},
		{"no rule", "tenant@snapp.cab", "ignore " + redisTarget, "Which rule"},
		{"drain blocker", "tenant@snapp.cab", "ignore " + redisTarget + " " + bestpractice.RuleMinAvailableAll + " reason: x", "cannot be ignored"},
		{"typo in the object", "tenant@snapp.cab", "ignore teh-1/pay-prod/Redis/cach " + redisRule + " reason: x", "No recent report"},
		{"empty part", "tenant@snapp.cab", "ignore teh-1//Redis/cache " + redisRule + " reason: x", "Name the object"},
		{"not ignored", "tenant@snapp.cab", "unignore " + redisTarget + " " + redisRule, "not ignored"},
	} {
		if reply := f.say(t, c.sender, "tenant-channel", c.msg); !strings.Contains(reply, c.want) {
			t.Errorf("%s: reply = %q, want %q", c.name, reply, c.want)
		}
	}
	if got := f.store.ActiveIgnores(time.Now()); len(got) != 0 {
		t.Errorf("a refusal must record nothing: %+v", got)
	}
}

// A name differing only by case is the same object, and the record keeps the
// names the finding has, not the typed ones.
func TestIgnoreMatchesWhateverTheCase(t *testing.T) {
	f := ignoreFixture(t)
	reply := f.say(t, "tenant@snapp.cab", "tenant-channel", "IGNORE TEH-1/pay-prod/redis/CACHE "+strings.ToUpper(redisRule)+" Reason: fine")
	if !strings.Contains(reply, "`"+redisTarget+"`") {
		t.Fatalf("reply: %s", reply)
	}
	if err := f.svc.BuildReport(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := f.latest(t); len(r.Findings) != 0 || len(r.Ignored) != 1 {
		t.Errorf("findings %v ignored %v", r.Findings, r.Ignored)
	}
}

// Questions that start with the word are still questions.
func TestIgnoreQuestionGoesToTheAgent(t *testing.T) {
	f := ignoreFixture(t)
	for _, q := range []string{"ignore the restarts and tell me why pods crash", "ignore kube-system/foo errors please"} {
		if reply := f.say(t, "tenant@snapp.cab", "tenant-channel", q); !strings.Contains(reply, "agent answer") {
			t.Errorf("%q: reply = %q", q, reply)
		}
	}
}

// `ignores` shows only the namespaces the sender can access.
func TestIgnoresListIsScoped(t *testing.T) {
	f := ignoreFixture(t)
	f.say(t, "tenant@snapp.cab", "tenant-channel", ignoreRedis)
	if reply := f.say(t, "stranger@snapp.cab", "tenant-channel", "ignores"); !strings.Contains(reply, "Nothing is ignored") {
		t.Errorf("leaked: %s", reply)
	}
	if reply := f.say(t, "admin@snapp.cab", "tenant-channel", "ignores"); !strings.Contains(reply, "cache only") {
		t.Errorf("a cluster-admin sees it: %s", reply)
	}
}

package bot

import (
	"strings"
	"testing"
)

// The decoration people put around a command must not defeat it.
func TestNormalizeCommandStripsDecoration(t *testing.T) {
	for in, want := range map[string]string{
		"alerts on":            "alerts on",
		"Alerts On.":           "alerts on",
		"alerts on please":     "alerts on",
		"  ALERTS   ON!  ":     "alerts on",
		"alerts off, thanks":   "alerts off",
		"schedules please":     "schedules",
		"help?":                "help",
		"unschedule 7":         "unschedule 7",
		"why is my pod dying?": "why is my pod dying",
	} {
		if got := normalizeCommand(in); got != want {
			t.Errorf("normalizeCommand(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAlertVerbAcceptsHowPeopleActuallyType(t *testing.T) {
	on := []string{"alerts on", "alert on", "alerts enable", "alerts start", "alerts watch", "watch alerts", "mark alert channel"}
	for _, cmd := range on {
		if v, ok := alertVerb(cmd); !ok || v != "on" {
			t.Errorf("%q -> (%q, %v), want on", cmd, v, ok)
		}
	}
	off := []string{"alerts off", "alert off", "alerts disable", "alerts stop", "unwatch alerts"}
	for _, cmd := range off {
		if v, ok := alertVerb(cmd); !ok || v != "off" {
			t.Errorf("%q -> (%q, %v), want off", cmd, v, ok)
		}
	}
	status := []string{"alerts", "alert", "alerts status", "alerts info"}
	for _, cmd := range status {
		if v, ok := alertVerb(cmd); !ok || v != "status" {
			t.Errorf("%q -> (%q, %v), want status", cmd, v, ok)
		}
	}
}

// A QUESTION that happens to mention alerts is a question, not a command. This
// is the line that keeps the command surface from swallowing real queries.
func TestAlertVerbDoesNotSwallowQuestions(t *testing.T) {
	for _, q := range []string{
		"which alerts are firing on teh-1",
		"alerts on teh-1 are firing, why",
		"can you turn alerts on for the sre channel",
		"why did the alert fire",
	} {
		if v, ok := alertVerb(normalizeCommand(q)); ok {
			t.Errorf("question %q was taken as command %q", q, v)
		}
	}
}

// Help must not advertise features that are switched off.
func TestHelpReflectsEnabledFeatures(t *testing.T) {
	full := helpText(true, true)
	for _, want := range []string{"schedule every day", "alerts on", "refresh", "what access do I have?"} {
		if !strings.Contains(full, want) {
			t.Errorf("help omits %q", want)
		}
	}
	bare := helpText(false, false)
	if strings.Contains(bare, "alerts on") || strings.Contains(bare, "schedules") {
		t.Errorf("help advertises disabled features:\n%s", bare)
	}
	if !strings.Contains(bare, "refresh") {
		t.Error("help must always cover access")
	}
}

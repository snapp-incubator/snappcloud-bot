package schedule

import (
	"testing"
	"time"
)

// Wednesday 2026-09-02 10:00 local.
func base() time.Time { return time.Date(2026, 9, 2, 10, 0, 0, 0, time.Local) }

func TestParseInterval(t *testing.T) {
	e, q, err := Parse("every 6h are my pods healthy?", base())
	if err != nil {
		t.Fatal(err)
	}
	if time.Duration(e.Every) != 6*time.Hour {
		t.Fatalf("every = %v", time.Duration(e.Every))
	}
	if q != "are my pods healthy?" {
		t.Fatalf("query = %q", q)
	}
	if !e.Next.Equal(base().Add(6 * time.Hour)) {
		t.Fatalf("next = %v", e.Next)
	}
}

func TestParseDailyAt(t *testing.T) {
	e, q, err := Parse("every day at 09:00 check my quota", base())
	if err != nil {
		t.Fatal(err)
	}
	if time.Duration(e.Every) != 24*time.Hour || e.At != "09:00" {
		t.Fatalf("entry = %+v", e)
	}
	// 09:00 today already passed at 10:00, so it must roll to tomorrow.
	want := time.Date(2026, 9, 3, 9, 0, 0, 0, time.Local)
	if !e.Next.Equal(want) {
		t.Fatalf("next = %v, want %v", e.Next, want)
	}
	if q != "check my quota" {
		t.Fatalf("query = %q", q)
	}
}

func TestParseDailyLaterToday(t *testing.T) {
	e, _, err := Parse("every day at 18:30 anything failing?", base())
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 9, 2, 18, 30, 0, 0, time.Local)
	if !e.Next.Equal(want) {
		t.Fatalf("next = %v, want %v", e.Next, want)
	}
}

func TestParseWeekday(t *testing.T) {
	e, q, err := Parse("every monday at 08:30 weekly report", base())
	if err != nil {
		t.Fatal(err)
	}
	if e.Next.Weekday() != time.Monday || time.Duration(e.Every) != 7*24*time.Hour {
		t.Fatalf("entry = %+v (weekday %v)", e, e.Next.Weekday())
	}
	if !e.Next.After(base()) {
		t.Fatalf("next %v must be in the future", e.Next)
	}
	if q != "weekly report" {
		t.Fatalf("query = %q", q)
	}
}

func TestParseShorthand(t *testing.T) {
	e, q, err := Parse("hourly show dropped flows", base())
	if err != nil {
		t.Fatal(err)
	}
	if time.Duration(e.Every) != time.Hour || q != "show dropped flows" {
		t.Fatalf("entry=%+v q=%q", e, q)
	}
}

func TestParseRejectsGibberish(t *testing.T) {
	for _, in := range []string{"sometimes check pods", "every day at 25:00 x", "every 0h x"} {
		if _, _, err := Parse(in, base()); err == nil {
			t.Errorf("Parse(%q) should fail", in)
		}
	}
}

// Regression: "schedule first run to 16:10" was swallowed into the question and
// asked of the LLM verbatim, and the schedule started 4h out anyway.
func TestParseFirstRunClause(t *testing.T) {
	now := time.Date(2026, 9, 2, 16, 8, 0, 0, time.UTC)
	cases := []string{
		"every 4h schedule first run to 16:10 are any pods restarting in ns?",
		"every 4h starting at 16:10 are any pods restarting in ns?",
		"every 4h are any pods restarting in ns? first run at 16:10",
		"every 4h from 16:10 are any pods restarting in ns?",
	}
	for _, in := range cases {
		e, q, err := Parse(in, now)
		if err != nil {
			t.Fatalf("Parse(%q): %v", in, err)
		}
		if q != "are any pods restarting in ns?" {
			t.Errorf("Parse(%q) query = %q, want the question alone", in, q)
		}
		want := time.Date(2026, 9, 2, 16, 10, 0, 0, time.UTC)
		if !e.Next.Equal(want) {
			t.Errorf("Parse(%q) first run = %s, want %s", in, e.Next, want)
		}
		if time.Duration(e.Every) != 4*time.Hour {
			t.Errorf("Parse(%q) interval = %s, want 4h", in, time.Duration(e.Every))
		}
	}
}

// A first-run time that has already passed today belongs tomorrow, not in the past.
func TestParseFirstRunAlreadyPassed(t *testing.T) {
	now := time.Date(2026, 9, 2, 16, 8, 0, 0, time.UTC)
	e, _, err := Parse("every 6h starting at 09:00 check quota", now)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC)
	if !e.Next.Equal(want) {
		t.Errorf("first run = %s, want %s", e.Next, want)
	}
}

// Without a first-run clause the question must survive untouched.
func TestParseLeavesQuestionAlone(t *testing.T) {
	now := time.Date(2026, 9, 2, 16, 8, 0, 0, time.UTC)
	e, q, err := Parse("every 4h are any pods restarting in snappcloud-tools on teh-1?", now)
	if err != nil {
		t.Fatal(err)
	}
	if q != "are any pods restarting in snappcloud-tools on teh-1?" {
		t.Errorf("query = %q", q)
	}
	if !e.Next.Equal(now.Add(4 * time.Hour)) {
		t.Errorf("first run = %s, want now+4h", e.Next)
	}
}

// Times the user types are read in the schedule zone, not the pod's.
func TestParseUsesGivenLocation(t *testing.T) {
	tehran, err := time.LoadLocation("Asia/Tehran")
	if err != nil {
		t.Skip("no tzdata")
	}
	now := time.Date(2026, 9, 2, 16, 8, 0, 0, tehran)
	e, _, err := Parse("every day at 09:00 check quota", now)
	if err != nil {
		t.Fatal(err)
	}
	h, m, _ := e.Next.In(tehran).Clock()
	if h != 9 || m != 0 {
		t.Errorf("next run at %02d:%02d Tehran, want 09:00", h, m)
	}
	if e.Next.Before(now) {
		t.Errorf("next run %s is in the past", e.Next)
	}
}

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

package bot

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/snapp-incubator/snappcloud-bot/internal/schedule"
)

// "run <id>" must reach only the caller's own schedules: an id that belongs to
// someone else reads as not found, because whether it exists is itself theirs.
func TestRunNowWillNotRunAnotherUsersSchedule(t *testing.T) {
	st := schedule.NewStore("", schedule.Limits{PerUser: 5, Total: 5, MinInterval: time.Minute})
	e, _, err := schedule.Parse("every day at 09:00 is anything failing?", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e.User, e.ChannelID = "owner@x", "c1"
	if err := st.Add(e); err != nil {
		t.Fatal(err)
	}

	var triggered int
	svc := &Service{sched: st, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		trigger: func(schedule.Entry) error { triggered++; return nil }}

	if got := svc.runNow("someone-else@x", e.ID); !strings.Contains(got, "No schedule") {
		t.Errorf("another user got: %s", got)
	}
	if triggered != 0 {
		t.Fatal("another user's schedule was started")
	}
	if got := svc.runNow("owner@x", e.ID); !strings.Contains(got, "Running schedule") {
		t.Errorf("the owner could not run their own: %s", got)
	}
	if triggered != 1 {
		t.Fatalf("owner's run started %d times", triggered)
	}
}

// A second run while the first is still working would post the report twice.
func TestRunNowReportsAnAlreadyRunningSchedule(t *testing.T) {
	st := schedule.NewStore("", schedule.Limits{PerUser: 5, Total: 5, MinInterval: time.Minute})
	e, _, err := schedule.Parse("every day at 09:00 anything failing?", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e.User = "owner@x"
	if err := st.Add(e); err != nil {
		t.Fatal(err)
	}
	svc := &Service{sched: st, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		trigger: func(schedule.Entry) error { return schedule.ErrBusy }}
	if got := svc.runNow("owner@x", e.ID); !strings.Contains(got, "already running") {
		t.Errorf("got: %s", got)
	}
}

package llm

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/snapp-incubator/snappcloud-bot/internal/agent"
)

type fakeModel struct {
	mu    sync.Mutex
	name  string
	fail  bool
	calls int
}

func (f *fakeModel) Complete(context.Context, agent.Request) (agent.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail {
		return agent.Response{}, errors.New(f.name + " down")
	}
	return agent.Response{Text: f.name}, nil
}

func (f *fakeModel) setFail(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = v
}

func (f *fakeModel) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newFO(p, b *fakeModel, threshold int, cooldown time.Duration) agent.LLM {
	return NewFailover(p, b, FailoverOptions{FailureThreshold: threshold, CooldownPeriod: cooldown},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func complete(t *testing.T, m agent.LLM) (string, error) {
	t.Helper()
	r, err := m.Complete(context.Background(), agent.Request{})
	return r.Text, err
}

func TestFailoverUsesPrimaryWhenHealthy(t *testing.T) {
	p, b := &fakeModel{name: "primary"}, &fakeModel{name: "backup"}
	fo := newFO(p, b, 3, time.Minute)
	for i := 0; i < 5; i++ {
		if got, err := complete(t, fo); err != nil || got != "primary" {
			t.Fatalf("got %q err %v", got, err)
		}
	}
	if b.count() != 0 {
		t.Fatalf("backup used while primary healthy: %d calls", b.count())
	}
}

// A failing primary must still answer the CURRENT request from the backup.
func TestFailoverAnswersFromBackupImmediately(t *testing.T) {
	p, b := &fakeModel{name: "primary", fail: true}, &fakeModel{name: "backup"}
	fo := newFO(p, b, 3, time.Minute)
	got, err := complete(t, fo)
	if err != nil || got != "backup" {
		t.Fatalf("got %q err %v — request should be served by the backup", got, err)
	}
}

func TestFailoverOpensAfterThresholdAndStopsCallingPrimary(t *testing.T) {
	p, b := &fakeModel{name: "primary", fail: true}, &fakeModel{name: "backup"}
	fo := newFO(p, b, 3, time.Hour)
	for i := 0; i < 3; i++ { // trip the breaker
		_, _ = complete(t, fo)
	}
	before := p.count()
	for i := 0; i < 4; i++ { // breaker open: primary must not be touched
		if got, _ := complete(t, fo); got != "backup" {
			t.Fatalf("expected backup while open, got %q", got)
		}
	}
	if p.count() != before {
		t.Fatalf("primary called %d times while breaker open", p.count()-before)
	}
}

// A single success resets the counter, so isolated errors never trip it.
func TestFailoverIsolatedErrorsDoNotTrip(t *testing.T) {
	p, b := &fakeModel{name: "primary"}, &fakeModel{name: "backup"}
	fo := newFO(p, b, 3, time.Hour)
	for i := 0; i < 5; i++ {
		p.setFail(true)
		_, _ = complete(t, fo) // one failure
		p.setFail(false)
		if got, _ := complete(t, fo); got != "primary" { // success resets
			t.Fatalf("primary should serve after recovery, got %q", got)
		}
	}
}

func TestFailoverReturnsToPrimaryAfterCooldown(t *testing.T) {
	p, b := &fakeModel{name: "primary", fail: true}, &fakeModel{name: "backup"}
	fo := newFO(p, b, 2, 30*time.Millisecond)
	_, _ = complete(t, fo)
	_, _ = complete(t, fo) // open

	p.setFail(false) // primary recovers
	time.Sleep(50 * time.Millisecond)

	got, err := complete(t, fo) // half-open probe
	if err != nil || got != "primary" {
		t.Fatalf("should fail back to primary after cooldown, got %q err %v", got, err)
	}
	// And stays on the primary.
	if got, _ := complete(t, fo); got != "primary" {
		t.Fatalf("expected primary after recovery, got %q", got)
	}
}

// If the probe fails, the cooldown restarts instead of hammering the primary.
func TestFailoverProbeFailureReopens(t *testing.T) {
	p, b := &fakeModel{name: "primary", fail: true}, &fakeModel{name: "backup"}
	fo := newFO(p, b, 2, 30*time.Millisecond)
	_, _ = complete(t, fo)
	_, _ = complete(t, fo) // open
	time.Sleep(50 * time.Millisecond)
	_, _ = complete(t, fo) // probe fails, reopens
	before := p.count()
	_, _ = complete(t, fo)
	if p.count() != before {
		t.Fatal("primary probed again immediately after a failed probe")
	}
}

func TestFailoverNilBackupReturnsPrimary(t *testing.T) {
	p := &fakeModel{name: "primary"}
	if got := NewFailover(p, nil, FailoverOptions{}, slog.New(slog.NewTextHandler(io.Discard, nil))); got != agent.LLM(p) {
		t.Fatal("nil backup must return the primary unchanged")
	}
}

package alerts

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type blockingInv struct {
	mu      sync.Mutex
	seen    []string
	cur     int
	peak    int
	release chan struct{}
}

func (b *blockingInv) Investigate(_ context.Context, _ Channel, batch Batch) error {
	b.mu.Lock()
	b.seen = append(b.seen, batch.Cluster)
	b.cur++
	if b.cur > b.peak {
		b.peak = b.cur
	}
	b.mu.Unlock()
	<-b.release
	b.mu.Lock()
	b.cur--
	b.mu.Unlock()
	return nil
}

// Three clusters alerting in one window are all investigated, at most
// Concurrency at a time, and the scheduling loop never waits for them.
func TestEveryClusterIsInvestigatedWithoutBlockingTheLoop(t *testing.T) {
	now := time.Now()
	agg := NewAggregator(Limits{Window: time.Minute})
	agg.SetClusterKey(func(al Alert) string { return al.Labels["cluster"] })
	for _, c := range []string{"teh-1", "teh-2", "box"} {
		agg.Add(onCluster("Down", c, now), now)
	}
	chs := NewChannels("")
	chs.Mark("ch", "alerts", "u@x")
	inv := &blockingInv{release: make(chan struct{})}
	r := NewRunner(agg, chs, inv, RunnerOptions{Concurrency: 2, Timeout: time.Minute},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	returned := make(chan struct{})
	go func() { r.runDue(context.Background(), now.Add(2*time.Minute)); close(returned) }()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("runDue blocked on its investigations")
	}
	close(inv.release)
	r.wait()

	if len(inv.seen) != 3 {
		t.Fatalf("investigated %v, want all three clusters", inv.seen)
	}
	if inv.peak > 2 {
		t.Errorf("%d investigations at once with concurrency 2", inv.peak)
	}
}

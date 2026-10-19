package authzclient

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func region(t *testing.T, name string, status int, namespaces []string, hits *atomic.Int32) Region {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"namespaces": namespaces})
	}))
	t.Cleanup(srv.Close)
	return Region{Name: name, URL: srv.URL}
}

// ResolveRegions asks only the regions it is given, and names each one it
// could not check instead of dropping it like Resolve does.
func TestResolveRegionsAsksOnlyTheNamedRegionsAndReportsFailures(t *testing.T) {
	var okHits, downHits, otherHits atomic.Int32
	c := New([]Region{
		region(t, "teh-1", http.StatusOK, []string{"pay-prod", "a-ns"}, &okHits),
		region(t, "ts-3", http.StatusNotFound, nil, &downHits),
		region(t, "teh-2", http.StatusOK, []string{"x"}, &otherHits),
	}, "tok", 5*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))

	scope, failed := c.ResolveRegions(context.Background(), "alice@snapp.cab", []string{"teh-1", "ts-3", "nope"})
	if otherHits.Load() != 0 {
		t.Error("asked a region that was not named")
	}
	if okHits.Load() != 1 || downHits.Load() != 1 {
		t.Errorf("hits teh-1=%d ts-3=%d", okHits.Load(), downHits.Load())
	}
	if got := scope["teh-1"].Namespaces; len(got) != 2 || got[0] != "a-ns" {
		t.Errorf("teh-1 scope = %v (want sorted)", got)
	}
	if len(scope) != 1 || failed["ts-3"] == nil || failed["nope"] == nil || len(failed) != 2 {
		t.Errorf("scope = %v, failed = %v", scope, failed)
	}
}

package bot

import (
	"path/filepath"
	"testing"
	"time"
)

func TestConvStorePersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conv.json")
	s1 := newConvStore(time.Hour, path)
	s1.put("saman@x|thread1", "conv-abc")
	s1.flush()

	// New store (simulated restart) loads the persisted mapping.
	s2 := newConvStore(time.Hour, path)
	if got := s2.get("saman@x|thread1"); got != "conv-abc" {
		t.Fatalf("conversation not restored after restart: %q", got)
	}
}

func TestConvStoreSkipsExpiredOnLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conv.json")
	s1 := newConvStore(time.Millisecond, path)
	s1.put("k", "old")
	s1.flush()
	time.Sleep(5 * time.Millisecond)
	s2 := newConvStore(time.Hour, path)
	if got := s2.get("k"); got != "" {
		t.Fatalf("expired entry should not load: %q", got)
	}
}

package bot

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/snapp-incubator/snappcloud-bot/internal/mattermost"
)

// convStore maps a conversation key to a Dify conversation id so follow-up
// messages continue the same Dify conversation (memory). Keyed per user AND
// per context: a Mattermost thread is one conversation; a direct-message channel
// is one conversation. Including the user keeps each Dify conversation tied to
// its owner (Dify conversations are per-user) even when several users post in
// the same thread.
//
// Persistence: when path is set, the map is loaded on startup and saved
// periodically + on shutdown, so users can continue past conversations across
// bot restarts. The bot is a singleton, so a single file (on a PVC) is enough.
// Without a path it is in-memory only (lost on restart). Entries expire after a
// TTL.
type convStore struct {
	ttl   time.Duration
	path  string
	mu    sync.Mutex
	m     map[string]convEntry
	dirty bool
}

type convEntry struct {
	id      string
	expires time.Time
}

// persisted is the on-disk shape of an entry.
type persisted struct {
	ID      string    `json:"id"`
	Expires time.Time `json:"exp"`
}

func newConvStore(ttl time.Duration, path string) *convStore {
	c := &convStore{ttl: ttl, path: path, m: make(map[string]convEntry)}
	c.load()
	return c
}

// convKeyFor derives the conversation key for a post: per user, and per thread
// (in a channel) or per DM channel.
func convKeyFor(identity string, p mattermost.Post) string {
	ctxID := p.ChannelID // DM: the persistent DM channel
	if !p.IsDirect() {
		ctxID = p.ThreadRoot() // channel: the thread
	}
	return identity + "|" + ctxID
}

// get returns the live conversation id for key, or "" if none/expired.
func (c *convStore) get(key string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[key]; ok && time.Now().Before(e.expires) {
		return e.id
	}
	return ""
}

// put stores (or refreshes) the conversation id for key.
func (c *convStore) put(key, id string) {
	if id == "" {
		return
	}
	c.mu.Lock()
	c.m[key] = convEntry{id: id, expires: time.Now().Add(c.ttl)}
	c.dirty = true
	c.mu.Unlock()
}

// drop forgets a key (e.g. after a stale-conversation error so the next message
// starts fresh).
func (c *convStore) drop(key string) {
	c.mu.Lock()
	delete(c.m, key)
	c.dirty = true
	c.mu.Unlock()
}

// StartSweeper evicts expired entries and flushes the store to disk until ctx is
// cancelled, then does a final flush.
func (c *convStore) StartSweeper(ctx context.Context) {
	if c.ttl <= 0 {
		return
	}
	// Tick often enough to persist recent changes; eviction is cheap.
	interval := 10 * time.Second
	if c.ttl < interval {
		interval = c.ttl
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			c.flush()
			return
		case now := <-ticker.C:
			c.mu.Lock()
			for k, e := range c.m {
				if now.After(e.expires) {
					delete(c.m, k)
					c.dirty = true
				}
			}
			c.mu.Unlock()
			c.flush()
		}
	}
}

// load reads the persisted map, skipping expired entries.
func (c *convStore) load() {
	if c.path == "" {
		return
	}
	data, err := os.ReadFile(c.path)
	if err != nil {
		return // missing/first run
	}
	var raw map[string]persisted
	if json.Unmarshal(data, &raw) != nil {
		return
	}
	now := time.Now()
	for k, e := range raw {
		if now.Before(e.Expires) {
			c.m[k] = convEntry{id: e.ID, expires: e.Expires}
		}
	}
}

// flush atomically writes the map to disk if it changed since the last write.
func (c *convStore) flush() {
	if c.path == "" {
		return
	}
	c.mu.Lock()
	if !c.dirty {
		c.mu.Unlock()
		return
	}
	out := make(map[string]persisted, len(c.m))
	for k, e := range c.m {
		out[k] = persisted{ID: e.id, Expires: e.expires}
	}
	c.dirty = false
	c.mu.Unlock()

	data, err := json.Marshal(out)
	if err != nil {
		return
	}
	tmp := c.path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, c.path) // atomic replace
	}
}

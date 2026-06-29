package bot

import (
	"context"
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
// In-memory only: the bot is a singleton. On restart the mapping is lost and
// conversations start fresh (Dify still holds the history server-side, but the
// bot no longer knows which id to resume). Entries expire after a TTL.
type convStore struct {
	ttl time.Duration
	mu  sync.Mutex
	m   map[string]convEntry
}

type convEntry struct {
	id      string
	expires time.Time
}

func newConvStore(ttl time.Duration) *convStore {
	return &convStore{ttl: ttl, m: make(map[string]convEntry)}
}

// key derives the conversation key for a post: per user, and per thread (in a
// channel) or per DM channel.
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
	c.mu.Unlock()
}

// drop forgets a key (e.g. after a stale-conversation error so the next message
// starts fresh).
func (c *convStore) drop(key string) {
	c.mu.Lock()
	delete(c.m, key)
	c.mu.Unlock()
}

// StartSweeper evicts expired entries until ctx is cancelled.
func (c *convStore) StartSweeper(ctx context.Context) {
	if c.ttl <= 0 {
		return
	}
	ticker := time.NewTicker(c.ttl)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			c.mu.Lock()
			for k, e := range c.m {
				if now.After(e.expires) {
					delete(c.m, k)
				}
			}
			c.mu.Unlock()
		}
	}
}

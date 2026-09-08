package alerts

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Channel is a channel marked for alert investigation.
type Channel struct {
	ID string `json:"id"`
	// Owner is the SSO identity whose authorization every investigation in this
	// channel runs with. It is stored as an IDENTITY, never as a scope: the
	// owner's access is resolved fresh on every batch, so marking a channel
	// cannot outlive the access it was marked with.
	Owner  string    `json:"owner"`
	Name   string    `json:"name,omitempty"`
	Marked time.Time `json:"marked"`
}

// ErrNotMarked means the channel is not an alert channel.
var ErrNotMarked = errors.New("this channel is not marked for alerts")

// Channels tracks marked channels, persisted so a restart does not silently
// stop investigating.
type Channels struct {
	path  string
	mu    sync.Mutex
	m     map[string]Channel
	dirty bool
}

// NewChannels loads persisted marks from path ("" = memory only).
func NewChannels(path string) *Channels {
	c := &Channels{path: path, m: map[string]Channel{}}
	c.load()
	return c
}

// Mark records a channel as an alert channel owned by identity. Re-marking
// transfers ownership, which is how a team hands over who the investigations
// run as.
func (c *Channels) Mark(id, name, identity string) Channel {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := Channel{ID: id, Owner: identity, Name: name, Marked: time.Now()}
	c.m[id] = ch
	c.dirty = true
	return ch
}

// Unmark stops alert investigation in a channel.
func (c *Channels) Unmark(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[id]; !ok {
		return ErrNotMarked
	}
	delete(c.m, id)
	c.dirty = true
	return nil
}

// Get returns the channel's mark.
func (c *Channels) Get(id string) (Channel, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch, ok := c.m[id]
	return ch, ok
}

// List returns every marked channel, oldest first.
func (c *Channels) List() []Channel {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Channel, 0, len(c.m))
	for _, ch := range c.m {
		out = append(out, ch)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Marked.Before(out[j].Marked) })
	return out
}

// Count returns how many channels are marked.
func (c *Channels) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

func (c *Channels) load() {
	if c.path == "" {
		return
	}
	data, err := os.ReadFile(c.path)
	if err != nil {
		return // first run
	}
	var list []Channel
	if json.Unmarshal(data, &list) != nil {
		return
	}
	for _, ch := range list {
		c.m[ch.ID] = ch
	}
}

// Flush writes the marks to disk if they changed.
func (c *Channels) Flush() {
	if c.path == "" {
		return
	}
	c.mu.Lock()
	if !c.dirty {
		c.mu.Unlock()
		return
	}
	list := make([]Channel, 0, len(c.m))
	for _, ch := range c.m {
		list = append(list, ch)
	}
	c.dirty = false
	c.mu.Unlock()

	data, err := json.Marshal(list)
	if err != nil {
		return
	}
	tmp := c.path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(c.path), 0o755)
	_ = os.Rename(tmp, c.path)
}

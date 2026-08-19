package bot

import (
	"sync"
	"time"
)

// rateLimiter is a per-user token bucket. It protects the bot, the LLM budget,
// and the downstream MCP servers/mcp-authz from a single client flooding
// requests — one authenticated user can only start `rate` turns per minute
// (with a small burst). Bounded memory: idle buckets are swept.
type rateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*bucket
	rate     float64 // tokens per second
	burst    float64 // max tokens
	disabled bool
}

type bucket struct {
	tokens float64
	last   time.Time
}

// newRateLimiter builds a limiter allowing `perMin` messages per minute per
// user with burst `burst`. perMin <= 0 disables limiting.
func newRateLimiter(perMin, burst int) *rateLimiter {
	if perMin <= 0 {
		return &rateLimiter{disabled: true}
	}
	if burst <= 0 {
		burst = perMin
	}
	return &rateLimiter{
		buckets: make(map[string]*bucket),
		rate:    float64(perMin) / 60.0,
		burst:   float64(burst),
	}
}

// allow reports whether user may start a request now, consuming one token.
func (r *rateLimiter) allow(user string) bool {
	if r.disabled {
		return true
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.buckets[user]
	if !ok {
		b = &bucket{tokens: r.burst, last: now}
		r.buckets[user] = b
	}
	// Refill.
	b.tokens += now.Sub(b.last).Seconds() * r.rate
	if b.tokens > r.burst {
		b.tokens = r.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweep drops buckets that have fully refilled (idle), keeping memory bounded.
func (r *rateLimiter) sweep() {
	if r.disabled {
		return
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for u, b := range r.buckets {
		if b.tokens+now.Sub(b.last).Seconds()*r.rate >= r.burst {
			delete(r.buckets, u)
		}
	}
}

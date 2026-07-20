package authzclient

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Resolver produces a user's per-cluster scope. Implemented by Client and by
// CachedResolver.
type Resolver interface {
	Resolve(ctx context.Context, user string) (Scope, error)
}

// Invalidator drops a user's cached scope. CachedResolver implements it; the
// bare Client does not (nothing to invalidate).
type Invalidator interface {
	Invalidate(user string)
}

// CachedResolver caches a user's aggregated Scope for a TTL so the bot does not
// re-query every region's mcp-authz API on each chat message.
//
//   - singleflight collapses concurrent misses for one user into a single sweep.
//   - a background sweeper drops expired entries so memory tracks active users.
//   - errors are never cached.
type CachedResolver struct {
	inner Resolver
	ttl   time.Duration

	mu      sync.RWMutex
	entries map[string]scopeEntry
	group   singleflight.Group
}

type scopeEntry struct {
	scope   Scope
	expires time.Time
}

// NewCachedResolver wraps inner with a TTL cache. A non-positive ttl disables
// caching and returns inner unchanged.
func NewCachedResolver(inner Resolver, ttl time.Duration) Resolver {
	if ttl <= 0 {
		return inner
	}
	return &CachedResolver{inner: inner, ttl: ttl, entries: make(map[string]scopeEntry)}
}

// Resolve returns the user's scope, served from cache when fresh.
func (c *CachedResolver) Resolve(ctx context.Context, user string) (Scope, error) {
	c.mu.RLock()
	e, ok := c.entries[user]
	c.mu.RUnlock()
	if ok && time.Now().Before(e.expires) {
		return e.scope, nil
	}

	v, err, _ := c.group.Do(user, func() (any, error) {
		c.mu.RLock()
		e, ok := c.entries[user]
		c.mu.RUnlock()
		if ok && time.Now().Before(e.expires) {
			return e.scope, nil
		}
		scope, err := c.inner.Resolve(ctx, user)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.entries[user] = scopeEntry{scope: scope, expires: time.Now().Add(c.ttl)}
		c.mu.Unlock()
		return scope, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(Scope), nil
}

// Invalidate drops a user's cached scope so the next Resolve re-queries every
// region. Used to pick up an authorization change without waiting out the TTL.
func (c *CachedResolver) Invalidate(user string) {
	c.mu.Lock()
	delete(c.entries, user)
	c.mu.Unlock()
}

// StartSweeper periodically evicts expired entries until ctx is cancelled.
func (c *CachedResolver) StartSweeper(ctx context.Context) {
	ticker := time.NewTicker(c.ttl)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			c.mu.Lock()
			for k, e := range c.entries {
				if now.After(e.expires) {
					delete(c.entries, k)
				}
			}
			c.mu.Unlock()
		}
	}
}

var (
	_ Resolver = (*Client)(nil)
	_ Resolver = (*CachedResolver)(nil)
)

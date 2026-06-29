package quota

import (
	"sync"
	"time"
)

// usageReadEntry is a cached usage value with its expiry.
type usageReadEntry struct {
	cents  int64
	expiry time.Time
}

// usageReadCache is a short-TTL in-process snapshot of per-key Redis usage. It
// removes the per-request MGET on the quota check hot path: a hit returns the
// cached base without Redis. Bounded staleness ≤ ttl (soft quota — accepted).
// Redis stays authoritative; this is a read accelerator only.
type usageReadCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]usageReadEntry
}

func newUsageReadCache(ttl time.Duration) *usageReadCache {
	if ttl <= 0 {
		ttl = time.Second
	}
	return &usageReadCache{ttl: ttl, entries: make(map[string]usageReadEntry)}
}

// get returns the cached cents and true when a fresh entry exists.
func (rc *usageReadCache) get(key string, now time.Time) (int64, bool) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	e, ok := rc.entries[key]
	if !ok || now.After(e.expiry) {
		return 0, false
	}
	return e.cents, true
}

// set stores cents for key with a fresh TTL.
func (rc *usageReadCache) set(key string, cents int64, now time.Time) {
	rc.mu.Lock()
	rc.entries[key] = usageReadEntry{cents: cents, expiry: now.Add(rc.ttl)}
	rc.mu.Unlock()
}

// peek returns the current un-flushed accumulated cents for key without draining
// — so a read can add this instance's own in-flight spend on top of the cached
// Redis base (self-consistency for soft quota).
func (a *usageAggregator) peek(key string) int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.deltas[key].cents
}

// EnableReadCache installs the T1b short-TTL usage read cache so GetUsageMulti
// avoids the per-request MGET. No-op when Redis is absent.
func (c *UsageCache) EnableReadCache(ttl time.Duration) {
	if c.rdb == nil {
		return
	}
	c.rc = newUsageReadCache(ttl)
}

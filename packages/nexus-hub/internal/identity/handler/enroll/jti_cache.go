package enroll

import (
	"sync"
	"time"
)

// jtiCache is a TTL-aware set of JWT IDs used to prevent enrollment-JWT
// replay. Each entry expires at the JWT's own `exp` time (passed in by
// the caller); a background sweep evicts expired entries every minute
// so the map size is bounded by the live-window size (~5 min worth of
// enrollment attempts) rather than growing for the lifetime of the
// process.
//
// Single-Hub scope only. A multi-Hub HA deployment needs a shared
// store (Redis SETNX with TTL) — tracked as a follow-up.
type jtiCache struct {
	mu      sync.Mutex
	entries map[string]time.Time

	stopOnce sync.Once
	stopCh   chan struct{}
	now      func() time.Time
}

func newJTICache() *jtiCache {
	c := &jtiCache{
		entries: make(map[string]time.Time),
		stopCh:  make(chan struct{}),
		now:     time.Now,
	}
	go c.sweepLoop(time.Minute)
	return c
}

// MarkSeen records `jti` with expiry `exp`. Returns true if this is the
// first time the JTI has been seen (caller may proceed), false on
// replay. JTIs whose `exp` is already in the past are rejected as
// replays so a stale token cannot reset itself via the sweep window.
func (c *jtiCache) MarkSeen(jti string, exp time.Time) bool {
	if jti == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[jti]; exists {
		return false
	}
	c.entries[jti] = exp
	return true
}

// Stop terminates the background sweep goroutine. Safe to call from
// any goroutine and idempotent.
func (c *jtiCache) Stop() {
	c.stopOnce.Do(func() { close(c.stopCh) })
}

func (c *jtiCache) sweepLoop(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-t.C:
			c.sweep()
		}
	}
}

func (c *jtiCache) sweep() {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for jti, exp := range c.entries {
		if now.After(exp) {
			delete(c.entries, jti)
		}
	}
}

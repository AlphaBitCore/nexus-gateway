package enroll

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillupload"
)

// jtiCache is a TTL-aware set of JWT IDs used to prevent enrollment-JWT
// replay. Each entry expires at the JWT's own `exp` time (passed in by
// the caller); a background sweep evicts expired entries every minute
// so the map size is bounded by the live-window size (~5 min worth of
// enrollment attempts) rather than growing for the lifetime of the
// process.
//
// Two layers: the in-process map is an L1 fast path that gives the
// single-Hub guarantee even when Redis is down; the optional `dedup` is the
// authoritative L2 (Redis SETNX with TTL) that makes the single-use property
// survive Hub restarts and extend to multi-Hub HA. A nil dedup is the legacy
// single-Hub-uptime behaviour.
type jtiCache struct {
	mu      sync.Mutex
	entries map[string]time.Time

	// dedup is the optional Redis SETNX L2. nil → in-memory only.
	dedup  spillupload.Dedup
	logger *slog.Logger

	stopOnce sync.Once
	stopCh   chan struct{}
	now      func() time.Time
}

func newJTICache(dedup spillupload.Dedup, logger *slog.Logger) *jtiCache {
	c := &jtiCache{
		entries: make(map[string]time.Time),
		dedup:   dedup,
		logger:  logger,
		stopCh:  make(chan struct{}),
		now:     time.Now,
	}
	go c.sweepLoop(time.Minute)
	return c
}

// jtiRedisKey namespaces the enrollment-JTI dedup key so it cannot collide with
// the spill-upload dedup keys that share the same Redis instance.
func jtiRedisKey(jti string) string { return "nexus:enroll:jti:" + jti }

// MarkSeen records `jti` with expiry `exp`. Returns true if this is the first
// time the JTI has been seen (caller may proceed), false on replay.
//
// L1 (in-process) is checked first so an in-flight replay is rejected instantly.
// When an L2 dedup is wired, a Redis SETNX is then attempted with TTL = exp-now:
// a miss (key already set — possibly by a redemption before THIS process
// started, or on another Hub) is a replay across the restart/HA boundary that L1
// alone would miss. A Redis error degrades to the L1 single-Hub
// guarantee rather than blocking enrollment on a transient outage.
func (c *jtiCache) MarkSeen(ctx context.Context, jti string, exp time.Time) bool {
	if jti == "" {
		return false
	}
	c.mu.Lock()
	if _, exists := c.entries[jti]; exists {
		c.mu.Unlock()
		return false
	}
	c.entries[jti] = exp
	c.mu.Unlock()

	if c.dedup != nil {
		ttl := exp.Sub(c.now())
		if ttl <= 0 {
			// Already expired — the JWT exp check should have caught this, but
			// never persist a non-expiring SETNX key for a stale token.
			return false
		}
		acquired, err := c.dedup.SetNX(ctx, jtiRedisKey(jti), ttl)
		if err != nil {
			if c.logger != nil {
				c.logger.Warn("enroll JTI dedup: redis SETNX failed; single-Hub guard only", "error", err)
			}
			return true // degrade to the L1 guarantee already recorded above
		}
		if !acquired {
			return false // seen in Redis: cross-restart / cross-Hub replay
		}
	}
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

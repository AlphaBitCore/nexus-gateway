package iam

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	defaultL1TTL   = 10 * time.Second
	defaultL2TTL   = 60 * time.Second
	redisKeyPrefix = "nexus:iam:policies:"
	redisOpTimeout = 500 * time.Millisecond
)

type l1Entry struct {
	policies []LoadedPolicy
	cachedAt time.Time
}

// PolicyCache is a two-tier cache for IAM policies.
// L1: in-process map with short TTL (10s default).
// L2: Redis with longer TTL (60s default). Optional — degrades to L1-only.
//
// Note: time-bounded policy attachments (`IamPolicyAttachment.expires_at`)
// are filtered at SQL load time, but the cache may serve a stale entry for up
// to L2 TTL (60s) after a grant expires. For break-glass / incident-response
// use cases this is acceptable — a minute of overhang at the deadline is within
// tolerance for windows measured in hours. For immediate revocation, callers
// can invoke Invalidate(principalKey) directly.
type PolicyCache struct {
	mu    sync.RWMutex
	l1    map[string]*l1Entry
	l1TTL time.Duration

	rdb   redis.UniversalClient
	l2TTL time.Duration
}

// NewPolicyCache creates a PolicyCache. If rdb is nil the cache operates in
// L1-only mode (no Redis).
func NewPolicyCache(rdb redis.UniversalClient) *PolicyCache {
	return &PolicyCache{
		l1:    make(map[string]*l1Entry),
		l1TTL: defaultL1TTL,
		rdb:   rdb,
		l2TTL: defaultL2TTL,
	}
}

// Get retrieves policies from the cache. It checks L1 first, then L2 (Redis).
// On an L2 hit the entry is promoted back to L1.
func (c *PolicyCache) Get(key string) ([]LoadedPolicy, bool) {
	// L1 check
	c.mu.RLock()
	entry, ok := c.l1[key]
	c.mu.RUnlock()
	if ok && time.Since(entry.cachedAt) < c.l1TTL {
		return entry.policies, true
	}

	// L2 check (Redis)
	if c.rdb == nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()
	data, err := c.rdb.Get(ctx, redisKeyPrefix+key).Bytes()
	if err != nil {
		return nil, false
	}
	var policies []LoadedPolicy
	if err := json.Unmarshal(data, &policies); err != nil {
		return nil, false
	}
	// Promote to L1
	c.mu.Lock()
	c.l1[key] = &l1Entry{policies: policies, cachedAt: time.Now()}
	c.mu.Unlock()
	return policies, true
}

// Put stores policies in both L1 and L2.
func (c *PolicyCache) Put(key string, policies []LoadedPolicy) {
	c.mu.Lock()
	c.l1[key] = &l1Entry{policies: policies, cachedAt: time.Now()}
	c.mu.Unlock()

	if c.rdb == nil {
		return
	}
	data, err := json.Marshal(policies)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()
	c.rdb.Set(ctx, redisKeyPrefix+key, data, c.l2TTL)
}

// Invalidate removes a specific key from both L1 and L2.
func (c *PolicyCache) Invalidate(key string) {
	c.mu.Lock()
	delete(c.l1, key)
	c.mu.Unlock()

	if c.rdb != nil {
		ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
		defer cancel()
		c.rdb.Del(ctx, redisKeyPrefix+key)
	}
}

// InvalidateAll clears all entries from L1 and removes all IAM policy keys
// from L2.
func (c *PolicyCache) InvalidateAll() {
	c.mu.Lock()
	c.l1 = make(map[string]*l1Entry)
	c.mu.Unlock()

	if c.rdb != nil {
		ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
		defer cancel()
		iter := c.rdb.Scan(ctx, 0, redisKeyPrefix+"*", 100).Iterator()
		for iter.Next(ctx) {
			c.rdb.Del(ctx, iter.Val())
		}
	}
}

// Size returns the number of L1 entries (regardless of expiry).
func (c *PolicyCache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.l1)
}

package quota

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// perfNoRedis is a THROWAWAY experiment switch (NEXUS_PERF_NO_REDIS=1): it
// short-circuits the per-request Redis usage read/increment so an A/B run can
// measure "what if Redis were free" without building the real cache layer.
// Returning zero usage makes the quota check behave like cold-start (allow).
// NOT for production — used only to validate that Redis round-trips are the RPS
// bottleneck before investing in T1b.
var perfNoRedis = os.Getenv("NEXUS_PERF_NO_REDIS") == "1"

// UsageCache tracks per-period cost usage in Redis (with in-memory fallback).
type UsageCache struct {
	rdb    redis.UniversalClient // nil = in-memory fallback
	logger *slog.Logger

	// In-memory fallback when Redis is unavailable.
	mu       sync.Mutex
	memUsage map[string]int64

	// agg is the T1b write-behind accumulator. When non-nil, IncrMulti adds to
	// it instead of writing Redis on the hot path; a background flusher (or
	// FlushUsage in tests) drains it to Redis. nil = legacy synchronous path.
	agg *usageAggregator

	// rc is the T1b short-TTL usage read cache. When non-nil, GetUsageMulti
	// serves the quota check from the in-process snapshot (+ un-flushed local
	// delta) instead of a per-request MGET. nil = legacy synchronous read.
	rc *usageReadCache
}

const usageCachePrefix = "quota:usage:"

// NewUsageCache creates a UsageCache. If rdb is nil, an in-memory map is used.
// Accepts redis.UniversalClient so standalone / sentinel / cluster all work;
// completes the Redis-universal migration, whose earlier pass
// missed this consumer and left cmd/ai-gateway/wiring failing to build.
func NewUsageCache(rdb redis.UniversalClient, logger *slog.Logger) *UsageCache {
	return &UsageCache{
		rdb:      rdb,
		logger:   logger,
		memUsage: make(map[string]int64),
	}
}

// usageKey returns "quota:usage:{targetType}:{targetID}:{periodKey}".
func usageKey(targetType, targetID, periodKey string) string {
	return usageCachePrefix + targetType + ":" + targetID + ":" + periodKey
}

// EnableWriteBehind installs the T1b write-behind accumulator so IncrMulti
// defers Redis writes off the request hot path. No-op when Redis is absent
// (in-memory fallback already does no Redis). Call once at wiring time; a
// background flusher must then call FlushUsage on an interval, and a final
// FlushUsage on shutdown to bound loss to one interval.
func (c *UsageCache) EnableWriteBehind() {
	if c.rdb == nil {
		return
	}
	c.agg = newUsageAggregator()
}

// FlushUsage drains the write-behind accumulator to Redis: one IncrBy + Expire
// per key, identical to the synchronous IncrMulti path, so Redis converges to
// the same value. Safe to call on an empty accumulator (no-op). Fail-open: a
// Redis error is returned (the caller logs + retries next tick); the drained
// deltas are NOT re-queued here — bounded loss on persistent Redis outage is the
// accepted soft-quota posture (never over-bills; under-bills ≤ outage window).
func (c *UsageCache) FlushUsage(ctx context.Context) error {
	if c.agg == nil || c.rdb == nil {
		return nil
	}
	deltas := c.agg.drain()
	if len(deltas) == 0 {
		return nil
	}
	pipe := c.rdb.Pipeline()
	for key, d := range deltas {
		pipe.IncrBy(ctx, key, d.cents)
		if ttl := periodTTL(d.periodKey); ttl > 0 {
			pipe.Expire(ctx, key, ttl)
		}
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("usage_cache: write-behind flush: %w", err)
	}
	return nil
}

// SetUsageForTest seeds the in-memory usage map with a fixed cost in
// cents for one (target, period) tuple. Intended exclusively for tests
// in sibling packages that need to drive Engine.Check past the
// over-limit threshold without depending on Redis state — production
// code reaches usage through IncrMulti / Backfill. No-op when the cache
// is Redis-backed (rdb != nil).
func (c *UsageCache) SetUsageForTest(targetType, targetID, periodKey string, costCents int64) {
	if c.rdb != nil {
		// Redis-backed caches own their state; the test should write to
		// the backing store directly. Silently no-op here to avoid hiding
		// a race between miniredis and an in-memory copy.
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.memUsage[usageKey(targetType, targetID, periodKey)] = costCents
}

// GetUsage returns current cost in cents for a target in a period.
// Returns 0 if not found (cold start case).
func (c *UsageCache) GetUsage(ctx context.Context, targetType, targetID, periodKey string) (int64, error) {
	key := usageKey(targetType, targetID, periodKey)

	if c.rdb != nil {
		val, err := c.rdb.Get(ctx, key).Result()
		if errors.Is(err, redis.Nil) {
			return 0, nil
		}
		if err != nil {
			return 0, fmt.Errorf("usage_cache: GET %s: %w", key, err)
		}
		cents, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("usage_cache: parse %s: %w", val, err)
		}
		return cents, nil
	}

	// In-memory fallback.
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.memUsage[key], nil
}

// UsageQuery identifies one (target, period) usage key for a batch read.
type UsageQuery struct {
	TargetType string
	TargetID   string
	PeriodKey  string
}

// GetUsageMulti reads the current cost (cents) for each query in a SINGLE Redis
// MGET instead of one GET per query — the per-level sequential GETs were a
// per-request latency cost proportional to the quota chain depth. Returns two
// positionally-aligned slices: usages[i] is the cents for queries[i] (0 when the
// key is missing, mirroring GetUsage's cold-start behaviour) and errs[i] is
// non-nil only when that element could not be read/parsed. The error posture is
// PER LEVEL: a whole-batch transport error sets errs[i] for every i (each caller
// level then fails open independently), and a single unparseable element sets
// only its own errs[i] — never failing the other levels. This preserves the
// fail-open posture of GetUsage exactly.
func (c *UsageCache) GetUsageMulti(ctx context.Context, queries []UsageQuery) (usages []int64, errs []error) {
	usages = make([]int64, len(queries))
	errs = make([]error, len(queries))
	if len(queries) == 0 {
		return usages, errs
	}
	if perfNoRedis {
		return usages, errs // experiment: zero usage, no Redis
	}

	if c.rdb != nil {
		keys := make([]string, len(queries))
		for i, q := range queries {
			keys[i] = usageKey(q.TargetType, q.TargetID, q.PeriodKey)
		}
		// T1b read cache: serve fresh entries from the in-process snapshot
		// (0 Redis on a full hit); MGET only the misses; add this instance's
		// un-flushed write-behind delta so it never under-counts its own spend.
		if c.rc != nil {
			now := time.Now()
			missIdx := make([]int, 0, len(keys))
			missKeys := make([]string, 0, len(keys))
			for i, k := range keys {
				if v, ok := c.rc.get(k, now); ok {
					usages[i] = v
				} else {
					missIdx = append(missIdx, i)
					missKeys = append(missKeys, k)
				}
			}
			if len(missKeys) > 0 {
				vals, err := c.rdb.MGet(ctx, missKeys...).Result()
				if err != nil {
					for _, i := range missIdx {
						errs[i] = fmt.Errorf("usage_cache: MGET: %w", err)
					}
				} else {
					for j, i := range missIdx {
						var cents int64
						if j < len(vals) && vals[j] != nil {
							s, ok := vals[j].(string)
							if !ok {
								errs[i] = fmt.Errorf("usage_cache: MGET %s: unexpected type %T", missKeys[j], vals[j])
								continue
							}
							parsed, perr := strconv.ParseInt(s, 10, 64)
							if perr != nil {
								errs[i] = fmt.Errorf("usage_cache: parse %s: %w", s, perr)
								continue
							}
							cents = parsed
						}
						usages[i] = cents
						c.rc.set(missKeys[j], cents, now) // cache misses (incl. 0 cold-start) to bound the TTL window
					}
				}
			}
			if c.agg != nil {
				for i, k := range keys {
					usages[i] += c.agg.peek(k)
				}
			}
			return usages, errs
		}
		vals, err := c.rdb.MGet(ctx, keys...).Result()
		if err != nil {
			// Whole-batch transport error: every level fails open.
			for i := range errs {
				errs[i] = fmt.Errorf("usage_cache: MGET: %w", err)
			}
			return usages, errs
		}
		for i := range queries {
			if i >= len(vals) || vals[i] == nil {
				continue // missing key → 0 (cold start)
			}
			s, ok := vals[i].(string)
			if !ok {
				errs[i] = fmt.Errorf("usage_cache: MGET %s: unexpected type %T", keys[i], vals[i])
				continue
			}
			cents, perr := strconv.ParseInt(s, 10, 64)
			if perr != nil {
				errs[i] = fmt.Errorf("usage_cache: parse %s: %w", s, perr)
				continue
			}
			usages[i] = cents
		}
		return usages, errs
	}

	// In-memory fallback.
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, q := range queries {
		usages[i] = c.memUsage[usageKey(q.TargetType, q.TargetID, q.PeriodKey)]
	}
	return usages, errs
}

// UsageLevel identifies a quota enforcement target for batch increment.
type UsageLevel struct {
	TargetType string
	TargetID   string
}

// IncrMulti increments usage for multiple levels in one Redis pipeline.
func (c *UsageCache) IncrMulti(ctx context.Context, levels []UsageLevel, periodKey string, costCents int64) error {
	if len(levels) == 0 || costCents <= 0 {
		return nil
	}
	if perfNoRedis {
		return nil // experiment: no Redis increment
	}
	if c.agg != nil {
		// T1b write-behind: accumulate locally, flush asynchronously. Keeps the
		// per-request hot path Redis-free; Redis converges via FlushUsage's
		// IncrBy. Bounded overshoot ≤ one flush interval (soft quota).
		for _, l := range levels {
			c.agg.add(usageKey(l.TargetType, l.TargetID, periodKey), periodKey, costCents)
		}
		return nil
	}

	if c.rdb != nil {
		pipe := c.rdb.Pipeline()
		for _, l := range levels {
			key := usageKey(l.TargetType, l.TargetID, periodKey)
			pipe.IncrBy(ctx, key, costCents)
			// EXPIRE overwrites any existing TTL; safe here because periodTTL is
			// absolute to the period end, so re-setting it on every call is
			// idempotent (the TTL always lands on the same wall-clock instant).
			ttl := periodTTL(periodKey)
			if ttl > 0 {
				pipe.Expire(ctx, key, ttl)
			}
		}
		_, err := pipe.Exec(ctx)
		if err != nil {
			return fmt.Errorf("usage_cache: pipeline exec: %w", err)
		}
		return nil
	}

	// In-memory fallback.
	c.mu.Lock()
	for _, l := range levels {
		key := usageKey(l.TargetType, l.TargetID, periodKey)
		c.memUsage[key] += costCents
	}
	c.mu.Unlock()
	return nil
}

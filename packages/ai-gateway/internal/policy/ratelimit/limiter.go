package ratelimit

import (
	"log/slog"

	"github.com/redis/go-redis/v9"
)

// Limiter dispatches rate limit checks to Redis (distributed) with automatic
// fallback to a local in-memory sliding window on Redis errors.
//
// REDIS-OUTAGE DEGRADATION (deliberate tradeoff). Redis is the only
// cross-instance coordination point. When the Redis Allow call errors, each
// gateway instance falls through to its OWN in-process LocalLimiter, which has
// no visibility into the other instances' counters. With N instances behind a
// load balancer, the effective cluster-wide limit during a Redis outage is
// therefore approximately N × the configured per-key limit (each instance
// independently admits up to `limit` requests per window). This is a fail-OPEN
// degradation: a Redis outage loosens enforcement rather than rejecting all
// traffic. It is accepted because (a) the alternative — fail closed — would
// turn a cache outage into a full availability outage, and (b) rate limits are
// an abuse-mitigation guardrail, not a hard correctness boundary like quota
// spend. Single-instance deployments (NewLocalOnly) are unaffected: N=1.
type Limiter struct {
	redis  *RedisLimiter
	local  *LocalLimiter
	logger *slog.Logger
}

// New creates a Limiter backed by Redis with local fallback.
// If rdb is nil, operates in local-only mode.
func New(rdb redis.UniversalClient, logger *slog.Logger) *Limiter {
	l := &Limiter{
		local:  NewLocalLimiter(),
		logger: logger,
	}
	if rdb != nil {
		l.redis = NewRedisLimiter(rdb, logger)
	}
	return l
}

// NewLocalOnly creates a Limiter without Redis (single-instance mode).
func NewLocalOnly(logger *slog.Logger) *Limiter {
	return &Limiter{
		local:  NewLocalLimiter(),
		logger: logger,
	}
}

// Allow checks whether a request identified by key is within the rate limit.
// limit is requests per window; windowMs is the window duration in milliseconds.
func (l *Limiter) Allow(key string, limit int, windowMs int64) (bool, int) {
	if limit <= 0 {
		return true, 0
	}

	if l.redis != nil {
		allowed, retryAfter, err := l.redis.Allow(key, limit, windowMs)
		if err != nil {
			l.logger.Warn("rate limiter Redis timeout, falling back to local", "key", key, "timeout", "500ms", "error", err)
		} else {
			return allowed, retryAfter
		}
	}

	return l.local.Allow(key, limit, windowMs)
}

// Cleanup prunes stale entries from the local limiter.
func (l *Limiter) Cleanup() {
	l.local.Cleanup()
}

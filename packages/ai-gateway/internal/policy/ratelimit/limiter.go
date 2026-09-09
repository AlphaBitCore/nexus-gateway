package ratelimit

import (
	"log/slog"
	"sync"
	"time"

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

	// Fallback logging is throttled to one line per fallbackLogInterval per
	// process, carrying the count it suppressed. A Redis outage makes EVERY
	// Allow take the fallback, and proxy admission calls Allow twice per
	// request, so the unthrottled form emitted two lines per request for the
	// duration — hundreds a second under load, none of them saying anything the
	// first one did not.
	fallbackMu       sync.Mutex
	fallbackLastLog  time.Time
	fallbackSuppress int64
}

// fallbackLogInterval bounds how often the Redis-fallback warning is emitted.
// Long enough that a sustained outage is a handful of lines an hour, short
// enough that an operator watching the log sees it start.
const fallbackLogInterval = 30 * time.Second

// noteRedisFallback reports whether this fallback should be logged, and how
// many were suppressed since the last one that was. The counter is reset by the
// caller that wins the interval, so the number attached to a line is the number
// that line stands for.
func (l *Limiter) noteRedisFallback(now time.Time) (log bool, suppressed int64) {
	l.fallbackMu.Lock()
	defer l.fallbackMu.Unlock()
	if now.Sub(l.fallbackLastLog) < fallbackLogInterval {
		l.fallbackSuppress++
		return false, 0
	}
	suppressed = l.fallbackSuppress
	l.fallbackSuppress = 0
	l.fallbackLastLog = now
	return true, suppressed
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
			if should, suppressed := l.noteRedisFallback(time.Now()); should {
				l.logger.Warn("rate limiter Redis unavailable, falling back to per-instance limiting",
					"key", key,
					"timeout", "500ms",
					"error", err,
					"suppressed_since_last", suppressed,
					"effect", "each gateway enforces the limit independently until Redis answers",
				)
			}
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

package ratelimit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// KeyPrefix is the Redis key namespace for rate limit sorted sets.
const KeyPrefix = "nexus:rl:"

// RedisLimiter uses a Lua script for atomic sliding window rate limiting.
type RedisLimiter struct {
	rdb    redis.UniversalClient
	script *redis.Script
	logger *slog.Logger
}

// NewRedisLimiter returns a Redis-backed limiter, probing the connection by
// loading the Lua script first.
//
// It returns a limiter even when the probe fails. The probe reports whether
// Redis was reachable AT STARTUP; returning nil for it made that momentary fact
// permanent, because Limiter.New calls this exactly once and a nil result
// leaves every later Allow on the per-instance LocalLimiter for the lifetime of
// the process. A gateway that started during a Redis failover, or ahead of
// Redis in an ordering-free compose/k8s bring-up, then enforced N x the
// configured limit across the fleet — silently, and until someone restarted it.
//
// Nothing is lost by continuing: Allow does not dispatch on the probe's result
// (see the note below), and its own error path already falls back to the local
// limiter per request and resumes using Redis the moment Redis answers. That is
// the correct response to a startup blip, and it is a path that already exists.
func NewRedisLimiter(rdb redis.UniversalClient, logger *slog.Logger) *RedisLimiter {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var sha string
	var err error
	for attempt := range 3 {
		sha, err = rdb.ScriptLoad(ctx, SlidingWindowLua).Result()
		if err == nil {
			break
		}
		logger.Warn("SCRIPT LOAD failed", "attempt", attempt+1, "error", err)
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		// Not fatal, and deliberately not a nil return: rate limiting stays
		// CLUSTER-WIDE as soon as Redis answers, because Allow reloads the
		// script on demand. Until then each instance limits locally, which is
		// the same degradation a mid-life Redis outage already produces.
		logger.Error("SCRIPT LOAD failed after retries; rate limiting is per-instance until Redis answers",
			"error", err,
			"effect", "each gateway enforces the limit independently, so the fleet-wide rate is multiplied by the instance count",
		)
		return &RedisLimiter{rdb: rdb, script: redis.NewScript(SlidingWindowLua), logger: logger}
	}

	logger.Info("Redis rate limiter script loaded", "sha", sha)
	// The load above is a STARTUP PROBE — it proves Redis is reachable and the
	// script compiles, which is what the nil return exists to report. It is not
	// what Allow dispatches on: a SHA captured once here stops resolving the
	// moment Redis restarts, fails over to a replica that never loaded the
	// script, or is SCRIPT FLUSHed, and the limiter would then error on every
	// call for the lifetime of the process. Allow goes through *redis.Script,
	// which re-sends the body on NOSCRIPT and reloads it as a side effect.
	return &RedisLimiter{rdb: rdb, script: redis.NewScript(SlidingWindowLua), logger: logger}
}

// Allow checks whether a request is within the rate limit.
// Returns (allowed, retryAfterSec, error). On Redis error the caller
// should fall back to the local limiter.
//
// The dispatch is `script.Run`, not a bare EvalSha, so a NOSCRIPT reply — the
// normal consequence of a Redis restart, a failover to a replica that never
// loaded the script, or a SCRIPT FLUSH — is recovered from by re-sending the
// body once, which reloads it. With a cached SHA and no fallback, that single
// event turned every subsequent call into an error, and since the caller's
// error path is "fall back to the local limiter", the cluster-wide limit
// silently and permanently degraded to per-instance: N replicas each enforcing
// the full quota. Nothing in the logs said the limit had changed meaning.
func (rl *RedisLimiter) Allow(key string, limit int, windowMs int64) (bool, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	now := time.Now().UnixMilli()
	member := fmt.Sprintf("%d:%s", now, randomSuffix())

	result, err := rl.script.Run(ctx, rl.rdb, []string{KeyPrefix + key}, limit, windowMs, now, member).Int64()
	if err != nil {
		return false, 0, fmt.Errorf("ratelimit: eval: %w", err)
	}

	if result == 0 {
		return true, 0, nil
	}
	retryAfterSec := int(result/1000) + 1
	if retryAfterSec < 1 {
		retryAfterSec = 1
	}
	return false, retryAfterSec, nil
}

var suffixPool = sync.Pool{
	New: func() any {
		b := make([]byte, 5)
		return &b
	},
}

func randomSuffix() string {
	bp := suffixPool.Get().(*[]byte)
	_, _ = rand.Read(*bp)
	s := hex.EncodeToString(*bp)
	suffixPool.Put(bp)
	return s
}

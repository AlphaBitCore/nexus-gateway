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
	rdb       redis.UniversalClient
	scriptSHA string
	logger    *slog.Logger
}

// NewRedisLimiter loads the Lua script into Redis and returns a limiter.
// Returns nil if the script cannot be loaded after retries.
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
		logger.Error("SCRIPT LOAD failed after retries, Redis rate limiting disabled", "error", err)
		return nil
	}

	logger.Info("Redis rate limiter script loaded", "sha", sha)
	return &RedisLimiter{rdb: rdb, scriptSHA: sha, logger: logger}
}

// Allow checks whether a request is within the rate limit.
// Returns (allowed, retryAfterSec, error). On Redis error the caller
// should fall back to the local limiter.
func (rl *RedisLimiter) Allow(key string, limit int, windowMs int64) (bool, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	now := time.Now().UnixMilli()
	member := fmt.Sprintf("%d:%s", now, randomSuffix())

	result, err := rl.rdb.EvalSha(ctx, rl.scriptSHA, []string{KeyPrefix + key}, limit, windowMs, now, member).Int64()
	if err != nil {
		return false, 0, fmt.Errorf("ratelimit: evalsha: %w", err)
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

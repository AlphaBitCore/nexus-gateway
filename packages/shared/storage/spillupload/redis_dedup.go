package spillupload

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisDedup is the production Dedup implementation backed by a single
// Redis SETNX with TTL. Callers wrap their [redis.UniversalClient] so the
// dedup works against standalone, sentinel, or cluster deployments alike.
type RedisDedup struct {
	client redis.UniversalClient
}

// NewRedisDedup constructs a Dedup over the supplied Redis client. The
// client is reused across calls; concurrent SetNX calls are safe.
func NewRedisDedup(client redis.UniversalClient) *RedisDedup {
	return &RedisDedup{client: client}
}

// SetNX writes `key` with value "1" and the given TTL iff the key does
// not exist. Returns (true, nil) when the caller acquired the slot;
// (false, nil) when the slot was already taken by a prior PUT.
func (r *RedisDedup) SetNX(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	if r == nil || r.client == nil {
		return false, fmt.Errorf("spillupload: nil redis client")
	}
	ok, err := r.client.SetNX(ctx, key, "1", ttl).Result()
	if err != nil {
		return false, fmt.Errorf("spillupload: redis SETNX: %w", err)
	}
	return ok, nil
}

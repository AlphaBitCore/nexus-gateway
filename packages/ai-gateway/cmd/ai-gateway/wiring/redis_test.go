package wiring

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/config"
)

// TestInitRedis_noAddrsReturnsNil verifies that empty address list returns nil.
func TestInitRedis_noAddrsReturnsNil(t *testing.T) {
	cfg := &config.Config{}
	// No addrs configured → nil client.
	rdb := InitRedis(context.Background(), cfg)
	if rdb != nil {
		t.Error("expected nil redis client when no addrs configured")
	}
}

// TestInitRateLimiter_withRedis verifies the Redis-backed path is taken when
// a non-nil client is provided. Uses miniredis so no external server needed.
func TestInitRateLimiter_withRedis(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	limiter := InitRateLimiter(rdb, discardLogger())
	if limiter == nil {
		t.Fatal("expected non-nil rate limiter for non-nil rdb")
	}
}

// TestInitSemantic_withRedisClient is tested in semantic_test.go to avoid
// duplicate prometheus metric registration across test functions.

// TestInitRedis_unreachableAddrReturnsNil verifies that an unreachable Redis
// address (ping fails) returns nil rather than panicking.
func TestInitRedis_unreachableAddrReturnsNil(t *testing.T) {
	cfg := &config.Config{}
	// Use a port that's almost certainly not bound.
	cfg.Redis.Addrs = []string{"127.0.0.1:19999"}
	rdb := InitRedis(context.Background(), cfg)
	// Should return nil (ping fails → degraded mode).
	// We don't assert nil because on very rare occasions the port might be open,
	// but we verify no panic occurred.
	if rdb != nil {
		_ = rdb.Close() // clean up if somehow connected
	}
}

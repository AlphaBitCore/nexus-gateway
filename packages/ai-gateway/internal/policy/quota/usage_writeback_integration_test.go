package quota

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// TestUsageCache_WriteBehind_DefersThenFlushes verifies the T1b hot-path
// contract: with write-behind enabled, IncrMulti does NOT touch Redis (the
// request path stays Redis-free — the whole point), and a later FlushUsage
// drains the accumulated deltas to Redis as IncrBy + Expire, summing repeats and
// preserving the period TTL. Redis remains the convergent authority.
func TestUsageCache_WriteBehind_DefersThenFlushes(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	c := NewUsageCache(rdb, testLogger())
	c.EnableWriteBehind()

	ctx := context.Background()
	period := "2026-06"
	levels := []UsageLevel{
		{TargetType: "virtual_key", TargetID: "vk1"},
		{TargetType: "org", TargetID: "org1"},
	}

	// Two increments on the hot path — must NOT reach Redis yet.
	if err := c.IncrMulti(ctx, levels, period, 100); err != nil {
		t.Fatalf("IncrMulti 1: %v", err)
	}
	if err := c.IncrMulti(ctx, levels, period, 50); err != nil {
		t.Fatalf("IncrMulti 2: %v", err)
	}

	vkKey := usageKey("virtual_key", "vk1", period)
	if mr.Exists(vkKey) {
		t.Fatalf("write-behind leaked to Redis on the hot path: %s exists before flush", vkKey)
	}

	// Flush drains to Redis: summed delta + a TTL.
	if err := c.FlushUsage(ctx); err != nil {
		t.Fatalf("FlushUsage: %v", err)
	}

	got, err := c.GetUsage(ctx, "virtual_key", "vk1", period)
	if err != nil {
		t.Fatalf("GetUsage: %v", err)
	}
	if got != 150 {
		t.Errorf("vk1 usage after flush = %d, want 150 (100+50 summed)", got)
	}
	orgGot, _ := c.GetUsage(ctx, "org", "org1", period)
	if orgGot != 150 {
		t.Errorf("org1 usage after flush = %d, want 150", orgGot)
	}
	if ttl := mr.TTL(vkKey); ttl <= 0 {
		t.Errorf("flushed key %s has no TTL (Expire not applied): %v", vkKey, ttl)
	}

	// Aggregator reset: a second flush with no new increments is a no-op.
	if err := c.FlushUsage(ctx); err != nil {
		t.Fatalf("second FlushUsage: %v", err)
	}
	got2, _ := c.GetUsage(ctx, "virtual_key", "vk1", period)
	if got2 != 150 {
		t.Errorf("usage changed on empty flush = %d, want 150 (no double-count)", got2)
	}
}

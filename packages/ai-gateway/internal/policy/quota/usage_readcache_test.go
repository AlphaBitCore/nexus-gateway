package quota

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// TestUsageCache_ReadCache_HitAvoidsRedis verifies the T1b read cache: after a
// cold read populates the local snapshot, a second read within the TTL returns
// the cached value WITHOUT touching Redis (proven by mutating Redis underneath
// and seeing the stale cached value). This removes the per-request MGET on the
// quota check hot path. Soft consistency: bounded staleness ≤ TTL (accepted).
func TestUsageCache_ReadCache_HitAvoidsRedis(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	c := NewUsageCache(rdb, testLogger())
	c.EnableReadCache(2 * time.Second)

	ctx := context.Background()
	period := "2026-06"
	key := usageKey("virtual_key", "vk1", period)
	mr.Set(key, "100")

	q := []UsageQuery{{TargetType: "virtual_key", TargetID: "vk1", PeriodKey: period}}

	// Cold read: populates the snapshot from Redis.
	u1, errs := c.GetUsageMulti(ctx, q)
	if errs[0] != nil || u1[0] != 100 {
		t.Fatalf("cold read = %d (err %v), want 100", u1[0], errs[0])
	}

	// Mutate Redis underneath. A cache HIT must NOT see this.
	mr.Set(key, "999")

	u2, _ := c.GetUsageMulti(ctx, q)
	if u2[0] != 100 {
		t.Errorf("warm read = %d, want 100 (cache hit must not re-read Redis)", u2[0])
	}
}

// TestUsageCache_ReadCache_AddsLocalDelta verifies that when write-behind is also
// on, the read reflects this instance's own un-flushed increments on top of the
// cached Redis base — so a caller never under-counts its own just-spent cost
// (the self-consistency invariant for soft quota).
func TestUsageCache_ReadCache_AddsLocalDelta(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	c := NewUsageCache(rdb, testLogger())
	c.EnableReadCache(2 * time.Second)
	c.EnableWriteBehind()

	ctx := context.Background()
	period := "2026-06"
	key := usageKey("virtual_key", "vk1", period)
	mr.Set(key, "100") // Redis base

	levels := []UsageLevel{{TargetType: "virtual_key", TargetID: "vk1"}}
	// Spend 30 locally (write-behind: not yet flushed to Redis).
	if err := c.IncrMulti(ctx, levels, period, 30); err != nil {
		t.Fatalf("IncrMulti: %v", err)
	}

	q := []UsageQuery{{TargetType: "virtual_key", TargetID: "vk1", PeriodKey: period}}
	u, _ := c.GetUsageMulti(ctx, q)
	if u[0] != 130 {
		t.Errorf("read with local delta = %d, want 130 (Redis 100 + un-flushed 30)", u[0])
	}
	_ = key
}

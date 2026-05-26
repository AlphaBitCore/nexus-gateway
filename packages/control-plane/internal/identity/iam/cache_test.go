package iam

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newMiniRedisClient(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, rdb
}

func TestCacheL1Hit(t *testing.T) {
	c := NewPolicyCache(nil)
	key := "user:alice"
	policies := []LoadedPolicy{{ID: "p1", Name: "test", Source: "direct"}}
	c.Put(key, policies)
	got, ok := c.Get(key)
	if !ok {
		t.Fatal("expected L1 cache hit")
	}
	if len(got) != 1 || got[0].ID != "p1" {
		t.Fatalf("got %+v, want [{ID:p1}]", got)
	}
}

func TestCacheL1Expiry(t *testing.T) {
	c := NewPolicyCache(nil)
	c.l1TTL = 10 * time.Millisecond
	c.Put("user:bob", []LoadedPolicy{{ID: "p1"}})
	time.Sleep(15 * time.Millisecond)
	_, ok := c.Get("user:bob")
	if ok {
		t.Fatal("expected L1 cache miss after TTL expiry")
	}
}

func TestCacheInvalidateSpecific(t *testing.T) {
	c := NewPolicyCache(nil)
	c.Put("user:alice", []LoadedPolicy{{ID: "p1"}})
	c.Put("user:bob", []LoadedPolicy{{ID: "p2"}})
	c.Invalidate("user:alice")
	if _, ok := c.Get("user:alice"); ok {
		t.Fatal("expected alice to be invalidated")
	}
	if _, ok := c.Get("user:bob"); !ok {
		t.Fatal("expected bob to still be cached")
	}
}

func TestCacheInvalidateAll(t *testing.T) {
	c := NewPolicyCache(nil)
	c.Put("user:alice", []LoadedPolicy{{ID: "p1"}})
	c.Put("user:bob", []LoadedPolicy{{ID: "p2"}})
	c.InvalidateAll()
	if _, ok := c.Get("user:alice"); ok {
		t.Fatal("expected alice invalidated")
	}
	if _, ok := c.Get("user:bob"); ok {
		t.Fatal("expected bob invalidated")
	}
}

func TestCacheSize(t *testing.T) {
	c := NewPolicyCache(nil)
	if c.Size() != 0 {
		t.Fatalf("empty cache size = %d", c.Size())
	}
	c.Put("user:alice", []LoadedPolicy{{ID: "p1"}})
	if c.Size() != 1 {
		t.Fatalf("cache size = %d, want 1", c.Size())
	}
}

func TestCache_L2HitPromotesToL1(t *testing.T) {
	// L2 hit (after L1 expiry) must promote back into L1 so the next
	// read is a fast L1 hit rather than another Redis round-trip.
	_, rdb := newMiniRedisClient(t)
	c := NewPolicyCache(rdb)

	policies := []LoadedPolicy{{ID: "p-l2"}}
	data, _ := json.Marshal(policies)
	// Pre-seed only L2.
	ctx := context.Background()
	if err := rdb.Set(ctx, redisKeyPrefix+"user:x", data, 0).Err(); err != nil {
		t.Fatal(err)
	}

	got, ok := c.Get("user:x")
	if !ok || len(got) != 1 || got[0].ID != "p-l2" {
		t.Fatalf("L2 hit: got %+v ok=%v", got, ok)
	}
	if c.Size() != 1 {
		t.Errorf("L2 hit should promote to L1; size=%d", c.Size())
	}
}

func TestCache_L2MalformedFallsThrough(t *testing.T) {
	// Garbled L2 entry must produce a cache miss, not a panic or a
	// half-decoded result. Critical: a corrupt Redis value cannot
	// admit a principal with a synthesized "no policies" answer.
	_, rdb := newMiniRedisClient(t)
	c := NewPolicyCache(rdb)
	ctx := context.Background()
	if err := rdb.Set(ctx, redisKeyPrefix+"user:bad", "{not-json", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get("user:bad"); ok {
		t.Error("malformed L2 entry should miss, not pretend hit")
	}
}

func TestCache_PutWritesToBothL1AndL2(t *testing.T) {
	mr, rdb := newMiniRedisClient(t)
	c := NewPolicyCache(rdb)

	c.Put("user:both", []LoadedPolicy{{ID: "p-x"}})

	// L1 verified via direct size.
	if c.Size() != 1 {
		t.Errorf("Put didn't write to L1: size=%d", c.Size())
	}
	// L2 verified via direct Redis read.
	raw, err := mr.Get(redisKeyPrefix + "user:both")
	if err != nil || raw == "" {
		t.Errorf("Put didn't write to L2: err=%v raw=%q", err, raw)
	}
}

func TestCache_InvalidateClearsL2Too(t *testing.T) {
	mr, rdb := newMiniRedisClient(t)
	c := NewPolicyCache(rdb)
	c.Put("user:inv", []LoadedPolicy{{ID: "p"}})

	c.Invalidate("user:inv")

	// L1 cleared.
	if _, ok := c.Get("user:inv"); ok {
		t.Error("L1 not cleared")
	}
	// L2 cleared.
	if _, err := mr.Get(redisKeyPrefix + "user:inv"); err == nil {
		t.Error("L2 not cleared — key still present")
	}
}

func TestCache_InvalidateAllScansAndClearsL2(t *testing.T) {
	mr, rdb := newMiniRedisClient(t)
	c := NewPolicyCache(rdb)
	c.Put("user:1", []LoadedPolicy{{ID: "a"}})
	c.Put("user:2", []LoadedPolicy{{ID: "b"}})
	c.Put("user:3", []LoadedPolicy{{ID: "c"}})

	c.InvalidateAll()

	if c.Size() != 0 {
		t.Errorf("L1 not cleared: size=%d", c.Size())
	}
	// Every iam policy key must be gone from L2.
	for _, k := range []string{"user:1", "user:2", "user:3"} {
		if _, err := mr.Get(redisKeyPrefix + k); err == nil {
			t.Errorf("L2 not cleared for %q", k)
		}
	}
}

func TestCache_RedisUnavailableDoesNotBlock(t *testing.T) {
	// rdb pointing at a closed miniredis must not block Put/Get/etc.
	// Cache operations should degrade gracefully — the 500ms timeout
	// protects the request hot path from a stalled Redis.
	mr, _ := miniredis.Run()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	mr.Close() // Redis is now unreachable
	t.Cleanup(func() { _ = rdb.Close() })

	c := NewPolicyCache(rdb)
	start := time.Now()
	c.Put("user:x", []LoadedPolicy{{ID: "p"}}) // L1 succeeds, L2 fails silently
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Put against dead Redis took %v — timeout broken?", d)
	}
	// L1 should still serve.
	if _, ok := c.Get("user:x"); !ok {
		t.Error("L1 should still serve even when L2 is dead")
	}
}

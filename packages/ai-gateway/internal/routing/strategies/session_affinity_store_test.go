package strategies

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// The property that matters for live traffic: a Redis that is DOWN, and a Redis
// that HANGS, must both leave the request unblocked. The second is the dangerous
// one — a failing client returns immediately and the fail-open branch works,
// while a hung one would add its latency to every chat request.
func TestSessionAffinity_RedisFailureNeverBlocksTraffic(t *testing.T) {
	t.Run("redis unreachable", func(t *testing.T) {
		// A client pointed at a closed port: every call errors.
		rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 10 * time.Millisecond})
		s := NewSessionAffinityStore(rdb, "test", nil)

		start := time.Now()
		s.Put(context.Background(), "vk", "sess", "model-a")
		got := s.Get(context.Background(), "vk", "sess")
		elapsed := time.Since(start)

		// The local tier answered, so the entry survives a dead Redis entirely.
		if got != "model-a" {
			t.Fatalf("the local tier must answer when Redis is down, got %q", got)
		}
		if elapsed > 2*affinityRedisTimeout+200*time.Millisecond {
			t.Fatalf("a dead Redis must not add real latency; took %v", elapsed)
		}
	})

	t.Run("redis hangs", func(t *testing.T) {
		s := NewSessionAffinityStore(hangingRedis{}, "test", nil)
		// Nothing in the local tier yet, so this read must reach the hung client
		// and give up on its own deadline rather than waiting forever.
		start := time.Now()
		got := s.Get(context.Background(), "vk", "cold")
		elapsed := time.Since(start)
		if got != "" {
			t.Fatalf("a hung Redis must answer as absence, got %q", got)
		}
		if elapsed > affinityRedisTimeout+200*time.Millisecond {
			t.Fatalf("a hung Redis must be bounded by affinityRedisTimeout; took %v", elapsed)
		}
	})
}

// A local-only store is a supported configuration, not a degraded one: on a
// single-instance gateway every turn reaches the same process.
func TestSessionAffinity_NilRedisIsLocalOnlyAndWorks(t *testing.T) {
	s := NewSessionAffinityStore(nil, "test", nil)
	s.Put(context.Background(), "vk", "sess", "model-a")
	if got := s.Get(context.Background(), "vk", "sess"); got != "model-a" {
		t.Fatalf("local-only store must remember, got %q", got)
	}
}

// The shared tier is what lets a second gateway instance find the entry.
func TestSessionAffinity_SharedTierCrossesInstances(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	instanceA := NewSessionAffinityStore(rdb, "test", nil)
	instanceB := NewSessionAffinityStore(rdb, "test", nil) // separate local tier

	instanceA.Put(context.Background(), "vk", "sess", "model-a")
	if got := instanceB.Get(context.Background(), "vk", "sess"); got != "model-a" {
		t.Fatalf("a second instance must find the entry through Redis, got %q", got)
	}
	// And it should now be local on B, so a third read does not need Redis.
	rdb.Close()
	if got := instanceB.Get(context.Background(), "vk", "sess"); got != "model-a" {
		t.Fatalf("the shared read must populate the local tier, got %q", got)
	}
}

// The entry must expire with the provider's cache, not outlive it.
func TestSessionAffinity_EntryExpiresWithTheProviderCache(t *testing.T) {
	s := NewSessionAffinityStore(nil, "test", nil)
	base := time.Now()
	s.now = func() time.Time { return base }
	s.Put(context.Background(), "vk", "sess", "model-a")

	s.now = func() time.Time { return base.Add(affinityTTL - time.Second) }
	if got := s.Get(context.Background(), "vk", "sess"); got != "model-a" {
		t.Fatalf("must still hold just inside the TTL, got %q", got)
	}
	s.now = func() time.Time { return base.Add(affinityTTL + time.Second) }
	if got := s.Get(context.Background(), "vk", "sess"); got != "" {
		t.Fatalf("must expire with the provider's cache, got %q", got)
	}
}

// Session ids are caller-asserted, so the local tier is a memory-growth vector a
// caller controls. It must stay bounded.
func TestSessionAffinity_LocalTierIsBounded(t *testing.T) {
	s := NewSessionAffinityStore(nil, "test", nil)
	for i := range affinityMemoryEntries + 1000 {
		s.Put(context.Background(), "vk", string(rune(i%1114111)), "m")
	}
	s.local.mu.Lock()
	n := len(s.local.entries)
	s.local.mu.Unlock()
	if n > affinityMemoryEntries {
		t.Fatalf("local tier must stay bounded at %d, grew to %d", affinityMemoryEntries, n)
	}
}

// hangingRedis blocks until the caller's context deadline fires, which is what a
// wedged connection looks like from the client's side.
type hangingRedis struct{ redis.UniversalClient }

func (hangingRedis) Get(ctx context.Context, _ string) *redis.StringCmd {
	<-ctx.Done()
	c := redis.NewStringCmd(ctx)
	c.SetErr(errors.New("hung"))
	return c
}

// A Redis key written without a TTL never expires, and session ids are
// caller-supplied — an unbounded number of permanent keys is a cache-exhaustion
// vector the caller controls. Every write must carry the expiry, and every turn
// must refresh it: what the TTL tracks is the age of the PROVIDER's cache entry,
// and each turn writes a new one.
func TestSessionAffinity_RedisKeyAlwaysCarriesTTL(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	s := NewSessionAffinityStore(rdb, "test", nil)
	s.Put(context.Background(), "vk", "sess", "model-a")

	ttl := mr.TTL("test:route:affinity:vk:sess")
	if ttl <= 0 {
		t.Fatalf("the key must expire; miniredis reports TTL=%v (0 means no expiry)", ttl)
	}
	if ttl > affinityTTL {
		t.Fatalf("TTL %v must not exceed affinityTTL %v", ttl, affinityTTL)
	}

	mr.FastForward(affinityTTL / 2)
	s.Put(context.Background(), "vk", "sess", "model-a")
	if refreshed := mr.TTL("test:route:affinity:vk:sess"); refreshed <= affinityTTL/2 {
		t.Fatalf("each turn must refresh the TTL, got %v", refreshed)
	}

	// And once it lapses the entry is gone from Redis, so an abandoned
	// conversation leaves nothing behind.
	mr.FastForward(affinityTTL + time.Second)
	if mr.Exists("test:route:affinity:vk:sess") {
		t.Fatal("an abandoned conversation must not leave a key behind")
	}
}

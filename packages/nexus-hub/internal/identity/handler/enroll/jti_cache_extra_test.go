package enroll

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeDedup is an in-memory stand-in for the Redis SETNX dedup. It persists
// across jtiCache instances within a test so it models Redis surviving a Hub
// restart. errOnce, when set, makes the next SetNX fail (transient-outage path).
type fakeDedup struct {
	mu      sync.Mutex
	seen    map[string]bool
	failNow bool
}

func newFakeDedup() *fakeDedup { return &fakeDedup{seen: map[string]bool{}} }

func (f *fakeDedup) SetNX(_ context.Context, key string, _ time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNow {
		f.failNow = false
		return false, errors.New("redis down")
	}
	if f.seen[key] {
		return false, nil // already consumed → replay
	}
	f.seen[key] = true
	return true, nil
}

// TestJTICache_RedisGuardSurvivesRestart is the SEC-M4-03 regression: with the
// Redis L2 wired, a JTI redeemed once cannot be replayed by a FRESH jtiCache
// (simulating a Hub restart that clears the in-memory L1) — the persistent dedup
// still reports the JTI as seen.
func TestJTICache_RedisGuardSurvivesRestart(t *testing.T) {
	dedup := newFakeDedup()
	exp := time.Now().Add(5 * time.Minute)

	c1 := newJTICache(dedup, nil)
	defer c1.Stop()
	if !c1.MarkSeen(context.Background(), "jti-1", exp) {
		t.Fatal("first redemption must be accepted")
	}

	// Simulate a Hub restart: fresh in-memory L1, same Redis L2.
	c2 := newJTICache(dedup, nil)
	defer c2.Stop()
	if c2.MarkSeen(context.Background(), "jti-1", exp) {
		t.Error("replay across restart must be REJECTED when the Redis L2 is wired (SEC-M4-03)")
	}
}

// TestJTICache_InMemoryOnly_ReplayableAcrossRestart documents the legacy
// (nil-dedup) behaviour the fix improves on: without the Redis L2, a fresh cache
// accepts the replay. This pins the contrast so a regression that drops the L2
// is visible.
func TestJTICache_InMemoryOnly_ReplayableAcrossRestart(t *testing.T) {
	exp := time.Now().Add(5 * time.Minute)
	c1 := newJTICache(nil, nil)
	defer c1.Stop()
	if !c1.MarkSeen(context.Background(), "jti-2", exp) {
		t.Fatal("first redemption must be accepted")
	}
	c2 := newJTICache(nil, nil) // "restart"
	defer c2.Stop()
	if !c2.MarkSeen(context.Background(), "jti-2", exp) {
		t.Error("in-memory-only cache accepts the replay across restart (legacy behaviour)")
	}
}

// TestJTICache_RedisError_DegradesToL1: a transient Redis SETNX error must not
// block enrollment — MarkSeen degrades to the L1 first-seen result.
func TestJTICache_RedisError_DegradesToL1(t *testing.T) {
	dedup := newFakeDedup()
	dedup.failNow = true
	c := newJTICache(dedup, nil)
	defer c.Stop()
	if !c.MarkSeen(context.Background(), "jti-3", time.Now().Add(time.Minute)) {
		t.Error("a Redis error must degrade to the L1 guard (accept), not block enrollment")
	}
	// L1 now holds it, so an in-process replay is still caught.
	if c.MarkSeen(context.Background(), "jti-3", time.Now().Add(time.Minute)) {
		t.Error("L1 must still reject an in-process replay after the Redis error")
	}
}

// TestJTICache_RedisFirstSeenThenReplay: within one process, the Redis layer
// also rejects a replay whose L1 entry was swept away (exp in the past path is
// covered separately; here the second call hits the seen-in-Redis branch via a
// fresh cache).
func TestJTICache_RedisRejectsSecondHub(t *testing.T) {
	dedup := newFakeDedup()
	exp := time.Now().Add(5 * time.Minute)
	hubA := newJTICache(dedup, nil)
	defer hubA.Stop()
	hubB := newJTICache(dedup, nil) // a second Hub sharing the same Redis
	defer hubB.Stop()
	if !hubA.MarkSeen(context.Background(), "jti-4", exp) {
		t.Fatal("hubA first redemption accepted")
	}
	if hubB.MarkSeen(context.Background(), "jti-4", exp) {
		t.Error("hubB must reject the same JTI via the shared Redis dedup (multi-Hub HA)")
	}
}

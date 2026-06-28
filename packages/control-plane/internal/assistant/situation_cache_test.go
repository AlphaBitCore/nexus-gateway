package assistant

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
)

// countingSituation counts Snapshot calls so a cache hit (no inner call) is provable.
type countingSituation struct {
	calls  int32
	health string
}

func (s *countingSituation) Snapshot(context.Context) (agent.Situation, error) {
	atomic.AddInt32(&s.calls, 1)
	return agent.Situation{Health: s.health}, nil
}

// TestSituationCache_HitWithinTTL: the second turn within the TTL reuses the snapshot
// and makes NO inner call (the ~8 admin reads are skipped) — the cache win.
func TestSituationCache_HitWithinTTL(t *testing.T) {
	inner := &countingSituation{health: "ok"}
	cache := newSituationCache(time.Minute)
	cs := cachedSituation{inner: inner, cache: cache, key: "u1"}

	got, _ := cs.Snapshot(context.Background())
	if got.Health != "ok" {
		t.Fatalf("first snapshot = %q, want ok", got.Health)
	}
	got2, _ := cs.Snapshot(context.Background())
	if got2.Health != "ok" {
		t.Fatalf("cached snapshot = %q, want ok", got2.Health)
	}
	if n := atomic.LoadInt32(&inner.calls); n != 1 {
		t.Fatalf("inner Snapshot called %d times, want 1 (second turn must hit the cache)", n)
	}
}

// TestSituationCache_RebuildsAfterTTL: once the entry expires the next turn rebuilds
// (fresh inner call) so ambient state cannot go stale beyond the TTL.
func TestSituationCache_RebuildsAfterTTL(t *testing.T) {
	inner := &countingSituation{health: "ok"}
	cache := newSituationCache(time.Minute)
	now := time.Unix(1000, 0)
	cache.now = func() time.Time { return now }
	cs := cachedSituation{inner: inner, cache: cache, key: "u1"}

	cs.Snapshot(context.Background()) // miss → call 1, expiry = now+1m
	now = now.Add(61 * time.Second)   // past the TTL
	cs.Snapshot(context.Background()) // miss again → call 2
	if n := atomic.LoadInt32(&inner.calls); n != 2 {
		t.Fatalf("inner Snapshot called %d times, want 2 (expired entry must rebuild)", n)
	}
}

// TestSituationCache_PerCallerIsolation: two principals never share a snapshot, so one
// caller's IAM-scoped view can't be served to another.
func TestSituationCache_PerCallerIsolation(t *testing.T) {
	cache := newSituationCache(time.Minute)
	a := &countingSituation{health: "a-view"}
	b := &countingSituation{health: "b-view"}
	ca := cachedSituation{inner: a, cache: cache, key: "userA"}
	cb := cachedSituation{inner: b, cache: cache, key: "userB"}

	gotA, _ := ca.Snapshot(context.Background())
	gotB, _ := cb.Snapshot(context.Background())
	if gotA.Health != "a-view" || gotB.Health != "b-view" {
		t.Fatalf("per-caller views crossed: A=%q B=%q", gotA.Health, gotB.Health)
	}
	// A's second turn returns A's view, not B's.
	if g, _ := ca.Snapshot(context.Background()); g.Health != "a-view" {
		t.Fatalf("userA second snapshot = %q, want a-view (no cross-caller bleed)", g.Health)
	}
	if atomic.LoadInt32(&a.calls) != 1 || atomic.LoadInt32(&b.calls) != 1 {
		t.Fatalf("each caller must build exactly once, got a=%d b=%d", a.calls, b.calls)
	}
}

// TestSituationCache_DefaultTTL: a non-positive TTL falls back to situationTTL.
func TestSituationCache_DefaultTTL(t *testing.T) {
	if c := newSituationCache(0); c.ttl != situationTTL {
		t.Fatalf("zero TTL must default to situationTTL, got %v", c.ttl)
	}
}

// erroringSituation always fails — used to prove an error is propagated and NOT
// cached (the kernel never errors, but the interface allows it).
type erroringSituation struct{ err error }

func (s erroringSituation) Snapshot(context.Context) (agent.Situation, error) {
	return agent.Situation{}, s.err
}

// TestSituationCache_DoesNotCacheError: an inner error is returned to the caller and
// nothing is stored, so the next turn retries rather than serving a bad snapshot.
func TestSituationCache_DoesNotCacheError(t *testing.T) {
	cache := newSituationCache(time.Minute)
	cs := cachedSituation{inner: erroringSituation{err: errTest}, cache: cache, key: "u1"}
	if _, err := cs.Snapshot(context.Background()); err == nil {
		t.Fatal("an inner error must propagate")
	}
	if _, ok := cache.get("u1"); ok {
		t.Fatal("an errored snapshot must NOT be cached")
	}
}

// TestSituationCache_ConcurrentAccess: the cache is safe under concurrent turns for
// the same caller (the registry mutex serialises get/put).
func TestSituationCache_ConcurrentAccess(t *testing.T) {
	inner := &countingSituation{health: "ok"}
	cache := newSituationCache(time.Minute)
	cs := cachedSituation{inner: inner, cache: cache, key: "u1"}
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if g, _ := cs.Snapshot(context.Background()); g.Health != "ok" {
				t.Errorf("concurrent snapshot = %q, want ok", g.Health)
			}
		}()
	}
	wg.Wait()
}

package semantic

import (
	"context"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/semantic/internal/testredis"
)

func newTestPoisonList(t *testing.T) (*RedisPoisonList, func()) {
	t.Helper()
	_, rdb, cleanup := testredis.NewMiniValkey(t)
	return NewRedisPoisonList(rdb), cleanup
}

func TestRedisPoisonList_AddAndIsPoisoned(t *testing.T) {
	pl, cleanup := newTestPoisonList(t)
	defer cleanup()

	ctx := context.Background()
	entryKey := "idx:abc123"
	vkScope := "v1:vk:42"

	// Not poisoned initially.
	ok, err := pl.IsPoisoned(ctx, entryKey, vkScope)
	if err != nil {
		t.Fatalf("IsPoisoned (pre-add) error: %v", err)
	}
	if ok {
		t.Fatal("IsPoisoned should be false before Add")
	}

	// Add with a 1-second TTL → poison TTL = 10s.
	if err := pl.Add(ctx, entryKey, vkScope, time.Second); err != nil {
		t.Fatalf("Add error: %v", err)
	}

	// Now it is poisoned.
	ok, err = pl.IsPoisoned(ctx, entryKey, vkScope)
	if err != nil {
		t.Fatalf("IsPoisoned (post-add) error: %v", err)
	}
	if !ok {
		t.Fatal("IsPoisoned should be true after Add")
	}
}

func TestRedisPoisonList_ZeroTTLUsesDefault(t *testing.T) {
	pl, cleanup := newTestPoisonList(t)
	defer cleanup()

	ctx := context.Background()
	if err := pl.Add(ctx, "key1", "scope1", 0); err != nil {
		t.Fatalf("Add with zero TTL error: %v", err)
	}
	ok, err := pl.IsPoisoned(ctx, "key1", "scope1")
	if err != nil || !ok {
		t.Fatalf("should be poisoned; err=%v, ok=%v", err, ok)
	}
}

func TestRedisPoisonList_TTLCap(t *testing.T) {
	pl, cleanup := newTestPoisonList(t)
	defer cleanup()

	ctx := context.Background()
	// Very long TTL: should be capped at 30 days.
	bigTTL := 365 * 24 * time.Hour
	if err := pl.Add(ctx, "key2", "scope2", bigTTL); err != nil {
		t.Fatalf("Add error: %v", err)
	}
	ok, err := pl.IsPoisoned(ctx, "key2", "scope2")
	if err != nil || !ok {
		t.Fatalf("should be poisoned after big TTL; err=%v ok=%v", err, ok)
	}
}

func TestRedisPoisonList_ScopeIsolation(t *testing.T) {
	pl, cleanup := newTestPoisonList(t)
	defer cleanup()

	ctx := context.Background()
	// Poison under scope A.
	if err := pl.Add(ctx, "keyX", "scopeA", time.Minute); err != nil {
		t.Fatalf("Add error: %v", err)
	}

	// Same key, different scope should NOT be poisoned.
	ok, err := pl.IsPoisoned(ctx, "keyX", "scopeB")
	if err != nil {
		t.Fatalf("IsPoisoned error: %v", err)
	}
	if ok {
		t.Fatal("keyX under scopeB should not be poisoned")
	}
}

// TestRedisPoisonList_TTLEviction verifies that a poisoned entry expires after
// its TTL. We add with a 1s TTL (poison TTL = 10s), then use miniredis
// FastForward to advance the clock past 10s — no real sleep required.
func TestRedisPoisonList_TTLEviction(t *testing.T) {
	_, rdb, mr, cleanup := testredis.NewMiniValkeyWithServer(t)
	defer cleanup()
	pl := NewRedisPoisonList(rdb)

	ctx := context.Background()
	if err := pl.Add(ctx, "expkey", "scope", time.Second); err != nil {
		t.Fatalf("Add error: %v", err)
	}

	// Confirm it is poisoned immediately.
	ok, err := pl.IsPoisoned(ctx, "expkey", "scope")
	if err != nil || !ok {
		t.Fatalf("should be poisoned immediately; err=%v ok=%v", err, ok)
	}

	// Advance the clock past poison TTL (1s × 10 = 10s).
	mr.FastForward(11 * time.Second)

	ok, err = pl.IsPoisoned(ctx, "expkey", "scope")
	if err != nil {
		t.Fatalf("IsPoisoned after expiry error: %v", err)
	}
	if ok {
		t.Fatal("entry should have expired by now")
	}
}

// TestRedisPoisonList_IsPoisoned_ErrorPath verifies that IsPoisoned propagates
// a Redis error (false, err) when the underlying client fails.
func TestRedisPoisonList_IsPoisoned_ErrorPath(t *testing.T) {
	_, rdb, cleanup := testredis.NewMiniValkey(t)
	defer cleanup()
	pl := NewRedisPoisonList(rdb)
	// Close the client so the next command fails.
	_ = rdb.Close()

	ctx := context.Background()
	ok, err := pl.IsPoisoned(ctx, "somekey", "somescope")
	if err == nil {
		t.Fatal("expected error from IsPoisoned on closed client")
	}
	if ok {
		t.Fatal("IsPoisoned should return false on error")
	}
}

func TestNopPoisonList_AlwaysFalse(t *testing.T) {
	var n nopPoisonList
	ctx := context.Background()
	ok, err := n.IsPoisoned(ctx, "k", "s")
	if err != nil || ok {
		t.Fatalf("nopPoisonList.IsPoisoned should return (false,nil); got (%v,%v)", ok, err)
	}
	if err := n.Add(ctx, "k", "s", time.Minute); err != nil {
		t.Fatalf("nopPoisonList.Add should not error; got %v", err)
	}
}

func TestPoisonKey_Format(t *testing.T) {
	k := poisonKey("idx:abc", "vk:1")
	want := "nexus:l2:poison:vk:1:idx:abc"
	if k != want {
		t.Errorf("poisonKey = %q, want %q", k, want)
	}
}

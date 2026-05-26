package store

import (
	"testing"
	"time"
)

// White-box tests that directly invoke each in-memory store's sweep()
// method. Sweep is the janitor's mutation path that reclaims expired
// entries from the underlying map; production calls it on a ticker
// driven by janitorInterval(ttl), which clamps to either ttl/5 or
// a 1-second floor — so binding (5min TTL → 60s interval) and pending
// (10min TTL → 120s interval) cannot be exercised within a unit test
// window via the ticker. Calling sweep() directly is the supported
// way to assert the conditional + delete contract: live entries
// survive, expired entries are dropped, and the operation is safe to
// call concurrently with the data-path methods because both take the
// same mu.

// TestAuthCodeStore_SweepDirect asserts sweep evicts expired entries
// and leaves live entries alone.
func TestAuthCodeStore_SweepDirect(t *testing.T) {
	s := NewAuthCodeStore(time.Hour)
	defer s.Close()

	s.Put("live", AuthCodeEntry{ExpiresAt: time.Now().Add(time.Hour)})
	s.Put("stale", AuthCodeEntry{ExpiresAt: time.Now().Add(-time.Hour)})

	s.sweep()

	// After sweep, "live" survives, "stale" is gone.
	s.mu.Lock()
	_, liveOK := s.data["live"]
	_, staleOK := s.data["stale"]
	s.mu.Unlock()
	if !liveOK {
		t.Fatal("sweep should preserve live entry")
	}
	if staleOK {
		t.Fatal("sweep should evict expired entry")
	}
}

// TestBindingStore_SweepDirect asserts sweep evicts expired binding
// handles and preserves live ones. Production calls sweep from the
// janitor on a 60s tick; this test exercises the same code path.
func TestBindingStore_SweepDirect(t *testing.T) {
	s := NewBindingStore()
	defer s.Close()

	s.Put("live", BindingEntry{ExpiresAt: time.Now().Add(time.Hour)})
	s.Put("stale", BindingEntry{ExpiresAt: time.Now().Add(-time.Hour)})

	s.sweep()

	s.mu.Lock()
	_, liveOK := s.data["live"]
	_, staleOK := s.data["stale"]
	s.mu.Unlock()
	if !liveOK {
		t.Fatal("sweep should preserve live binding")
	}
	if staleOK {
		t.Fatal("sweep should evict expired binding")
	}
}

// TestPendingAuthzStore_SweepDirect mirrors the sweep contract for
// the pending-authorize store: live entries kept, expired entries
// dropped from the underlying map.
func TestPendingAuthzStore_SweepDirect(t *testing.T) {
	s := NewPendingAuthzStore()
	defer s.Close()

	s.Put("live", PendingAuthzEntry{ExpiresAt: time.Now().Add(time.Hour)})
	s.Put("stale", PendingAuthzEntry{ExpiresAt: time.Now().Add(-time.Hour)})

	s.sweep()

	s.mu.Lock()
	_, liveOK := s.data["live"]
	_, staleOK := s.data["stale"]
	s.mu.Unlock()
	if !liveOK {
		t.Fatal("sweep should preserve live pending entry")
	}
	if staleOK {
		t.Fatal("sweep should evict expired pending entry")
	}
}

// TestJanitorInterval_ClampedToOneSecondFloor asserts the documented
// floor — tiny TTLs do not produce a sub-second ticker that would
// burn CPU. This guards against accidental regression of the floor.
func TestJanitorInterval_ClampedToOneSecondFloor(t *testing.T) {
	if got := janitorInterval(10 * time.Millisecond); got != time.Second {
		t.Fatalf("janitorInterval(10ms) = %v, want 1s floor", got)
	}
	if got := janitorInterval(time.Second); got != time.Second {
		t.Fatalf("janitorInterval(1s) = %v, want 1s floor", got)
	}
}

// TestJanitorInterval_UsesFifthOfTTLWhenAboveFloor asserts that for
// larger TTLs the cadence scales to ttl/5 so a long-TTL store does
// not waste sweeps.
func TestJanitorInterval_UsesFifthOfTTLWhenAboveFloor(t *testing.T) {
	if got := janitorInterval(10 * time.Second); got != 2*time.Second {
		t.Fatalf("janitorInterval(10s) = %v, want 2s", got)
	}
	if got := janitorInterval(5 * time.Minute); got != time.Minute {
		t.Fatalf("janitorInterval(5min) = %v, want 1min", got)
	}
}

package store_test

import (
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
)

// TestPendingAuthzStore_HasLiveEntry asserts Has returns true for a live
// non-expired entry without consuming it (Take after Has must still work).
func TestPendingAuthzStore_HasLiveEntry(t *testing.T) {
	s := store.NewPendingAuthzStore()
	defer s.Close()

	s.Put("alive", store.PendingAuthzEntry{
		ClientID:  "agent-desktop",
		ExpiresAt: time.Now().Add(time.Minute),
	})

	if !s.Has("alive") {
		t.Fatal("Has should report true for a live entry")
	}
	// Take must still succeed — Has is non-consuming.
	if _, ok := s.Take("alive"); !ok {
		t.Fatal("Take after Has must still succeed")
	}
}

// TestPendingAuthzStore_HasMissing asserts Has returns false when the
// authctx was never Put.
func TestPendingAuthzStore_HasMissing(t *testing.T) {
	s := store.NewPendingAuthzStore()
	defer s.Close()

	if s.Has("never") {
		t.Fatal("Has should report false for unknown authctx")
	}
}

// TestPendingAuthzStore_HasExpiredSweeps asserts Has reports false for
// an expired entry AND removes it lazily so a subsequent Take cannot
// resurrect the stale row.
func TestPendingAuthzStore_HasExpiredSweeps(t *testing.T) {
	s := store.NewPendingAuthzStore()
	defer s.Close()

	s.Put("stale", store.PendingAuthzEntry{
		ExpiresAt: time.Now().Add(-time.Second), // already expired
	})

	if s.Has("stale") {
		t.Fatal("Has should report false for expired entry")
	}
	if _, ok := s.Take("stale"); ok {
		t.Fatal("Take must not resurrect a swept-by-Has entry")
	}
}

// TestPendingAuthzStore_SetIdPID_Live asserts SetIdPID mutates the
// stamped IdPID on a live entry and reports true. The subsequent Take
// must observe the new IdPID.
func TestPendingAuthzStore_SetIdPID_Live(t *testing.T) {
	s := store.NewPendingAuthzStore()
	defer s.Close()

	s.Put("ctx", store.PendingAuthzEntry{
		ClientID:  "agent-desktop",
		ExpiresAt: time.Now().Add(time.Minute),
	})
	if ok := s.SetIdPID("ctx", "idp_okta"); !ok {
		t.Fatal("SetIdPID should report true for a live entry")
	}
	e, ok := s.Take("ctx")
	if !ok || e.IdPID != "idp_okta" {
		t.Fatalf("Take after SetIdPID: ok=%v IdPID=%q", ok, e.IdPID)
	}
}

// TestPendingAuthzStore_SetIdPID_Missing asserts SetIdPID reports false
// when the authctx is unknown — the OIDC begin handler relies on this
// to refuse to begin a flow that the user never started.
func TestPendingAuthzStore_SetIdPID_Missing(t *testing.T) {
	s := store.NewPendingAuthzStore()
	defer s.Close()

	if s.SetIdPID("unknown", "idp_x") {
		t.Fatal("SetIdPID on unknown authctx must report false")
	}
}

// TestPendingAuthzStore_SetIdPID_ExpiredSweeps asserts SetIdPID
// reports false for an expired entry AND removes it lazily.
func TestPendingAuthzStore_SetIdPID_ExpiredSweeps(t *testing.T) {
	s := store.NewPendingAuthzStore()
	defer s.Close()

	s.Put("stale", store.PendingAuthzEntry{
		ExpiresAt: time.Now().Add(-time.Second),
	})
	if s.SetIdPID("stale", "idp_x") {
		t.Fatal("SetIdPID on expired entry must report false")
	}
	// Subsequent Take must NOT find the swept entry.
	if _, ok := s.Take("stale"); ok {
		t.Fatal("expired entry must be evicted after SetIdPID returns false")
	}
}

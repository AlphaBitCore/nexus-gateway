package exemption

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/identity"
)

// TestRebuild_ReplacesContents verifies Rebuild replaces the entire items
// map atomically — entries present before the call must be gone after.
func TestRebuild_ReplacesContents(t *testing.T) {
	s := NewStore(slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Pre-populate with a stale entry.
	s.Add("10.0.0.1", "old.example.com", time.Hour, "old", "admin")
	if got := len(s.List()); got != 1 {
		t.Fatalf("precondition: expected 1 exemption, got %d", got)
	}

	future := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	s.Rebuild([]identity.ActiveExemption{
		{
			ID:         "e-new-1",
			SourceIP:   "10.0.0.2",
			TargetHost: "new.example.com",
			ExpiresAt:  future,
			Reason:     "shadow-synced",
			ApprovedBy: "alice",
		},
	})

	got := s.List()
	if len(got) != 1 {
		t.Fatalf("expected 1 exemption after Rebuild, got %d", len(got))
	}
	e := got[0]
	if e.ID != "e-new-1" {
		t.Errorf("ID = %q, want %q", e.ID, "e-new-1")
	}
	if e.SourceIP != "10.0.0.2" {
		t.Errorf("SourceIP = %q, want %q", e.SourceIP, "10.0.0.2")
	}
	if e.TargetHost != "new.example.com" {
		t.Errorf("TargetHost = %q, want %q", e.TargetHost, "new.example.com")
	}
	if e.Reason != "shadow-synced" {
		t.Errorf("Reason = %q, want %q", e.Reason, "shadow-synced")
	}
	// ApprovedBy maps to CreatedBy internally.
	if e.CreatedBy != "alice" {
		t.Errorf("CreatedBy = %q, want %q", e.CreatedBy, "alice")
	}
	if e.CreatedAt.IsZero() {
		t.Errorf("CreatedAt is zero; Rebuild should stamp it with time.Now()")
	}
}

// TestRebuild_DropsExpired verifies entries whose ExpiresAt is in the past
// are filtered out at Rebuild time.
func TestRebuild_DropsExpired(t *testing.T) {
	s := NewStore(slog.New(slog.NewTextHandler(io.Discard, nil)))

	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	future := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)

	s.Rebuild([]identity.ActiveExemption{
		{ID: "expired", SourceIP: "10.0.0.1", TargetHost: "a", ExpiresAt: past},
		{ID: "active", SourceIP: "10.0.0.2", TargetHost: "b", ExpiresAt: future},
	})

	got := s.List()
	if len(got) != 1 {
		t.Fatalf("expected 1 active exemption, got %d", len(got))
	}
	if got[0].ID != "active" {
		t.Errorf("ID = %q, want %q", got[0].ID, "active")
	}
}

// TestRebuild_DropsUnparseableExpiry verifies entries with invalid ExpiresAt
// strings are silently dropped rather than panicking.
func TestRebuild_DropsUnparseableExpiry(t *testing.T) {
	s := NewStore(slog.New(slog.NewTextHandler(io.Discard, nil)))

	future := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	s.Rebuild([]identity.ActiveExemption{
		{ID: "bad", SourceIP: "10.0.0.1", TargetHost: "a", ExpiresAt: "not-a-date"},
		{ID: "good", SourceIP: "10.0.0.2", TargetHost: "b", ExpiresAt: future},
	})

	got := s.List()
	if len(got) != 1 {
		t.Fatalf("expected 1 exemption, got %d", len(got))
	}
	if got[0].ID != "good" {
		t.Errorf("ID = %q, want %q", got[0].ID, "good")
	}
}

// TestRebuild_EmptyClearsAll verifies passing an empty slice wipes the store.
func TestRebuild_EmptyClearsAll(t *testing.T) {
	s := NewStore(slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.Add("10.0.0.1", "x", time.Hour, "r", "u")

	s.Rebuild([]identity.ActiveExemption{})

	if got := len(s.List()); got != 0 {
		t.Errorf("expected empty store after Rebuild(nil), got %d entries", got)
	}
}

// TestRebuild_SnapshotRoundTrip verifies Rebuild and Snapshot are
// inverses for the fields that cross the shadow boundary.
func TestRebuild_SnapshotRoundTrip(t *testing.T) {
	s := NewStore(slog.New(slog.NewTextHandler(io.Discard, nil)))
	future := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)

	in := []identity.ActiveExemption{
		{
			ID:         "rt",
			SourceIP:   "10.0.0.7",
			TargetHost: "api.example.com",
			ExpiresAt:  future,
			Reason:     "ticket-42",
			ApprovedBy: "carol",
			Disabled:   true,
		},
	}
	s.Rebuild(in)

	snap := s.Snapshot()
	if len(snap.Entries) != 1 {
		t.Fatalf("snapshot entries = %d, want 1", len(snap.Entries))
	}
	out := snap.Entries[0]
	if out.ID != in[0].ID || out.SourceIP != in[0].SourceIP ||
		out.TargetHost != in[0].TargetHost || out.Reason != in[0].Reason ||
		out.ApprovedBy != in[0].ApprovedBy || out.Disabled != in[0].Disabled {
		t.Errorf("round-trip mismatch: got %+v, want %+v", out, in[0])
	}
}

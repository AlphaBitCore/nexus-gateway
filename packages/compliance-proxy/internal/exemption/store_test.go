package exemption

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/identity"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestStore_AddAndList(t *testing.T) {
	s := NewStore(testLogger())

	e := s.Add("10.0.0.1", "api.openai.com", 1*time.Hour, "test", "admin")
	if e == nil {
		t.Fatal("Add returned nil")
	}
	if e.ID == "" {
		t.Fatal("ID should be set")
	}
	if e.SourceIP != "10.0.0.1" {
		t.Fatalf("SourceIP = %q, want 10.0.0.1", e.SourceIP)
	}
	if e.TargetHost != "api.openai.com" {
		t.Fatalf("TargetHost = %q, want api.openai.com", e.TargetHost)
	}
	if e.Reason != "test" {
		t.Fatalf("Reason = %q, want test", e.Reason)
	}
	if e.CreatedBy != "admin" {
		t.Fatalf("CreatedBy = %q, want admin", e.CreatedBy)
	}
	if e.ExpiresAt.Before(time.Now()) {
		t.Fatal("ExpiresAt should be in the future")
	}

	list := s.List()
	if len(list) != 1 {
		t.Fatalf("List() len = %d, want 1", len(list))
	}
	if list[0].ID != e.ID {
		t.Fatalf("List()[0].ID = %q, want %q", list[0].ID, e.ID)
	}
}

func TestStore_Remove(t *testing.T) {
	s := NewStore(testLogger())
	e := s.Add("10.0.0.1", "api.openai.com", 1*time.Hour, "test", "admin")

	// Remove existing.
	if !s.Remove(e.ID) {
		t.Fatal("Remove should return true for existing exemption")
	}
	if len(s.List()) != 0 {
		t.Fatal("List should be empty after Remove")
	}

	// Remove non-existing.
	if s.Remove("nonexistent-id") {
		t.Fatal("Remove should return false for non-existing exemption")
	}
}

func TestStore_IsExempt_ExactMatch(t *testing.T) {
	s := NewStore(testLogger())
	s.Add("10.0.0.1", "api.openai.com", 1*time.Hour, "test", "admin")

	// Exact match.
	exempt, e := s.IsExempt("10.0.0.1", "api.openai.com")
	if !exempt || e == nil {
		t.Fatal("should be exempt for exact match")
	}

	// Different IP.
	exempt, _ = s.IsExempt("10.0.0.2", "api.openai.com")
	if exempt {
		t.Fatal("should not be exempt for different IP")
	}

	// Different host.
	exempt, _ = s.IsExempt("10.0.0.1", "api.anthropic.com")
	if exempt {
		t.Fatal("should not be exempt for different host")
	}
}

func TestStore_IsExempt_CIDRMatch(t *testing.T) {
	s := NewStore(testLogger())
	s.Add("10.0.0.0/24", "api.openai.com", 1*time.Hour, "test", "admin")

	// IP within CIDR.
	exempt, e := s.IsExempt("10.0.0.5", "api.openai.com")
	if !exempt || e == nil {
		t.Fatal("10.0.0.5 should match 10.0.0.0/24")
	}

	// IP at boundary.
	exempt, _ = s.IsExempt("10.0.0.255", "api.openai.com")
	if !exempt {
		t.Fatal("10.0.0.255 should match 10.0.0.0/24")
	}

	// IP outside CIDR.
	exempt, _ = s.IsExempt("10.0.1.1", "api.openai.com")
	if exempt {
		t.Fatal("10.0.1.1 should not match 10.0.0.0/24")
	}
}

func TestStore_IsExempt_WildcardHost(t *testing.T) {
	s := NewStore(testLogger())
	s.Add("10.0.0.1", "*.openai.com", 1*time.Hour, "test", "admin")

	// Subdomain match.
	exempt, e := s.IsExempt("10.0.0.1", "api.openai.com")
	if !exempt || e == nil {
		t.Fatal("api.openai.com should match *.openai.com")
	}

	// Deep subdomain match.
	exempt, _ = s.IsExempt("10.0.0.1", "v1.api.openai.com")
	if !exempt {
		t.Fatal("v1.api.openai.com should match *.openai.com")
	}

	// Apex domain must NOT match wildcard (standard wildcard semantics).
	exempt, _ = s.IsExempt("10.0.0.1", "openai.com")
	if exempt {
		t.Fatal("openai.com should NOT match *.openai.com (wildcard excludes apex)")
	}

	// Different domain.
	exempt, _ = s.IsExempt("10.0.0.1", "api.anthropic.com")
	if exempt {
		t.Fatal("api.anthropic.com should not match *.openai.com")
	}
}

func TestStore_IsExempt_WildcardSourceIP(t *testing.T) {
	s := NewStore(testLogger())
	s.Add("*", "api.openai.com", 1*time.Hour, "test", "admin")

	exempt, e := s.IsExempt("192.168.1.1", "api.openai.com")
	if !exempt || e == nil {
		t.Fatal("wildcard source should match any IP")
	}
}

func TestStore_IsExempt_WildcardTargetHost(t *testing.T) {
	s := NewStore(testLogger())
	s.Add("10.0.0.1", "*", 1*time.Hour, "test", "admin")

	exempt, e := s.IsExempt("10.0.0.1", "anything.example.com")
	if !exempt || e == nil {
		t.Fatal("wildcard target should match any host")
	}
}

func TestStore_DisabledSkipsMatch(t *testing.T) {
	s := NewStore(testLogger())
	future := time.Now().Add(1 * time.Hour).Format(time.RFC3339)
	s.Rebuild([]identity.ActiveExemption{
		{ID: "e1", SourceIP: "10.0.0.1", TargetHost: "api.openai.com", ExpiresAt: future, Reason: "r", ApprovedBy: "a", Disabled: true},
	})
	exempt, _ := s.IsExempt("10.0.0.1", "api.openai.com")
	if exempt {
		t.Fatal("disabled exemption must not match")
	}
	s.Rebuild([]identity.ActiveExemption{
		{ID: "e1", SourceIP: "10.0.0.1", TargetHost: "api.openai.com", ExpiresAt: future, Reason: "r", ApprovedBy: "a", Disabled: false},
	})
	exempt2, _ := s.IsExempt("10.0.0.1", "api.openai.com")
	if !exempt2 {
		t.Fatal("enabled exemption should match")
	}
}

func TestStore_Expiry(t *testing.T) {
	s := NewStore(testLogger())
	s.Add("10.0.0.1", "api.openai.com", 1*time.Millisecond, "test", "admin")

	// Sleep past expiry.
	time.Sleep(10 * time.Millisecond)

	// Expired exemption should not appear in list.
	list := s.List()
	if len(list) != 0 {
		t.Fatalf("List() len = %d, want 0 (expired)", len(list))
	}

	// Expired exemption should not match.
	exempt, _ := s.IsExempt("10.0.0.1", "api.openai.com")
	if exempt {
		t.Fatal("expired exemption should not match")
	}
}

func TestStore_Expiry_Cleanup(t *testing.T) {
	s := NewStore(testLogger())
	s.Add("10.0.0.1", "api.openai.com", 1*time.Millisecond, "test", "admin")

	time.Sleep(10 * time.Millisecond)

	// Run cleanup.
	ctx, cancel := context.WithCancel(context.Background())
	s.StartCleanup(ctx, 5*time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	cancel()

	// Store should have purged the expired entry.
	s.mu.RLock()
	count := len(s.items)
	s.mu.RUnlock()
	if count != 0 {
		t.Fatalf("items count = %d after cleanup, want 0", count)
	}
}

func TestStore_Concurrent(t *testing.T) {
	s := NewStore(testLogger())
	const goroutines = 50

	var wg sync.WaitGroup
	wg.Add(goroutines * 3)

	// Concurrent adds.
	for range goroutines {
		go func() {
			defer wg.Done()
			s.Add("10.0.0.1", "api.openai.com", 1*time.Hour, "concurrent test", "admin")
		}()
	}

	// Concurrent reads.
	for range goroutines {
		go func() {
			defer wg.Done()
			s.IsExempt("10.0.0.1", "api.openai.com")
		}()
	}

	// Concurrent lists.
	for range goroutines {
		go func() {
			defer wg.Done()
			s.List()
		}()
	}

	wg.Wait()

	list := s.List()
	if len(list) != goroutines {
		t.Fatalf("List() len = %d, want %d", len(list), goroutines)
	}
}

// TestStore_IsExempt_FutureEffectiveFromSkips covers the
// `!eff.IsZero() && eff.After(now)` skip branch in IsExempt — without
// it, an exemption that's been approved but not yet active would still
// match and let traffic bypass the pipeline early.
func TestStore_IsExempt_FutureEffectiveFromSkips(t *testing.T) {
	s := NewStore(testLogger())
	now := time.Now()
	s.Rebuild([]identity.ActiveExemption{{
		ID:            "future-1",
		SourceIP:      "10.0.0.1",
		TargetHost:    "api.example.com",
		ExpiresAt:     now.Add(2 * time.Hour).Format(time.RFC3339),
		EffectiveFrom: now.Add(1 * time.Hour).Format(time.RFC3339), // not yet effective
	}})

	exempt, _ := s.IsExempt("10.0.0.1", "api.example.com")
	if exempt {
		t.Fatal("entry with future EffectiveFrom must not match")
	}
}

// TestRebuild_DropsFutureEffectiveFrom covers the
// `if !eff.IsZero() && eff.After(now) { dropped++; continue }` branch
// in Rebuild. Snapshot must reflect the drop.
//
// Wait — actually this isn't a drop on Rebuild, it's a runtime skip in
// IsExempt. Re-reading: store.go:107-109 IS a Rebuild drop. So an entry
// with future EffectiveFrom must NOT appear in the resulting items map.
func TestRebuild_DropsFutureEffectiveFromAtApplyTime(t *testing.T) {
	s := NewStore(testLogger())
	now := time.Now()
	s.Rebuild([]identity.ActiveExemption{{
		ID:            "future-1",
		SourceIP:      "10.0.0.1",
		TargetHost:    "api.example.com",
		ExpiresAt:     now.Add(2 * time.Hour).Format(time.RFC3339),
		EffectiveFrom: now.Add(1 * time.Hour).Format(time.RFC3339),
	}})
	// Rebuild's drop semantics: not-yet-effective entries are dropped at
	// apply time so the store size is 0. This matches the Hub-shadow
	// contract that "active" exemptions are pre-filtered.
	if got := len(s.List()); got != 0 {
		t.Errorf("future EffectiveFrom must be dropped at Rebuild, got %d entries", got)
	}
}

// TestRebuild_KeepsValidEffectiveFromAndSurfacesInSnapshot covers two
// branches together: Rebuild parses EffectiveFrom successfully (line
// 102-105), and Snapshot writes back the EffectiveFrom field
// (line 174-176).
func TestRebuild_KeepsValidEffectiveFromAndSurfacesInSnapshot(t *testing.T) {
	s := NewStore(testLogger())
	now := time.Now()
	effRFC := now.Add(-1 * time.Hour).UTC().Format(time.RFC3339) // already effective
	s.Rebuild([]identity.ActiveExemption{{
		ID:            "eff-1",
		SourceIP:      "10.0.0.1",
		TargetHost:    "api.example.com",
		ExpiresAt:     now.Add(2 * time.Hour).Format(time.RFC3339),
		EffectiveFrom: effRFC,
	}})

	snap := s.Snapshot()
	if len(snap.Entries) != 1 {
		t.Fatalf("expected 1 snapshot entry, got %d", len(snap.Entries))
	}
	if snap.Entries[0].EffectiveFrom == "" {
		t.Errorf("Snapshot must round-trip EffectiveFrom for entries that carry it")
	}
}

// TestRebuild_DropsUnparseableEffectiveFromKeepsEntry covers the
// silent-drop of an unparseable EffectiveFrom — the entry itself is
// still kept (eff stays zero) and matched without the gate.
func TestRebuild_DropsUnparseableEffectiveFromKeepsEntry(t *testing.T) {
	s := NewStore(testLogger())
	now := time.Now()
	s.Rebuild([]identity.ActiveExemption{{
		ID:            "bad-eff",
		SourceIP:      "10.0.0.1",
		TargetHost:    "api.example.com",
		ExpiresAt:     now.Add(2 * time.Hour).Format(time.RFC3339),
		EffectiveFrom: "not-a-timestamp", // parse fails → eff stays zero
	}})
	if got := len(s.List()); got != 1 {
		t.Fatalf("unparseable EffectiveFrom must keep entry (eff stays zero), got %d", got)
	}
	exempt, _ := s.IsExempt("10.0.0.1", "api.example.com")
	if !exempt {
		t.Error("entry with zero EffectiveFrom must still match")
	}
}

// TestSnapshot_SkipsExpiredEntries covers the `e.ExpiresAt.Before(now)`
// continue in Snapshot. Without this, an item that expired between the
// last cleanup tick and the read would leak into the runtime-config
// surface.
func TestSnapshot_SkipsExpiredEntries(t *testing.T) {
	s := NewStore(testLogger())
	// Bypass Rebuild's drop-past-expiry filter by writing into items
	// directly — we want an expired item to exist at Snapshot time.
	s.mu.Lock()
	s.items["expired-1"] = &Exemption{
		ID:         "expired-1",
		SourceIP:   "10.0.0.1",
		TargetHost: "api.example.com",
		ExpiresAt:  time.Now().Add(-1 * time.Hour),
	}
	s.mu.Unlock()

	snap := s.Snapshot()
	if len(snap.Entries) != 0 {
		t.Errorf("Snapshot must skip expired entries, got %d", len(snap.Entries))
	}
}

// TestMatchSourceIP_InvalidInputsReturnFalse covers the three error
// branches in matchSourceIP: invalid CIDR string, invalid client IP
// inside a valid CIDR, and one-side-nil exact-IP compare.
func TestMatchSourceIP_InvalidInputsReturnFalse(t *testing.T) {
	cases := []struct {
		name      string
		spec      string
		clientIP  string
		wantMatch bool
	}{
		{"invalid cidr spec", "10.0.0.0/notanumber", "10.0.0.1", false},
		{"valid cidr but invalid client ip", "10.0.0.0/24", "not-an-ip", false},
		{"valid spec ip but invalid client ip", "10.0.0.1", "not-an-ip", false},
		{"invalid spec ip exact", "not-an-ip", "10.0.0.1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchSourceIP(tc.spec, tc.clientIP); got != tc.wantMatch {
				t.Errorf("matchSourceIP(%q, %q) = %v, want %v", tc.spec, tc.clientIP, got, tc.wantMatch)
			}
		})
	}
}

func TestStore_CaseSensitiveHost(t *testing.T) {
	s := NewStore(testLogger())
	s.Add("10.0.0.1", "API.OpenAI.COM", 1*time.Hour, "test", "admin")

	// Case-insensitive match.
	exempt, _ := s.IsExempt("10.0.0.1", "api.openai.com")
	if !exempt {
		t.Fatal("host matching should be case-insensitive")
	}
}

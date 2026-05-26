package quota

import (
	"context"
	"testing"
)

// TestPolicyCache_SetPoliciesForTest_RoundTrip pins the test-only seam:
// write through SetPoliciesForTest, read back through FindPolicy + the
// flat PolicySnapshot read.
func TestPolicyCache_SetPoliciesForTest_RoundTrip(t *testing.T) {
	pc := NewPolicyCache(nil, nil)
	pc.SetPoliciesForTest(map[string][]CachedPolicy{
		"virtual_key": {{ID: "p1", Scope: "virtual_key"}},
		"organization": {
			{ID: "p2a", Scope: "organization"},
			{ID: "p2b", Scope: "organization"},
		},
	})

	if got := pc.FindPolicy("virtual_key", "", ""); got == nil || got.ID != "p1" {
		t.Errorf("FindPolicy virtual_key: got %+v, want id=p1", got)
	}
	all := pc.PolicySnapshot()
	if len(all) != 3 {
		t.Errorf("PolicySnapshot total = %d, want 3", len(all))
	}
}

// TestPolicyCache_SetOverridesForTest_RoundTrip pins nil-skip + read.
func TestPolicyCache_SetOverridesForTest_RoundTrip(t *testing.T) {
	pc := NewPolicyCache(nil, nil)
	pc.SetOverridesForTest(map[string]*CachedOverride{
		"virtual_key:vk-1": {TargetType: "virtual_key", TargetID: "vk-1", CostLimitCents: 5000},
		"organization:org-1": {
			TargetType: "organization", TargetID: "org-1", CostLimitCents: 10000,
		},
		"organization:nil-skip": nil, // exercises the nil-skip arm
	})

	if got := pc.OverrideSnapshot(); len(got) != 2 {
		t.Errorf("nil entry not skipped: got %d, want 2", len(got))
	}
	if got := pc.GetOverride("virtual_key", "vk-1"); got == nil || got.CostLimitCents != 5000 {
		t.Errorf("GetOverride round-trip: got %+v, want CostLimitCents=5000", got)
	}
}

// TestPolicyCache_SetOrgParentsForTest_RoundTrip pins org-hierarchy seed.
func TestPolicyCache_SetOrgParentsForTest_RoundTrip(t *testing.T) {
	pc := NewPolicyCache(nil, nil)
	pc.SetOrgParentsForTest(map[string]string{
		"org-child":      "org-parent",
		"org-grandchild": "org-child",
	})
	parents := pc.OrgParents()
	if parents["org-child"] != "org-parent" || parents["org-grandchild"] != "org-child" {
		t.Errorf("OrgParents round-trip failed: %v", parents)
	}
}

// TestUsageCache_SetUsageForTest_InMemoryRoundTrip — in-memory cache
// (rdb == nil) seeds + reads through GetUsage.
func TestUsageCache_SetUsageForTest_InMemoryRoundTrip(t *testing.T) {
	uc := NewUsageCache(nil, nil)
	uc.SetUsageForTest("virtual_key", "vk-1", "2026-05", 1234)

	ctx := context.Background()
	got, err := uc.GetUsage(ctx, "virtual_key", "vk-1", "2026-05")
	if err != nil {
		t.Fatalf("GetUsage err: %v", err)
	}
	if got != 1234 {
		t.Errorf("GetUsage = %d, want 1234", got)
	}

	// Missing key returns 0 (cold-start contract).
	got, err = uc.GetUsage(ctx, "virtual_key", "vk-missing", "2026-05")
	if err != nil {
		t.Fatalf("GetUsage missing: %v", err)
	}
	if got != 0 {
		t.Errorf("GetUsage missing = %d, want 0", got)
	}
}

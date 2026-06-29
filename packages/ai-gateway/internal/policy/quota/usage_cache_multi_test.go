package quota

// GetUsageMulti uses one MGET replacing the
// per-level GET loop. Locks the per-query alignment, missing→0, mixed-period
// keys, and per-element fail-open semantics.

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestGetUsageMulti_Redis(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	c := NewUsageCache(rdb, testLogger())
	ctx := context.Background()

	// Seed two keys (different periods), leave a third missing.
	mr.Set(usageKey("virtual_key", "vk1", "2026-06"), "150")
	mr.Set(usageKey("org", "org1", "2026-06-19"), "9000") // daily period

	q := []UsageQuery{
		{TargetType: "virtual_key", TargetID: "vk1", PeriodKey: "2026-06"},
		{TargetType: "org", TargetID: "org1", PeriodKey: "2026-06-19"},
		{TargetType: "project", TargetID: "p404", PeriodKey: "2026-06"}, // missing
	}
	usages, errs := c.GetUsageMulti(ctx, q)
	if len(usages) != 3 || len(errs) != 3 {
		t.Fatalf("len mismatch: usages=%d errs=%d", len(usages), len(errs))
	}
	for i, e := range errs {
		if e != nil {
			t.Fatalf("errs[%d] = %v, want nil", i, e)
		}
	}
	if usages[0] != 150 {
		t.Errorf("vk usage = %d, want 150", usages[0])
	}
	if usages[1] != 9000 {
		t.Errorf("org usage = %d, want 9000 (mixed daily period key)", usages[1])
	}
	if usages[2] != 0 {
		t.Errorf("missing key usage = %d, want 0 (cold start)", usages[2])
	}
}

func TestGetUsageMulti_ParseErrorIsPerLevel(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	c := NewUsageCache(rdb, testLogger())
	ctx := context.Background()

	mr.Set(usageKey("virtual_key", "good", "2026-06"), "42")
	mr.Set(usageKey("org", "bad", "2026-06"), "not-an-int")

	q := []UsageQuery{
		{TargetType: "virtual_key", TargetID: "good", PeriodKey: "2026-06"},
		{TargetType: "org", TargetID: "bad", PeriodKey: "2026-06"},
	}
	usages, errs := c.GetUsageMulti(ctx, q)
	if errs[0] != nil {
		t.Errorf("good level must not error, got %v", errs[0])
	}
	if usages[0] != 42 {
		t.Errorf("good usage = %d, want 42", usages[0])
	}
	if errs[1] == nil {
		t.Error("bad level must carry a per-level parse error (fail-open per level), got nil")
	}
	if usages[1] != 0 {
		t.Errorf("bad usage = %d, want 0", usages[1])
	}
}

func TestGetUsageMulti_InMemoryAndEmpty(t *testing.T) {
	c := NewUsageCache(nil, testLogger()) // in-memory fallback
	c.SetUsageForTest("virtual_key", "vk1", "2026-06", 77)
	ctx := context.Background()

	usages, errs := c.GetUsageMulti(ctx, []UsageQuery{
		{TargetType: "virtual_key", TargetID: "vk1", PeriodKey: "2026-06"},
		{TargetType: "org", TargetID: "missing", PeriodKey: "2026-06"},
	})
	if usages[0] != 77 || usages[1] != 0 || errs[0] != nil || errs[1] != nil {
		t.Errorf("in-memory multi = %v / %v, want [77 0] / [nil nil]", usages, errs)
	}

	// Empty query set must return empty slices, no panic.
	u, e := c.GetUsageMulti(ctx, nil)
	if len(u) != 0 || len(e) != 0 {
		t.Errorf("empty query: got %v / %v", u, e)
	}
}

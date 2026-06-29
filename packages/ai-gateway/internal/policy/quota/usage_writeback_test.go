package quota

import (
	"testing"
)

// TestUsageAggregator_AccumulatesPerKey verifies the write-behind aggregator
// sums increments per full usage key (which embeds targetType:targetID:periodKey)
// so two periods or two targets never cross-contaminate, carries the periodKey
// each key needs to re-apply its Expire on flush, and drain returns the
// accumulated deltas then resets to empty (so the next interval starts from 0 —
// the invariant that keeps Redis the convergent sum across N instances without
// double-counting).
func TestUsageAggregator_AccumulatesPerKey(t *testing.T) {
	a := newUsageAggregator()

	a.add("quota:usage:virtual_key:vk1:2026-06", "2026-06", 100)
	a.add("quota:usage:virtual_key:vk1:2026-06", "2026-06", 50) // same key -> 150
	a.add("quota:usage:org:org1:2026-06", "2026-06", 30)        // different target
	a.add("quota:usage:virtual_key:vk1:2026-07", "2026-07", 7)  // different period (rollover)

	got := a.drain()

	type ent struct {
		cents     int64
		periodKey string
	}
	want := map[string]ent{
		"quota:usage:virtual_key:vk1:2026-06": {150, "2026-06"},
		"quota:usage:org:org1:2026-06":        {30, "2026-06"},
		"quota:usage:virtual_key:vk1:2026-07": {7, "2026-07"},
	}
	if len(got) != len(want) {
		t.Fatalf("drain returned %d keys, want %d: %v", len(got), len(want), got)
	}
	for k, w := range want {
		if got[k].cents != w.cents {
			t.Errorf("drain[%q].cents = %d, want %d", k, got[k].cents, w.cents)
		}
		if got[k].periodKey != w.periodKey {
			t.Errorf("drain[%q].periodKey = %q, want %q", k, got[k].periodKey, w.periodKey)
		}
	}

	// After drain the aggregator must be empty (next interval starts at 0).
	if again := a.drain(); len(again) != 0 {
		t.Errorf("second drain must be empty (reset invariant), got %v", again)
	}
}

// TestUsageAggregator_DrainEmptyIsNil verifies draining an empty aggregator is a
// cheap no-op (nil/empty), so the flush ticker does no Redis work when idle.
func TestUsageAggregator_DrainEmptyIsNil(t *testing.T) {
	a := newUsageAggregator()
	if got := a.drain(); len(got) != 0 {
		t.Errorf("empty drain must be empty, got %v", got)
	}
}

// TestUsageAggregator_IgnoresNonPositive verifies non-positive / empty inputs are
// dropped (mirrors IncrMulti's no-op on costCents <= 0).
func TestUsageAggregator_IgnoresNonPositive(t *testing.T) {
	a := newUsageAggregator()
	a.add("k", "p", 0)
	a.add("k", "p", -5)
	a.add("", "p", 10)
	if got := a.drain(); len(got) != 0 {
		t.Errorf("non-positive/empty must be ignored, got %v", got)
	}
}

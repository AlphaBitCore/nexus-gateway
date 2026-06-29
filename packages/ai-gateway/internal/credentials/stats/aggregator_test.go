package credstats

import (
	"testing"
)

// TestStatsAggregator_AccumulatesCountAndTimestamps verifies the write-behind
// stats aggregator: count is ADDITIVE (summed across attempts), the timestamps
// are LAST-WRITER-WINS (latest only — they are not counters), and drain returns
// the per-credential deltas then resets so the next interval starts at 0 (the
// invariant that keeps Redis HINCRBY from double-counting).
func TestStatsAggregator_AccumulatesCountAndTimestamps(t *testing.T) {
	a := newStatsAggregator()

	a.recordSuccess("cred1", "2026-06-20T10:00:00Z")
	a.recordSuccess("cred1", "2026-06-20T10:00:01Z") // count -> 2, okAt -> latest
	a.recordSuccess("cred2", "2026-06-20T10:00:02Z")

	got := a.drain()

	if len(got) != 2 {
		t.Fatalf("drain returned %d creds, want 2: %v", len(got), got)
	}
	if got["cred1"].count != 2 {
		t.Errorf("cred1 count = %d, want 2 (additive)", got["cred1"].count)
	}
	if got["cred1"].okAt != "2026-06-20T10:00:01Z" {
		t.Errorf("cred1 okAt = %q, want latest 2026-06-20T10:00:01Z (LWW)", got["cred1"].okAt)
	}
	if got["cred1"].usedAt != "2026-06-20T10:00:01Z" {
		t.Errorf("cred1 usedAt = %q, want latest (LWW)", got["cred1"].usedAt)
	}
	if got["cred2"].count != 1 {
		t.Errorf("cred2 count = %d, want 1", got["cred2"].count)
	}

	// Reset invariant: second drain is empty.
	if again := a.drain(); len(again) != 0 {
		t.Errorf("second drain must be empty, got %v", again)
	}
}

// TestStatsAggregator_DrainEmptyIsNil verifies an empty drain is a no-op so the
// flusher does no Redis work when idle.
func TestStatsAggregator_DrainEmptyIsNil(t *testing.T) {
	a := newStatsAggregator()
	if got := a.drain(); len(got) != 0 {
		t.Errorf("empty drain must be empty, got %v", got)
	}
}

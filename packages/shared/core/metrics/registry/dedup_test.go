package registry

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestDedupSendsFirstOccurrenceImmediately(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	d := NewDedup(clock, 60*time.Second, 100)

	evt := DiagEvent{MessageHash: "h1", Message: "boom", OccurredAt: now}
	out := d.Submit(evt)
	if len(out) != 1 || out[0].RepeatCount != 1 {
		t.Fatalf("first submit must emit RepeatCount=1, got %+v", out)
	}
}

func TestDedupSuppressesWithinWindow(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	d := NewDedup(clock, 60*time.Second, 100)

	d.Submit(DiagEvent{MessageHash: "h1", Message: "boom", OccurredAt: now})
	out := d.Submit(DiagEvent{MessageHash: "h1", Message: "boom", OccurredAt: now})
	if len(out) != 0 {
		t.Fatalf("repeat within window must not emit, got %+v", out)
	}
}

func TestDedupEmitsSummaryAtWindowEnd(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	d := NewDedup(clock, 60*time.Second, 100)

	d.Submit(DiagEvent{MessageHash: "h1", Message: "boom", OccurredAt: now})
	d.Submit(DiagEvent{MessageHash: "h1", OccurredAt: now})
	d.Submit(DiagEvent{MessageHash: "h1", OccurredAt: now})

	// jump forward past the window
	now = now.Add(61 * time.Second)
	emitted := d.Tick()
	if len(emitted) != 1 {
		t.Fatalf("expected 1 summary, got %d", len(emitted))
	}
	if emitted[0].RepeatCount != 3 {
		t.Errorf("RepeatCount = %d, want 3", emitted[0].RepeatCount)
	}
}

func TestDedupBufferOverflowDropsOldest(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	d := NewDedup(clock, 60*time.Second, 2)

	d.Submit(DiagEvent{MessageHash: "h1"})
	d.Submit(DiagEvent{MessageHash: "h2"})
	d.Submit(DiagEvent{MessageHash: "h3"}) // forces eviction

	if d.DroppedCount() == 0 {
		t.Error("expected drops > 0 after overflow")
	}
}


// TestDedupCollapsedCounterCountsSuppressedDuplicates verifies that the
// optional collapsed-counter (wired via SetCollapsedCounter) is incremented
// by (RepeatCount - 1) on every Tick that emits a summary. The increment
// equals exactly the number of duplicate events the producer suppressed.
func TestDedupCollapsedCounterCountsSuppressedDuplicates(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	d := NewDedup(clock, 60*time.Second, 100)

	prom := prometheus.NewRegistry()
	reg := NewRegistry(prom)
	counter := reg.NewCounter("diag.dedup_collapsed_total", []string{"thing_type", "severity"})
	d.SetCollapsedCounter(counter, "agent")

	// Submit one first-occurrence + four suppressed duplicates. RepeatCount
	// reaches 5; the producer suppressed 4 emits beyond the first.
	d.Submit(DiagEvent{MessageHash: "h1", Level: "error"})
	for i := 0; i < 4; i++ {
		d.Submit(DiagEvent{MessageHash: "h1", Level: "error"})
	}

	now = now.Add(61 * time.Second)
	emitted := d.Tick()
	if len(emitted) != 1 {
		t.Fatalf("expected 1 summary, got %d", len(emitted))
	}
	if emitted[0].RepeatCount != 5 {
		t.Fatalf("RepeatCount = %d, want 5", emitted[0].RepeatCount)
	}

	// The counter must equal RepeatCount - 1 = 4 for the (agent, error)
	// label combination.
	samples := reg.Collect()
	got := float64(-1)
	for _, s := range samples {
		if s.Name == "diag.dedup_collapsed_total" {
			got = s.Value
			if want := "severity=error;thing_type=agent"; s.DimensionKey != want {
				t.Errorf("dimension_key = %q, want %q", s.DimensionKey, want)
			}
		}
	}
	if got != 4 {
		t.Errorf("collapsed counter = %v, want 4", got)
	}
}

// TestDedupCollapsedCounterOmittedForSingletons confirms a record that
// fires exactly once produces no summary AND no collapsed-counter increment
// — there's nothing to collapse.
func TestDedupCollapsedCounterOmittedForSingletons(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	d := NewDedup(clock, 60*time.Second, 100)

	prom := prometheus.NewRegistry()
	reg := NewRegistry(prom)
	counter := reg.NewCounter("diag.dedup_collapsed_total", []string{"thing_type", "severity"})
	d.SetCollapsedCounter(counter, "agent")

	d.Submit(DiagEvent{MessageHash: "h1", Level: "error"})
	now = now.Add(61 * time.Second)
	emitted := d.Tick()
	if len(emitted) != 0 {
		t.Fatalf("singleton must not emit a summary, got %d", len(emitted))
	}

	for _, s := range reg.Collect() {
		if s.Name == "diag.dedup_collapsed_total" && s.Value != 0 {
			t.Errorf("collapsed counter = %v on singleton, want 0", s.Value)
		}
	}
}

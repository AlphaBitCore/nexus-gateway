package registry

import (
	"sync"
	"time"
)

// Dedup is the client-side dedup ring buffer described in spec §7.2 / §7.4.
//
// Behaviour:
//   - On Submit, the first occurrence of a (messageHash) within the active
//     window is emitted immediately with RepeatCount=1.
//   - Subsequent occurrences within the window increment an internal counter
//     and are suppressed (Submit returns an empty slice).
//   - When the window expires for a record, Tick returns a single "summary"
//     event with the final RepeatCount — but only if RepeatCount > 1; a hash
//     that fired exactly once needs no summary.
//   - The buffer is bounded by maxActive: when adding a new hash would
//     exceed the limit, the record with the earliest `first` time is evicted
//     and DroppedCount is incremented.
//
// Dedup is safe for concurrent use.
type Dedup struct {
	clock     func() time.Time
	window    time.Duration
	maxActive int

	mu             sync.Mutex
	active         map[string]*dedupRecord
	dropped        uint64
	collapsed      *Counter // optional producer-side observability; see SetCollapsedCounter.
	collapsedLabel string   // pinned thing_type value for collapsed counter.
}

type dedupRecord struct {
	first       time.Time
	repeatCount int
	template    DiagEvent
}

// NewDedup constructs a Dedup with the supplied clock (use time.Now in
// production; a closure for testing), suppression window, and maximum active
// hash count.
func NewDedup(clock func() time.Time, window time.Duration, maxActive int) *Dedup {
	return &Dedup{
		clock:     clock,
		window:    window,
		maxActive: maxActive,
		active:    map[string]*dedupRecord{},
	}
}

// SetCollapsedCounter wires an opsmetrics Counter that records how many
// suppressed events were collapsed into each summary emit. Each Tick that
// emits a summary for a record with RepeatCount=N increments the counter by
// (N-1) — i.e. the number of duplicates the producer hid from the wire.
//
// Labels: {thing_type, severity}. thing_type is pinned at wire time (callers
// supply the Thing's type string, e.g. "agent", "ai-gateway"); severity comes
// from the suppressed DiagEvent's Level field. Pass nil counter to detach.
//
// Optional — Submit/Tick behaviour is unchanged when no counter is wired, so
// existing call sites that never call this method keep their current contract.
func (d *Dedup) SetCollapsedCounter(c *Counter, thingType string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.collapsed = c
	d.collapsedLabel = thingType
}

// Submit feeds an event into the dedup buffer. Returns events that should be
// sent immediately. May be empty (suppressed) or a single first-occurrence
// event with RepeatCount=1.
func (d *Dedup) Submit(evt DiagEvent) []DiagEvent {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := d.clock()

	if rec, ok := d.active[evt.MessageHash]; ok {
		// Within window: bump count, suppress.
		rec.repeatCount++
		return nil
	}

	if len(d.active) >= d.maxActive {
		// Evict the oldest by `first` time.
		var (
			oldestKey  string
			oldestTime time.Time
			haveAny    bool
		)
		for k, r := range d.active {
			if !haveAny || r.first.Before(oldestTime) {
				oldestKey = k
				oldestTime = r.first
				haveAny = true
			}
		}
		delete(d.active, oldestKey)
		d.dropped++
	}

	d.active[evt.MessageHash] = &dedupRecord{
		first:       now,
		repeatCount: 1,
		template:    evt,
	}
	evt.RepeatCount = 1
	evt.OccurredAt = now
	return []DiagEvent{evt}
}

// Tick walks the active map and emits summary events for records whose
// suppression window has ended. Records with RepeatCount == 1 are quietly
// removed (the first-occurrence emit was sufficient).
//
// When a collapsed-counter is wired (see SetCollapsedCounter), Tick also
// increments nexus_diag_dedup_collapsed_total{thing_type,severity} by
// (RepeatCount - 1) for each summary it emits — i.e. the count of duplicates
// the producer suppressed.
func (d *Dedup) Tick() []DiagEvent {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := d.clock()
	var out []DiagEvent
	for k, rec := range d.active {
		if now.Sub(rec.first) < d.window {
			continue
		}
		if rec.repeatCount > 1 {
			summary := rec.template
			summary.RepeatCount = rec.repeatCount
			summary.OccurredAt = now
			out = append(out, summary)
			if d.collapsed != nil {
				severity := summary.Level
				if severity == "" {
					severity = "unknown"
				}
				d.collapsed.With(d.collapsedLabel, severity).Add(float64(rec.repeatCount - 1))
			}
		}
		delete(d.active, k)
	}
	return out
}

// DroppedCount returns the cumulative number of records evicted due to the
// maxActive cap.
func (d *Dedup) DroppedCount() uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dropped
}

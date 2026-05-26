// Package alerteval is the Hub-side streaming alert evaluator. It subscribes
// to MQ traffic + audit events under a dedicated consumer group, maintains
// in-memory ring buffers per registered Aggregator, and ticks every 5s
// (default) to evaluate threshold rules. Aggregators emit Decisions which
// the Engine turns into alerting.Raiser Raise / Resolve calls.
//
// The Engine is registered as a single named scheduler.Job ("alerteval-engine")
// alongside the existing Hub state-table alerting jobs. Single-instance
// enforcement reuses cfg.Scheduler.Enabled (no new flag).
//
// Spec: alerteval-streaming-engine-design §internal.
package alerteval

import (
	"sync"
	"time"
)

// Window is a fixed-bucket ring buffer keyed by 1-second epoch buckets.
// Each bucket stores (a, b). Interpretation depends on Aggregator type:
//
//	CountInWindow:   (count, _)
//	RatioInWindow:   (numerator, denominator)
//	SumInWindow:     (sum_x, count)
//
// Window is safe for concurrent Add (mutex-guarded). Sum is also guarded.
type Window struct {
	mu        sync.Mutex
	buckets   []bucketAB
	headEpoch int64 // unix seconds for buckets[head]; 0 means uninitialised
	head      int   // index in buckets corresponding to headEpoch
}

type bucketAB struct {
	a float64
	b float64
}

// NewWindow creates a Window sized to cover capSeconds of 1-second buckets.
// capSeconds must be >= 1; passing < 1 panics (caller bug).
func NewWindow(capSeconds int) *Window {
	if capSeconds < 1 {
		panic("alerteval: NewWindow capSeconds must be >= 1")
	}
	return &Window{buckets: make([]bucketAB, capSeconds)}
}

// Add records a sample (a, b) at time at. Old buckets are evicted as time
// advances. Calls older than the buffer's lookback range are silently dropped
// (this happens only on out-of-order events, which JetStream avoids in the
// common case).
func (w *Window) Add(at time.Time, a, b float64) {
	w.mu.Lock()
	defer w.mu.Unlock()

	atEpoch := at.Unix()
	w.advance(atEpoch)
	if atEpoch < w.headEpoch-int64(len(w.buckets)-1) {
		return // older than what the buffer can hold
	}
	if atEpoch > w.headEpoch {
		// advance() should have caught this; defensive.
		return
	}
	offset := int(w.headEpoch - atEpoch)
	idx := (w.head - offset + len(w.buckets)) % len(w.buckets)
	w.buckets[idx].a += a
	w.buckets[idx].b += b
}

// Sum returns the totals over the last lookback ending at now. lookback is
// clamped to the window's capacity.
func (w *Window) Sum(lookback time.Duration, now time.Time) (a, b float64) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.advance(now.Unix())
	steps := int(lookback.Seconds())
	if steps > len(w.buckets) {
		steps = len(w.buckets)
	}
	if steps < 1 {
		return 0, 0
	}
	for i := range steps {
		idx := (w.head - i + len(w.buckets)) % len(w.buckets)
		a += w.buckets[idx].a
		b += w.buckets[idx].b
	}
	return a, b
}

// advance moves head forward to nowEpoch, zeroing out skipped buckets.
// Caller must hold w.mu.
func (w *Window) advance(nowEpoch int64) {
	if w.headEpoch == 0 {
		w.headEpoch = nowEpoch
		return
	}
	if nowEpoch <= w.headEpoch {
		return
	}
	delta := nowEpoch - w.headEpoch
	if delta >= int64(len(w.buckets)) {
		// Entire ring has expired; clear all buckets and start fresh.
		for i := range w.buckets {
			w.buckets[i] = bucketAB{}
		}
		w.headEpoch = nowEpoch
		w.head = 0
		return
	}
	for range delta {
		w.head = (w.head + 1) % len(w.buckets)
		w.buckets[w.head] = bucketAB{}
	}
	w.headEpoch = nowEpoch
}

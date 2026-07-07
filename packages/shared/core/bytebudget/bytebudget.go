// Package bytebudget is a lock-free, soft byte semaphore: it bounds the total bytes
// concurrently "in use" to a budget, blocking Acquire while the budget is exhausted
// and waking blocked acquirers when Release frees space. It is a reusable
// back-pressure primitive for any producer/consumer pipeline whose real resource is
// BYTES, not item count — e.g. an in-memory queue that pins variable-size payloads: a
// producer Acquires its item's byte weight before enqueuing and Releases it when the
// item leaves the pipeline. A full budget blocks the producer, which back-pressures
// upstream instead of dropping or growing without bound.
//
// SOFT and LOCK-FREE by design (no mutex): `used` is an atomic and the
// check-then-add in Acquire races, so the budget may be OVERSHOT by up to roughly
// (concurrent acquirers × their weights). That is deliberate — this is a memory
// back-pressure knob, not exact accounting. A zero-overshoot bound is possible
// lock-free too (a CompareAndSwap retry loop), but it contends hardest exactly at
// the full-budget boundary where every producer is hammering the same word; the
// single wait-free Add never retries, and the overshoot it admits is bounded by
// producer concurrency — negligible against a memory-scale budget configured
// below real headroom.
//
// Wake-up uses a cap-1 buffered channel signalled non-blockingly on Release, plus a
// short polling fallback in Acquire, so a signal that races an about-to-wait acquirer
// can never wedge it forever (a missed edge is re-checked within WakePollInterval).
package bytebudget

import (
	"sync/atomic"
	"time"
)

// WakePollInterval bounds how long a blocked Acquire waits before re-checking the
// budget even with no Release signal — the safety net against a missed wake-up edge
// (the atomic check and the channel signal are not one operation, so a Release can
// slip between an acquirer's over-budget check and its channel wait). Short enough to
// pick up freed budget promptly, long enough that a fully blocked pipeline does not
// spin.
const WakePollInterval = 20 * time.Millisecond

// Budget is a soft, lock-free byte semaphore. The zero value is not usable; call New.
type Budget struct {
	budget  int64
	used    atomic.Int64
	notFull chan struct{} // cap 1; a non-blocking send on Release nudges one waiter
	stop    <-chan struct{}
}

// New makes a budget of at most `budget` bytes. A non-positive budget disables
// back-pressure (Acquire always admits) — a caller can wire it to turn the bound off.
// stop unblocks any waiter when it is closed/fired (shutdown), so a Close that drains
// the pipeline stays bounded; pass nil for a budget that only ever unblocks on Release.
func New(budget int64, stop <-chan struct{}) *Budget {
	return &Budget{
		budget:  budget,
		notFull: make(chan struct{}, 1),
		stop:    stop,
	}
}

// TryAcquire reserves n bytes only when the budget has room RIGHT NOW — the
// non-blocking admission. Same rule as Acquire (room, or an empty pipeline so an
// oversized n is never wedged), same soft race. Two callers: pipelines that must
// never block (lossy modes shed the item on false), and blocking callers that
// want to observe "about to wait" (TryAcquire first, count, then Acquire).
func (b *Budget) TryAcquire(n int64) bool {
	if b == nil || b.budget <= 0 {
		return true
	}
	// Admit if there is room, OR if the pipeline is empty (used<=0) so an oversized
	// item (n > budget) is not stuck forever. Soft: the check and the Add race, so
	// used may briefly exceed budget — accepted (see package doc).
	if u := b.used.Load(); u < b.budget || u <= 0 {
		b.used.Add(n)
		return true
	}
	return false
}

// Acquire reserves n bytes, blocking while the budget is exhausted. It returns true
// once reserved, or false if stop fired first (shutdown) — the caller then handles the
// item without enqueuing. An n larger than the whole budget is admitted alone once
// used drops to 0, so a single item bigger than the budget can never deadlock the
// pipeline. A non-positive budget admits immediately (back-pressure disabled).
func (b *Budget) Acquire(n int64) bool {
	if b == nil || b.budget <= 0 {
		return true
	}
	if b.TryAcquire(n) { // fast path: no timer allocated while the budget has room
		return true
	}
	// One reusable timer per blocked Acquire, Reset each round — a time.After per
	// poll iteration allocated a fresh runtime timer every ≤20ms for EVERY blocked
	// producer, pure garbage churn in exactly the wedged regime. Reset without a
	// drain is safe on Go 1.23+ timer semantics (unbuffered channel; Reset discards
	// a pending fire).
	t := time.NewTimer(WakePollInterval)
	defer t.Stop()
	for {
		select {
		case <-b.notFull:
		case <-t.C:
		case <-b.stop:
			return false
		}
		if b.TryAcquire(n) {
			return true
		}
		t.Reset(WakePollInterval)
	}
}

// Release returns n bytes to the budget and nudges one blocked acquirer. Safe on a
// nil receiver / disabled budget (no-op). n must match the Acquire that reserved it.
func (b *Budget) Release(n int64) {
	if b == nil || b.budget <= 0 || n == 0 {
		return
	}
	b.used.Add(-n)
	select {
	case b.notFull <- struct{}{}: // wake one waiter; cap-1 buffer coalesces bursts
	default:
	}
}

// InUse reports the current reserved bytes (metrics / tests). May transiently exceed
// the budget or go negative under the soft race — treat it as approximate.
func (b *Budget) InUse() int64 {
	if b == nil {
		return 0
	}
	return b.used.Load()
}

// Budget returns the configured ceiling (0 = disabled).
func (b *Budget) Budget() int64 {
	if b == nil {
		return 0
	}
	return b.budget
}

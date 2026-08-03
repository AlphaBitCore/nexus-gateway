package proxy

// The L2 semantic cache write-back runs detached from the request that
// produced it. This file owns the admission gate that bounds how many of
// those writes may be outstanding at once, and the counter that reports when
// the bound binds. See scheduleL2Write in proxy_l2.go for the caller.

import (
	"runtime"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// l2WriteMax bounds concurrently outstanding L2 write goroutines process-wide.
//
// Sized against the steady state it must not disturb: an L2 write is one
// embedding call plus one Valkey HSET, and the outstanding set is
// arrival-rate × write-latency. 64 slots per CPU leaves more than an order of
// magnitude of headroom over a healthy box's steady state while capping the
// worst case — a stalled embedding provider holding every write for the full
// 5-second deadline — at a fixed multiple of the response-body size rather
// than at the arrival rate. The unit is a slot, not a byte, because the body
// size is not known to the gate; see the shed counter for whether the cap
// binds in practice.
//
// Deliberately not an operator knob: the value only has to be far enough above
// the steady state that it never sheds a healthy deployment, and low enough
// that a stalled provider cannot walk the heap to GOMEMLIMIT. Both properties
// hold across box sizes once it scales with GOMAXPROCS.
//
// Atomic because tests substitute a small cap while other tests in the package
// run in parallel; production writes it exactly once, at init.
var l2WriteMax atomic.Int64

// l2WriteInflight is the current outstanding count. Process-wide rather than
// per-Handler because the resource it protects — the heap — is process-wide.
var l2WriteInflight atomic.Int64

func init() { l2WriteMax.Store(int64(64 * runtime.GOMAXPROCS(0))) }

// acquireL2WriteSlot takes a slot, or reports false when the cap is reached.
// Increment-then-check keeps the admitted path a single atomic add, and the
// rejecting path always returns its slot before reporting false, so capacity
// cannot be stranded. Mirrors admissionGate.acquire.
func acquireL2WriteSlot() bool {
	if l2WriteInflight.Add(1) > l2WriteMax.Load() {
		l2WriteInflight.Add(-1)
		return false
	}
	return true
}

// releaseL2WriteSlot returns a slot. Deferred inside the write goroutine so it
// covers the error and panic-unwind paths as well as the normal one.
func releaseL2WriteSlot() { l2WriteInflight.Add(-1) }

// l2WriteShedTotal counts L2 semantic writes dropped because the outstanding
// write set was at its cap — the signal that the embedding provider is slow
// enough for write-back to fall behind. A rising value means reduced L2 hit
// rate, never a failed client request.
//
// The service is not in the name: which binary emitted this is the `job`
// label's job, set by the scrape config (prometheus-naming-architecture.md §1).
// It sits in the `cache` subsystem alongside nexus_cache_l2_writes_total.
var l2WriteShedTotal = promauto.NewCounter(prometheus.CounterOpts{
	Namespace: "nexus",
	Subsystem: "cache",
	Name:      "l2_write_shed_total",
	Help:      "L2 semantic cache write-backs dropped because the in-flight write cap was reached.",
})

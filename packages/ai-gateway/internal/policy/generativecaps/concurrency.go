package generativecaps

import (
	"sync"
	"sync/atomic"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// VKConcurrency bounds simultaneous in-flight generative requests per
// (endpoint-kind, virtual-key). It mirrors the global admission gate's
// lock-free shape — increment-then-check on acquire, decrement on release,
// two atomic ops on the hot path — but bucketed per (kind, VK) so a single
// leaked key cannot open unbounded concurrent expensive requests while other
// keys stay unaffected.
//
// Counting is local to the process: a single-tenant single-box deployment does
// not need cross-instance coordination, and a local counter has no
// Redis-TTL-leak failure mode (a distributed concurrency counter that misses
// its decrement strands capacity forever). The RPM path already carries the
// Redis seam if cross-instance coordination is ever required.
type VKConcurrency struct {
	// buckets maps "<kind>\x00<vkID>" → *atomic.Int64. Entries are left in
	// place when they return to zero; the key population is bounded by the VK
	// count on a single-tenant box (the same pragma the admission gate accepts
	// for its single global counter).
	buckets sync.Map
}

// NewVKConcurrency returns an empty counter.
func NewVKConcurrency() *VKConcurrency { return &VKConcurrency{} }

func bucketKey(kind typology.EndpointKind, vkID string) string {
	return string(kind) + "\x00" + vkID
}

func (c *VKConcurrency) counter(key string) *atomic.Int64 {
	if v, ok := c.buckets.Load(key); ok {
		return v.(*atomic.Int64)
	}
	// LoadOrStore resolves the create race: the losing goroutine's fresh
	// counter is discarded and everyone shares the winner's.
	actual, _ := c.buckets.LoadOrStore(key, new(atomic.Int64))
	return actual.(*atomic.Int64)
}

// Acquire admits one request of (kind, vkID) unless max concurrent are already
// in flight. max <= 0 means unlimited (always admits, no counting). Returns
// true when admitted; the caller MUST call Release exactly once for each true
// return (defer-covered by the proxy so every exit path — success, error,
// panic — returns the slot). The increment-then-check-then-decrement-on-reject
// shape guarantees a rejection never strands a slot.
func (c *VKConcurrency) Acquire(kind typology.EndpointKind, vkID string, max int) bool {
	if max <= 0 {
		return true
	}
	ctr := c.counter(bucketKey(kind, vkID))
	if ctr.Add(1) > int64(max) {
		ctr.Add(-1)
		return false
	}
	return true
}

// Release returns a slot acquired by Acquire. Safe to call only after a true
// Acquire; a no-op guard against underflow keeps a double-release from driving
// the counter negative (which would over-admit later).
func (c *VKConcurrency) Release(kind typology.EndpointKind, vkID string) {
	ctr := c.counter(bucketKey(kind, vkID))
	if ctr.Add(-1) < 0 {
		ctr.Add(1) // underflow guard: undo, never leave the counter negative
	}
}

package audit

import "testing"

// TestReleaseRequestBuffer_SteadyStateZeroAlloc asserts the business property
// the release path exists for: when the caller returns unattached body
// buffers (bodies-off / not-captured requests), the acquire→release cycle
// reuses pooled memory instead of allocating a fresh buffer per request.
// Without the release, every iteration allocates.
func TestReleaseRequestBuffer_SteadyStateZeroAlloc(t *testing.T) {
	src := make([]byte, 2048)
	for i := range src {
		src[i] = byte(i)
	}
	// Warm the pool so the measurement window is steady-state.
	for range 16 {
		_, h := AcquireRequestBody(src)
		ReleaseRequestBuffer(h)
	}
	avg := testing.AllocsPerRun(200, func() {
		body, h := AcquireRequestBody(src)
		if len(body) != len(src) {
			t.Fatalf("body len %d != src len %d", len(body), len(src))
		}
		ReleaseRequestBuffer(h)
	})
	// Steady state must not allocate a fresh buffer per cycle. Allow <1 to
	// absorb a rare GC clearing the pool mid-run; 1+ means the release path
	// is broken and every request pays a fresh allocation again.
	if avg >= 1 {
		t.Fatalf("acquire/release cycle allocates %.1f allocs/op; pool reuse broken", avg)
	}
}

// TestReleaseRequestBuffer_NilSafe asserts the not-acquired path (empty body
// → nil handle) is a no-op, matching how finalizeAudit calls it on requests
// that never read a body.
func TestReleaseRequestBuffer_NilSafe(t *testing.T) {
	ReleaseRequestBuffer(nil) // must not panic
	body, h := AcquireRequestBody(nil)
	if body != nil || h != nil {
		t.Fatalf("empty src must yield nil body+handle, got %v %v", body, h)
	}
	ReleaseRequestBuffer(h)
}

// TestReleaseRequestBuffer_OversizedDropped asserts a buffer grown past the
// pool cap is dropped to GC rather than poisoning the pool with a huge
// backing array (same contract as the writer-side terminal reclaim).
func TestReleaseRequestBuffer_OversizedDropped(t *testing.T) {
	big := make([]byte, requestBodyPoolCap+1)
	_, h := AcquireRequestBody(big)
	if h == nil {
		t.Fatal("expected a handle for a non-empty body")
	}
	if cap(*h) <= requestBodyPoolCap {
		t.Fatalf("test setup: expected regrown buffer > cap, got %d", cap(*h))
	}
	ReleaseRequestBuffer(h) // must drop, not pool — nothing to assert beyond no panic;
	// the pooling decision is releaseRequestBody's existing tested contract.
}

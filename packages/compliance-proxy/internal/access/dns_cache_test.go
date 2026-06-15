package access

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingResolver records how many upstream lookups happened and can
// block each one on a gate so concurrent callers provably overlap.
type countingResolver struct {
	calls atomic.Int64
	addrs []net.IPAddr
	err   error
	gate  chan struct{} // when non-nil, each lookup waits on it
}

func (r *countingResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	r.calls.Add(1)
	if r.gate != nil {
		<-r.gate
	}
	return r.addrs, r.err
}

// TestCachingResolver_CollapsesConcurrentLookups proves N concurrent
// lookups for the same host issue exactly ONE upstream resolve — the
// thundering-herd collapse that removes the per-CONNECT DNS storm.
func TestCachingResolver_CollapsesConcurrentLookups(t *testing.T) {
	gate := make(chan struct{})
	up := &countingResolver{addrs: []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, gate: gate}
	c := newCachingResolver(up, time.Minute)

	const n = 50
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.LookupIPAddr(context.Background(), "example.com"); err != nil {
				t.Errorf("lookup: %v", err)
			}
		}()
	}
	// Give the goroutines time to pile up on the single in-flight resolve,
	// then release it.
	time.Sleep(20 * time.Millisecond)
	close(gate)
	wg.Wait()

	if got := up.calls.Load(); got != 1 {
		t.Fatalf("upstream called %d times for %d concurrent lookups, want 1 (herd not collapsed)", got, n)
	}
}

// TestCachingResolver_ReusesWithinTTL_RefreshesAfter drives a fake clock:
// a second lookup within the TTL is served from cache (no upstream call),
// and one past the TTL re-resolves.
func TestCachingResolver_ReusesWithinTTL_RefreshesAfter(t *testing.T) {
	up := &countingResolver{addrs: []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}}}
	c := newCachingResolver(up, 10*time.Second)
	base := time.Unix(1_700_000_000, 0)
	cur := base
	c.now = func() time.Time { return cur }

	if _, err := c.LookupIPAddr(context.Background(), "h"); err != nil {
		t.Fatal(err)
	}
	cur = base.Add(5 * time.Second) // within TTL
	if _, err := c.LookupIPAddr(context.Background(), "h"); err != nil {
		t.Fatal(err)
	}
	if got := up.calls.Load(); got != 1 {
		t.Fatalf("within-TTL lookup re-resolved: upstream calls=%d, want 1", got)
	}
	cur = base.Add(11 * time.Second) // past TTL
	if _, err := c.LookupIPAddr(context.Background(), "h"); err != nil {
		t.Fatal(err)
	}
	if got := up.calls.Load(); got != 2 {
		t.Fatalf("post-TTL lookup did not re-resolve: upstream calls=%d, want 2", got)
	}
}

// TestCachingResolver_ErrorsNotCached proves a FAILED resolve is NOT
// negative-cached: each sequential lookup re-resolves. Negative-caching an
// error would let one transient/cancelled failure deny the host for the
// whole TTL via the private-IP check (finding M2-1).
func TestCachingResolver_ErrorsNotCached(t *testing.T) {
	sentinel := errors.New("nxdomain")
	up := &countingResolver{err: sentinel}
	c := newCachingResolver(up, time.Minute)

	for i := range 3 {
		if _, err := c.LookupIPAddr(context.Background(), "bad"); !errors.Is(err, sentinel) {
			t.Fatalf("lookup %d err=%v, want sentinel", i, err)
		}
	}
	if got := up.calls.Load(); got != 3 {
		t.Fatalf("errors must re-resolve (not cached): upstream calls=%d, want 3", got)
	}
}

// TestCachingResolver_ConcurrentErrorsStillCollapse proves the in-flight
// single-flight still holds for failures: N concurrent erroring lookups
// share ONE upstream call even though the error is not cached afterward.
func TestCachingResolver_ConcurrentErrorsStillCollapse(t *testing.T) {
	gate := make(chan struct{})
	up := &countingResolver{err: errors.New("servfail"), gate: gate}
	c := newCachingResolver(up, time.Minute)

	var wg sync.WaitGroup
	for range 30 {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = c.LookupIPAddr(context.Background(), "h") }()
	}
	time.Sleep(20 * time.Millisecond)
	close(gate)
	wg.Wait()
	if got := up.calls.Load(); got != 1 {
		t.Fatalf("concurrent erroring lookups must collapse: calls=%d want 1", got)
	}
}

// TestCachingResolver_ResolverCancelDoesNotPoison is the M2-1 regression:
// the caller that triggered the resolve cancels its own ctx mid-flight,
// but the detached resolve still completes successfully and is cached — a
// later lookup gets the good result, NOT the cancellation, and the host is
// never denied.
func TestCachingResolver_ResolverCancelDoesNotPoison(t *testing.T) {
	gate := make(chan struct{})
	up := &countingResolver{addrs: []net.IPAddr{{IP: net.ParseIP("5.5.5.5")}}, gate: gate}
	c := newCachingResolver(up, time.Minute)

	ctxA, cancelA := context.WithCancel(context.Background())
	aErr := make(chan error, 1)
	go func() { _, err := c.LookupIPAddr(ctxA, "h"); aErr <- err }()
	time.Sleep(20 * time.Millisecond) // A started the (gated) detached resolve

	cancelA() // A abandons; its ctx must NOT cancel the shared resolve
	if err := <-aErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("the cancelling caller should see its own ctx error, got %v", err)
	}

	close(gate) // the detached resolve completes successfully + caches
	// A later lookup must get the GOOD cached result, not a poisoned error.
	// Poll briefly for the resolve to finish caching.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		addrs, err := c.LookupIPAddr(context.Background(), "h")
		if err == nil && len(addrs) == 1 {
			break
		}
		if time.Now().After(deadline.Add(-10 * time.Millisecond)) {
			t.Fatalf("later lookup poisoned: addrs=%v err=%v", addrs, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := up.calls.Load(); got != 1 {
		t.Fatalf("resolve should have run exactly once: calls=%d", got)
	}
}

// TestCachingResolver_ContextCancelWhileWaiting proves a caller waiting on
// an in-flight lookup honours its own context cancellation.
func TestCachingResolver_ContextCancelWhileWaiting(t *testing.T) {
	gate := make(chan struct{})
	up := &countingResolver{addrs: []net.IPAddr{{IP: net.ParseIP("2.2.2.2")}}, gate: gate}
	c := newCachingResolver(up, time.Minute)

	// First caller becomes the resolver and blocks on the gate.
	started := make(chan struct{})
	go func() { close(started); _, _ = c.LookupIPAddr(context.Background(), "h") }()
	<-started
	time.Sleep(10 * time.Millisecond) // ensure the in-flight entry exists

	// Second caller waits on the same in-flight entry, then is cancelled.
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { _, err := c.LookupIPAddr(ctx, "h"); errCh <- err }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiting caller err=%v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled waiter did not return")
	}
	close(gate) // let the first resolve finish
}

// TestCachingResolver_SweepsWhenOverCap fills the map past the cap with
// expired entries, then a fresh lookup must trigger the sweep+reset.
func TestCachingResolver_SweepsWhenOverCap(t *testing.T) {
	up := &countingResolver{addrs: []net.IPAddr{{IP: net.ParseIP("3.3.3.3")}}}
	c := newCachingResolver(up, time.Second)
	base := time.Unix(1_700_000_000, 0)
	c.now = func() time.Time { return base }

	// Seed cap+1 completed, already-expired entries.
	c.mu.Lock()
	for i := 0; i <= dnsCacheMaxHosts; i++ {
		e := &dnsCacheEntry{ready: make(chan struct{}), expires: base.Add(-time.Second)}
		close(e.ready)
		c.entries[hostKey(i)] = e
	}
	c.mu.Unlock()

	if _, err := c.LookupIPAddr(context.Background(), "fresh"); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	n := len(c.entries)
	c.mu.Unlock()
	if n > dnsCacheMaxHosts {
		t.Fatalf("cache exceeded cap after sweep: %d > %d", n, dnsCacheMaxHosts)
	}
}

// TestCachingResolver_ResetsWhenSweepCannotShrink fills the map past the
// cap with LIVE (unexpired) entries so the sweep deletes nothing and the
// map must be reset instead — the cap's hard backstop.
func TestCachingResolver_ResetsWhenSweepCannotShrink(t *testing.T) {
	up := &countingResolver{addrs: []net.IPAddr{{IP: net.ParseIP("4.4.4.4")}}}
	c := newCachingResolver(up, time.Minute)
	base := time.Unix(1_700_000_000, 0)
	c.now = func() time.Time { return base }

	c.mu.Lock()
	for i := 0; i <= dnsCacheMaxHosts; i++ {
		e := &dnsCacheEntry{ready: make(chan struct{}), expires: base.Add(time.Hour)} // live
		close(e.ready)
		c.entries[hostKey(i)] = e
	}
	c.mu.Unlock()

	if _, err := c.LookupIPAddr(context.Background(), "fresh"); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	n := len(c.entries)
	c.mu.Unlock()
	// After reset the map holds only the fresh lookup's entry.
	if n > dnsCacheMaxHosts {
		t.Fatalf("cache exceeded cap after reset: %d > %d", n, dnsCacheMaxHosts)
	}
}

func hostKey(i int) string {
	return "host-" + string(rune('a'+i%26)) + "-" + time.Duration(i).String()
}

package pidcache

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/api"
)

func TestCache_MissResolvesThenHits(t *testing.T) {
	c := New()
	base := time.Unix(1_700_000_000, 0)
	c.now = func() time.Time { return base }

	var calls atomic.Int64
	lookup := func(pid int) (api.ProcessMeta, error) {
		calls.Add(1)
		return api.ProcessMeta{PID: pid, Name: "chrome"}, nil
	}

	m, err := c.Get(42, lookup)
	if err != nil || m.Name != "chrome" {
		t.Fatalf("cold get: %+v %v", m, err)
	}
	m2, _ := c.Get(42, lookup) // within TTL → cache hit
	if m2 != m {
		t.Fatalf("warm get diverged: %+v vs %+v", m2, m)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("lookup ran %d times, want 1 (cache miss on warm get)", got)
	}
}

func TestCache_ExpiresAfterTTL(t *testing.T) {
	c := New()
	base := time.Unix(1_700_000_000, 0)
	cur := base
	c.now = func() time.Time { return cur }
	var calls atomic.Int64
	lookup := func(pid int) (api.ProcessMeta, error) {
		calls.Add(1)
		return api.ProcessMeta{PID: pid}, nil
	}
	c.Get(7, lookup)
	cur = base.Add(defaultTTL + time.Second)
	c.Get(7, lookup) // expired → re-resolve
	if got := calls.Load(); got != 2 {
		t.Fatalf("expired entry not re-resolved: calls=%d want 2", got)
	}
}

func TestCache_CachesErrors(t *testing.T) {
	c := New()
	sentinel := errors.New("exited")
	var calls atomic.Int64
	lookup := func(pid int) (api.ProcessMeta, error) {
		calls.Add(1)
		return api.ProcessMeta{}, sentinel
	}
	for i := range 3 {
		if _, err := c.Get(9, lookup); !errors.Is(err, sentinel) {
			t.Fatalf("get %d err=%v want sentinel", i, err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("error not cached: calls=%d want 1", got)
	}
}

func TestCache_EvictsExpiredWhenOverCap(t *testing.T) {
	c := New()
	base := time.Unix(1_700_000_000, 0)
	c.now = func() time.Time { return base }
	lookup := func(pid int) (api.ProcessMeta, error) { return api.ProcessMeta{PID: pid}, nil }

	// Seed cap entries, all expired.
	c.mu.Lock()
	for i := range c.maxEntry {
		c.m[i] = entry{expires: base.Add(-time.Second)}
	}
	c.mu.Unlock()

	c.Get(999999, lookup) // triggers sweep (all expired → removed) then insert
	c.mu.Lock()
	_, has := c.m[999999]
	n := len(c.m)
	c.mu.Unlock()
	if !has {
		t.Fatal("new entry not stored after sweep")
	}
	if n > c.maxEntry {
		t.Fatalf("cache over cap after sweep: %d", n)
	}
}

func TestCache_ResetsWhenSweepCannotShrink(t *testing.T) {
	c := New()
	base := time.Unix(1_700_000_000, 0)
	c.now = func() time.Time { return base }
	lookup := func(pid int) (api.ProcessMeta, error) { return api.ProcessMeta{PID: pid}, nil }

	// Seed cap entries, all LIVE → sweep deletes none → map reset.
	c.mu.Lock()
	for i := range c.maxEntry {
		c.m[i] = entry{expires: base.Add(time.Hour)}
	}
	c.mu.Unlock()

	c.Get(999999, lookup)
	c.mu.Lock()
	_, has := c.m[999999]
	n := len(c.m)
	c.mu.Unlock()
	if !has {
		t.Fatal("new entry not stored after reset")
	}
	if n > c.maxEntry {
		t.Fatalf("cache over cap after reset: %d", n)
	}
}

func TestCache_ConcurrentGet_RaceClean(t *testing.T) {
	c := New()
	var calls atomic.Int64
	lookup := func(pid int) (api.ProcessMeta, error) {
		calls.Add(1)
		return api.ProcessMeta{PID: pid, Name: "p"}, nil
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 1000 {
				if m, _ := c.Get(i%16, lookup); m.Name != "p" {
					t.Errorf("bad meta %+v", m)
					return
				}
			}
		}()
	}
	wg.Wait()
}

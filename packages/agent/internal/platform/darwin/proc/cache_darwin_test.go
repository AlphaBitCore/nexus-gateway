//go:build darwin

package proc

import (
	"os"
	"testing"
	"time"
)

// resetCache clears global cache state between tests so cases don't leak
// entries or a mutated clock into each other.
func resetCache() {
	cacheMu.Lock()
	cacheMap = make(map[int]cacheEntry)
	cacheMu.Unlock()
	nowFn = time.Now
}

// TestProcessInfoCached_ServesCachedWithinTTL proves a second lookup for
// the same PID within the TTL returns the cached value WITHOUT touching
// libproc again — the property the hot-path optimization depends on. We
// assert it by seeding the cache directly and confirming the seeded
// (sentinel) value is returned rather than a real resolve.
func TestProcessInfoCached_ServesCachedWithinTTL(t *testing.T) {
	resetCache()
	defer resetCache()

	base := time.Unix(1_700_000_000, 0)
	nowFn = func() time.Time { return base }

	const pid = 424242
	want := Meta{PID: pid, Path: "/Applications/Sentinel.app/Contents/MacOS/Sentinel", Name: "Sentinel", BundleID: "com.example.sentinel"}
	cacheMu.Lock()
	cacheMap[pid] = cacheEntry{meta: want, expires: base.Add(cacheTTL)}
	cacheMu.Unlock()

	got, err := ProcessInfoCached(pid)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Fatalf("cache hit returned %+v, want seeded %+v (did it bypass the cache and re-resolve?)", got, want)
	}
}

// TestProcessInfoCached_ExpiresAfterTTL proves an entry past its TTL is
// not served — the lookup falls through to a real resolve. For a PID that
// does not exist, the real resolve returns an error, which is exactly how
// we detect that the stale entry was discarded.
func TestProcessInfoCached_ExpiresAfterTTL(t *testing.T) {
	resetCache()
	defer resetCache()

	base := time.Unix(1_700_000_000, 0)
	current := base
	nowFn = func() time.Time { return current }

	const pid = -1 // never a real PID → ProcessInfo returns an error
	cacheMu.Lock()
	cacheMap[pid] = cacheEntry{
		meta:    Meta{PID: pid, Path: "/stale", Name: "stale"},
		expires: base.Add(cacheTTL),
	}
	cacheMu.Unlock()

	// Advance past the TTL: the stale entry must be discarded and the
	// real (failing) resolve must run.
	current = base.Add(cacheTTL + time.Second)
	got, err := ProcessInfoCached(pid)
	if err == nil {
		t.Fatalf("expected resolve error for pid %d after expiry, got meta=%+v — stale entry was served", pid, got)
	}
	if got.Path == "/stale" {
		t.Fatalf("expired entry was served (path=/stale) instead of re-resolving")
	}
}

// TestProcessInfoCached_CachesErrors proves an errored resolve is cached
// for the TTL so a burst of flows for an exited PID does not re-hit
// libproc on every flow. We seed an error entry and confirm it is served.
func TestProcessInfoCached_CachesErrors(t *testing.T) {
	resetCache()
	defer resetCache()

	base := time.Unix(1_700_000_000, 0)
	nowFn = func() time.Time { return base }

	const pid = 999999
	sentinel := errSentinel("exited")
	cacheMu.Lock()
	cacheMap[pid] = cacheEntry{meta: Meta{PID: pid}, err: sentinel, expires: base.Add(cacheTTL)}
	cacheMu.Unlock()

	_, err := ProcessInfoCached(pid)
	if err != sentinel {
		t.Fatalf("cached error not served: got %v, want sentinel", err)
	}
}

// TestSweepExpiredLocked_DropsOnlyExpired confirms the cap-triggered sweep
// removes expired entries and keeps live ones.
func TestSweepExpiredLocked_DropsOnlyExpired(t *testing.T) {
	resetCache()
	defer resetCache()

	base := time.Unix(1_700_000_000, 0)
	cacheMu.Lock()
	cacheMap[1] = cacheEntry{expires: base.Add(-time.Second)} // expired
	cacheMap[2] = cacheEntry{expires: base.Add(time.Hour)}    // live
	sweepExpiredLocked(base)
	_, has1 := cacheMap[1]
	_, has2 := cacheMap[2]
	cacheMu.Unlock()

	if has1 {
		t.Error("expired entry pid=1 survived the sweep")
	}
	if !has2 {
		t.Error("live entry pid=2 was wrongly swept")
	}
}

// TestProcessInfoCached_MissResolvesThenStores exercises the real
// resolve-and-store path: a cold lookup for the live test process must
// resolve via libproc, populate the cache, and a second lookup must be
// served from the cache (same value) without re-resolving.
func TestProcessInfoCached_MissResolvesThenStores(t *testing.T) {
	resetCache()
	defer resetCache()

	pid := os.Getpid()
	first, err := ProcessInfoCached(pid)
	if err != nil {
		t.Fatalf("cold resolve of self pid failed: %v", err)
	}
	if first.PID != pid || first.Path == "" {
		t.Fatalf("cold resolve returned incomplete meta: %+v", first)
	}

	cacheMu.Lock()
	_, cached := cacheMap[pid]
	cacheMu.Unlock()
	if !cached {
		t.Fatal("cold resolve did not populate the cache")
	}

	second, err := ProcessInfoCached(pid)
	if err != nil || second != first {
		t.Fatalf("warm lookup diverged: got (%+v, %v) want (%+v, nil)", second, err, first)
	}
}

// TestProcessInfoCached_EvictsWhenOverCap fills the cache to the cap with
// live entries, then a fresh lookup must trigger the sweep-then-reset
// branch (nothing expired → map reset) before inserting the new entry.
func TestProcessInfoCached_EvictsWhenOverCap(t *testing.T) {
	resetCache()
	defer resetCache()

	base := time.Unix(1_700_000_000, 0)
	nowFn = func() time.Time { return base }

	cacheMu.Lock()
	for i := range cacheMaxEntries {
		cacheMap[1_000_000+i] = cacheEntry{
			meta:    Meta{PID: 1_000_000 + i},
			expires: base.Add(cacheTTL), // all live → sweep removes none
		}
	}
	cacheMu.Unlock()

	// A real resolve of the live test process; the over-cap map must be
	// reset before the new entry lands.
	pid := os.Getpid()
	if _, err := ProcessInfoCached(pid); err != nil {
		t.Fatalf("resolve under cap pressure failed: %v", err)
	}

	cacheMu.Lock()
	n := len(cacheMap)
	_, has := cacheMap[pid]
	cacheMu.Unlock()
	if !has {
		t.Error("new entry not stored after cap reset")
	}
	if n > cacheMaxEntries {
		t.Errorf("cache exceeded cap after eviction: %d > %d", n, cacheMaxEntries)
	}
}

type errSentinel string

func (e errSentinel) Error() string { return string(e) }

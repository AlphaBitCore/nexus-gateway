//go:build darwin

package proc

import (
	"sync"
	"time"
)

// ProcessInfo resolves a PID's executable path, name, bundle ID and owner
// via libproc plus one or two Info.plist disk reads (DetectBundleID and,
// for version-named helpers, BundleDisplayNameFromPath). On the macOS NE
// decision path that lookup runs once per intercepted flow, and a browser
// opening dozens of connections from a single PID would otherwise re-read
// the same Info.plist dozens of times, serially, before each flow's
// decision is returned to the extension.
//
// The metadata of a live PID is immutable, so the result is cached by PID
// for cacheTTL. PID reuse within the TTL is the only correctness risk: the
// kernel would have to recycle the number onto a different process inside
// the window, which on a desktop is rare, and the only consequence is a
// mislabeled audit row — the relay/interception decision never depends on
// process metadata. That tradeoff is deliberately accepted for the large
// hot-path win.

const (
	// cacheTTL bounds how long a resolved entry is reused. Short enough
	// that PID reuse onto a different process almost never aliases;
	// long enough that a burst of connections from one app pays the
	// disk read once.
	cacheTTL = 30 * time.Second
	// cacheMaxEntries caps the map so a long-lived daemon that has seen
	// many short-lived PIDs cannot grow it without bound. A desktop has
	// at most a few hundred live PIDs; when the cap is hit the expired
	// sweep runs first, and only if still over the cap is the whole map
	// dropped (the next lookups simply re-resolve).
	cacheMaxEntries = 4096
)

type cacheEntry struct {
	meta    Meta
	err     error
	expires time.Time
}

var (
	cacheMu  sync.Mutex
	cacheMap = make(map[int]cacheEntry)
	// nowFn is a seam so tests can drive expiry without sleeping.
	nowFn = time.Now
)

// ProcessInfoCached returns process metadata for pid, serving a cached
// result when one is present and unexpired. Concurrency-safe. Errors are
// cached too (with the same TTL) so a flood of flows for an exited PID
// does not hammer libproc on every flow.
func ProcessInfoCached(pid int) (Meta, error) {
	now := nowFn()

	cacheMu.Lock()
	if e, ok := cacheMap[pid]; ok && now.Before(e.expires) {
		cacheMu.Unlock()
		return e.meta, e.err
	}
	cacheMu.Unlock()

	// Resolve outside the lock — the disk reads + syscalls must not
	// serialize unrelated PIDs behind one another.
	meta, err := ProcessInfo(pid)

	cacheMu.Lock()
	if len(cacheMap) >= cacheMaxEntries {
		sweepExpiredLocked(now)
		if len(cacheMap) >= cacheMaxEntries {
			// Still full of live entries — reset rather than grow
			// unbounded. Re-resolution is cheap relative to a leak.
			cacheMap = make(map[int]cacheEntry, cacheMaxEntries)
		}
	}
	cacheMap[pid] = cacheEntry{meta: meta, err: err, expires: now.Add(cacheTTL)}
	cacheMu.Unlock()

	return meta, err
}

// sweepExpiredLocked drops every expired entry. Caller holds cacheMu.
func sweepExpiredLocked(now time.Time) {
	for pid, e := range cacheMap {
		if !now.Before(e.expires) {
			delete(cacheMap, pid)
		}
	}
}

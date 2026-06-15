// Package pidcache is an OS-neutral, concurrency-safe cache of process
// metadata keyed by PID. Every platform's interception path resolves the
// owning process for each intercepted connection; without a cache a
// browser opening dozens of connections from one PID re-runs the (disk /
// syscall / directory-service) lookup once per connection. The metadata of
// a live PID is immutable, so a short TTL collapses that to one lookup per
// process. PID reuse within the TTL is the only correctness risk, and its
// worst case is a mislabeled audit row — the interception decision never
// depends on process metadata.
//
// The cache holds the OS-specific lookup behind a function value so the
// platform-tagged packages (darwin/linux/windows) share this tested core
// while their syscall wrappers stay thin.
package pidcache

import (
	"sync"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/api"
)

// LookupFunc resolves process metadata for a PID from the OS.
type LookupFunc func(pid int) (api.ProcessMeta, error)

const (
	defaultTTL      = 30 * time.Second
	defaultMaxEntry = 4096
)

type entry struct {
	meta    api.ProcessMeta
	err     error
	expires time.Time
}

// Cache is a PID-keyed metadata cache with TTL + size bound. The zero value
// is not usable; construct with New.
type Cache struct {
	ttl      time.Duration
	maxEntry int
	now      func() time.Time

	mu sync.Mutex
	m  map[int]entry
}

// New returns a cache with the default 30s TTL and 4096-entry bound.
func New() *Cache {
	return &Cache{
		ttl:      defaultTTL,
		maxEntry: defaultMaxEntry,
		now:      time.Now,
		m:        make(map[int]entry),
	}
}

// Get returns metadata for pid, serving a cached result when present and
// unexpired, otherwise calling lookup and caching the outcome (errors are
// cached too, so a flood of connections for an exited PID does not hammer
// the OS). Concurrency-safe; lookup runs outside the lock so independent
// PIDs do not serialize.
func (c *Cache) Get(pid int, lookup LookupFunc) (api.ProcessMeta, error) {
	now := c.now()

	c.mu.Lock()
	if e, ok := c.m[pid]; ok && now.Before(e.expires) {
		c.mu.Unlock()
		return e.meta, e.err
	}
	c.mu.Unlock()

	meta, err := lookup(pid)

	c.mu.Lock()
	if len(c.m) >= c.maxEntry {
		c.sweepExpiredLocked(now)
		if len(c.m) >= c.maxEntry {
			c.m = make(map[int]entry, c.maxEntry)
		}
	}
	c.m[pid] = entry{meta: meta, err: err, expires: now.Add(c.ttl)}
	c.mu.Unlock()

	return meta, err
}

func (c *Cache) sweepExpiredLocked(now time.Time) {
	for pid, e := range c.m {
		if !now.Before(e.expires) {
			delete(c.m, pid)
		}
	}
}

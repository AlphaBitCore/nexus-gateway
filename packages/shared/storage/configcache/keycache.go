package configcache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"golang.org/x/sync/singleflight"
)

// KeyLoader returns the value for a single key.
type KeyLoader[K comparable, V any] func(ctx context.Context, key K) (V, error)

// KeyCache is a per-key lazy LRU + TTL cache with singleflight collapse on
// concurrent misses. It does not preload; entries are populated on first Get.
type KeyCache[K comparable, V any] struct {
	loader KeyLoader[K, V]
	lru    *lru.Cache[K, entry[V]]
	sf     singleflight.Group
	ttl    time.Duration
	log    *slog.Logger
	name   string
	now    func() time.Time

	onHit         func(name string)
	onMiss        func(name string)
	onInvalidate  func(name string, count int)
	hitCount      atomic.Uint64
	missCount     atomic.Uint64
	invalidations atomic.Uint64
}

type entry[V any] struct {
	value    V
	loadedAt time.Time
}

// KeyOption configures a KeyCache at construction time.
type KeyOption func(o *keyOptions)

type keyOptions struct {
	logger       *slog.Logger
	name         string
	now          func() time.Time
	onHit        func(name string)
	onMiss       func(name string)
	onInvalidate func(name string, count int)
}

// WithKeyLogger sets the slog.Logger used for cache events.
func WithKeyLogger(l *slog.Logger) KeyOption {
	return func(o *keyOptions) { o.logger = l }
}

// WithKeyName sets a human-readable name surfaced in logs and metrics.
func WithKeyName(name string) KeyOption {
	return func(o *keyOptions) { o.name = name }
}

// WithKeyClock injects a clock function for tests.
func WithKeyClock(now func() time.Time) KeyOption {
	return func(o *keyOptions) { o.now = now }
}

// WithKeyOnHit registers a callback fired on every cache hit.
func WithKeyOnHit(fn func(name string)) KeyOption {
	return func(o *keyOptions) { o.onHit = fn }
}

// WithKeyOnMiss registers a callback fired on every cache miss.
func WithKeyOnMiss(fn func(name string)) KeyOption {
	return func(o *keyOptions) { o.onMiss = fn }
}

// WithKeyOnInvalidate registers a callback fired when Invalidate or Purge
// removes entries. The count argument is the number of entries removed.
func WithKeyOnInvalidate(fn func(name string, count int)) KeyOption {
	return func(o *keyOptions) { o.onInvalidate = fn }
}

// NewKeyCache builds a KeyCache. capacity is the LRU upper bound; ttl is
// the per-entry expiry (zero means no TTL — entries live until evicted by
// LRU pressure or invalidated).
func NewKeyCache[K comparable, V any](loader KeyLoader[K, V], capacity int, ttl time.Duration, opts ...KeyOption) (*KeyCache[K, V], error) {
	if loader == nil {
		return nil, errors.New("configcache: loader must not be nil")
	}
	if capacity <= 0 {
		return nil, fmt.Errorf("configcache: capacity must be > 0, got %d", capacity)
	}
	o := keyOptions{logger: slog.Default(), name: "keycache", now: time.Now}
	for _, opt := range opts {
		opt(&o)
	}
	cache, err := lru.New[K, entry[V]](capacity)
	if err != nil {
		return nil, fmt.Errorf("configcache: build lru: %w", err)
	}
	return &KeyCache[K, V]{
		loader:       loader,
		lru:          cache,
		ttl:          ttl,
		log:          o.logger,
		name:         o.name,
		now:          o.now,
		onHit:        o.onHit,
		onMiss:       o.onMiss,
		onInvalidate: o.onInvalidate,
	}, nil
}

// Get returns the value for key. On hit returns the cached value. On miss
// (or expired entry) loads via the loader, caches the result, and returns.
// Concurrent misses on the same key are collapsed via singleflight.
func (c *KeyCache[K, V]) Get(ctx context.Context, key K) (V, error) {
	if e, ok := c.lru.Get(key); ok && !c.expired(e.loadedAt) {
		c.hitCount.Add(1)
		if c.onHit != nil {
			c.onHit(c.name)
		}
		return e.value, nil
	}
	c.missCount.Add(1)
	if c.onMiss != nil {
		c.onMiss(c.name)
	}

	v, err, _ := c.sf.Do(c.sfKey(key), func() (any, error) {
		// Re-check inside singleflight in case a parallel caller already
		// populated the entry while we were waiting on the group.
		if e, ok := c.lru.Get(key); ok && !c.expired(e.loadedAt) {
			return e.value, nil
		}
		val, lerr := c.loader(ctx, key)
		if lerr != nil {
			return nil, lerr
		}
		c.lru.Add(key, entry[V]{value: val, loadedAt: c.now()})
		return val, nil
	})
	if err != nil {
		var zero V
		return zero, err
	}
	return v.(V), nil
}

// Invalidate removes the given keys from the cache. Missing keys are
// ignored. Returns the count of entries actually removed.
func (c *KeyCache[K, V]) Invalidate(keys ...K) int {
	removed := 0
	for _, k := range keys {
		if c.lru.Remove(k) {
			removed++
		}
	}
	if removed > 0 {
		c.invalidations.Add(uint64(removed))
		if c.onInvalidate != nil {
			c.onInvalidate(c.name, removed)
		}
		c.log.Debug("keycache invalidated", "cache", c.name, "removed", removed)
	}
	return removed
}

// Purge clears all entries.
func (c *KeyCache[K, V]) Purge() {
	n := c.lru.Len()
	c.lru.Purge()
	if n > 0 {
		c.invalidations.Add(uint64(n))
		if c.onInvalidate != nil {
			c.onInvalidate(c.name, n)
		}
		c.log.Info("keycache purged", "cache", c.name, "removed", n)
	}
}

// Size returns the current number of entries.
func (c *KeyCache[K, V]) Size() int {
	return c.lru.Len()
}

// Name returns the cache name set via WithKeyName.
func (c *KeyCache[K, V]) Name() string {
	return c.name
}

// Stats returns hit/miss/invalidation counters since cache creation.
func (c *KeyCache[K, V]) Stats() (hits, misses, invalidations uint64) {
	return c.hitCount.Load(), c.missCount.Load(), c.invalidations.Load()
}

func (c *KeyCache[K, V]) expired(loadedAt time.Time) bool {
	if c.ttl <= 0 {
		return false
	}
	return c.now().Sub(loadedAt) > c.ttl
}

// sfKey converts the typed key to a singleflight string key. Singleflight
// keys must be string-typed; we use fmt.Sprint which handles all comparable
// types safely.
func (c *KeyCache[K, V]) sfKey(k K) string {
	return fmt.Sprint(k)
}

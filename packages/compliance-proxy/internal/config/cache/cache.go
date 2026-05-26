package cache

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Invalidatable is the interface that typed caches implement so the
// Subscriber can dispatch invalidation events without knowing T.
type Invalidatable interface {
	Name() string
	InvalidateCache()
}

// Cache is a type-safe config cache for a single category. It replaces the
// untyped Manager for new code. Consumers get (T, error) from Get instead
// of (interface{}, error).
type Cache[T any] struct {
	mu          sync.RWMutex
	name        string
	data        T
	lastRefresh time.Time
	valid       bool
	ttl         time.Duration
	loadFn      func(ctx context.Context) (T, error)
	logger      *slog.Logger
}

// NewCache creates a typed config cache.
func NewCache[T any](name string, ttl time.Duration, loadFn func(ctx context.Context) (T, error), logger *slog.Logger) *Cache[T] {
	return &Cache[T]{
		name:   name,
		ttl:    ttl,
		loadFn: loadFn,
		logger: logger,
	}
}

// Name returns the cache category name (used by Subscriber for dispatch).
func (c *Cache[T]) Name() string { return c.name }

// Get retrieves cached data. If invalid or expired, reloads via loadFn.
func (c *Cache[T]) Get(ctx context.Context) (T, error) {
	c.mu.RLock()
	if c.valid && !c.isExpired() {
		data := c.data
		c.mu.RUnlock()
		if CacheHits != nil {
			CacheHits.With(c.name).Inc()
		}
		c.updateStaleness()
		return data, nil
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()

	// Double-check after acquiring write lock.
	if c.valid && !c.isExpired() {
		if CacheHits != nil {
			CacheHits.With(c.name).Inc()
		}
		// Inline staleness update — cannot call updateStaleness() here
		// because we already hold the write lock and updateStaleness
		// acquires a read lock, which would deadlock.
		if !c.lastRefresh.IsZero() && Staleness != nil {
			Staleness.With(c.name).Set(time.Since(c.lastRefresh).Seconds())
		}
		return c.data, nil
	}

	if CacheMisses != nil {
		CacheMisses.With(c.name).Inc()
	}

	data, err := c.loadFn(ctx)
	if err != nil {
		c.logger.Warn("config cache reload failed",
			slog.String("category", c.name),
			slog.String("error", err.Error()),
		)
		if c.valid || !c.lastRefresh.IsZero() {
			return c.data, err // return stale
		}
		var zero T
		return zero, err
	}

	now := time.Now()
	c.data = data
	c.lastRefresh = now
	c.valid = true

	if LastRefresh != nil {
		LastRefresh.With(c.name).Set(float64(now.Unix()))
	}
	if Staleness != nil {
		Staleness.With(c.name).Set(0)
	}

	c.logger.Info("config cache refreshed", slog.String("category", c.name))
	return data, nil
}

// InvalidateCache marks the cache as invalid. Next Get triggers a reload.
func (c *Cache[T]) InvalidateCache() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.valid = false
	c.logger.Info("config cache invalidated", slog.String("category", c.name))
}

// StalenessSeconds returns seconds since last refresh, or -1 if never loaded.
func (c *Cache[T]) StalenessSeconds() float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.lastRefresh.IsZero() {
		return -1
	}
	return time.Since(c.lastRefresh).Seconds()
}

func (c *Cache[T]) isExpired() bool {
	if c.ttl <= 0 {
		return false
	}
	return time.Since(c.lastRefresh) > c.ttl
}

func (c *Cache[T]) updateStaleness() {
	c.mu.RLock()
	lr := c.lastRefresh
	c.mu.RUnlock()
	if !lr.IsZero() && Staleness != nil {
		Staleness.With(c.name).Set(time.Since(lr).Seconds())
	}
}

// GetAndCallback retrieves data and calls fn with it. Used by Subscriber for
// eager reload after invalidation.
func (c *Cache[T]) GetAndCallback(ctx context.Context, fn func(T)) error {
	data, err := c.Get(ctx)
	if err != nil {
		return fmt.Errorf("reload %s: %w", c.name, err)
	}
	if fn != nil {
		fn(data)
	}
	return nil
}

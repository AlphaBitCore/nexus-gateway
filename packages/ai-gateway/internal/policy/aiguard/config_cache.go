// packages/ai-gateway/internal/policy/aiguard/config_cache.go
package aiguard

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/configstore"
)

// Loader abstracts the persistent config source (production:
// configstore.AIGuardStore). Interface exists so ConfigCache can be unit
// tested without a real pgxpool.
type Loader interface {
	Load(ctx context.Context) (*configstore.AIGuardConfig, error)
}

// ConfigCache is the in-memory hot-path cache for AIGuardConfig. The HTTP
// handler reads config on every classify request; a single DB hit per TTL
// window plus a single-flight load under contention keeps the hot path
// allocation-light.
//
// Invalidation is push-based: a shadow listener (Task 35) calls Invalidate
// the moment Admin UI saves a new config, so the effective TTL acts as a
// safety net rather than the primary refresh mechanism.
type ConfigCache struct {
	loader Loader
	ttl    time.Duration
	logger *slog.Logger

	snap   atomic.Pointer[cachedConfig]
	loadMu sync.Mutex
}

// cachedConfig carries the snapshot plus its expiry so Get can decide
// freshness without holding the lock.
type cachedConfig struct {
	cfg      *configstore.AIGuardConfig
	expireAt time.Time
}

// NewConfigCache returns a cache that reloads via loader and expires
// entries after ttl. logger must be non-nil (use a discard handler for
// tests).
func NewConfigCache(loader Loader, ttl time.Duration, logger *slog.Logger) *ConfigCache {
	return &ConfigCache{loader: loader, ttl: ttl, logger: logger}
}

// Get returns the current config snapshot, loading on miss / expiry.
//
// Concurrency model: a lock-free happy path for cache hits (atomic load +
// time compare), plus a mutex-guarded slow path with double-check to make
// concurrent misses collapse into a single backend load.
//
// Error handling: if a prior snapshot exists and the loader fails, we log a
// warning and keep serving stale config (fail-open). Pre-GA we prefer stale
// config over hard-failing the hot path. Only when there has never been a
// successful load do we bubble the error up to the caller.
func (c *ConfigCache) Get(ctx context.Context) (*configstore.AIGuardConfig, error) {
	if snap := c.snap.Load(); snap != nil && time.Now().Before(snap.expireAt) {
		return snap.cfg, nil
	}
	c.loadMu.Lock()
	defer c.loadMu.Unlock()
	// Double-check: another goroutine may have refreshed while we were
	// waiting for the lock.
	if snap := c.snap.Load(); snap != nil && time.Now().Before(snap.expireAt) {
		return snap.cfg, nil
	}
	cfg, err := c.loader.Load(ctx)
	if err != nil {
		c.logger.Warn("aiguard: config load failed; keeping stale snapshot", "error", err)
		if snap := c.snap.Load(); snap != nil {
			return snap.cfg, nil
		}
		return nil, err
	}
	c.snap.Store(&cachedConfig{cfg: cfg, expireAt: time.Now().Add(c.ttl)})
	return cfg, nil
}

// Invalidate clears the cached snapshot; the next Get will reload.
// Called by the shadow/pubsub listener whenever ai_guard_config changes.
func (c *ConfigCache) Invalidate() { c.snap.Store(nil) }

package pipeline

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/configcache"
)

// HookConfigLoader loads hook configs from the database.
type HookConfigLoader func(ctx context.Context) ([]core.HookConfig, error)

// HookConfigCache is the unified hook-config cache used by all three
// data-plane services. Storage is delegated to
// shared/configcache.SnapshotCache for atomic-pointer swap and metrics;
// on top of the snapshot a PolicyResolver holds the compiled pipeline state.
//
// Two operating modes via the ttl argument to NewHookConfigCache:
//
//   - ttl > 0 (AI Gateway, Compliance Proxy): push-driven invalidation via
//     Hub thingclient OnConfigChanged calling Reload, plus a TTL backstop
//     on Resolver() that closes any gap if the push channel is degraded.
//
//   - ttl = 0 (Agent): pure push mode. Resolver() never auto-reloads; the
//     configsync layer pulls hook configs from Hub and calls Reload. The
//     agent has no direct DB access, so no TTL backstop is meaningful.
type HookConfigCache struct {
	snap     *configcache.SnapshotCache[core.HookConfig]
	resolver *PolicyResolver
	ttl      time.Duration
	logger   *slog.Logger

	mu       sync.Mutex
	lastLoad time.Time
}

// NewHookConfigCache creates a cache.
func NewHookConfigCache(loader HookConfigLoader, registry *core.HookRegistry, ttl time.Duration, logger *slog.Logger) *HookConfigCache {
	if logger == nil {
		logger = slog.Default()
	}
	c := &HookConfigCache{
		resolver: NewPolicyResolver(nil, registry, logger),
		ttl:      ttl,
		logger:   logger,
	}

	// SnapshotCache stores hook configs by ID; on every successful
	// (re)load the on-load hook materializes them back into a slice and
	// swaps the resolver atomically. Errors during load preserve the
	// previous snapshot, so the resolver also keeps its previous state.
	snapLoader := func(ctx context.Context) (map[string]core.HookConfig, error) {
		cfgs, err := loader(ctx)
		if err != nil {
			return nil, err
		}
		out := make(map[string]core.HookConfig, len(cfgs))
		for _, cfg := range cfgs {
			out[cfg.ID] = cfg
		}
		return out, nil
	}

	c.snap = configcache.NewSnapshotCache(
		snapLoader,
		configcache.WithSnapshotName("hook_configs"),
		configcache.WithSnapshotLogger(logger),
		configcache.WithSnapshotOnLoad(func(_ string, size int) {
			cfgs := make([]core.HookConfig, 0, size)
			for _, v := range c.snap.All() {
				cfgs = append(cfgs, v)
			}
			c.resolver.Swap(cfgs)
			c.mu.Lock()
			c.lastLoad = time.Now()
			c.mu.Unlock()
		}),
	)
	return c
}

// Start performs the initial load. The TTL backstop and any external
// invalidation source (currently the Hub thingclient OnConfigChanged
// callback) drive subsequent reloads.
func (c *HookConfigCache) Start(ctx context.Context) error {
	if err := c.snap.Load(ctx); err != nil {
		c.logger.Warn("initial hook config load failed, continuing with empty config", "error", err)
	}
	return nil
}

// Resolver returns the PolicyResolver with the current config snapshot.
// In TTL mode (ttl > 0) triggers a reload if the cache is stale. In
// pure push mode (ttl == 0) returns the resolver directly without any
// TTL bookkeeping.
func (c *HookConfigCache) Resolver(ctx context.Context) *PolicyResolver {
	if c.ttl > 0 {
		c.mu.Lock()
		stale := time.Since(c.lastLoad) > c.ttl
		c.mu.Unlock()
		if stale {
			_ = c.Reload(ctx)
		}
	}
	return c.resolver
}

// Reload forces an immediate reload from the database.
func (c *HookConfigCache) Reload(ctx context.Context) error {
	return c.snap.Load(ctx)
}

// HookSnapshotEntry is the redacted view of a HookConfig used by runtime
// introspection. The per-hook Config map is dropped because it often
// carries webhook URLs / auth headers / API keys.
type HookSnapshotEntry struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	ImplementationID  string   `json:"implementationId"`
	Stage             string   `json:"stage"`
	Priority          int      `json:"priority"`
	Enabled           bool     `json:"enabled"`
	FailBehavior      string   `json:"failBehavior"`
	TimeoutMs         int      `json:"timeoutMs"`
	ApplicableIngress []string `json:"applicableIngress"`
}

// Snapshot returns the loaded hook configs as a redacted slice for
// runtime introspection. Returns nil when the cache has not loaded yet.
func (c *HookConfigCache) Snapshot() []HookSnapshotEntry {
	if c == nil || c.snap == nil {
		return nil
	}
	all := c.snap.All()
	out := make([]HookSnapshotEntry, 0, len(all))
	for _, h := range all {
		out = append(out, HookSnapshotEntry{
			ID:                h.ID,
			Name:              h.Name,
			ImplementationID:  h.ImplementationID,
			Stage:             h.Stage,
			Priority:          h.Priority,
			Enabled:           h.Enabled,
			FailBehavior:      h.FailBehavior,
			TimeoutMs:         h.TimeoutMs,
			ApplicableIngress: h.ApplicableIngress,
		})
	}
	return out
}

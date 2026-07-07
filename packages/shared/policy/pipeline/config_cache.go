package pipeline

import (
	"context"
	"errors"
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
// Config changes are low-frequency, push-driven events: the Hub thingclient
// OnConfigChanged callback calls Reload when an admin edits hook config. The
// request path NEVER loads — Resolver is a pure snapshot getter, so a slow
// database can never stall or stampede request goroutines (thousands of
// concurrent requests each observing a stale TTL and issuing their own full
// reload is exactly the failure mode this design forbids).
//
// Two operating modes via the ttl argument to NewHookConfigCache:
//
//   - ttl > 0 (AI Gateway, Compliance Proxy): push-driven invalidation via
//     Reload, plus a background ticker started by Start that refreshes every
//     ttl as a backstop if the push channel is degraded. The backstop runs
//     off the request path and holds at most one load in flight.
//
//   - ttl = 0 (Agent): pure push mode. No ticker; the configsync layer
//     pulls hook configs from Hub and calls Reload. The agent has no direct
//     DB access, so no TTL backstop is meaningful.
type HookConfigCache struct {
	snap     *configcache.SnapshotCache[core.HookConfig]
	resolver *PolicyResolver
	ttl      time.Duration
	logger   *slog.Logger

	// loadMu serializes snapshot loads (backstop tick, Hub push Reload,
	// startup). Loads commit in start order, so the last committer read the
	// freshest rows — a slow backstop load can never overwrite a newer push
	// result with stale content. Never touched on the request path.
	loadMu sync.Mutex
}

// ttlRefreshTimeout bounds a single background backstop load so a wedged
// database cannot pin the refresh goroutine's context forever. Loads are
// serial (one ticker loop), so a slow load also cannot stack.
const ttlRefreshTimeout = 30 * time.Second

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
			// Only swap (and recompile hook matchers + union prefilters) when the
			// content actually changed. This callback fires on EVERY load including
			// the TTL backstop refresh; a no-change reload must not churn the
			// generation under load. Real changes (startup, Hub push delta) differ
			// and swap normally.
			c.resolver.SwapIfContentChanged(cfgs)
			// Build the (potentially expensive) engines off the request path.
			// Background and best-effort: it must never block this callback,
			// which runs at startup, on Hub push, and on the backstop ticker.
			// The lazy build in resolve() is the fallback if a request arrives
			// before prewarm finishes. swapGen guards staleness.
			go c.resolver.Prewarm()
		}),
	)
	return c
}

// Start performs the initial load and, in TTL mode, starts the background
// backstop ticker that refreshes the snapshot every ttl. The ticker stops
// when ctx is canceled, so callers must pass a process-lifetime context.
// Push invalidation (Hub thingclient OnConfigChanged → Reload) remains the
// primary update path; the ticker only closes the gap when push is degraded.
func (c *HookConfigCache) Start(ctx context.Context) error {
	if err := c.load(ctx); err != nil {
		c.logger.Warn("initial hook config load failed, continuing with empty config", "error", err)
	}
	if c.ttl > 0 {
		go c.refreshLoop(ctx)
	}
	return nil
}

// refreshLoop is the TTL backstop: one serial background loop, so at most
// one backstop load is ever in flight regardless of load or DB latency.
func (c *HookConfigCache) refreshLoop(ctx context.Context) {
	ticker := time.NewTicker(c.ttl)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.load(ctx); err != nil {
				c.logger.Warn("hook config backstop refresh failed; previous snapshot stays active", "error", err)
			}
		}
	}
}

// load runs one serialized, deadline-bounded snapshot load. Every load —
// startup, backstop tick, Hub push — goes through here, so a wedged
// database can pin at most one goroutine for at most ttlRefreshTimeout,
// and loads commit in start order (see loadMu).
func (c *HookConfigCache) load(ctx context.Context) error {
	// A nil context must fail gracefully (as snap.Load does) rather than panic
	// in WithTimeout — the agent's shadow-apply path surfaces a nil-context
	// reload as a normal error and keeps the prior policy.
	if ctx == nil {
		return errors.New("configcache: nil context")
	}
	c.loadMu.Lock()
	defer c.loadMu.Unlock()
	rctx, cancel := context.WithTimeout(ctx, ttlRefreshTimeout)
	defer cancel()
	return c.snap.Load(rctx)
}

// Resolver returns the PolicyResolver holding the current config snapshot.
// Pure getter on the request hot path: freshness is owned by push (Reload)
// and the Start backstop ticker, never by request goroutines.
func (c *HookConfigCache) Resolver(_ context.Context) *PolicyResolver {
	return c.resolver
}

// Reload forces an immediate reload from the database. Called by the Hub
// push-invalidation path (thingclient OnConfigChanged) and boot wiring.
func (c *HookConfigCache) Reload(ctx context.Context) error {
	return c.load(ctx)
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

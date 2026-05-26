package geminicache

import (
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/cacheconfig"
)

// ProviderInfo is the minimal Provider description the ManagerSet needs to
// build a per-provider Manager. The full store.Provider is intentionally NOT
// imported here to keep this package leaf-level.
type ProviderInfo struct {
	ID          string
	AdapterType string // "gemini" | "vertex" | other (will be filtered)
}

// ProviderLister returns the current set of Providers in the gateway's view.
// Implementations typically wrap cachelayer.Layer.ProvidersAll() with a
// type adapter; see ai-gateway main.go for the binding.
type ProviderLister func() []ProviderInfo

// ManagerSet maintains a pool of geminicache.Manager instances, one per
// Gemini/Vertex Provider, each resolved against the three-tier cache config.
//
// Hot-path lookup is `mgrSet.Get(providerID)` — sync.Map.Load, O(1). The set
// is reconciled when SetConfig(blob) or ReloadProviders() is called. Both
// paths converge on rebuild(), which reuses existing Manager pointers where
// possible and tears down managers for Providers that no longer exist.
type ManagerSet struct {
	rdb     redis.UniversalClient
	res     KeyResolver
	metrics *Metrics
	logger  *slog.Logger
	lister  ProviderLister

	// cfgBlob is the last-seen 3-tier cache payload. atomic.Pointer
	// is used so concurrent rebuilds don't race on Read; rebuilds themselves
	// are serialised by the rebuildMu mutex below.
	cfgBlob atomic.Pointer[cacheconfig.CacheConfigBlob]

	// managers is the live map[providerID]*Manager. sync.Map gives lock-free
	// hot-path Load and supports concurrent Range during rebuild.
	managers sync.Map

	// rebuildMu serialises rebuild() so two concurrent shadow events do not
	// race on the manager map structure.
	rebuildMu sync.Mutex
}

// NewSet constructs a ManagerSet. rdb, metrics, and KeyResolver are shared
// across all per-provider Managers (single Redis pool, single metrics
// namespace, single api-key lookup path). Pass an empty Config initially —
// SetConfig is required before any Manager is created.
func NewSet(rdb redis.UniversalClient, res KeyResolver, metrics *Metrics, lister ProviderLister, logger *slog.Logger) *ManagerSet {
	if logger == nil {
		logger = slog.Default()
	}
	if metrics == nil {
		metrics = NewMetrics(nil)
	}
	return &ManagerSet{
		rdb:     rdb,
		res:     res,
		metrics: metrics,
		logger:  logger,
		lister:  lister,
	}
}

// SetConfig replaces the last-known cache blob and triggers a rebuild
// of every per-provider Manager. Called from the gateway's OnConfigChanged
// callback when the `cache` shadow key fires.
func (s *ManagerSet) SetConfig(blob cacheconfig.CacheConfigBlob) {
	clone := blob // shallow copy; CacheConfigBlob contains maps, but rebuild treats them as read-only snapshots
	s.cfgBlob.Store(&clone)
	s.rebuild()
}

// ReloadProviders re-derives the per-provider Manager map using the LAST
// cache blob and the CURRENT provider list. Called from the gateway's
// OnConfigChanged callback when the `providers` shadow key fires.
func (s *ManagerSet) ReloadProviders() {
	if s.cfgBlob.Load() == nil {
		// No config yet — next SetConfig will pick up the latest provider list.
		return
	}
	s.rebuild()
}

// Get returns the Manager for the given Provider, or nil if the Provider is
// not in the Gemini/Vertex family (or has not yet been resolved). Hot-path
// safe — single sync.Map.Load.
func (s *ManagerSet) Get(providerID string) *Manager {
	v, ok := s.managers.Load(providerID)
	if !ok {
		return nil
	}
	return v.(*Manager)
}

// rebuild reconciles the manager map against the current blob + provider
// list. Idempotent — safe to call multiple times in quick succession.
func (s *ManagerSet) rebuild() {
	s.rebuildMu.Lock()
	defer s.rebuildMu.Unlock()

	blobPtr := s.cfgBlob.Load()
	if blobPtr == nil {
		return
	}
	blob := *blobPtr

	providers := s.lister()
	seen := make(map[string]struct{}, len(providers))

	for _, p := range providers {
		if p.AdapterType != "gemini" && p.AdapterType != "vertex" {
			continue
		}
		seen[p.ID] = struct{}{}

		eff := cacheconfig.Resolve(blob, p.ID, p.AdapterType)
		cfg := Config{
			Enabled:                 eff.CacheEnabled,
			MinSystemChars:          eff.MinSystemChars,
			TTLSeconds:              eff.TTLSeconds,
			CircuitBreakerThreshold: eff.CircuitBreakerThreshold,
			CircuitBreakerOpenSecs:  eff.CircuitBreakerOpenSecs,
		}

		if existing, ok := s.managers.Load(p.ID); ok {
			existing.(*Manager).Reload(cfg)
		} else {
			mgr := New(s.rdb, s.res, s.metrics, cfg, s.logger.With(
				slog.String("provider_id", p.ID),
				slog.String("adapter_type", p.AdapterType),
			))
			s.managers.Store(p.ID, mgr)
		}
	}

	// Tear down managers whose Provider is gone.
	s.managers.Range(func(k, _ any) bool {
		id := k.(string)
		if _, kept := seen[id]; !kept {
			s.managers.Delete(id)
			s.logger.Info("geminicache manager torn down for removed provider", "provider_id", id)
		}
		return true
	})
}

// SnapshotForIntrospection returns a debug view of the current managers map.
// Used by the gateway's /debug/runtime endpoint.
func (s *ManagerSet) SnapshotForIntrospection() map[string]any {
	out := map[string]any{}
	s.managers.Range(func(k, v any) bool {
		mgr := v.(*Manager)
		out[k.(string)] = map[string]any{
			"enabled":          mgr.cfg.get().Enabled,
			"min_system_chars": mgr.cfg.get().MinSystemChars,
			"ttl_seconds":      mgr.cfg.get().TTLSeconds,
		}
		return true
	})
	return out
}

// cache.go — cachelayer + Gemini cache manager set wiring.
package wiring

import (
	"context"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	geminicache "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/gemini"
	cachelayer "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/layer"
	credmanager "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/credentials/manager"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// InitCacheLayer creates and starts the in-memory snapshot cache.
// Returns the layer; callers must not proceed if error is non-nil (the
// entire hot path depends on cacheLayer).
func InitCacheLayer(ctx context.Context, db *store.DB, logger *slog.Logger, opsReg *registry.Registry) (*cachelayer.Layer, error) {
	cacheLayer, err := cachelayer.New(db, logger, cachelayer.Config{
		Metrics: cachelayer.NewMetrics(opsReg),
	})
	if err != nil {
		return nil, err
	}
	if err := cacheLayer.Start(ctx); err != nil {
		slog.Warn("cachelayer initial load reported errors; continuing in degraded mode", "error", err)
	}
	return cacheLayer, nil
}

// InitGeminiCacheMgrSet creates the per-provider Gemini cache manager pool.
func InitGeminiCacheMgrSet(
	rdb redis.UniversalClient,
	cacheLayer *cachelayer.Layer,
	credMgr *credmanager.Manager,
	logger *slog.Logger,
) *geminicache.ManagerSet {
	listGeminiProviders := func() []geminicache.ProviderInfo {
		if cacheLayer == nil {
			return nil
		}
		all := cacheLayer.ProvidersAll()
		out := make([]geminicache.ProviderInfo, 0, len(all))
		for id, p := range all {
			if p.AdapterType != "gemini" && p.AdapterType != "vertex" {
				continue
			}
			out = append(out, geminicache.ProviderInfo{ID: id, AdapterType: p.AdapterType})
		}
		return out
	}
	return geminicache.NewSet(
		rdb,
		GeminiKeyResolverFrom(cacheLayer, credMgr),
		geminicache.NewMetrics(prometheus.DefaultRegisterer),
		listGeminiProviders,
		logger,
	)
}

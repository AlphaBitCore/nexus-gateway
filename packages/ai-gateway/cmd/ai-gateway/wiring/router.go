// routingcore.go — router resolver + smart routing deps wiring.
package wiring

import (
	"context"
	"log/slog"

	cachelayer "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/layer"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provtarget "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/target"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/capability"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/llm"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/strategies"
)

// the production Router must implement the optional content-rule probe
// the proxy gate gives the lazy-canonical decision through (Handler.needCanonical
// type-asserts this interface). Assert it at compile time so a future refactor
// that drops Resolver.RequestNeedsCanonical fails the build instead of silently
// reverting to always-compute (which would mask the optimization in prod).
var _ interface {
	RequestNeedsCanonical(context.Context, string) bool
} = (*routing.Resolver)(nil)

// InitRouter builds the strategy registry, health ranker, and resolver.
// Returns (strategyReg, healthRanker, resolver, capCache).
func InitRouter(
	ctx context.Context,
	cacheLayer *cachelayer.Layer,
	healthTracker *store.HealthTracker,
	ptResolver *provtarget.PgResolver,
	adapterReg *provcore.Registry,
	logger *slog.Logger,
	enforceNamedModelModality bool,
) (*strategies.StrategyRegistry, *routingcore.HealthRanker, *routing.Resolver, *capability.Cache) {
	capCache := capability.NewCache()
	strategyReg := strategies.NewStrategyRegistry()
	healthRanker := routingcore.NewHealthRanker(healthTracker)
	routerResolver := routing.NewResolver(cacheLayer, strategyReg, healthRanker, logger, capCache, enforceNamedModelModality)

	// One catalogue source, read once per request when the routing context is
	// prepared. The smart strategy still receives it as a dependency so it can
	// fall back for callers that prepare no context, but the prepared pool is
	// what it reads on the live path — the same snapshot, narrowed once.
	var smartStore routingcore.SmartStore
	if cacheLayer != nil {
		smartStore = routingcore.NewSmartStoreDB(cacheLayer)
		routerResolver = routerResolver.WithModelPool(smartStore)
	}

	var smartDeps *strategies.SmartDeps
	if cacheLayer != nil && ptResolver != nil {
		smartDeps = &strategies.SmartDeps{
			Store:     smartStore,
			Lookup:    routerResolver.LookupTargetFunc(),
			RouterLLM: newRouterDecider(ctx, ptResolver, adapterReg, cacheLayer, logger),
			Logger:    logger,
		}
	}
	// The latency strategy reads the health tracker's windowed p95 to order
	// targets. A nil tracker → nil stats func → every target reads cold (safe
	// random load-balance), so the strategy never strands a request.
	var latencyStats routingcore.LatencyStatsFunc
	if healthTracker != nil {
		latencyStats = healthTracker.GetLatencyP95
	}
	strategies.RegisterAllStrategies(strategyReg, routerResolver.LookupTargetFunc(), latencyStats, smartDeps)
	strategyReg.Freeze()

	return strategyReg, healthRanker, routerResolver, capCache
}

// newRouterDecider builds the production router-LLM decider with catalog
// pricing attached, so the router call's cost lands on the request's
// traffic_event row. Reuses the SAME price source as the AI Guard wiring
// (AIGuardModelLookup, satisfied by *cachelayer.Layer — in-memory snapshot
// reads, no DB roundtrip) via the shared newModelPriceLookup helper (see
// aiguard.go), which returns (0,0) on lookup failure or an unpriced model —
// so an unpriced router model leaves the cost zero rather than failing the
// request — and logs a Warn naming the model id either way.
func newRouterDecider(
	ctx context.Context,
	resolver provtarget.Resolver,
	adapters llm.AdapterLookup,
	models AIGuardModelLookup,
	logger *slog.Logger,
) *llm.AdapterDecider {
	d := llm.NewAdapterDecider(resolver, adapters, logger)
	if models == nil {
		return d
	}
	d.PriceLookup = newModelPriceLookup(ctx, models, logger, "router")
	return d
}

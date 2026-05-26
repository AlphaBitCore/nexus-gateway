// routingcore.go — router resolver + smart routing deps wiring.
package wiring

import (
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

// InitRouter builds the strategy registry, health ranker, and resolver.
// Returns (strategyReg, healthRanker, resolver, capCache).
func InitRouter(
	cacheLayer *cachelayer.Layer,
	healthTracker *store.HealthTracker,
	ptResolver *provtarget.PgResolver,
	adapterReg *provcore.Registry,
	logger *slog.Logger,
) (*strategies.StrategyRegistry, *routingcore.HealthRanker, *routing.Resolver, *capability.Cache) {
	capCache := capability.NewCache()
	strategyReg := strategies.NewStrategyRegistry()
	healthRanker := routingcore.NewHealthRanker(healthTracker)
	routerResolver := routing.NewResolver(cacheLayer, strategyReg, healthRanker, logger, capCache)

	var smartDeps *strategies.SmartDeps
	if cacheLayer != nil && ptResolver != nil {
		smartDeps = &strategies.SmartDeps{
			Store:     routingcore.NewSmartStoreDB(cacheLayer),
			Lookup:    routerResolver.LookupTargetFunc(),
			RouterLLM: llm.NewAdapterDecider(ptResolver, adapterReg, logger),
			Logger:    logger,
		}
	}
	strategies.RegisterAllStrategies(strategyReg, routerResolver.LookupTargetFunc(), smartDeps)
	strategyReg.Freeze()

	return strategyReg, healthRanker, routerResolver, capCache
}

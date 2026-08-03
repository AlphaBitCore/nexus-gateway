package strategies

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// endpointUsesLLMRouter reports whether the smart strategy's LLM task-router
// applies to this endpoint. Only the text-completion kinds (chat and the
// Responses API) have the task types the router classifies; every other
// modality uses the deterministic modality-aware path instead.
func endpointUsesLLMRouter(kind typology.EndpointKind) bool {
	return kind == typology.EndpointKindChat ||
		kind == typology.EndpointKindResponses ||
		kind == "" // unclassified defaults to the chat router (back-compat)
}

// modalityAutoTargets resolves model=auto on a non-chat endpoint to every
// enabled model of that endpoint's modality (image models for image
// generation, audio models for tts/stt/realtime, …), honouring the VK's
// allowed-models allowlist. It returns all matching targets — the resolver's
// health-aware reorder and the executor's failover then pick among them — so
// auto degrades to "any available model of the right modality" without an LLM
// router call. An empty result falls through to the configured default, exactly
// like the chat path.
func (s *SmartStrategy) modalityAutoTargets(ctx context.Context, rctx *core.RoutingContext, trace *[]core.TraceEntry, start time.Time) ([]core.RoutingTarget, error) {
	candidates, err := s.deps.Store.ListEnabledCandidates(ctx, rctx.EndpointType)
	if err != nil {
		s.deps.Logger.Warn("smart: list modality candidates failed", "endpoint", rctx.EndpointType, "error", err)
		return nil, nil
	}

	// Order the candidates cheapest-first so non-chat auto has a predictable
	// cost story (chat auto weighs cost via the LLM router; the modalities
	// below have no task classifier, so a deterministic price order is the
	// honest substitute for "smart"). The resolver's health-aware reorder runs
	// on top and is stable within a health tier, so the effective order is
	// "healthy, then cheapest". A nil input price sorts last (unpriced models
	// are the least preferred default).
	ordered := append([]core.SmartModelRow(nil), candidates...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return inputPriceOrMax(ordered[i]) < inputPriceOrMax(ordered[j])
	})

	targets := make([]core.RoutingTarget, 0, len(ordered))
	for _, c := range ordered {
		if rctx.VirtualKey != nil && len(rctx.VirtualKey.AllowedModels) > 0 &&
			!core.ModelMatchesAllowedRefs(c.ModelID, c.ProviderModelID, c.ProviderID, rctx.VirtualKey.AllowedModels) {
			continue
		}
		target, lErr := s.deps.Lookup(ctx, c.ProviderID, c.ModelID)
		if lErr != nil {
			continue
		}
		targets = append(targets, *target)
	}

	*trace = append(*trace, core.TraceEntry{
		StrategyType: "smart",
		Decision:     fmt.Sprintf("modality-auto: %d %s candidate(s)", len(targets), rctx.EndpointType),
		DurationMs:   int(time.Since(start).Milliseconds()),
	})
	return targets, nil
}

// inputPriceOrMax returns the model's per-1M input price, or +Inf when it has
// no price so unpriced models sort last in the cheapest-first modality order.
func inputPriceOrMax(m core.SmartModelRow) float64 {
	if m.InputPricePM == nil {
		return math.MaxFloat64
	}
	return *m.InputPricePM
}

// smartFallback resolves the default model from SmartConfig, or returns empty.
func smartFallback(ctx context.Context, cfg *SmartConfig, deps SmartDeps, trace *[]core.TraceEntry, start time.Time) ([]core.RoutingTarget, error) {
	if cfg.DefaultProviderID == "" || cfg.DefaultModelID == "" {
		return nil, nil
	}
	target, err := deps.Lookup(ctx, cfg.DefaultProviderID, cfg.DefaultModelID)
	if err != nil {
		return nil, nil //nolint:nilerr // smart fallback is best-effort; missing default is not fatal
	}
	*trace = append(*trace, core.TraceEntry{
		StrategyType: "smart",
		Decision:     fmt.Sprintf("falling back to default %s [%s/%s]", core.FormatTargetFriendly(target), cfg.DefaultProviderID, cfg.DefaultModelID),
		DurationMs:   int(time.Since(start).Milliseconds()),
	})
	return []core.RoutingTarget{*target}, nil
}

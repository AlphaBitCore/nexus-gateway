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
	// The pipeline read the catalogue for this endpoint kind once, in Stage A.
	// Fetching again here would answer the same question from a second
	// snapshot: a model enabled between the two reads is routable by one and
	// not the other, and which one wins depends on timing.
	// No self-fetch, for the reason measured in strategy_smart.go: the context
	// said to carry no pool has no caller, and the wiring gives the resolver and
	// this strategy the same store in the same branch.
	candidates := rctx.ModelPool

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

	// No capability filter, and that is a decision rather than an omission.
	// For every non-chat kind the ENDPOINT IS the capability: an image model
	// serves image generation and nothing else, so filtering by kind — which
	// Stage A already did when it built this pool — has already answered the
	// question a capability floor would ask. The chat path needs more because a
	// chat model may or may not take images, tools or audio on the same
	// endpoint.
	//
	// It stops being true the moment a non-chat model declares a required-
	// modality floor of its own: an image EDITING model that requires an input
	// image would be offered to a plain text-to-image request. Nothing in the
	// shipped catalogue does, and a test holds that — see
	// TestSmart_OnlyChatRowsDeclareAModalityFloor, which is what will say so
	// when it changes.
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
// pool is the candidate list AFTER the capability filter, or nil where no
// filtered pool exists (catalog load failed, pool empty, or the request was not
// normalizable so the filter never ran).
//
// Membership proves the default cleared the same bar as every other candidate;
// absence means the filter dropped it, and dispatching it anyway discards the
// filter the caller's `auto` asked for. A nil pool leaves the default
// unexamined — nothing to check it against, and refusing would turn a degraded
// catalog into a dead gateway.
func smartFallback(ctx context.Context, cfg *SmartConfig, deps SmartDeps, trace *[]core.TraceEntry, start time.Time, pool []core.SmartModelRow) ([]core.RoutingTarget, error) {
	// An admin leaving it empty is saying "if the router cannot answer, do not
	// route". This substitutes for a missing pick; it does not invent a policy.
	if cfg.DefaultProviderID == "" || cfg.DefaultModelID == "" {
		return nil, nil
	}
	if len(pool) == 0 || poolContains(pool, cfg.DefaultModelID) {
		target, err := deps.Lookup(ctx, cfg.DefaultProviderID, cfg.DefaultModelID)
		if err != nil {
			return nil, nil //nolint:nilerr // best-effort; a missing default is not fatal
		}
		*trace = append(*trace, core.TraceEntry{
			StrategyType: "smart",
			Decision:     fmt.Sprintf("falling back to default %s [%s/%s]", core.FormatTargetFriendly(target), cfg.DefaultProviderID, cfg.DefaultModelID),
			DurationMs:   int(time.Since(start).Milliseconds()),
		})
		return []core.RoutingTarget{*target}, nil
	}
	*trace = append(*trace, core.TraceEntry{
		StrategyType: "smart",
		Decision: fmt.Sprintf(
			"default %s cannot serve this request; falling back within the filtered pool instead",
			cfg.DefaultModelID),
		DurationMs: int(time.Since(start).Milliseconds()),
	})
	for i := range pool {
		target, err := deps.Lookup(ctx, pool[i].ProviderID, pool[i].ModelID)
		if err != nil {
			continue
		}
		*trace = append(*trace, core.TraceEntry{
			StrategyType: "smart",
			Decision:     fmt.Sprintf("falling back to %s from the filtered pool", core.FormatTargetFriendly(target)),
			DurationMs:   int(time.Since(start).Milliseconds()),
		})
		return []core.RoutingTarget{*target}, nil
	}
	return nil, nil
}

// poolContains matches ModelID and ModelCode both — defaultModelId is
// admin-entered and either spelling reaches here.
func poolContains(pool []core.SmartModelRow, modelID string) bool {
	for i := range pool {
		if pool[i].ModelID == modelID || pool[i].ModelCode == modelID {
			return true
		}
	}
	return false
}

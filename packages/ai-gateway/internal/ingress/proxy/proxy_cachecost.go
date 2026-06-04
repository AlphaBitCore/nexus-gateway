package proxy

import (
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/freshness"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// proxy_cachecost.go holds the cache pre-lookup classification, time-sensitivity
// freshness adapters, and the per-request cost computation split out of proxy.go
// (behavior unchanged).

func classifyCachePreLookup(
	cacheEnabled, hasNoCacheHeader, hasTargets, passthroughBypassCache bool,
	detector timeSensitiveDetector,
	canonicalMessages []freshness.ChatMessage,
	skipTimeSensitivePolicy bool,
) (audit.GatewayCacheStatus, audit.GatewayCacheSkipReason) {
	switch {
	case !cacheEnabled:
		return audit.GatewayCacheSkipped, audit.GatewayCacheSkipReasonDisabled
	case !hasTargets:
		return audit.GatewayCacheSkipped, audit.GatewayCacheSkipReasonDisabled
	case passthroughBypassCache:
		return audit.GatewayCacheSkipped, audit.GatewayCacheSkipReasonPassthrough
	case hasNoCacheHeader:
		return audit.GatewayCacheSkipped, audit.GatewayCacheSkipReasonNoCache
	case skipTimeSensitivePolicy && detector != nil && isTimeSensitive(detector, canonicalMessages):
		return audit.GatewayCacheSkipped, audit.GatewayCacheSkipReasonTimeSensitive
	default:
		return "", ""
	}
}

// timeSensitiveDetector is a narrow interface matching freshness.Detector.
// Tests may inject a stub; production wires *freshness.Detector directly.
type timeSensitiveDetector interface {
	IsTimeSensitive(messages []freshness.ChatMessage) (bool, string)
}

// isTimeSensitive is a thin adapter between the detector interface and the
// caller.  Kept as a standalone function so it can be tested in isolation.
func isTimeSensitive(d timeSensitiveDetector, msgs []freshness.ChatMessage) bool {
	if d == nil || len(msgs) == 0 {
		return false
	}
	matched, _ := d.IsTimeSensitive(msgs)
	return matched
}

// normMessagesToFreshness projects normcore.Message slice (from the canonical
// NormalizedPayload built at Phase 3.5) into the flat freshness.ChatMessage
// representation that the time-sensitivity detector accepts.
//
// Each normcore.Message may carry multiple ContentBlocks; this function
// concatenates all ContentText blocks with a single space separator to produce
// a flat string.  Non-text blocks (image refs, tool use, tool results) are
// omitted — the detector only reasons over text.  An empty result (all
// messages have non-text content only) returns nil, which the detector treats
// as "not time-sensitive" (fail-open).
func normMessagesToFreshness(msgs []normcore.Message) []freshness.ChatMessage {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]freshness.ChatMessage, 0, len(msgs))
	for _, m := range msgs {
		var textParts []string
		for _, block := range m.Content {
			if block.Type == normcore.ContentText && block.Text != "" {
				textParts = append(textParts, block.Text)
			}
		}
		if len(textParts) == 0 {
			continue
		}
		out = append(out, freshness.ChatMessage{
			Role:    string(m.Role),
			Content: strings.Join(textParts, " "),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// estimatedCostUSD computes the per-request USD cost from token counts and
// per-million-token prices. Both prices use the same units as
// Model.inputPricePerMillion / outputPricePerMillion (USD per 1M tokens),
// so we just divide the token counts by 1e6 and multiply.
//
// Zero or missing prices yield zero cost — e.g. when the routed Model row
// has not had prices set yet, or when a request fails before any tokens
// are consumed. This matches what the analytics surfaces expect: a NULL
// estimated_cost_usd is "we could not bill this", but we now persist 0
// when we know there were no chargeable tokens (cache hits, error
// responses), keeping cost summaries internally consistent.
func estimatedCostUSD(promptTok, completionTok int64, inPricePM, outPricePM float64) float64 {
	const million = 1_000_000.0
	return float64(promptTok)*inPricePM/million + float64(completionTok)*outPricePM/million
}

// computeCacheCosts recomputes rec.EstimatedCostUsd and populates the cache
// cost/savings fields using the provider_pricing table.
//
// Token-bucket semantics differ by provider wire format:
//   - Anthropic: input_tokens (→ PromptTokens) are NON-cached only; cache-read
//     and cache-write tokens are separate billing buckets in CacheReadTokens /
//     CacheCreationTokens.
//   - OpenAI / Gemini: prompt_tokens (→ PromptTokens) is the TOTAL input
//     including any cached subset; CacheReadTokens is a sub-count of PromptTokens.
//
// EstimatedCostUsd is recomputed from scratch using provider_pricing so the
// result is internally consistent regardless of what quotaInPrice (the model
// table's configured price) was set to. This prevents negative cost values
// that occurred when the two price sources diverged and savings > base cost.
func (h *Handler) computeCacheCosts(rec *audit.Record, target routingcore.RoutingTarget) {
	if h.deps.CachePricing == nil {
		return
	}
	if rec.CacheCreationTokens == 0 && rec.CacheReadTokens == 0 {
		return
	}
	p := h.deps.CachePricing.LookupCachePricing(target.AdapterType, target.ProviderID, target.ModelCode)
	if p == nil {
		return
	}
	const million = 1_000_000.0
	if rec.CacheCreationTokens > 0 {
		rec.CacheWriteCostUsd = float64(rec.CacheCreationTokens) * p.CacheWriteUSDPerM / million
	}
	if rec.CacheReadTokens > 0 {
		// Savings = what would have been paid at standard input price minus
		// the cheaper cache-read price, using the provider's own price list.
		rec.CacheReadSavingsUsd = float64(rec.CacheReadTokens) * (p.InputUSDPerM - p.CacheReadUSDPerM) / million
	}
	rec.CacheNetSavingsUsd = rec.CacheReadSavingsUsd - rec.CacheWriteCostUsd

	// Recompute EstimatedCostUsd from scratch.
	//
	// PromptTokens is always "total input including cached" across every
	// adapter (the normalizer sums input_tokens + cache_read + cache_creation
	// into it at codec time). computeCacheCosts subtracts both cache buckets
	// to get the uncached remainder, then bills each bucket at its own rate.
	// Without this, cached tokens would be charged at full input price AND
	// again at the cache rate.
	regularInput := rec.PromptTokens - rec.CacheReadTokens - rec.CacheCreationTokens
	if regularInput < 0 {
		regularInput = 0
	}
	rec.EstimatedCostUsd = float64(regularInput)*p.InputUSDPerM/million +
		float64(rec.CacheReadTokens)*p.CacheReadUSDPerM/million +
		float64(rec.CacheCreationTokens)*p.CacheWriteUSDPerM/million +
		float64(rec.CompletionTokens)*p.OutputUSDPerM/million
}

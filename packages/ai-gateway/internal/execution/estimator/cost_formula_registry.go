// Package estimator — cost_formula_registry.go implements the per-endpoint
// cost formula dispatch table.
//
// Each endpoint typology (chat, embeddings, …) owns its own formula.
// New endpoint types call RegisterFormula on init to add their formula
// without changing the dispatcher in proxy.go.
package estimator

import (
	"log/slog"
	"sync"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// BillableUnits captures the per-endpoint resource consumption the cost
// formula consumes.
//
// Only the two token fields every live cost path stamps are kept. The
// registered formulas — chatCostFormula and embeddingsCostFormula — price
// exclusively from PromptTokens + CompletionTokens, and all five proxy cost
// call sites populate exactly these two (proxy_cache_hits.go,
// proxy_responses.go, proxy_upstream.go). Reasoning tokens are already
// folded into CompletionTokens by the upstream usage normalizer (all three
// frontier providers bill reasoning at the output rate — see
// metrics.CalculateCost), so a separate ReasoningTokens unit would be
// double-priced or dead. Cache-read, image, audio, video, and batch-row
// units were speculative fields no formula or call site ever set;
// they are removed rather than carried as dead surface.
type BillableUnits struct {
	PromptTokens     int
	CompletionTokens int
}

// CostFormula returns the estimated USD cost for a request given its
// billable units and the model's configured pricing snapshot.
type CostFormula func(units BillableUnits, prices metrics.ModelPrices) metrics.Cost

// formulaRegistry is the process-wide endpoint → formula map. Access is
// guarded by formulaMu. Populated at package init with the two built-in
// formulas keyed by the canonical typology.EndpointKind string vocabulary;
// new endpoint types add entries via RegisterFormula.
var (
	formulaMu  sync.RWMutex
	formulaMap = map[string]CostFormula{
		string(typology.EndpointKindChat):       chatCostFormula,
		string(typology.EndpointKindEmbeddings): embeddingsCostFormula,
	}
)

// unregisteredWarned dedupes the missing-formula warning to one log line
// per endpoint string for the process lifetime, so a high-QPS stream of
// stt / tts / image requests against a gap in the registry does not flood
// the log while still surfacing the gap exactly once.
var unregisteredWarned sync.Map // endpoint string -> struct{}

// Lookup returns the CostFormula registered for the given endpoint kind
// string. When no formula is found, the chat formula is returned as a
// safe default so callers are never blocked on missing entries — but the
// fallback is no longer silent: the first lookup of an unregistered
// endpoint is logged at WARN so an operator can see that e.g. stt / tts /
// image_generation traffic is being token-mispriced through the chat
// formula until a dedicated formula is registered. Endpoint strings match
// audit.Record.EndpointType — canonical typology kind values ("chat",
// "embeddings", "stt", "tts", "image_generation", "batch").
func Lookup(endpoint string) CostFormula {
	formulaMu.RLock()
	f, ok := formulaMap[endpoint]
	formulaMu.RUnlock()
	if ok {
		return f
	}
	if _, loaded := unregisteredWarned.LoadOrStore(endpoint, struct{}{}); !loaded {
		slog.Warn("estimator: no cost formula registered for endpoint; falling back to chat formula (tokens may be mispriced)",
			"endpoint", endpoint)
	}
	return chatCostFormula
}

// RegisterFormula registers a custom CostFormula for the given endpoint
// kind. New endpoint types call it from their init() so the dispatcher
// in proxy.go remains unchanged. Re-registration replaces the existing
// formula for that kind. Passing a nil formula is a no-op.
func RegisterFormula(endpoint string, f CostFormula) {
	if f == nil {
		return
	}
	formulaMu.Lock()
	defer formulaMu.Unlock()
	formulaMap[endpoint] = f
}

// chatCostFormula prices chat / completions / responses requests via the
// standard token-bucket arithmetic: prompt × inputPrice + completion ×
// outputPrice. Wraps costForTokens so both the estimator dry-run path
// and the proxy cost-stamp path share identical arithmetic.
func chatCostFormula(u BillableUnits, prices metrics.ModelPrices) metrics.Cost {
	return costForTokens(u.PromptTokens, u.CompletionTokens, prices)
}

// embeddingsCostFormula prices embedding requests. Embeddings have no
// completion tokens; cost = (promptTokens / 1M) × inputPricePerMillion.
// CompletionTokens is ignored.
func embeddingsCostFormula(u BillableUnits, prices metrics.ModelPrices) metrics.Cost {
	return costForTokens(u.PromptTokens, 0, prices)
}

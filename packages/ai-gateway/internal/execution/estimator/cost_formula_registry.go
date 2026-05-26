// Package estimator — cost_formula_registry.go implements the per-endpoint
// cost formula dispatch table.
//
// Each endpoint typology (chat, embeddings, …) owns its own formula.
// New endpoint types call RegisterFormula on init to add their formula
// without changing the dispatcher in proxy.go.
package estimator

import (
	"sync"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/metrics"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// BillableUnits captures the per-endpoint resource consumption the cost
// formula consumes. All fields are zero unless the relevant typology
// produced them.
type BillableUnits struct {
	PromptTokens     int
	CompletionTokens int
	ReasoningTokens  int
	CachedTokens     int
	Images           int     // image-gen output count
	AudioSeconds     float64 // TTS / STT
	VideoSeconds     float64 // video-gen
	Requests         int     // batch row count
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

// Lookup returns the CostFormula registered for the given endpoint kind
// string. When no formula is found, the chat formula is returned as a
// safe default so callers are never blocked on missing entries. Endpoint
// strings match audit.Record.EndpointType — canonical typology kind
// values ("chat", "embeddings", "stt", "tts", "image_generation",
// "batch").
func Lookup(endpoint string) CostFormula {
	formulaMu.RLock()
	defer formulaMu.RUnlock()
	if f, ok := formulaMap[endpoint]; ok {
		return f
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
// CompletionTokens and all other BillableUnits fields are ignored.
func embeddingsCostFormula(u BillableUnits, prices metrics.ModelPrices) metrics.Cost {
	return costForTokens(u.PromptTokens, 0, prices)
}

// costForUnits synthesises a provcore.Usage from the BillableUnits and
// delegates to metrics.CalculateCost. Exposes the full cache-aware
// arithmetic for callers that populate CachedTokens.
func costForUnits(u BillableUnits, prices metrics.ModelPrices) metrics.Cost {
	cacheRead := u.CachedTokens
	prompt := u.PromptTokens
	completion := u.CompletionTokens
	reasoning := u.ReasoningTokens
	usage := provcore.Usage{
		PromptTokens:     &prompt,
		CompletionTokens: &completion,
		CacheReadTokens:  &cacheRead,
		ReasoningTokens:  &reasoning,
	}
	return metrics.CalculateCost(usage, prices)
}

// CostForUnitsExported is the exported test-accessible entry point for
// costForUnits. Used by unit tests to verify cache-aware cost accounting
// in the BillableUnits path without requiring an end-to-end request.
// Not intended for production call sites outside the estimator package —
// use Lookup(endpoint)(units, prices) for dispatched cost calculation.
func CostForUnitsExported(u BillableUnits, prices metrics.ModelPrices) metrics.Cost {
	return costForUnits(u, prices)
}

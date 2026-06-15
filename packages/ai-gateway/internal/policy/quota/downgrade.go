package quota

import (
	"cmp"
	"slices"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

// TargetPricing holds a routing target's pricing for downgrade comparison.
//
// Priced carries the real "this model has a price row" signal from the store
// (see store.ModelPricing.Priced). An unpriced candidate prices to $0 and would
// otherwise win the cheapest-fits comparison, then get re-priced to 0 and slip
// past the very cost cap that triggered the downgrade — the exact hole the
// primary-model unpriced guard closes. SelectCheapestIndex therefore skips
// unpriced candidates so the downgrade boundary fails closed too.
type TargetPricing struct {
	Index         int
	ModelID       string
	InputPricePM  float64
	OutputPricePM float64
	Priced        bool
}

// SelectCheapestIndex returns the index of the cheapest target that fits
// within the given budget. Returns -1 if no target fits.
//
// It is only called on the quota downgrade path, i.e. always under an active
// cost cap. An unpriced candidate (Priced=false) has no enforceable cost and
// can never satisfy a cost cap, so it is skipped regardless of budget — never
// confused with a genuinely free model (Priced=true, zero rates), which stays
// selectable. If every candidate is unpriced the function returns -1 and the
// caller rejects with QUOTA_EXCEEDED, mirroring the primary-model fail-closed
// behaviour rather than serving unaccounted spend.
func SelectCheapestIndex(targets []TargetPricing, estimate CostEstimate, budget float64) int {
	type scored struct {
		index int
		cost  float64
	}

	var candidates []scored
	for _, t := range targets {
		if !t.Priced {
			continue // unpriced → uncountable against a cost cap → never a valid downgrade target.
		}
		cost := (float64(estimate.EstimatedInputTokens)*t.InputPricePM +
			float64(estimate.MaxOutputTokens)*t.OutputPricePM) / 1_000_000
		if cost <= budget {
			candidates = append(candidates, scored{index: t.Index, cost: cost})
		}
	}

	if len(candidates) == 0 {
		return -1
	}

	slices.SortFunc(candidates, func(a, b scored) int {
		return cmp.Compare(a.cost, b.cost)
	})
	return candidates[0].index
}

// TargetPricingFromStore converts store.ModelPricing results into
// TargetPricing with correct indices for the caller's model ID list.
func TargetPricingFromStore(pricing []store.ModelPricing) []TargetPricing {
	result := make([]TargetPricing, len(pricing))
	for i, mp := range pricing {
		result[i] = TargetPricing{
			Index:         i,
			ModelID:       mp.ModelID,
			InputPricePM:  mp.InputPricePM,
			OutputPricePM: mp.OutputPricePM,
			Priced:        mp.Priced,
		}
	}
	return result
}

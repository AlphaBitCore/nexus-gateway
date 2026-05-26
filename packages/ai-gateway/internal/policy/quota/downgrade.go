package quota

import (
	"cmp"
	"slices"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

// TargetPricing holds a routing target's pricing for downgrade comparison.
type TargetPricing struct {
	Index         int
	ModelID       string
	InputPricePM  float64
	OutputPricePM float64
}

// SelectCheapestIndex returns the index of the cheapest target that fits
// within the given budget. Returns -1 if no target fits.
func SelectCheapestIndex(targets []TargetPricing, estimate CostEstimate, budget float64) int {
	type scored struct {
		index int
		cost  float64
	}

	var candidates []scored
	for _, t := range targets {
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
		}
	}
	return result
}

// Package quota implements cost-based quota enforcement for the AI gateway
// using a hierarchical policy/override system with Redis-backed usage tracking.
package quota

// CostEstimate is the pre-call cost prediction used by Engine.Check().
type CostEstimate struct {
	EstimatedInputTokens int64
	MaxOutputTokens      int64
	InputPricePM         float64 // price per million tokens
	OutputPricePM        float64
}

// EstimatedCost returns the predicted cost in USD.
func (e CostEstimate) EstimatedCost() float64 {
	return (float64(e.EstimatedInputTokens)*e.InputPricePM +
		float64(e.MaxOutputTokens)*e.OutputPricePM) / 1_000_000
}

// ActualUsage is the post-call usage for reconciliation.
type ActualUsage struct {
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	InputPricePM     float64
	OutputPricePM    float64
}

// ActualCost returns the real cost in USD.
func (u ActualUsage) ActualCost() float64 {
	return (float64(u.PromptTokens)*u.InputPricePM +
		float64(u.CompletionTokens)*u.OutputPricePM) / 1_000_000
}

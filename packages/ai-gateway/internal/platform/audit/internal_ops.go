package audit

// InternalOpsEntry is one internal model call made on a user request's behalf,
// as persisted into the traffic_event.internal_ops_breakdown JSONB array.
//
// The JSON keys are camelCase because the column is passed through verbatim:
// trafficstore reads it as json.RawMessage and the CP traffic drawer indexes
// the parsed objects directly (CostBreakdown.tsx reads b.type / b.model /
// b.costUsd against the TrafficEvent.internalOpsBreakdown type in
// control-plane-ui/src/api/types.ts). No layer rewrites the casing, so
// snake_case keys would leave every internal-ops line rendering $0.00. This is
// also the casing every sibling field on the traffic API uses
// (embeddingCostUsd, aiGuardCostUsd). The keys are a persisted contract: they
// may be added to but not renamed.
//
// The type exists to give internal spend a recoverable basis. Each internal
// cost column (router_cost_usd, ai_guard_cost_usd, embedding_cost_usd) stores
// an amount and nothing else, so a mis-priced internal call leaves no trace to
// audit against — which is precisely how router calls billing cached prompt
// tokens at the full input rate went undetected in our own data and remain
// unrecomputable for every row already written.
type InternalOpsEntry struct {
	// Type names the internal caller: "smart-router" today. Fixed vocabulary —
	// consumers group on it.
	Type string `json:"type"`
	// Model is the Nexus Model id that SERVED the internal call, not any model
	// it selected or judged.
	Model string `json:"model,omitempty"`
	// ProviderID is the provider that served this specific call. Per-entry
	// rather than per-row: two smart-routing rules on one request can be served
	// by different vendors, and traffic_event.router_provider_id can only name
	// the first (see drainRouterCost's KNOWN LIMITATION). The entries keep the
	// true per-vendor split even where the flat column cannot.
	ProviderID string `json:"providerId,omitempty"`

	// PromptTokens is the TOTAL input including the cached share below, on
	// every adapter — never the uncached remainder. CacheReadTokens and
	// CacheCreationTokens are sub-counts of it, not additions to it.
	PromptTokens        int `json:"promptTokens,omitempty"`
	CompletionTokens    int `json:"completionTokens,omitempty"`
	CacheReadTokens     int `json:"cacheReadTokens,omitempty"`
	CacheCreationTokens int `json:"cacheCreationTokens,omitempty"`

	// CostUsd is what this one call cost, billed per token bucket at its own
	// rate (costing.Rates.EstimateUSD). Summing CostUsd across entries of type
	// "smart-router" reproduces traffic_event.router_cost_usd.
	CostUsd float64 `json:"costUsd,omitempty"`
}

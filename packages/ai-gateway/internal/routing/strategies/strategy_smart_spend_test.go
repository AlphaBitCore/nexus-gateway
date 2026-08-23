package strategies

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/llm"
)

// TestStampRouterSpend_CarriesCostAndUsageBasis pins that the router call's
// token basis travels with its amount. The proxy stage turns this entry into
// the traffic_event internal_ops_breakdown row, which is the only place the
// charge can later be checked against the tokens it was computed from.
func TestStampRouterSpend_CarriesCostAndUsageBasis(t *testing.T) {
	got := stampRouterSpend(core.TraceEntry{StrategyType: "smart"}, llm.Decision{
		CostUsd:             0.0031,
		ServedProviderID:    "prov-openai",
		ModelID:             "gpt-4o-mini", // the model PICKED, must not be recorded as the router
		PromptTokens:        5120,
		CompletionTokens:    18,
		CacheReadTokens:     4864,
		CacheCreationTokens: 100,
	}, "model-gpt-4o")

	want := core.TraceEntry{
		StrategyType:  "smart",
		RouterCostUsd: 0.0031, RouterProviderID: "prov-openai",
		RouterModelID:      "model-gpt-4o",
		RouterPromptTokens: 5120, RouterCompletionTokens: 18,
		RouterCacheReadTokens: 4864, RouterCacheCreationTokens: 100,
	}
	if got != want {
		t.Errorf("entry = %+v, want %+v", got, want)
	}
}

// TestStampRouterSpend_ServingModelNotPickedModel pins the distinction that
// makes the record meaningful: RouterModelID is the model that SERVED the
// router call, never the model the router chose. Recording the picked model
// would attribute the router's own token spend to a model that never ran.
func TestStampRouterSpend_ServingModelNotPickedModel(t *testing.T) {
	got := stampRouterSpend(core.TraceEntry{}, llm.Decision{
		ModelID: "claude-haiku", ProviderID: "prov-anthropic",
		ServedProviderID: "prov-openai",
		PromptTokens:     900, CompletionTokens: 10,
	}, "model-gpt-4o")

	if got.RouterModelID != "model-gpt-4o" {
		t.Errorf("RouterModelID = %q, want the SERVING model model-gpt-4o", got.RouterModelID)
	}
	if got.RouterProviderID != "prov-openai" {
		t.Errorf("RouterProviderID = %q, want the SERVING provider prov-openai", got.RouterProviderID)
	}
}

// TestStampRouterSpend_ZeroDecisionStampsNothing pins the failure path: every
// Decider error returns a zero Decision, and a failed router call is not
// billed. It must also leave no model id, or the breakdown would carry an
// entry for a call that produced nothing.
func TestStampRouterSpend_ZeroDecisionStampsNothing(t *testing.T) {
	in := core.TraceEntry{StrategyType: "smart", Decision: "router LLM timeout (3000ms)"}
	got := stampRouterSpend(in, llm.Decision{}, "model-gpt-4o")
	if got != in {
		t.Errorf("entry = %+v, want it unchanged %+v — a failed router call is not billed", got, in)
	}
}

// TestStampRouterSpend_UnpricedCallStillRecordsUsage pins that usage is
// stamped independently of cost. An unpriced router model returns real tokens
// and a zero amount; dropping its usage would erase the only evidence that the
// call — and the vendor charge behind it — happened at all.
func TestStampRouterSpend_UnpricedCallStillRecordsUsage(t *testing.T) {
	got := stampRouterSpend(core.TraceEntry{}, llm.Decision{
		CostUsd: 0, ServedProviderID: "prov-moonshot",
		PromptTokens: 900, CompletionTokens: 12,
	}, "model-kimi")

	if got.RouterPromptTokens != 900 || got.RouterCompletionTokens != 12 {
		t.Errorf("tokens = (%d,%d), want (900,12) on an unpriced call",
			got.RouterPromptTokens, got.RouterCompletionTokens)
	}
	if got.RouterModelID != "model-kimi" {
		t.Errorf("RouterModelID = %q, want model-kimi", got.RouterModelID)
	}
	if got.RouterCostUsd != 0 {
		t.Errorf("RouterCostUsd = %v, want 0", got.RouterCostUsd)
	}
}

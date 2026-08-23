// strategy_smart_spend.go — router-LLM spend accounting for the smart
// strategy. The router call is real vendor spend made on the request's behalf;
// this is the single place that decides which trace entry carries it upward to
// the proxy stage, and from which field the serving vendor is read.
package strategies

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/llm"
)

// stampRouterSpend attaches the router call's own cost and the provider that
// SERVED that call to one trace entry, which the proxy stage drains onto the
// request's audit record.
//
// Exactly ONE entry per router call may carry these fields: the stage SUMS
// RouterCostUsd across the trace, so a second stamped entry double-charges the
// request. The provider comes from decision.ServedProviderID — the vendor that
// answered the router call — never from decision.ProviderID, which is the
// provider of the model the router PICKED and is routinely a different vendor.
//
// The token counts ride along on the same entry and under the same
// exactly-one-entry rule, because they are the basis of the cost beside them:
// a row carrying only an amount cannot be re-checked, which is what made the
// full-rate over-charge on cached router tokens invisible in our own data.
// routerModelID is the model that SERVED the router call (cfg.RouterModelID),
// never decision.ModelID, which is the model the router chose.
//
// A zero Decision (every Decider error path returns one) stamps nothing, which
// is the intended outcome: a failed router call is not billed.
func stampRouterSpend(e core.TraceEntry, decision llm.Decision, routerModelID string) core.TraceEntry {
	e.RouterCostUsd = decision.CostUsd
	e.RouterProviderID = decision.ServedProviderID
	e.RouterPromptTokens = decision.PromptTokens
	e.RouterCompletionTokens = decision.CompletionTokens
	e.RouterCacheReadTokens = decision.CacheReadTokens
	e.RouterCacheCreationTokens = decision.CacheCreationTokens
	// Only stamped alongside real usage: an error-path zero Decision must not
	// put a model id on an entry that records no call.
	if decision.PromptTokens > 0 || decision.CompletionTokens > 0 {
		e.RouterModelID = routerModelID
	}
	return e
}

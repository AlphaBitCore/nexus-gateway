package core

import "github.com/goccy/go-json"

// contentReadingStrategyTypes is the single source of truth for strategy node
// types that read the canonical request payload (rctx.Request.Messages) at
// evaluation time. Today only "smart" inspects the user prompt to make an LLM
// routing decision; every other strategy routes on model/provider/condition
// metadata and never touches message content.
//
// The lazy-canonical lazy-canonical gate (needCanonical) uses this set to decide
// whether the request-path canonical must be computed. Adding a NEW
// content-aware strategy MUST add its type here in the same change, or the gate
// will leave rctx.Request nil and the strategy will silently route on its
// fallback path — a wrong-route regression with no error. See
// docs/developers/specs/perf-phase-e-lazy-canonical.md (A6).
var contentReadingStrategyTypes = map[string]struct{}{
	"smart": {},
}

// StrategyTreeReadsContent reports whether the strategy node tree contains any
// node whose type reads the canonical request payload. The walk is recursive
// because a content-reading node can nest arbitrarily inside fallback targets,
// loadbalance weighted nodes, or conditional branches/default — a shallow
// root-type check would miss those and is a false-negative (A1).
func StrategyTreeReadsContent(n *StrategyNode) bool {
	if n == nil {
		return false
	}
	if _, ok := contentReadingStrategyTypes[n.Type]; ok {
		return true
	}
	for i := range n.Targets {
		if StrategyTreeReadsContent(&n.Targets[i]) {
			return true
		}
	}
	for i := range n.Weighted {
		if StrategyTreeReadsContent(&n.Weighted[i].Node) {
			return true
		}
	}
	for i := range n.Conditions {
		if StrategyTreeReadsContent(&n.Conditions[i].Then) {
			return true
		}
	}
	return StrategyTreeReadsContent(n.Default)
}

// ConfigReadsContent is the json.RawMessage entry point used by the
// needCanonical gate over a rule's stored StrategyNode tree (RoutingRule.Config).
//
// Fail-safe: an empty config carries no strategy and reads no content (false),
// but a MALFORMED config returns true — we would rather compute the canonical
// needlessly (perf cost only) than risk leaving a content-reading strategy
// without its input (functional regression). Callers memoize the result by a
// hash of the raw bytes so the parse runs once per distinct config.
func ConfigReadsContent(config json.RawMessage) bool {
	if len(config) == 0 {
		return false
	}
	var node StrategyNode
	if err := json.Unmarshal(config, &node); err != nil {
		return true
	}
	return StrategyTreeReadsContent(&node)
}

package routing

import (
	"fmt"

	"github.com/goccy/go-json"
)

// strategyNodeShape is a FULL structural mirror of the AI Gateway resolver's
// core.StrategyNode discriminated union (packages/ai-gateway/internal/routing/
// core/types.go), including every nested sub-structure. The CP module cannot
// import the gateway's internal package, so the mirror is maintained in
// lockstep by hand — TestStrategyNodeShape_GatewayLockstep pins the wire
// contract with golden samples for every node type.
//
// A FULL mirror (typed sub-structures, not []json.RawMessage elements) is
// load-bearing: the resolver unmarshals the persisted config into
// core.StrategyNode on EVERY request and fails CLOSED on a type mismatch, so
// a nested element whose field carries the wrong JSON type (e.g. a weighted
// target with "weight":"5") that slipped past a shallower projection would be
// broadcast fleet-wide and then reject every request on that rule. Decoding
// here with the same goccy decoder and the same field types guarantees CP
// acceptance is a strict subset of what the resolver parses.
type strategyNodeShape struct {
	Type string `json:"type"`

	// single
	ProviderID string `json:"providerId"`
	ModelID    string `json:"modelId"`

	// fallback
	Targets       []strategyNodeShape `json:"targets"`
	OnStatusCodes []int               `json:"onStatusCodes"`

	// loadbalance
	Algorithm       string                `json:"algorithm"`
	WeightedTargets []weightedTargetShape `json:"weightedTargets"`
	StickyOn        string                `json:"stickyOn"`
	StickyTTLMs     int                   `json:"stickyTtlMs"`

	// conditional
	Conditions []conditionalBranchShape `json:"conditions"`
	Default    *strategyNodeShape       `json:"default"`

	// ab_split
	ABTargets []abTargetShape `json:"abTargets"`

	// latency
	LatencyTargets []latencyTargetShape `json:"latencyTargets"`

	// smart
	RouterProviderID  string   `json:"routerProviderId"`
	RouterModelID     string   `json:"routerModelId"`
	SystemPrompt      string   `json:"systemPrompt"`
	Temperature       *float64 `json:"temperature"`
	MaxTokens         int      `json:"maxTokens"`
	TimeoutMs         int      `json:"timeoutMs"`
	DefaultProviderID string   `json:"defaultProviderId"`
	DefaultModelID    string   `json:"defaultModelId"`

	// policy (stage-0 only)
	AllowModelIDs    []string `json:"allowModelIds"`
	DenyModelIDs     []string `json:"denyModelIds"`
	AllowProviderIDs []string `json:"allowProviderIds"`
	DenyProviderIDs  []string `json:"denyProviderIds"`
}

// weightedTargetShape mirrors core.WeightedTarget.
type weightedTargetShape struct {
	Weight int               `json:"weight"`
	Node   strategyNodeShape `json:"node"`
}

// conditionalBranchShape mirrors core.ConditionalBranch.
type conditionalBranchShape struct {
	When map[string]any    `json:"when"`
	Then strategyNodeShape `json:"then"`
}

// abTargetShape mirrors core.ABTarget.
type abTargetShape struct {
	ProviderID string `json:"providerId"`
	ModelID    string `json:"modelId"`
	Weight     int    `json:"weight"`
}

// latencyTargetShape mirrors core.LatencyTarget.
type latencyTargetShape struct {
	ProviderID string `json:"providerId"`
	ModelID    string `json:"modelId"`
}

// maxLatencyTargets mirrors core.MaxLatencyTargets in the AI Gateway (kept in
// lockstep manually — the CP module cannot import the gateway's internal
// package). Bounds the latency strategy's per-request target fan-out.
const maxLatencyTargets = 32

// maxStrategyNodeDepth mirrors core.MaxRoutingDepth: the resolver stops
// evaluating deeper than this, so a deeper tree is dead configuration —
// reject it at the write boundary instead of persisting unreachable nodes.
const maxStrategyNodeDepth = 10

// validateStrategyConfig shape-checks the config JSON RawMessage against the
// full strategy node wire shape, recursively. raw == nil/empty/`null` is
// treated as "no config supplied" and left to the caller's required-field
// check. Rejected: a non-object JSON document; any field (top-level OR
// nested — weighted targets, conditional branches, fallback children) whose
// JSON type mismatches the resolver's struct; an unknown node `type` at any
// depth; a tree deeper than the resolver evaluates.
//
// Returns ("", true) when valid; (operator-facing message, false) otherwise.
func validateStrategyConfig(raw json.RawMessage) (string, bool) {
	s := string(raw)
	if len(raw) == 0 || s == "null" {
		return "", true
	}
	var shape strategyNodeShape
	if err := json.Unmarshal(raw, &shape); err != nil {
		return fmt.Sprintf("config is not a valid strategy object: %v", err), false
	}
	if msg := validateNodeShape(&shape, 0); msg != "" {
		return msg, false
	}
	// The latency strategy resolves and ranks every listed target on each
	// request, so bound the fan-out at persistence rather than letting an
	// oversized config land and be decoded per-request fleet-wide. The gateway
	// also trims to the same cap on unmarshal (core.MaxLatencyTargets); this keeps
	// the huge config from ever being stored. Targets may be authored under either
	// the "latencyTargets" or the generic "targets" key.
	if shape.Type == "latency" {
		n := len(shape.LatencyTargets)
		if len(shape.Targets) > n {
			n = len(shape.Targets)
		}
		if n > maxLatencyTargets {
			return fmt.Sprintf("a latency strategy allows at most %d targets, got %d", maxLatencyTargets, n), false
		}
	}
	return "", true
}

// validateNodeShape walks the strategy tree enforcing the per-node rules the
// resolver depends on: a known `type` when present, and bounded depth. Field
// TYPE correctness is already guaranteed by the typed unmarshal into
// strategyNodeShape; this walk covers the semantic checks a decode cannot.
func validateNodeShape(n *strategyNodeShape, depth int) string {
	if depth > maxStrategyNodeDepth {
		return fmt.Sprintf("strategy tree exceeds the maximum depth of %d the gateway evaluates", maxStrategyNodeDepth)
	}
	if n.Type != "" {
		if _, ok := validStrategyTypes[n.Type]; !ok {
			return fmt.Sprintf("config.type %q is not a recognized strategy node type (allowed: %s)", n.Type, strategyTypeList())
		}
	}
	for i := range n.Targets {
		if msg := validateNodeShape(&n.Targets[i], depth+1); msg != "" {
			return msg
		}
	}
	for i := range n.WeightedTargets {
		if msg := validateNodeShape(&n.WeightedTargets[i].Node, depth+1); msg != "" {
			return msg
		}
	}
	for i := range n.Conditions {
		if msg := validateNodeShape(&n.Conditions[i].Then, depth+1); msg != "" {
			return msg
		}
	}
	if n.Default != nil {
		if msg := validateNodeShape(n.Default, depth+1); msg != "" {
			return msg
		}
	}
	return ""
}

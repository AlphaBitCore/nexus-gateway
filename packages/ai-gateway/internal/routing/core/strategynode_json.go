package core

import "github.com/goccy/go-json"

// UnmarshalJSON decodes a StrategyNode, then reconciles the ab_split target
// list. The admin UI (source of truth for the config wire shape) authors
// ab_split targets under the generic "targets" key — the same key fallback
// uses for its []StrategyNode list — rather than "abTargets". Every ab_split
// rule persisted through the UI therefore stores {type:"ab_split","targets":[
// {providerId,modelId,weight}]}. Without this shim those targets decode into
// Targets ([]StrategyNode, which drops the weight) and ABTargets stays empty,
// so the ab_split strategy resolves zero targets and the rule silently routes
// nothing. Hydrating ABTargets from "targets" (only when "abTargets" was not
// supplied) makes existing UI-authored rules route correctly with no data
// migration, while still honoring an explicit "abTargets" key if present.
func (n *StrategyNode) UnmarshalJSON(data []byte) error {
	type alias StrategyNode
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*n = StrategyNode(a)
	if n.Type == "ab_split" && len(n.ABTargets) == 0 {
		var probe struct {
			ABTargets []ABTarget `json:"targets"`
		}
		if err := json.Unmarshal(data, &probe); err == nil {
			n.ABTargets = probe.ABTargets
		}
	}
	// Like ab_split above, the admin UI authors latency targets under the
	// generic "targets" key, which the base unmarshal decodes into Targets
	// ([]StrategyNode, dropping providerId/modelId into empty-Type phantom
	// nodes) rather than LatencyTargets. Hydrate LatencyTargets from "targets"
	// when an explicit "latencyTargets" key was not supplied so UI-authored
	// rules resolve. The phantom Targets entries are left populated but are
	// never consumed for a latency node — dispatch and enumeration key on Type.
	if n.Type == "latency" && len(n.LatencyTargets) == 0 {
		var probe struct {
			LatencyTargets []LatencyTarget `json:"targets"`
		}
		if err := json.Unmarshal(data, &probe); err == nil {
			n.LatencyTargets = probe.LatencyTargets
		}
	}
	// Bound the latency fan-out: the strategy resolves and ranks every listed
	// target on each request, so an oversized list is capped here (overflow
	// dropped) rather than multiplying the per-request hot-path cost unbounded.
	if n.Type == "latency" && len(n.LatencyTargets) > MaxLatencyTargets {
		n.LatencyTargets = n.LatencyTargets[:MaxLatencyTargets]
	}
	return nil
}

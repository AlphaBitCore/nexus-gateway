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
	return nil
}

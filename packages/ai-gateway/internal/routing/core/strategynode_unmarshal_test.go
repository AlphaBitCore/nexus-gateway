package core

import (
	"github.com/goccy/go-json"
	"testing"
)

// TestStrategyNodeUnmarshalABSplitTargets locks the compat shim that lets an
// ab_split rule authored by the admin UI actually route. The UI persists
// ab_split targets under the generic "targets" key; the resolver reads
// ABTargets. Decoding the real UI config blob must populate ABTargets (with
// weights preserved) so the ab_split strategy resolves real targets instead of
// "no abTargets configured". This is the JSON path the in-memory struct tests
// never exercised, which is why the mismatch shipped.
func TestStrategyNodeUnmarshalABSplitTargets(t *testing.T) {
	// Verbatim shape written by buildRoutingApiConfig's ab_split branch.
	raw := []byte(`{
		"type": "ab_split",
		"targets": [
			{"weight": 70, "modelId": "m-a", "providerId": "p-a"},
			{"weight": 30, "modelId": "m-b", "providerId": "p-b"}
		]
	}`)

	var node StrategyNode
	if err := json.Unmarshal(raw, &node); err != nil {
		t.Fatalf("unmarshal ab_split config: %v", err)
	}

	if len(node.ABTargets) != 2 {
		t.Fatalf("ABTargets not hydrated from \"targets\": got %d, want 2", len(node.ABTargets))
	}
	if node.ABTargets[0].ProviderID != "p-a" || node.ABTargets[0].ModelID != "m-a" || node.ABTargets[0].Weight != 70 {
		t.Errorf("target[0] = %+v, want {p-a m-a 70}", node.ABTargets[0])
	}
	if node.ABTargets[1].ProviderID != "p-b" || node.ABTargets[1].ModelID != "m-b" || node.ABTargets[1].Weight != 30 {
		t.Errorf("target[1] = %+v, want {p-b m-b 30}", node.ABTargets[1])
	}
}

// TestStrategyNodeUnmarshalABSplitPrefersExplicitAbTargets ensures an explicit
// "abTargets" key still wins and is not overwritten by the "targets" shim.
func TestStrategyNodeUnmarshalABSplitPrefersExplicitAbTargets(t *testing.T) {
	raw := []byte(`{
		"type": "ab_split",
		"abTargets": [{"weight": 100, "modelId": "m-x", "providerId": "p-x"}],
		"targets": [{"weight": 1, "modelId": "m-ignored", "providerId": "p-ignored"}]
	}`)

	var node StrategyNode
	if err := json.Unmarshal(raw, &node); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(node.ABTargets) != 1 || node.ABTargets[0].ModelID != "m-x" {
		t.Fatalf("explicit abTargets should win, got %+v", node.ABTargets)
	}
}

// TestStrategyNodeUnmarshalMalformed surfaces the decode error rather than
// silently yielding a zero node that would route nothing.
func TestStrategyNodeUnmarshalMalformed(t *testing.T) {
	var node StrategyNode
	if err := json.Unmarshal([]byte(`{"type": "ab_split", "targets": "not-an-array"}`), &node); err == nil {
		t.Fatal("expected error decoding malformed ab_split config, got nil")
	}
}

// TestStrategyNodeUnmarshalFallbackTargetsUnaffected guards against the shim
// leaking into fallback nodes, whose "targets" are []StrategyNode.
func TestStrategyNodeUnmarshalFallbackTargetsUnaffected(t *testing.T) {
	raw := []byte(`{
		"type": "fallback",
		"targets": [
			{"type": "single", "providerId": "p-a", "modelId": "m-a"},
			{"type": "single", "providerId": "p-b", "modelId": "m-b"}
		]
	}`)

	var node StrategyNode
	if err := json.Unmarshal(raw, &node); err != nil {
		t.Fatalf("unmarshal fallback config: %v", err)
	}
	if len(node.Targets) != 2 {
		t.Fatalf("fallback Targets = %d, want 2", len(node.Targets))
	}
	if len(node.ABTargets) != 0 {
		t.Errorf("fallback node must not hydrate ABTargets, got %d", len(node.ABTargets))
	}
	if node.Targets[0].ProviderID != "p-a" || node.Targets[1].ModelID != "m-b" {
		t.Errorf("fallback targets mis-decoded: %+v", node.Targets)
	}
}

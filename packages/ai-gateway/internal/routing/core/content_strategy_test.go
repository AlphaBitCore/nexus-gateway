package core

import (
	"github.com/goccy/go-json"
	"testing"
)

// TestStrategyTreeReadsContent locks the Phase-E A1/A6 invariant: a routing
// strategy that reads the canonical request payload (currently only "smart")
// must be detected NO MATTER how deeply it nests, because a false-negative
// starves smart routing of rctx.Request and silently routes to the default
// model. ab_split targets are leaves (provider/model only) and can never carry
// a nested strategy, so they must NOT trigger detection.
func TestStrategyTreeReadsContent(t *testing.T) {
	tests := []struct {
		name string
		node *StrategyNode
		want bool
	}{
		{"nil", nil, false},
		{"single", &StrategyNode{Type: "single", ProviderID: "p", ModelID: "m"}, false},
		{"top-level smart", &StrategyNode{Type: "smart", RouterModelID: "r"}, true},
		{
			"smart nested in fallback targets",
			&StrategyNode{Type: "fallback", Targets: []StrategyNode{
				{Type: "single"},
				{Type: "smart"},
			}},
			true,
		},
		{
			"smart nested in loadbalance weighted node",
			&StrategyNode{Type: "loadbalance", Weighted: []WeightedTarget{
				{Weight: 1, Node: StrategyNode{Type: "single"}},
				{Weight: 2, Node: StrategyNode{Type: "smart"}},
			}},
			true,
		},
		{
			"smart nested in conditional then",
			&StrategyNode{Type: "conditional", Conditions: []ConditionalBranch{
				{When: map[string]any{"x": 1}, Then: StrategyNode{Type: "smart"}},
			}},
			true,
		},
		{
			"smart nested in conditional default",
			&StrategyNode{Type: "conditional",
				Conditions: []ConditionalBranch{{Then: StrategyNode{Type: "single"}}},
				Default:    &StrategyNode{Type: "smart"},
			},
			true,
		},
		{
			"deeply nested smart (fallback>loadbalance>conditional default)",
			&StrategyNode{Type: "fallback", Targets: []StrategyNode{
				{Type: "loadbalance", Weighted: []WeightedTarget{
					{Node: StrategyNode{Type: "conditional", Default: &StrategyNode{Type: "smart"}}},
				}},
			}},
			true,
		},
		{
			"no smart anywhere",
			&StrategyNode{Type: "fallback", Targets: []StrategyNode{
				{Type: "loadbalance", Weighted: []WeightedTarget{
					{Node: StrategyNode{Type: "single"}},
				}},
				{Type: "conditional",
					Conditions: []ConditionalBranch{{Then: StrategyNode{Type: "single"}}},
					Default:    &StrategyNode{Type: "single"},
				},
			}},
			false,
		},
		{
			"ab_split leaf targets never trigger",
			&StrategyNode{Type: "ab_split", ABTargets: []ABTarget{
				{ProviderID: "p", ModelID: "m", Weight: 50},
			}},
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StrategyTreeReadsContent(tt.node); got != tt.want {
				t.Fatalf("StrategyTreeReadsContent = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestConfigReadsContent exercises the json.RawMessage entry point used by the
// needCanonical gate, including a malformed config which must fail SAFE (treat
// as content-reading so the canonical is computed — never starve a consumer).
func TestConfigReadsContent(t *testing.T) {
	smart, _ := json.Marshal(StrategyNode{Type: "fallback", Targets: []StrategyNode{{Type: "smart"}}})
	plain, _ := json.Marshal(StrategyNode{Type: "single", ProviderID: "p", ModelID: "m"})

	if !ConfigReadsContent(smart) {
		t.Fatal("nested-smart config must read content")
	}
	if ConfigReadsContent(plain) {
		t.Fatal("single-strategy config must not read content")
	}
	if !ConfigReadsContent([]byte(`{not json`)) {
		t.Fatal("malformed config must fail SAFE (treat as content-reading)")
	}
	if ConfigReadsContent(nil) {
		t.Fatal("nil/empty config has no strategy → not content-reading")
	}
}

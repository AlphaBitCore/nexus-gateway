package routing

import (
	"context"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// TestBranchIsolation_ASiblingBranchIsNeverReachableFromAFailedOne.
//
// `conditional` and `ab_split` choose ONE branch. When that branch's target
// fails, recovery must draw from the rule's own fallback chain — never from the
// arm the rule deliberately did not take. A request that matched "branch A" and
// was answered by branch B was routed by a rule that never said so, and the
// traffic row names the rule either way.
//
// Asserted on the PLAN rather than on the strategies' return values. Both
// strategies happen to return a single target today, which makes the invariant
// true by construction — and that construction is exactly what a future
// two-branch strategy would change without failing anything. The plan is where
// a sibling would have to appear to be dispatched, so the plan is where the
// question is asked.
func TestBranchIsolation_ASiblingBranchIsNeverReachableFromAFailedOne(t *testing.T) {
	for _, tc := range []struct {
		name     string
		strategy string
		config   any
	}{
		{
			name:     "conditional",
			strategy: "conditional",
			config: map[string]any{
				"type": "conditional",
				"conditions": []map[string]any{
					// The first branch matches every request, so "taken" is the
					// arm and "sibling" is the one the rule did not take.
					{"when": map[string]any{}, "then": map[string]any{
						"type": "single", "providerId": "p1", "modelId": "m-taken"}},
					{"when": map[string]any{"requestedModel": "never-matches"}, "then": map[string]any{
						"type": "single", "providerId": "p1", "modelId": "m-sibling"}},
				},
			},
		},
		{
			name:     "ab_split",
			strategy: "ab_split",
			config: map[string]any{
				"type": "ab_split",
				// All the weight on one arm, so the roll is deterministic and
				// "sibling" is the arm the rule did not take.
				"abTargets": []map[string]any{
					{"providerId": "p1", "modelId": "m-taken", "weight": 100},
					{"providerId": "p1", "modelId": "m-sibling", "weight": 0},
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newResolverFixture()
			f.addProvider("p1", true)
			f.addModel("m-taken", "p1", "taken", true)
			f.addModel("m-sibling", "p1", "sibling", true)
			f.addModel("m-chain", "p1", "chain", true)

			f.addRule(store.RoutingRule{
				ID:            "r-branching",
				Name:          "branching",
				StrategyType:  tc.strategy,
				PipelineStage: 1,
				Config:        mustJSON(t, tc.config),
				// The rule's OWN answer for a failure, and the only place
				// recovery may draw from.
				FallbackChain: mustJSON(t, []map[string]any{
					{"providerId": "p1", "modelId": "m-chain"},
				}),
			})

			res, err := f.resolver.ResolveTargets(context.Background(), &core.RoutingContext{
				RequestedModel: core.RequestedModel{ID: "auto"},
				EndpointType:   "chat",
			})
			if err != nil {
				t.Fatalf("ResolveTargets: %v", err)
			}

			var ids []string
			for _, tg := range res.AllTargets() {
				ids = append(ids, tg.ModelID)
			}
			if len(ids) == 0 {
				t.Fatalf("the rule resolved nothing; the scenario needs a plan to inspect")
			}
			if ids[0] != "m-taken" {
				t.Fatalf("plan leads with %s, want the branch the rule took: %v", ids[0], ids)
			}
			for _, id := range ids {
				if id == "m-sibling" {
					t.Errorf("the branch the rule did NOT take is in the plan: %v\n"+
						"  a failure on the taken arm would fall sideways into it, and the "+
						"traffic row would name this rule for an answer it never chose", ids)
				}
			}
			// The chain IS reachable — that is the difference being asserted,
			// and without it this test would pass against a plan that carried
			// no recovery at all.
			var hasChain bool
			for _, id := range ids {
				if id == "m-chain" {
					hasChain = true
				}
			}
			if !hasChain {
				t.Errorf("the rule's own fallback chain is absent from the plan: %v — then "+
					"this test cannot tell 'the sibling is excluded' from 'nothing is "+
					"included'", ids)
			}
		})
	}
}

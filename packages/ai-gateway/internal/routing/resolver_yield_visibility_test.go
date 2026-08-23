package routing

import (
	"context"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/goccy/go-json"
)

// pipelineText joins the pipeline trace so a test can ask whether an operator
// would find the explanation, without pinning the exact wording.
func pipelineText(plan *core.RoutingPlan) string {
	var b strings.Builder
	for _, e := range plan.PipelineTrace {
		b.WriteString(e.Decision)
		b.WriteString("\n")
	}
	return b.String()
}

// TestResolve_EveryWayARuleYieldsItsSlotIsRecorded.
//
// A rule can match the request and route nothing, four different ways. Each
// time, the rule below it is tried — which is the behaviour we want — and each
// time the admin is left with a rule that shows enabled and green while a
// different rule's name lands on the traffic row.
//
// The service log is not the answer. The operator who can read journald is not
// the admin who authored the rule; the channel the admin has is
// traffic_event.routing_trace, and the pipeline trace is what fills it. So the
// assertion is on the trace, not on the log.
//
// The reasons are asserted by their distinguishing word rather than verbatim,
// because the value is that the four are TOLD APART: "does not parse" sends the
// admin to the config, "no target" to the strategy, "allowed models" to the
// key. A single "rule skipped" for all four would satisfy a laxer test and
// leave the admin exactly where they started.
func TestResolve_EveryWayARuleYieldsItsSlotIsRecorded(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rule     store.RoutingRule
		vk       *core.VKContext
		wantWord string
	}{
		{
			name: "the strategy configuration does not parse",
			rule: store.RoutingRule{
				ID: "r-bad", Name: "broken json", StrategyType: "single", PipelineStage: 1,
				Priority: 100, Config: json.RawMessage(`{"type":"single",`),
			},
			wantWord: "does not parse",
		},
		{
			name: "the strategy cannot be evaluated",
			rule: store.RoutingRule{
				ID: "r-unknown", Name: "unknown type", StrategyType: "single", PipelineStage: 1,
				Priority: 100, Config: json.RawMessage(`{"type":"no-such-strategy"}`),
			},
			wantWord: "could not be evaluated",
		},
		{
			name: "it resolves no target",
			rule: store.RoutingRule{
				ID: "r-empty", Name: "points at nothing", StrategyType: "single", PipelineStage: 1,
				Priority: 100, Config: json.RawMessage(`{"type":"single","providerId":"p1","modelId":"gone"}`),
			},
			wantWord: "no target",
		},
		{
			name: "every target it resolved is outside this key's allowed models",
			rule: store.RoutingRule{
				ID: "r-vk", Name: "not for this key", StrategyType: "single", PipelineStage: 1,
				Priority: 100, Config: json.RawMessage(`{"type":"single","providerId":"p1","modelId":"m1"}`),
			},
			vk: &core.VKContext{ID: "vk-1", AllowedModels: []store.AllowedModelRef{
				{ProviderID: "p2", ModelID: "m2"},
			}},
			wantWord: "allowed models",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newResolverFixture()
			f.addProvider("p1", true)
			f.addProvider("p2", true)
			f.addModel("m1", "p1", "gpt-4o", true)
			f.addModel("m2", "p2", "claude", true)
			f.addRule(tc.rule)
			// A lower-priority rule this key CAN reach, so the request is still
			// served and the yield is the only thing that changed.
			f.addRule(store.RoutingRule{
				ID: "r-below", Name: "the rule underneath", StrategyType: "single", PipelineStage: 1,
				Priority: 1, Config: json.RawMessage(`{"type":"single","providerId":"p2","modelId":"m2"}`),
			})

			plan, err := f.resolver.Resolve(context.Background(), &core.RoutingContext{
				RequestedModel: core.RequestedModel{ID: "gpt-4o"},
				VirtualKey:     tc.vk,
			})
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if plan.RuleID != "r-below" {
				t.Fatalf("primary rule = %q, want the rule underneath — this case is not "+
					"exercising a yield at all", plan.RuleID)
			}

			got := pipelineText(plan)
			if !strings.Contains(got, tc.rule.Name) {
				t.Errorf("the trace does not name the rule that yielded (%q):\n%s\n"+
					"the admin sees their rule enabled, a different rule on the traffic row, "+
					"and nothing connecting the two", tc.rule.Name, got)
			}
			if !strings.Contains(got, tc.wantWord) {
				t.Errorf("the trace does not say %q:\n%s\nknowing a rule yielded without "+
					"knowing why sends the admin to the wrong place — the config, the "+
					"strategy and the key are four different fixes", tc.wantWord, got)
			}
		})
	}
}

// TestResolve_ALosingRulesReasoningIsAttributedToIt.
//
// The strategy trace is one list shared by every rule the resolver tries. A
// rule that resolves a target and then yields — because this key cannot reach
// it — leaves "resolved <target>" in that list, with no rule attached. Read
// afterwards it looks like the plan's own decision: a target that was never
// dispatched to, sitting in the trace of a request that went somewhere else.
//
// That is worse than an absent entry. An operator debugging "why did this
// request go to claude" reads a line saying gpt-4o was resolved, and the trace
// has actively misled them.
func TestResolve_ALosingRulesReasoningIsAttributedToIt(t *testing.T) {
	f := newResolverFixture()
	f.addProvider("p1", true)
	f.addProvider("p2", true)
	f.addModel("m1", "p1", "gpt-4o", true)
	f.addModel("m2", "p2", "claude", true)
	f.addRule(store.RoutingRule{
		ID: "r-loser", Name: "resolves then yields", StrategyType: "single", PipelineStage: 1,
		Priority: 100, Config: json.RawMessage(`{"type":"single","providerId":"p1","modelId":"m1"}`),
	})
	f.addRule(store.RoutingRule{
		ID: "r-winner", Name: "the rule underneath", StrategyType: "single", PipelineStage: 1,
		Priority: 1, Config: json.RawMessage(`{"type":"single","providerId":"p2","modelId":"m2"}`),
	})

	plan, err := f.resolver.Resolve(context.Background(), &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "gpt-4o"},
		VirtualKey: &core.VKContext{ID: "vk-1", AllowedModels: []store.AllowedModelRef{
			{ProviderID: "p2", ModelID: "m2"},
		}},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if plan.RuleID != "r-winner" {
		t.Fatalf("primary = %q, want r-winner", plan.RuleID)
	}

	var sawLoser, unattributed bool
	for _, e := range plan.Trace {
		switch e.RuleID {
		case "":
			unattributed = true
		case "r-loser":
			sawLoser = true
		}
	}
	if unattributed {
		t.Errorf("a trace entry names no rule: %+v — once more than one rule is evaluated per "+
			"request, an unattributed entry reads as the winner's reasoning", plan.Trace)
	}
	if !sawLoser {
		t.Errorf("the losing rule left no attributed entry: %+v — its reasoning is either "+
			"missing or wearing the winner's name", plan.Trace)
	}
}

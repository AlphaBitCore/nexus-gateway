package strategies

import (
	"context"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/llm"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// The SEAM, which two surviving mutants proved was untested: the filter had a
// test for what it does with the flag, and the proxy had a test for reading the
// flag out of a body, and NOTHING asserted that the flag travels from the
// routing context into the filter. Setting the argument to a literal `false` in
// strategy_smart.go left every other test green while the feature did nothing.
//
// Each half being tested is not the same as the whole being connected.
func TestSmart_StructuredOutput_FlagReachesTheFilter(t *testing.T) {
	capable := core.SmartModelRow{
		ModelID: "m-capable", ModelCode: "claude-sonnet-4-6", ProviderID: "p1",
		Features: []string{"streaming", "structured_outputs"},
	}
	// kimi-k2.5 is the model this dimension exists for: it accepts a
	// json_schema and answers in prose with HTTP 200, so nothing downstream can
	// report the mismatch.
	silent := core.SmartModelRow{
		ModelID: "m-silent", ModelCode: "kimi-k2.5", ProviderID: "p1",
		Features: []string{"streaming", "function_calling"},
	}

	// The filter runs BEFORE the catalog is rendered for the router, so the
	// proof is that the router was never OFFERED the silent model — not merely
	// that it did not pick it. That is what makes this an eligibility
	// constraint rather than a hint the router could overrule. (A decider that
	// names a filtered-out model is not a realistic fixture: in production it
	// only ever sees the filtered catalog.)
	decider := &fakeDecider{decision: llm.Decision{ModelID: "claude-sonnet-4-6", Reason: "capable"}}
	fx := newSmartFixture(t, decider, []core.SmartModelRow{capable, silent})
	strat := &SmartStrategy{deps: fx.deps()}
	node := core.StrategyNode{RouterProviderID: "p-router", RouterModelID: "m-router"}

	rctx := &core.RoutingContext{
		Request: &normalize.NormalizedPayload{
			Kind:     normalize.KindAIChat,
			Messages: []normalize.Message{textMsg(normalize.RoleUser, "give me a verdict")},
		},
		RequiresStructuredOutput: true,
	}

	trace := []core.TraceEntry{}
	targets, err := strat.Evaluate(context.Background(), node, fx.pool(rctx), &trace)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	for _, tg := range targets {
		if tg.ModelID == "m-silent" {
			t.Fatalf("routing chose kimi-k2.5 for a json_schema request — the requirement did "+
				"not reach the capability filter; targets=%+v", targets)
		}
	}
	if len(targets) == 0 {
		t.Fatal("no target at all — the filter should have narrowed the pool, not emptied it")
	}
	if strings.Contains(decider.lastReq.SystemPrompt, "kimi-k2.5") {
		t.Errorf("the router was still offered kimi-k2.5 for a json_schema request — the " +
			"requirement did not reach the capability filter")
	}
	if !strings.Contains(decider.lastReq.SystemPrompt, "claude-sonnet-4-6") {
		t.Errorf("the catalog lost the model that CAN honour the schema")
	}
}

// The mirror: without the requirement the same pool must stay whole, so the
// seam carries the flag rather than always narrowing.
func TestSmart_StructuredOutput_NoFlagLeavesThePoolAlone(t *testing.T) {
	capable := core.SmartModelRow{
		ModelID: "m-capable", ModelCode: "claude-sonnet-4-6", ProviderID: "p1",
		Features: []string{"streaming", "structured_outputs"},
	}
	silent := core.SmartModelRow{
		ModelID: "m-silent", ModelCode: "kimi-k2.5", ProviderID: "p1",
		Features: []string{"streaming", "function_calling"},
	}
	decider := &fakeDecider{decision: llm.Decision{ModelID: "kimi-k2.5", Reason: "cheapest"}}
	fx := newSmartFixture(t, decider, []core.SmartModelRow{capable, silent})
	strat := &SmartStrategy{deps: fx.deps()}
	node := core.StrategyNode{RouterProviderID: "p-router", RouterModelID: "m-router"}

	rctx := &core.RoutingContext{
		Request: &normalize.NormalizedPayload{
			Kind:     normalize.KindAIChat,
			Messages: []normalize.Message{textMsg(normalize.RoleUser, "just chat")},
		},
	}

	trace := []core.TraceEntry{}
	targets, err := strat.Evaluate(context.Background(), node, fx.pool(rctx), &trace)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	var chose bool
	for _, tg := range targets {
		if tg.ModelID == "m-silent" {
			chose = true
		}
	}
	if !chose {
		t.Errorf("kimi-k2.5 was excluded from a request that never asked for a schema — the "+
			"dimension must narrow only when the requirement is stated; targets=%+v", targets)
	}
}

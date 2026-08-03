package strategies

import (
	"context"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/llm"
)

// Context-upgrade arming: alongside the router's pick, the smart
// strategy returns the largest-context candidate as a second,
// ContextUpgradeOnly target — the executor fails over to it exactly
// when the picked model overflows, closing the loop the estimate-based
// filter cannot (the estimate is coarse; the upstream verdict is
// authoritative).

func TestSmart_Upgrade_ArmedWhenLargerWindowCandidateExists(t *testing.T) {
	small := core.SmartModelRow{ModelID: "m-small", ModelCode: "compact", ProviderID: "p1", MaxContextTokens: intPtr(128000)}
	big := core.SmartModelRow{ModelID: "m-big", ModelCode: "grande", ProviderID: "p1", MaxContextTokens: intPtr(1000000)}
	decider := &fakeDecider{decision: llm.Decision{ModelID: "compact", Reason: "cheap"}}
	fx := newSmartFixture(t, decider, []core.SmartModelRow{small, big})
	node := core.StrategyNode{RouterProviderID: "p-router", RouterModelID: "m-router"}
	trace := []core.TraceEntry{}
	strat := &SmartStrategy{deps: fx.deps()}

	out, err := strat.Evaluate(context.Background(), node, aiChatRctx(), &trace, 0, nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected picked + upgrade targets, got %d: %+v", len(out), out)
	}
	if out[0].ModelID != "m-small" || out[0].ContextUpgradeOnly {
		t.Fatalf("primary must be the router's pick without the upgrade mark: %+v", out[0])
	}
	if out[1].ModelID != "m-big" || !out[1].ContextUpgradeOnly {
		t.Fatalf("second target must be the largest-window candidate marked ContextUpgradeOnly: %+v", out[1])
	}
	found := false
	for _, e := range trace {
		if strings.Contains(e.Decision, "context-upgrade") {
			found = true
		}
	}
	if !found {
		t.Errorf("upgrade arming must be trace-recorded: %+v", trace)
	}
}

func TestSmart_Upgrade_NotArmedWhenPickIsLargest(t *testing.T) {
	small := core.SmartModelRow{ModelID: "m-small", ModelCode: "compact", ProviderID: "p1", MaxContextTokens: intPtr(128000)}
	big := core.SmartModelRow{ModelID: "m-big", ModelCode: "grande", ProviderID: "p1", MaxContextTokens: intPtr(1000000)}
	decider := &fakeDecider{decision: llm.Decision{ModelID: "grande", Reason: "big"}}
	fx := newSmartFixture(t, decider, []core.SmartModelRow{small, big})
	node := core.StrategyNode{RouterProviderID: "p-router", RouterModelID: "m-router"}
	trace := []core.TraceEntry{}
	strat := &SmartStrategy{deps: fx.deps()}

	out, err := strat.Evaluate(context.Background(), node, aiChatRctx(), &trace, 0, nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(out) != 1 || out[0].ModelID != "m-big" {
		t.Fatalf("largest pick must not arm an upgrade: %+v", out)
	}
}

func TestSmart_Upgrade_NotArmedWhenWindowsUndeclared(t *testing.T) {
	a := core.SmartModelRow{ModelID: "m-a", ModelCode: "alpha", ProviderID: "p1"}
	b := core.SmartModelRow{ModelID: "m-b", ModelCode: "beta", ProviderID: "p1"}
	decider := &fakeDecider{decision: llm.Decision{ModelID: "alpha", Reason: "ok"}}
	fx := newSmartFixture(t, decider, []core.SmartModelRow{a, b})
	node := core.StrategyNode{RouterProviderID: "p-router", RouterModelID: "m-router"}
	trace := []core.TraceEntry{}
	strat := &SmartStrategy{deps: fx.deps()}

	out, err := strat.Evaluate(context.Background(), node, aiChatRctx(), &trace, 0, nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("undeclared windows must not arm an upgrade: %+v", out)
	}
}

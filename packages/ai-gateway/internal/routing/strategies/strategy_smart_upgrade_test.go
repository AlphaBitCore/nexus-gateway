package strategies

import (
	"context"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/llm"
)

// Context-upgrade arming: the largest-context candidate is added as a
// ContextUpgradeOnly target, which the executor uses exactly when the picked
// model overflows and never for a transient failure. It closes the loop the
// estimate-based filter cannot — the estimate is coarse, the upstream verdict
// is authoritative.
//
// It is not made redundant by the re-selection pool that now rides behind the
// pick. That pool is ordered for FAILOVER — a different provider first, then
// the cheaper model — and the largest window is usually the priciest model in
// the catalogue, so it is exactly what those two rules push out. Arming is what
// puts it within reach for the one failure it answers, at a price the walk pays
// only when the failure happens. The fixtures below carry prices for that
// reason: without them the ordering degenerates and the largest model gets
// carried as an ordinary target by accident, which is the shape that would let
// these tests pass while the mechanism did nothing.

func priceOf(v float64) *float64 { return &v }

// cheapPool is three cheap models on three providers plus one very large,
// very expensive one. The re-selection pool takes two of the cheap ones; the
// big one is left for arming.
func cheapPool() []core.SmartModelRow {
	return []core.SmartModelRow{
		{ModelID: "m-small", ModelCode: "compact", ProviderID: "p1",
			MaxContextTokens: intPtr(128000), InputPricePM: priceOf(1), OutputPricePM: priceOf(2)},
		{ModelID: "m-mid", ModelCode: "middling", ProviderID: "p2",
			MaxContextTokens: intPtr(200000), InputPricePM: priceOf(1), OutputPricePM: priceOf(3)},
		{ModelID: "m-alt", ModelCode: "alternate", ProviderID: "p3",
			MaxContextTokens: intPtr(150000), InputPricePM: priceOf(2), OutputPricePM: priceOf(3)},
		{ModelID: "m-big", ModelCode: "grande", ProviderID: "p4",
			MaxContextTokens: intPtr(1000000), InputPricePM: priceOf(40), OutputPricePM: priceOf(80)},
	}
}

func evaluateSmart(t *testing.T, rows []core.SmartModelRow, pick string) ([]core.RoutingTarget, []core.TraceEntry) {
	t.Helper()
	decider := &fakeDecider{decision: llm.Decision{ModelID: pick, Reason: "test"}}
	fx := newSmartFixture(t, decider, rows)
	node := core.StrategyNode{RouterProviderID: "p-router", RouterModelID: "m-router"}
	trace := []core.TraceEntry{}
	strat := &SmartStrategy{deps: fx.deps()}
	out, err := strat.Evaluate(context.Background(), node, fx.pool(aiChatRctx()), &trace)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	return out, trace
}

func TestSmart_Upgrade_ArmedWhenLargerWindowCandidateExists(t *testing.T) {
	out, trace := evaluateSmart(t, cheapPool(), "compact")

	if out[0].ModelID != "m-small" || out[0].ContextUpgradeOnly {
		t.Fatalf("primary must be the router's pick without the upgrade mark: %+v", out[0])
	}

	// The arming rule: the largest window is in the plan, marked, and LAST —
	// behind every ordinary target, so a transient failure reaches an ordinary
	// one first and never spills onto the expensive model.
	last := out[len(out)-1]
	if last.ModelID != "m-big" || !last.ContextUpgradeOnly {
		t.Fatalf("the largest-window candidate must be armed at the end of the plan: %+v", out)
	}
	for _, tgt := range out[:len(out)-1] {
		if tgt.ContextUpgradeOnly {
			t.Errorf("an ordinary re-selection target is marked ContextUpgradeOnly: %+v", tgt)
		}
		if tgt.ModelID == "m-big" {
			t.Errorf("the priciest model was carried as an ordinary failover target; a 5xx on the "+
				"pick would then be answered at 40x the price: %+v", out)
		}
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
	out, _ := evaluateSmart(t, cheapPool(), "grande")

	if out[0].ModelID != "m-big" {
		t.Fatalf("primary must be the router's pick: %+v", out)
	}
	for _, tgt := range out {
		if tgt.ContextUpgradeOnly {
			t.Fatalf("nothing in the pool has a larger window than the pick, so there is no "+
				"upgrade to arm: %+v", out)
		}
	}
}

func TestSmart_Upgrade_NotArmedWhenWindowsUndeclared(t *testing.T) {
	rows := []core.SmartModelRow{
		{ModelID: "m-a", ModelCode: "alpha", ProviderID: "p1"},
		{ModelID: "m-b", ModelCode: "beta", ProviderID: "p2"},
		{ModelID: "m-c", ModelCode: "gamma", ProviderID: "p3"},
	}
	out, _ := evaluateSmart(t, rows, "alpha")

	for _, tgt := range out {
		if tgt.ContextUpgradeOnly {
			t.Fatalf("no candidate declares a window, so no candidate can be known to be "+
				"larger: %+v", out)
		}
	}
}

// TestSmart_Upgrade_NotArmedWhenTheLargestIsAlreadyCarried closes the seam the
// re-selection pool opened.
//
// When the pool's failover ordering happens to carry the largest-window model
// anyway — a small catalogue, or a large model that is also cheap — arming a
// second copy would put one model in the plan twice. The duplicate is not
// cosmetic: the call budget is derived from the plan's length, so it buys a
// request extra attempts, and the second copy is ContextUpgradeOnly, so those
// attempts are ones the walk mostly refuses to use.
func TestSmart_Upgrade_NotArmedWhenTheLargestIsAlreadyCarried(t *testing.T) {
	rows := []core.SmartModelRow{
		{ModelID: "m-small", ModelCode: "compact", ProviderID: "p1",
			MaxContextTokens: intPtr(128000), InputPricePM: priceOf(1), OutputPricePM: priceOf(2)},
		{ModelID: "m-big", ModelCode: "grande", ProviderID: "p2",
			MaxContextTokens: intPtr(1000000), InputPricePM: priceOf(1), OutputPricePM: priceOf(1)},
	}
	out, _ := evaluateSmart(t, rows, "compact")

	seen := map[string]int{}
	for _, tgt := range out {
		seen[tgt.ModelID]++
	}
	if seen["m-big"] != 1 {
		t.Fatalf("m-big appears %d times; the re-selection pool already carries it, so arming "+
			"adds a duplicate that inflates the call budget: %+v", seen["m-big"], out)
	}
	for _, tgt := range out {
		if tgt.ContextUpgradeOnly {
			t.Errorf("m-big is reachable for every failure as an ordinary target; a restricted "+
				"copy alongside it says two different things about one model: %+v", out)
		}
	}
}

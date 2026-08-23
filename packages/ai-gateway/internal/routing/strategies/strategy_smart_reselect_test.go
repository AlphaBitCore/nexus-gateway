package strategies

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/llm"
)

// reselectPool is five models across four providers with distinct prices, so
// both halves of the ordering rule have something to decide.
// The output prices are deliberately the REVERSE of the input prices, so a
// comparison that sums both halves produces a different order from one that
// reads input alone. A fixture where the two agree cannot tell the design's
// rule from the one that disagreed with it.
func reselectPool() []core.SmartModelRow {
	return []core.SmartModelRow{
		{ModelID: "m-pick", ModelCode: "picked", ProviderID: "p1",
			InputPricePM: priceOf(5), OutputPricePM: priceOf(5)},
		{ModelID: "m-cheap-in", ModelCode: "cheapin", ProviderID: "p2",
			InputPricePM: priceOf(1), OutputPricePM: priceOf(90)}, // sum 91
		{ModelID: "m-mid-in", ModelCode: "midin", ProviderID: "p3",
			InputPricePM: priceOf(2), OutputPricePM: priceOf(40)}, // sum 42
		{ModelID: "m-dear-in", ModelCode: "dearin", ProviderID: "p4",
			InputPricePM: priceOf(30), OutputPricePM: priceOf(1)}, // sum 31
	}
}

// TestSmart_TheFilteredPoolIsCarriedBehindTheRoutersPick.
//
// The pool the router chose from is already filtered for this request — the
// key's allowlist, required capabilities, a window that holds the prompt — so
// its other members are the right thing to try when the choice fails.
// Returning only the pick left `model:auto` with one target: one 5xx and the
// walk had to leave the rule entirely, for a rule whose whole job is to pick
// among many.
func TestSmart_TheFilteredPoolIsCarriedBehindTheRoutersPick(t *testing.T) {
	out, trace := evaluateSmart(t, reselectPool(), "picked")

	if len(out) != smartReselectDepth+1 {
		t.Fatalf("plan carries %d target(s), want %d — the pool the router chose from was "+
			"discarded, so a transient failure has nowhere to go inside this rule: %+v",
			len(out), smartReselectDepth+1, out)
	}
	if out[0].ModelID != "m-pick" {
		t.Fatalf("the router's choice must stay first: %+v", out)
	}
	// Every companion is trace-recorded: a target in the plan that no line
	// accounts for is what an operator cannot reconcile against the rule.
	for _, tgt := range out[1:] {
		found := false
		for _, e := range trace {
			if strings.Contains(e.Decision, "re-selection pool") && strings.Contains(e.Decision, tgt.ModelID) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is in the plan with nothing on the trace saying why: %+v", tgt.ModelID, trace)
		}
	}
}

// TestSmart_ReselectionFollowsThePoolsPriceOrder pins the ordering rule, which
// is the design's and not this function's to invent: ascending input price,
// undeclared last, the pick promoted to position zero and the rest following.
//
// An earlier version ordered by "a new provider first, then the cheaper model"
// and asserted that here. Provider preference belongs to the recovery engine —
// `selectNext` applies it after a 429 or a 5xx, the failures a per-provider
// quota or outage actually explains. Applying it in the strategy imposed it on
// every failure, including the ones it does not fit, and used a second price
// rule that disagreed with the one beside it.
func TestSmart_ReselectionFollowsThePoolsPriceOrder(t *testing.T) {
	out, _ := evaluateSmart(t, reselectPool(), "picked")

	got := []string{out[1].ModelID, out[2].ModelID}
	want := []string{"m-cheap-in", "m-mid-in"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("companions = %v, want %v — ascending INPUT price. Summing both halves "+
				"reverses this fixture exactly (m-dear-in has the lowest total and the highest "+
				"input), which is how the two implementations disagreed.", got, want)
		}
	}
}

// TestSmart_ReselectionSortsUndeclaredPriceLast is the other half of the rule,
// and the half the two implementations disagreed on: a model declaring an
// OUTPUT price but no input price. Summing both halves made it look cheap; the
// design says an undeclared INPUT price sorts last, because an unknown cost is
// not a low one.
func TestSmart_ReselectionSortsUndeclaredPriceLast(t *testing.T) {
	rows := []core.SmartModelRow{
		{ModelID: "m-pick", ModelCode: "picked", ProviderID: "p0",
			InputPricePM: priceOf(9), OutputPricePM: priceOf(9)},
		{ModelID: "m-no-input", ModelCode: "noinput", ProviderID: "p1",
			InputPricePM: nil, OutputPricePM: priceOf(2)},
		{ModelID: "m-priced", ModelCode: "priced", ProviderID: "p2",
			InputPricePM: priceOf(5), OutputPricePM: priceOf(50)},
	}
	out, _ := evaluateSmart(t, rows, "picked")

	if out[1].ModelID != "m-priced" {
		t.Fatalf("first companion = %s, want m-priced. A model stating no input price sorted "+
			"ahead of one that states 5/M — an unknown cost read as a low one.", out[1].ModelID)
	}
	if out[2].ModelID != "m-no-input" {
		t.Fatalf("second companion = %s, want m-no-input last", out[2].ModelID)
	}
}

// TestSmart_ReselectionSurvivesAPoolThatResolvesNothing bounds the failure
// path. Each companion costs a scan of the pool, so a catalogue that resolves
// none of them would rescan it once per slot; the run of failures ends the
// search instead.
func TestSmart_ReselectionSurvivesAPoolThatResolvesNothing(t *testing.T) {
	rows := []core.SmartModelRow{{ModelID: "m-pick", ModelCode: "picked", ProviderID: "p0"}}
	for i := range 30 {
		id := fmt.Sprintf("m-ghost-%d", i)
		rows = append(rows, core.SmartModelRow{ModelID: id, ModelCode: "ghost-" + id, ProviderID: "p1"})
	}
	decider := &fakeDecider{decision: llm.Decision{ModelID: "picked", Reason: "test"}}
	fx := newSmartFixture(t, decider, rows)
	// Only the pick resolves; every companion lookup fails.
	lookups := 0
	fx.lookup = func(_ context.Context, pid, mid string) (*core.RoutingTarget, error) {
		lookups++
		if mid == "m-pick" {
			return &core.RoutingTarget{ProviderID: pid, ModelID: mid}, nil
		}
		return nil, errors.New("not in the snapshot")
	}
	trace := []core.TraceEntry{}
	strat := &SmartStrategy{deps: fx.deps()}
	out, err := strat.Evaluate(context.Background(), core.StrategyNode{
		RouterProviderID: "p-router", RouterModelID: "m-router",
	}, fx.pool(aiChatRctx()), &trace)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(out) != 1 || out[0].ModelID != "m-pick" {
		t.Fatalf("the router's own choice must still be served when no companion resolves: %+v", out)
	}
	// The output is identical whether or not the search stops early, so the
	// WORK is what has to be asserted. Without the bound the loop keeps asking
	// about every row in the pool.
	if lookups > smartReselectDepth+2 {
		t.Errorf("%d lookups against a %d-model pool where none resolve; the search must give up "+
			"after a run of failures rather than walking the catalogue", lookups, len(rows))
	}
}

// TestSmart_ThePlanSizeDoesNotGrowWithThePool.
//
// EffectiveCallBudget is derived from the plan's LENGTH, so every target the
// strategy adds raises the ceiling on what one request may spend. A production
// pool is tens of models; if the plan tracked it, the ceiling would too — for a
// request that almost always succeeds on the first call.
//
// Asserted as "same plan for a 4-model pool and a 41-model one", with a stated
// numeric ceiling. Comparing against smartReselectDepth would compare the bound
// with itself: raising the constant would raise the expectation with it and the
// test could never go red, which is what the first version of this did.
func TestSmart_ThePlanSizeDoesNotGrowWithThePool(t *testing.T) {
	small := []core.SmartModelRow{
		{ModelID: "m-pick", ModelCode: "picked", ProviderID: "p0"},
		{ModelID: "m-1", ModelCode: "one", ProviderID: "p1"},
		{ModelID: "m-2", ModelCode: "two", ProviderID: "p2"},
		{ModelID: "m-3", ModelCode: "three", ProviderID: "p3"},
	}
	large := append([]core.SmartModelRow{}, small...)
	for i := range 37 {
		id := fmt.Sprintf("m-bulk-%d", i)
		large = append(large, core.SmartModelRow{
			ModelID: id, ModelCode: "bulk-" + id, ProviderID: fmt.Sprintf("p-bulk-%d", i),
		})
	}

	fromSmall, _ := evaluateSmart(t, small, "picked")
	fromLarge, _ := evaluateSmart(t, large, "picked")

	if len(fromSmall) != len(fromLarge) {
		t.Fatalf("a 4-model pool gives %d targets and a 41-model pool gives %d — the call budget "+
			"is derived from that length, so the ceiling on one request's spend now tracks the "+
			"size of the catalogue", len(fromSmall), len(fromLarge))
	}
	// The stated ceiling, independent of the constant. Four is the plan a rule
	// may hold: the router's pick, its companions, and at most one armed
	// context upgrade.
	if len(fromLarge) > 4 {
		t.Fatalf("plan holds %d targets; at two attempts each that is a budget of %d upstream "+
			"calls for one request: %+v", len(fromLarge), len(fromLarge)*2, fromLarge)
	}
}

// TestSmart_ReselectionOrdersByPriceEvenOnThePicksOwnProvider proves price is
// the ONLY term in the pool's order.
//
// Every other fixture here gives each row its own provider, so a provider-first
// rule and a price-only rule produce the same list and neither test can tell
// them apart. This one makes them disagree: the cheapest companion sits on the
// SAME provider as the pick, and a much dearer one sits elsewhere. Price-only
// puts the cheap sibling first; anything that prefers a new provider puts the
// dear one first.
//
// Provider preference is the recovery engine's — `selectNext` applies it after
// a 429 or a 5xx, the failures a per-provider quota or outage actually
// explains. Re-introducing it here would impose it on every failure, including
// the ones it does not fit.
func TestSmart_ReselectionOrdersByPriceEvenOnThePicksOwnProvider(t *testing.T) {
	rows := []core.SmartModelRow{
		{ModelID: "m-pick", ModelCode: "picked", ProviderID: "p1",
			InputPricePM: priceOf(5), OutputPricePM: priceOf(5)},
		{ModelID: "m-cheap-same-provider", ModelCode: "cheapsame", ProviderID: "p1",
			InputPricePM: priceOf(1), OutputPricePM: priceOf(1)},
		{ModelID: "m-dear-other-provider", ModelCode: "dearother", ProviderID: "p2",
			InputPricePM: priceOf(30), OutputPricePM: priceOf(30)},
	}
	out, _ := evaluateSmart(t, rows, "picked")

	if out[1].ModelID != "m-cheap-same-provider" {
		t.Fatalf("first companion = %s, want m-cheap-same-provider — the pool orders by input "+
			"price and nothing else. A model six times dearer sorted ahead of it because it "+
			"was on a different provider, which is a decision the walk makes at dispatch "+
			"time, with the failure in hand", out[1].ModelID)
	}
	if out[2].ModelID != "m-dear-other-provider" {
		t.Fatalf("second companion = %s, want m-dear-other-provider", out[2].ModelID)
	}
}

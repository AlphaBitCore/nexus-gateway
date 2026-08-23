package strategies

// The smart strategy's own escape hatch throws away the filter it just ran.
//
// Eight call sites fall back to the rule's configured default model when the
// router cannot answer. Four of them — router client absent, router call
// failed, router named a model nobody has, target lookup failed — hold a
// candidate pool that has ALREADY been through the capability filter, and hand
// it back for a default that was never checked against anything.
//
// The caller sent `auto`. Every model reachable from that point is a model WE
// chose, so a refusal one of them produces about an attachment it cannot read
// is ours, not the upstream's — the same rule that governs the recovery list in
// the resolver. Filtering the pool and then dispatching outside it is the
// defect this program is named after, one layer up.

import (
	"context"
	"errors"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/llm"
)

// visionAndBlind is the shape every case here needs: a model that can read the
// attachment, and the configured default which cannot. `blind` declares text
// only, so the capability filter drops it before the router is ever consulted —
// and then the fallback reaches straight past the filter for it.
func visionAndBlind() []core.SmartModelRow {
	return []core.SmartModelRow{
		{ModelID: "m-vision", ModelCode: "vision", ProviderID: "p1", ProviderName: "p1",
			InputModalities: []string{"text", "image"}},
		{ModelID: "m-blind", ModelCode: "blind", ProviderID: "p1", ProviderName: "p1",
			InputModalities: []string{"text"}},
	}
}

// smartNodeWithDefault builds a rule whose default is the blind model.
func smartNodeWithDefault() core.StrategyNode {
	return core.StrategyNode{
		RouterProviderID:  "p-router",
		RouterModelID:     "m-router",
		DefaultProviderID: "p1",
		DefaultModelID:    "m-blind",
	}
}

func TestSmartFallback_DoesNotDispatchADefaultTheFilterDropped(t *testing.T) {
	for _, tc := range []struct {
		name    string
		decider *fakeDecider
		wire    bool // wire the router client at all
	}{
		{name: "router client not wired", wire: false},
		{name: "router call failed", wire: true,
			decider: &fakeDecider{err: errors.New("router timeout")}},
		{name: "router named a model nobody has", wire: true,
			decider: &fakeDecider{decision: llm.Decision{ModelID: "does-not-exist"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := visionAndBlind()
			fx := newSmartFixture(t, tc.decider, rows)
			deps := fx.deps()
			if !tc.wire {
				deps.RouterLLM = nil
			}
			strat := &SmartStrategy{deps: deps}
			trace := []core.TraceEntry{}

			out, err := strat.Evaluate(context.Background(), smartNodeWithDefault(),
				poolOf(imageRctx(), rows), &trace)
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if len(out) == 0 {
				t.Fatalf("no target at all — the fallback must still route the request; "+
					"trace: %+v", trace)
			}
			if out[0].ModelID == "m-blind" {
				t.Errorf("dispatched m-blind, the configured default, for a request "+
					"carrying an image it declares it cannot read.\n"+
					"  the capability filter had already dropped it, and m-vision was "+
					"sitting in the filtered pool.\n"+
					"  trace: %+v", trace)
			}
		})
	}
}

// The other direction, so the fix cannot be "always ignore the default": when
// the configured default CAN serve the request it must still win. An admin who
// names a default is expressing a preference, and overriding it whenever the
// router stumbles would quietly make that setting meaningless.
func TestSmartFallback_UsesTheConfiguredDefaultWhenItCanServe(t *testing.T) {
	rows := visionAndBlind()
	node := smartNodeWithDefault()
	node.DefaultModelID = "m-vision" // the default can read the image

	fx := newSmartFixture(t, &fakeDecider{err: errors.New("router timeout")}, rows)
	strat := &SmartStrategy{deps: fx.deps()}
	trace := []core.TraceEntry{}

	out, err := strat.Evaluate(context.Background(), node, fx.pool(imageRctx()), &trace)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(out) != 1 || out[0].ModelID != "m-vision" {
		t.Errorf("got %+v, want the configured default m-vision — it serves this "+
			"request, so the admin's choice stands", out)
	}
}

// And when there is no pool to choose from, the default is all there is. The
// candidate list failed to load or came back empty; refusing here would turn a
// degraded catalog into a dead gateway, which is strictly worse than trying the
// one model the admin nominated.
func TestSmartFallback_KeepsTheDefaultWhenThereIsNoPool(t *testing.T) {
	fx := newSmartFixture(t, &fakeDecider{err: errors.New("router timeout")}, nil)
	fx.store = &fakeSmartStore{rows: nil}
	deps := fx.deps()
	deps.Lookup = func(_ context.Context, pid, mid string) (*core.RoutingTarget, error) {
		return &core.RoutingTarget{ProviderID: pid, ModelID: mid, ModelCode: mid}, nil
	}
	strat := &SmartStrategy{deps: deps}
	trace := []core.TraceEntry{}

	// The context carries no pool on purpose: that IS the case under test.
	out, err := strat.Evaluate(context.Background(), smartNodeWithDefault(),
		imageRctx(), &trace)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(out) != 1 || out[0].ModelID != "m-blind" {
		t.Errorf("got %+v, want the default m-blind — with an empty candidate pool "+
			"there is nothing to check it against and nothing better to send", out)
	}
}

// A rule's defaultModelId is admin-entered and the admin UI shows model CODES,
// so the configured value is at least as likely to be "blind" as "m-blind".
// Matching on the id alone reads a perfectly valid default as absent from the
// pool, which sends the request to the pool's first entry instead — silently
// overriding a setting that was never wrong. Caught by mutation: dropping the
// ModelCode arm left every other test in this file green.
func TestSmartFallback_DefaultMayBeSpelledAsAModelCode(t *testing.T) {
	// A SECOND capable model, and the default names the one that is NOT first
	// in the pool. Written the other way round first, both branches returned
	// the same model — recognising the code and failing to recognise it both
	// ended at the pool's first entry, so the assertion could not tell them
	// apart and the mutation survived.
	rows := append(visionAndBlind(), core.SmartModelRow{
		ModelID: "m-other", ModelCode: "other", ProviderID: "p1", ProviderName: "p1",
		InputModalities: []string{"text", "image"},
	})
	node := smartNodeWithDefault()
	node.DefaultModelID = "other" // the CODE of a capable model, second in the pool

	fx := newSmartFixture(t, &fakeDecider{err: errors.New("router timeout")}, rows)
	deps := fx.deps()
	deps.Lookup = func(_ context.Context, pid, mid string) (*core.RoutingTarget, error) {
		for _, r := range rows {
			if r.ModelID == mid || r.ModelCode == mid {
				return &core.RoutingTarget{ProviderID: pid, ModelID: r.ModelID, ModelCode: r.ModelCode}, nil
			}
		}
		return nil, errors.New("not found")
	}
	strat := &SmartStrategy{deps: deps}
	trace := []core.TraceEntry{}

	out, err := strat.Evaluate(context.Background(), node, fx.pool(imageRctx()), &trace)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(out) != 1 || out[0].ModelID != "m-other" {
		t.Errorf("got %+v, want m-other — the default was configured by CODE and can "+
			"serve this request, so it must be recognised as being in the pool rather "+
			"than replaced by the pool's first entry.\n  trace: %+v", out, trace)
	}
}

// The fifth fallback site: the router picked a real model and resolving it to a
// target failed. Same pool, same rule — and no case reached it until a mutation
// showed this site could be handed a nil pool with every test still green.
func TestSmartFallback_LookupFailureSiteStillHonoursTheFilter(t *testing.T) {
	rows := visionAndBlind()
	fx := newSmartFixture(t, &fakeDecider{decision: llm.Decision{ModelID: "vision"}}, rows)
	deps := fx.deps()
	deps.Lookup = func(_ context.Context, _, mid string) (*core.RoutingTarget, error) {
		if mid == "m-vision" || mid == "vision" {
			return nil, errors.New("credential resolve failed")
		}
		return &core.RoutingTarget{ProviderID: "p1", ModelID: mid, ModelCode: mid}, nil
	}
	strat := &SmartStrategy{deps: deps}
	trace := []core.TraceEntry{}

	out, err := strat.Evaluate(context.Background(), smartNodeWithDefault(), poolOf(imageRctx(), rows), &trace)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(out) > 0 && out[0].ModelID == "m-blind" {
		t.Errorf("the lookup-failure fallback dispatched m-blind for a request "+
			"carrying an image it cannot read.\n  trace: %+v", trace)
	}
}

// The no-user-content fallback site, which no case reached: an attachment can
// ride on a non-user message, so the capability filter runs and drops the blind
// default, and then this site falls back. Mutation-proven — routing this site's
// pool to nil left every other test green.
func TestSmartFallback_NoUserContentSiteStillHonoursTheFilter(t *testing.T) {
	rows := visionAndBlind()
	fx := newSmartFixture(t, &fakeDecider{decision: llm.Decision{ModelID: "vision"}}, rows)
	strat := &SmartStrategy{deps: fx.deps()}
	trace := []core.TraceEntry{}

	rctx := imageRctx()
	// Same content, attributed to the assistant: the filter still sees an image,
	// and the strategy takes the "no user content" exit.
	rctx.Request.Messages[0].Role = "assistant"

	out, err := strat.Evaluate(context.Background(), smartNodeWithDefault(), fx.pool(rctx), &trace)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(out) > 0 && out[0].ModelID == "m-blind" {
		t.Errorf("the no-user-content fallback dispatched m-blind for a request "+
			"carrying an image it cannot read.\n  trace: %+v", trace)
	}
}

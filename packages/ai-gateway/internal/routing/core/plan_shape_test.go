package core

import "testing"

func planFixture() *RouteResult {
	return &RouteResult{Dispatch: []RoutingTarget{
		{ModelID: "chosen", ProviderID: "p1", Source: "primary"},
		{ModelID: "chain-1", ProviderID: "p2", Source: "fallback"},
		{ModelID: "chain-2", ProviderID: "p3", Source: "recovery"},
	}}
}

func ids(ts []RoutingTarget) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.ModelID
	}
	return out
}

func eq(a []string, b ...string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The role views (Ranked/Chain) were offered as accessors and deleted unused.
//
// They were the stated compensation for storing one ordered list instead of
// two: "a caller has to say which it means". No caller ever did — every
// production reader wants either the dispatch order or its head, and the one
// reader that genuinely splits by role (the audit projection) reads Source
// directly, because it distinguishes an inline chain from a lower rule and the
// two-way view could not.
//
// Recorded rather than silently dropped: an accessor nobody calls compensates
// for nothing, and a design note claiming otherwise is the kind of thing that
// reads as satisfied when it is not.

// TestPlanPrimary_IsTheHeadOfTheDispatchOrderNotTheStrategysPick.
//
// When every ranked target is filtered away — a modality guard, a wire-format
// filter — the request is still served, by the chain. The traffic row and the
// quota price must name the model the request will actually reach; naming the
// strategy's pick would price a model nobody calls and attribute the request to
// a target that was dropped.
func TestPlanPrimary_IsTheHeadOfTheDispatchOrderNotTheStrategysPick(t *testing.T) {
	p := &RouteResult{Dispatch: []RoutingTarget{
		{ModelID: "chain-only", ProviderID: "p2", Source: "fallback"},
	}}
	if p.Primary().ModelID != "chain-only" {
		t.Errorf("Primary() = %q — with the ranked half filtered away the request is served "+
			"by the chain, and the row must name what served it", p.Primary().ModelID)
	}
	if p.Primary().Source != "fallback" {
		t.Errorf("Primary().Source = %q — nothing of the strategy's answer survived, and the "+
			"audit row must say the request was served by a backup rather than attribute it "+
			"to a rule's own pick", p.Primary().Source)
	}

	var empty RouteResult
	if empty.Primary().ModelID != "" {
		t.Error("Primary() on an empty plan must be the zero target, not a panic")
	}
}

// TestPlanPromote_MovesOneTargetAndKeepsTheRestBehindIt.
//
// The quota downgrade promotes the cheapest affordable model, frequently from
// the chain. Everything else has to stay, in order: an earlier version cut the
// plan down to the single downgraded target, which made the next transient
// failure terminal — a second penalty for being near a quota that nobody chose
// to impose.
func TestPlanPromote_MovesOneTargetAndKeepsTheRestBehindIt(t *testing.T) {
	p := planFixture()
	p.Promote(RoutingTarget{ModelID: "chain-2", ProviderID: "p3", Source: "recovery"})

	if got := ids(p.AllTargets()); !eq(got, "chain-2", "chosen", "chain-1") {
		t.Errorf("order = %v, want the promoted target first and everything else behind it "+
			"in its existing order", got)
	}
	if len(p.AllTargets()) != 3 {
		t.Errorf("the plan lost targets during promotion: %v — a transient failure after a "+
			"downgrade then has nowhere to go", ids(p.AllTargets()))
	}
}

// TestPlanNarrow_KeepsExactlyWhatItIsGiven.
//
// The wire-compatibility filter removes targets whose adapter cannot serve the
// caller's ingress format. It is a removal, not a reordering, and it must not
// resurrect anything: a target the filter rejected is one the request cannot
// reach at all.
func TestPlanNarrow_KeepsExactlyWhatItIsGiven(t *testing.T) {
	p := planFixture()
	p.Narrow([]RoutingTarget{p.Dispatch[2], p.Dispatch[0]})

	if got := ids(p.AllTargets()); !eq(got, "chain-2", "chosen") {
		t.Errorf("after Narrow the plan is %v, want exactly the kept targets in the given "+
			"order", got)
	}
	if p.Primary().ModelID != "chain-2" {
		t.Errorf("Primary() after Narrow = %q — the head must follow the narrowed list, not "+
			"the list it replaced", p.Primary().ModelID)
	}
}

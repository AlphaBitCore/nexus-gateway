package executor

import (
	"time"

	cfgpolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/policy"
	"reflect"
	"testing"

	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// TestWalkState_EveryFactTravelsWithItsTarget is a structural assertion, and it
// is here because the hazard it replaces was invisible at compile time.
//
// The walk used to carry four slices indexed by target position — attempt
// counts, eliminated, restable, explained — plus the targets themselves.
// selectNext took two of them as adjacent `[]bool` parameters, so passing them
// in the wrong order compiled cleanly and silently changed which targets could
// be tried again. An adversarial review found that the guards those slices
// encoded could be removed without a single test noticing.
//
// One struct per target makes the mistake unspeakable: there is no ordering to
// get wrong, and a new fact about a target cannot be added anywhere except
// beside the target it describes.
func TestWalkState_EveryFactTravelsWithItsTarget(t *testing.T) {
	targets := []routingcore.RoutingTarget{
		{ModelID: "a", ProviderID: "p1"},
		{ModelID: "b", ProviderID: "p2"},
	}
	w := newWalk(targets, 2)

	if len(w) != len(targets) {
		t.Fatalf("newWalk produced %d states for %d targets", len(w), len(targets))
	}
	for i := range w {
		if w[i].target.ModelID != targets[i].ModelID {
			t.Errorf("state %d describes %q, target is %q", i, w[i].target.ModelID, targets[i].ModelID)
		}
		if w[i].attempts != 0 || w[i].eliminated || w[i].restable || w[i].explained || w[i].rule != 0 || w[i].waits != 0 || w[i].dispatchesLeft != 2 || w[i].lastFailure != "" {
			t.Errorf("state %d starts dirty: %+v — a fresh plan has nothing tried, nothing ruled out, and each target holding its full allowance", i, w[i])
		}
	}

	// Every field the walk reasons with lives on the target's own state. A
	// field added outside it would reintroduce exactly the parallel-slice
	// hazard this type exists to remove, so the shape is asserted rather than
	// merely intended.
	//
	// The set is exact in both directions on purpose. A field can be added only
	// with an entry here saying what it means, and a field can be REMOVED only
	// deliberately — this list went red when a `list` field, pre-placed for a
	// plan split that had not happened, was deleted. Nothing wrote it and
	// nothing read it, which made the struct claim to record a provenance it
	// did not have.
	want := map[string]bool{
		"target": true, "attempts": true, "eliminated": true,
		"restable": true, "explained": true, "rule": true,
		// waits: how many pauses this target has already served, across every
		// turn — see TestWalkState_ATargetsBackoffScheduleDoesNotRestartWithItsTurn.
		"waits": true,
		// dispatchesLeft: this target's dispatch allowance for the whole
		// request — see TestExecutor_ThePerTargetLimitBoundsTheREQUEST.
		"dispatchesLeft": true,
		// lastFailure: what this target last failed with, readable after the
		// walk has been elsewhere — see
		// TestExecutor_ARetryThatSucceedsALaterTurnNamesTheFailureItRecoveredFrom.
		"lastFailure": true,
	}
	ty := reflect.TypeOf(walkState{})
	for i := range ty.NumField() {
		name := ty.Field(i).Name
		if !want[name] {
			t.Errorf("walkState gained field %q without the test that says what it means; "+
				"if it is per-target state it belongs here, and if it is not it does not "+
				"belong on this type", name)
		}
		delete(want, name)
	}
	for name := range want {
		t.Errorf("walkState lost field %q — the walk kept its state somewhere else again", name)
	}
}

// TestWalkState_ATargetsBackoffScheduleDoesNotRestartWithItsTurn.
//
// The thing being backed off is the TARGET. A walk that comes back to one after
// it has already been made to wait twice inside its first turn must not then
// hand it a shorter pause than the ones it has already outlasted — which is
// what an index counting position-within-a-turn does, because that index starts
// over every time the target is selected again.
//
// Asserted as a monotonic schedule rather than as specific durations: the
// numbers are the policy's, the property is that asking again never goes
// backwards.
func TestWalkState_ATargetsBackoffScheduleDoesNotRestartWithItsTurn(t *testing.T) {
	p := cfgpolicy.RetryPolicy{
		BackoffInitial: 10 * time.Millisecond,
		BackoffMax:     10 * time.Second, // high enough that the clamp is not what makes it monotonic
	}
	w := newWalk([]routingcore.RoutingTarget{{ProviderID: "p", ModelID: "m"}}, 2)

	// Two waits inside the first turn, then the turn ends and the walk comes
	// back — the third wait is the one that used to restart.
	first := w[0].nextBackoff(p)
	second := w[0].nextBackoff(p)
	w[0].attempts++ // the turn ended; the next wait is the way back into a new one
	third := w[0].nextBackoff(p)

	if second <= first {
		t.Fatalf("waits inside one turn did not escalate: %s then %s", first, second)
	}
	if third <= second {
		t.Errorf("the wait on the way back into a turn is %s, no longer than the %s this "+
			"target already served inside its last one — the schedule restarted with the "+
			"turn instead of escalating with the target", third, second)
	}
}

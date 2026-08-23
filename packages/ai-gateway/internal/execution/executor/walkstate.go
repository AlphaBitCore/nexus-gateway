package executor

import (
	"time"

	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	cfgpolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/policy"
)

// walkState is what the dispatch loop knows about each target as it goes.
//
// It replaces four parallel slices indexed by target position plus two loose
// variables. That arrangement had a specific hazard, not just an aesthetic one:
// selectNext took two adjacent same-typed `[]bool` parameters, so passing them
// in the wrong order compiled cleanly and changed which targets could be tried
// again. Every fact about a target now travels with the target.
//
// It carries only facts the loop reads. A field pre-placed for a change that
// has not happened is written by nobody and read by nobody, and it makes the
// struct look like it records something it does not — which is how the parallel
// slices grew in the first place.
type walkState struct {
	target routingcore.RoutingTarget

	// attempts counts the TURNS this target has been given, not the dispatches
	// made in them — one turn runs the per-target retry loop, which may dispatch
	// up to MaxAttemptsPerTarget times. Zero means untried, which is what makes
	// a target eligible for the fresh pass. It counts turns and nothing else:
	// what sizes the pause before the next one is `waits`.
	attempts int

	// eliminated: out for this request. Either the failure was scoped to this
	// target — a window that cannot hold the prompt, an endpoint it does not
	// serve, a permission it lacks — or it failed before dispatching at all,
	// which repeats identically and costs no budget. The second case is why
	// this flag exists: without it a target that spends nothing is selected
	// forever and the budget never advances to end the walk.
	eliminated bool

	// restable: the last failure was one the policy retries, so this target may
	// come round again once everything untried has had a turn. A class the
	// policy excludes from retry must not reach one through resting either.
	restable bool

	// rule is the index of the routing rule this target came from, counted in
	// the order the rules appear in the plan. Zero is the primary rule.
	//
	// It bounds selection: the walk may only choose from the LOWEST rule index
	// that still has a target which has not been ELIMINATED. An attempted
	// target keeps its rule alive — it has to, or a rested retry could never
	// re-enter one. Without the index the plan is a flat list of equals, and a rule an admin wrote as a
	// lower-priority alternative starts serving traffic on the primary rule's
	// first transient failure — which is a different rule's answer arriving
	// under this rule's name on the traffic row.
	rule int

	// dispatchesLeft is how many more times this TARGET may be dispatched to on
	// this REQUEST — the whole request, not one turn.
	//
	// It exists because two mechanisms answer "try this target again": the
	// per-target retry loop inside a turn, and the walk coming back to a rested
	// target later. They answer different questions — a blip wants an immediate
	// retry, a rate limit wants elapsed time — so both are kept. What must NOT be
	// duplicated is the BOUND: with the limit living inside the retry loop, every
	// turn handed the target a fresh allowance, so a plan configured for two
	// attempts per target dispatched four times to one provider and the
	// request-level budget silently outranked the per-target knob.
	//
	// Reaching zero does NOT eliminate the target. "Exhausted" means ruled out,
	// and a target that is merely out of attempts has not been ruled out — the
	// distinction is what keeps the walk from advancing past a rule the admin
	// wrote for this situation while it still had answers.
	dispatchesLeft int

	// lastFailure is the class of this target's most recent failed dispatch,
	// kept beside the target because the walk may come back to it several turns
	// later. A variable scoped to one turn cannot answer "what did this target
	// last fail with" once the walk has been elsewhere — and that is exactly the
	// question the metric emitted on a successful retry has to answer.
	lastFailure cfgpolicy.ErrorClass

	// waits counts how many times this target has been made to pause before a
	// dispatch, across every turn. It is what sizes the next pause, so the
	// schedule escalates with the target rather than restarting with the turn.
	waits int

	// explained: this target's fate is recorded somewhere — an attempt, a skip
	// with a reason. Read only by the positional gap check, which now runs only
	// when selection was positional.
	explained bool
}

// newWalk builds the per-target state for a plan. maxPerTarget is each
// target's dispatch allowance for the whole request.
func newWalk(targets []routingcore.RoutingTarget, maxPerTarget int) []walkState {
	w := make([]walkState, len(targets))
	// Rule indices are assigned by first appearance, not by sorting: the plan
	// already carries the rules in priority order, and re-deriving that order
	// here would be a second answer to which rule outranks which.
	index := map[string]int{}
	next := 0
	for i, t := range targets {
		idx, seen := index[t.RuleID]
		if !seen {
			idx = next
			index[t.RuleID] = idx
			next++
		}
		w[i] = walkState{target: t, rule: idx, dispatchesLeft: maxPerTarget}
	}
	return w
}

// currentRule is the lowest rule index that still has a target which has not
// been ELIMINATED. Rules above it are exhausted; rules below it are not
// reachable yet. Attempted targets still count — a rule whose targets have all
// been tried but not ruled out is still the rule a rested retry belongs to.
//
// "Exhausted" means ELIMINATED, never out of budget. A rule whose targets are
// merely expensive in attempts has not been ruled out, and advancing past it
// because the call budget ran low would hand the request to a rule the admin
// wrote for a different situation while the intended one still had answers.
func currentRule(w []walkState) int {
	best := -1
	for i := range w {
		if w[i].eliminated {
			continue
		}
		if best < 0 || w[i].rule < best {
			best = w[i].rule
		}
	}
	return best
}

// dispatchedBefore reports whether this target has already been dispatched to
// on this request — in an earlier turn or earlier in this one. Derived from the
// allowance rather than tracked separately, so the two cannot disagree.
func (w *walkState) dispatchedBefore() bool { return w.waits > 0 }

// nextBackoff is the pause before this target's next dispatch, and the act of
// asking for it is what advances the schedule. One counter for both places a
// target is made to wait — inside a turn and on the way back into one — so the
// two cannot disagree about how long this target has already been waiting.
func (w *walkState) nextBackoff(p cfgpolicy.RetryPolicy) time.Duration {
	w.waits++
	return computeBackoff(w.waits, p)
}

// ceilingStopEntries is what the chain says when a ceiling — the call budget or
// the walk deadline — ends the walk at the top of the loop.
//
// Two kinds of entry, and the difference between them is the point. tIdx had
// just been SELECTED, so its entry says `stopped` and carries the reason it was
// reached for. Everything else never got a turn, so those say `skipped` and
// carry NO selection reason: a reason computed for a different target puts a
// decision on the record that nobody made about this one, and an operator
// reading the chain cannot tell it from one that was.
//
// The selected target is recorded on its own rather than through the
// never-attempted filter, because on a REVISIT the filter cannot see it — its
// earlier turn already moved the attempt count off zero, so the walk's last
// decision would leave no entry at all.
//
// Marks each recorded target explained, which is the same claim the entry
// makes: this target's fate is on the record.
func ceilingStopEntries(walk []walkState, targets []routingcore.RoutingTarget, tIdx int, selectionReason, reason string) []Attempt {
	out := make([]Attempt, 0, len(targets))
	walk[tIdx].explained = true
	out = append(out, Attempt{
		Target:          targets[tIdx],
		SelectionReason: selectionReason,
		Error:           "stopped: " + reason,
	})
	for i := range targets {
		if i == tIdx || walk[i].eliminated || walk[i].attempts > 0 {
			continue
		}
		walk[i].explained = true
		out = append(out, Attempt{Target: targets[i], Error: "skipped: " + reason})
	}
	return out
}

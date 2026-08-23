// select_next.go — the ORDER: which target the walk tries next after a
// failure, and the reason recorded for that choice. classify.go holds the class
// itself and the predicates that say what a class means for one target
// (aborts, eliminates, charges the credential); this file is the only place
// that decides where the walk goes.
package executor

// selectNext picks the target to try after a failure, and it is the whole
// reason the classes in classify.go are finer than the treatments they map to.
//
// Position is the wrong answer for two of them. A list ordered by price puts
// the next-cheapest model next: after a context overflow that model's window is
// as likely to be smaller as larger, so a walk can overflow several times in a
// row; and after a rate limit it is very often on the SAME provider, whose
// quota is exactly what was just exhausted, because a plan built from one
// provider's catalogue lists that provider's models together.
//
// For everything else position IS the answer — it is the order the strategy
// stated, and nothing about the failure argues with it.
//
// A target that failed transiently is DEPRIORITISED, not finished: it sorts
// behind everything untried and comes round again while it has dispatches left.
// That allowance is per-target and per-REQUEST, so a target coming round cannot
// spend what was allotted to another one — which is why the return needs no
// opt-in flag guarding it. The flag it replaces existed to stop a survivor
// absorbing an eliminated target's slack, and a per-target allowance makes that
// impossible by construction. What that
// buys is elapsed time. Same-target retries happen in milliseconds; coming back
// after trying two other providers means several upstream round-trips have
// passed, which is exactly what a rate limit needs and what an in-place retry
// cannot give it. A walk that tried each target once would stop after two
// attempts on a two-entry plan however much budget was configured.
//
// It decides ORDER only, with one exception it cannot delegate: a target with
// no dispatch allowance left is not a candidate, because returning it would put
// the walk in a loop that spends nothing and therefore never ends. Everything
// else about whether a target is still allowed — its provider
// eliminated, the budget spent — is the walk's business, because the walk is
// what records why a target was passed over. Filtering here instead would make
// those targets vanish from the trace rather than appear in it with a reason.
//
// Returns -1 when nothing is left to try.
func selectNext(w []walkState, last errClass, lastProviderID string) (int, string) {
	// Selection never crosses a rule boundary while the current rule still has
	// a target that has not been ruled out. Every clause below — largest
	// window, different provider, next in list, the resting pass — is scoped to
	// the current rule by this predicate, so none of them has to remember the
	// boundary and none of them can forget it.
	rule := currentRule(w)
	inRule := func(i int) bool { return w[i].rule == rule }
	fresh := func(i int) bool { return !w[i].eliminated && w[i].attempts == 0 && inRule(i) }

	switch last {
	case classContextOverflow:
		// The one dimension that just failed is the one to optimise. A
		// declared window of zero is unknown, not small: it sorts behind
		// every model that states a size, but ahead of nothing.
		best := -1
		for i := range w {
			if !fresh(i) {
				continue
			}
			if best < 0 || w[i].target.MaxContextTokens > w[best].target.MaxContextTokens {
				best = i
			}
		}
		if best >= 0 {
			return best, "largest-window"
		}

	case classRate429, class5xx, classTimeout, classNetwork, classUnclassified:
		// Prefer a different provider. A quota is per-provider, and so is an
		// outage; the sibling model next in the list shares both.
		for i := range w {
			if fresh(i) && w[i].target.ProviderID != lastProviderID {
				return i, "different-provider"
			}
		}
	}

	for i := range w {
		if fresh(i) {
			return i, "next-in-list"
		}
	}

	// Come round to whichever rested target has been left longest — fewest
	// attempts first, then list order — so a second pass spreads over the plan
	// instead of hammering its head.
	//
	// Eliminated targets are excluded, and that covers both the failures scoped
	// to a target and every failure that happened BEFORE dispatch: a target
	// with no credential, no adapter, or a body the bridge cannot translate is
	// not tired but broken, and breaks identically every time. Resting one of
	// those is a loop that spends no budget and so never ends — not
	// hypothetical, it is what happened the first time this pass was written.
	//
	// And the failure must be one the policy retries. RetryOn is the admin
	// saying which failures deserve another attempt; a class excluded there
	// must not come back through this door.
	best := -1
	for i := range w {
		// dispatchesLeft is checked here and not folded into `eliminated`,
		// because they mean different things: a target out of attempts is not
		// ruled out, and marking it so would let the walk advance past its rule.
		if w[i].eliminated || !w[i].restable || !inRule(i) || w[i].dispatchesLeft <= 0 {
			continue
		}
		if best < 0 || w[i].attempts < w[best].attempts {
			best = i
		}
	}
	if best >= 0 {
		return best, "deprioritised-retry"
	}
	return -1, ""
}

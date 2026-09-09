package pipeline

import (
	"testing"
	"time"
)

// fakeClock drives the window on a stated schedule. A real clock would make
// every threshold below a race against the machine's load, and the numbers are
// the entire point of this type.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }

func newTestHealth() (*hookHealth, *fakeClock) {
	c := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	h := newHookHealth()
	h.now = c.now
	return h, c
}

// feed records n executions, the first `fails` of them as failures, advancing
// the clock a little between each so they do not all land in one bucket.
func feed(h *hookHealth, c *fakeClock, impl string, n, fails int) {
	for i := range n {
		h.Record(impl, i < fails, nil)
		c.add(10 * time.Millisecond)
	}
}

// feedMix records n executions with the failures SPREAD OUT — one failure per
// `every` executions.
//
// The distinction matters and cost a test. The window is evaluated on every
// execution against the trailing samples, not once at the end against the batch
// average, so front-loading the failures produces a genuine 100% trailing rate
// partway through. Both readings are legitimate questions; this helper asks the
// one about a steady low rate.
func feedMix(h *hookHealth, c *fakeClock, impl string, n, every int) {
	for i := range n {
		h.Record(impl, every > 0 && i%every == 0, nil)
		c.add(10 * time.Millisecond)
	}
}

// The floor exists so a cold start cannot trip. One failed request is a rate of
// 100% over one sample, which says nothing at all — and it is exactly the shape
// of the first request after a deploy.
func TestHookHealthDoesNotTripBelowTheSampleFloor(t *testing.T) {
	h, c := newTestHealth()
	// A LITERAL count, not degradeMinSamples-1. Deriving the input from the
	// constant under test makes the test move with it: lowering the floor to 1
	// also lowers this to zero executions, and the assertion passes by doing
	// nothing. Caught by mutation — the first version of this test could not see
	// the floor being removed at all.
	const handful = 5
	feed(h, c, "pii-detector", handful, handful)
	if h.Degraded("pii-detector") {
		t.Errorf("tripped on %d consecutive failures — a rate over a handful of samples "+
			"means nothing, and the first request after a deploy has exactly this shape",
			handful)
	}
	if degradeMinSamples <= handful {
		t.Errorf("degradeMinSamples is %d, at or below the %d this test feeds — the floor "+
			"has been lowered past what this gate can see", degradeMinSamples, handful)
	}
}

// Half the traffic failing is the line between "intermittent" and "not working".
func TestHookHealthTripsAtHalfAndNotBelow(t *testing.T) {
	t.Run("a steady rate below the line stays clean", func(t *testing.T) {
		h, c := newTestHealth()
		// One failure in every three, spread out: 33%, and no trailing prefix
		// ever reaches half.
		feedMix(h, c, "pii", 60, 3)
		if h.Degraded("pii") {
			t.Error("tripped at a steady 33% — below the line the per-execution fail posture " +
				"is the right handling and this must stay quiet")
		}
	})

	t.Run("just under the trip rate stays clean", func(t *testing.T) {
		h, c := newTestHealth()
		// A steady 45%: above the clear rate, below the trip rate, and — the part
		// that takes care — arranged so no TRAILING PREFIX reaches half either,
		// since the window is evaluated on every execution. Two successes first,
		// then strict alternation: every prefix of S,S,(F,S)* is under 50% and
		// the whole run is 9/20.
		//
		// Without this arm the trip rate was bracketed from below only, and
		// lowering it from 0.50 to 0.40 left the entire package green.
		for range 2 {
			h.Record("pii", false, nil)
			c.add(10 * time.Millisecond)
		}
		for i := range 18 {
			h.Record("pii", i%2 == 0, nil)
			c.add(10 * time.Millisecond)
		}
		if h.Degraded("pii") {
			t.Errorf("tripped at a steady 45%%, under the %.0f%% threshold",
				degradeTripRate*100)
		}
	})

	t.Run("at the trip rate it trips", func(t *testing.T) {
		h, c := newTestHealth()
		// One failure in every two, spread out: exactly 50%.
		feedMix(h, c, "pii", 40, 2)
		if !h.Degraded("pii") {
			t.Error("did not trip at exactly 50%, the documented threshold")
		}
	})
}

// The window is trailing, not a batch average, and the difference is the point
// of having a window at all.
//
// A hook that fails twenty times in a row and then recovers has a batch average
// well under the line, and it WAS broken — for those twenty requests, which went
// out unguarded. Averaging that away would make the detector blind to exactly
// the burst it exists to catch, so it trips on the burst and then clears through
// the hysteresis rule like anything else.
func TestHookHealthTripsOnABurstEvenWhenTheBatchAverageIsLow(t *testing.T) {
	h, c := newTestHealth()
	feed(h, c, "pii", degradeMinSamples, degradeMinSamples) // the burst
	if !h.Degraded("pii") {
		t.Fatal("a run of failures long enough to clear the sample floor did not trip")
	}
	feed(h, c, "pii", 80, 0) // recovery traffic; batch average is now 20%
	if !h.Degraded("pii") {
		t.Error("cleared without a full window in the new state — the hysteresis rule is " +
			"what keeps the signal from flapping at the boundary")
	}
}

// One threshold in both directions oscillates at the boundary and turns the
// signal into noise. Recovery is therefore a LOWER rate held for a full window.
func TestHookHealthClearsOnlyWithHysteresisAndAFullWindow(t *testing.T) {
	h, c := newTestHealth()
	feed(h, c, "pii", 30, 30)
	if !h.Degraded("pii") {
		t.Fatal("setup: did not trip")
	}

	// Clean traffic, but still inside the window it tripped in: the recent
	// failures are still in the buckets and the state must hold.
	feed(h, c, "pii", 20, 0)
	if !h.Degraded("pii") {
		t.Error("cleared while the failures that tripped it were still inside the window")
	}

	// Recovery traffic that is BETTER but not good enough: below the trip rate
	// so it does not re-trip, above the clear rate so it must not clear either.
	// Without this arm the hysteresis is untested — 0% recovery traffic clears
	// under any clear rate at all, which is how the first version of this test
	// stayed green when the clear rate was mutated up to 0.99.
	c.add(degradeWindow)
	feedMix(h, c, "pii", 60, 3) // a steady 33%: above 25%, below 50%
	if !h.Degraded("pii") {
		t.Errorf("cleared at a steady 33%%, which is above the %.0f%% clear rate — one "+
			"threshold in both directions makes the signal flap at the boundary",
			degradeClearRate*100)
	}

	// Past a full window of clean traffic, it recovers.
	c.add(degradeWindow)
	feed(h, c, "pii", degradeMinSamples+5, 0)
	if h.Degraded("pii") {
		t.Error("stayed degraded after a full window of clean traffic")
	}
}

// feedQuiet records n executions at the SAME instant, so the caller controls
// exactly how much time passes. feed advances the clock itself, which is fine
// when only the sample count matters and useless when the question is how long
// a state was held.
func feedQuiet(h *hookHealth, impl string, n int, failed bool) {
	for range n {
		h.Record(impl, failed, nil)
	}
}

// The dwell term, isolated and bracketed from both sides.
//
// The other hysteresis test cannot see it: it lets the failures age out of the
// window, so the RATE term alone explains the delay and shortening the dwell
// changes nothing. Mutation caught that — halving the dwell left the whole
// package green.
//
// Here the rate term is satisfied almost immediately (a burst of failures
// drowned by clean traffic in the same window), so from the second bucket
// onward the ONLY thing still holding the state degraded is the dwell.
func TestHookHealthHoldsTheDegradedStateForAFullWindow(t *testing.T) {
	h, c := newTestHealth()

	feedQuiet(h, "pii", degradeMinSamples+5, true)
	if !h.Degraded("pii") {
		t.Fatal("setup: did not trip")
	}
	trippedAt := c.t

	for range 4 * degradeBuckets {
		// Enough clean traffic that the failure rate is far under the clear
		// threshold from the very next evaluation.
		feedQuiet(h, "pii", 60, false)
		if !h.Degraded("pii") {
			break
		}
		c.add(degradeBucketDur)
	}
	held := c.t.Sub(trippedAt)

	if h.Degraded("pii") {
		t.Fatalf("still degraded after %v of clean traffic — it never clears", held)
	}
	if held < time.Duration(degradeBuckets-1)*degradeBucketDur {
		t.Errorf("cleared after %v; the contract is a full %v window in the new state, and a "+
			"shorter dwell is the boundary flapping the hysteresis exists to stop",
			held, degradeWindow)
	}
	if held > 2*degradeWindow {
		t.Errorf("took %v to clear under entirely clean traffic, far past the %v window — a "+
			"dwell that long makes the signal stale rather than stable", held, degradeWindow)
	}
}

// A window per implementation. One broken hook must not accuse the hook next to
// it, which is the whole reason the tag names an implementation.
func TestHookHealthIsPerImplementation(t *testing.T) {
	h, c := newTestHealth()
	feed(h, c, "broken", 30, 30)
	feed(h, c, "healthy", 30, 0)

	if !h.Degraded("broken") {
		t.Error("the failing hook is not marked degraded")
	}
	if h.Degraded("healthy") {
		t.Error("a healthy hook was marked degraded — the windows are shared")
	}
}

// The counted event is "the hook did not produce a verdict". A hook that REJECTS
// every request is a policy doing its job, and counting that as a failure would
// light the alarm exactly when compliance is working.
func TestHookHealthCountsFailuresNotVerdicts(t *testing.T) {
	h, c := newTestHealth()
	// Every execution succeeded; what they DECIDED is not this type's business,
	// and Record is never told.
	feed(h, c, "blocker", 50, 0)
	if h.Degraded("blocker") {
		t.Error("a hook that returned a verdict on every request was marked degraded")
	}
}

// A window whose buckets have all aged out must not carry its old counts
// forward: a hook that failed hard three seconds ago and has been idle since is
// not currently failing.
func TestHookHealthForgetsWhatFellOutOfTheWindow(t *testing.T) {
	h, c := newTestHealth()
	feed(h, c, "pii", 30, 30)
	if !h.Degraded("pii") {
		t.Fatal("setup: did not trip")
	}
	// Jump well past the window, then send clean traffic. Nothing that failed is
	// still inside it.
	c.add(10 * degradeWindow)
	feed(h, c, "pii", degradeMinSamples+5, 0)
	if h.Degraded("pii") {
		t.Error("a failure older than the window still holds the state degraded")
	}
}

// A nil tracker is the state a Pipeline built outside a PolicyResolver is in
// (several tests, and the compliance-proxy's direct construction). It must be
// inert rather than a panic on the request path.
func TestHookHealthNilIsInert(t *testing.T) {
	var h *hookHealth
	h.Record("pii", true, nil)
	if h.Degraded("pii") {
		t.Error("a nil tracker reported a hook degraded")
	}
}

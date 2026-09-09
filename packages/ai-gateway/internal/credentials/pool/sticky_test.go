package credpool

import (
	"fmt"
	"math"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/credstate"
)

// Sticky selection as `hash(stickyKey) % len(eligible)` satisfies what the existing
// suite asserts — determinism,
// order-independence, exclusion of OPEN entries — which is why that suite stays
// green while both defects ship. These arms cover the two properties it does
// NOT have.

func stickyKeys(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("vk-%06d", i)
	}
	return out
}

func countPicks(t *testing.T, pool []Entry, keys []string) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for _, k := range keys {
		got := Select(append([]Entry(nil), pool...), k)
		if got == nil {
			t.Fatalf("Select returned nil for %q with a non-empty eligible pool", k)
		}
		counts[got.ID]++
	}
	return counts
}

// idFamilies are the credential-id shapes this has to work for. The
// same-prefix family is not hypothetical: it is what exposed the hash defect.
// Under that spelling, scoring ids that share a prefix and differ near the end collapses the
// distribution completely — the weight-8 entry takes 100% of keys and a
// half-open probe is never probed — because FNV-1a's high bits barely move.
// Production ids are UUIDs, which hides it; asserting over one hand-picked
// family is how a suite certifies a property the implementation lacks.
var idFamilies = map[string][]string{
	"same-prefix": {"cred-1", "cred-2", "cred-3", "cred-4", "cred-5"},
	"uuid": {
		"7f3a91c2-0000-4000-8000-000000000001",
		"b0075000-0000-4000-8000-0000000000a0",
		"e65623cf-60e8-489f-a91f-6e4869216d15",
		"f2b78c3c-fd1d-4297-916c-6ae75418c7f7",
		"3f45ca57-c842-4e40-a901-4d607f3fe064",
	},
	"ulid-shaped": {
		"01J8Z4KQ2P0000000000000A1",
		"01J8Z4KQ2P0000000000000A2",
		"01J8Z4KQ2P0000000000000A3",
		"01J8Z4KQ2P0000000000000A4",
		"01J8Z4KQ2P0000000000000A5",
	},
}

// TestSticky_ShareFollowsWeight: an operator's selectionWeight has to mean the
// same thing on the sticky path as on the weighted-random one. Under the modulo
// it meant nothing at all — every eligible credential took 1/N.
//
// The tolerance is RELATIVE to each entry's analytic share, so a light entry is
// held to the same standard as a heavy one. A one-sided cap generous enough for
// the smallest share would admit a badly wrong distribution for the largest.
func TestSticky_ShareFollowsWeight(t *testing.T) {
	weights := []int{8, 1, 3}
	const total = 8 + 1 + 3

	for family, ids := range idFamilies {
		t.Run(family, func(t *testing.T) {
			pool := make([]Entry, len(weights))
			for i, w := range weights {
				pool[i] = Entry{ID: ids[i], Weight: w}
			}
			keys := stickyKeys(24000)
			counts := countPicks(t, pool, keys)

			for i, w := range weights {
				want := float64(w) / total
				got := float64(counts[ids[i]]) / float64(len(keys))
				if rel := (got - want) / want; rel < -0.25 || rel > 0.25 {
					t.Errorf("%s (weight %d) took %.4f of sticky keys, want %.4f ±25%%; full distribution %v",
						ids[i], w, got, want, counts)
				}
			}
		})
	}
}

// Equal weights must produce equal shares. Nothing asserted this before, and
// the pool used by the disruption test below was in fact splitting
// 25/12.5/12.5/25/25 across five equal entries while every test passed.
func TestSticky_EqualWeightsShareEqually(t *testing.T) {
	for family, ids := range idFamilies {
		t.Run(family, func(t *testing.T) {
			pool := make([]Entry, len(ids))
			for i, id := range ids {
				pool[i] = Entry{ID: id, Weight: 5}
			}
			keys := stickyKeys(30000)
			counts := countPicks(t, pool, keys)

			want := 1.0 / float64(len(ids))
			for _, id := range ids {
				got := float64(counts[id]) / float64(len(keys))
				if rel := (got - want) / want; rel < -0.15 || rel > 0.15 {
					t.Errorf("%s took %.4f of sticky keys, want %.4f ±15%% — equal weights are not sharing equally; full distribution %v",
						id, got, want, counts)
				}
			}
		})
	}
}

// TestSticky_HalfOpenProbeGetsATrickle is the case that matters operationally.
// filterEligible clamps a HALF_OPEN entry to weight 1 precisely so it is
// re-tested gently; the modulo handed it a full 1/N of the sticky traffic, so
// the probe became a load test of a credential that had just been failing.
//
// Effective weights are 10/10/1, so the analytic share is 1/21 = 0.0476. The
// band is two-sided and tight around that: the first version capped at 0.12,
// which passed at a MEASURED 0.089 — 1.9x the target, and an artefact of the
// hash rather than sampling noise — without anyone noticing.
func TestSticky_HalfOpenProbeGetsATrickle(t *testing.T) {
	const want = 1.0 / 21.0

	for family, ids := range idFamilies {
		t.Run(family, func(t *testing.T) {
			pool := []Entry{
				{ID: ids[0], Weight: 10},
				{ID: ids[1], Weight: 10},
				{ID: ids[2], Weight: 10, Circuit: credstate.CircuitHalfOpen},
			}
			keys := stickyKeys(24000)
			counts := countPicks(t, pool, keys)

			got := float64(counts[ids[2]]) / float64(len(keys))
			if rel := (got - want) / want; rel < -0.30 || rel > 0.30 {
				t.Errorf("the half-open probe took %.4f of sticky traffic, want %.4f ±30%% — it is clamped to weight 1, so it should be a trickle, not a share; full distribution %v",
					got, want, counts)
			}
			if counts[ids[2]] == 0 {
				t.Error("the half-open probe received nothing — it must still be probed, or the circuit can never close")
			}
		})
	}
}

// TestSticky_OneCredentialLeavingMovesOnlyItsOwnKeys: stickiness exists to
// maximise provider-side prompt-cache reuse. Under `% len(eligible)`, one
// credential opening its circuit changed the divisor and so remapped nearly
// every VK — discarding the cache fleet-wide during an incident, which is
// exactly when the extra upstream cost lands.
func TestSticky_OneCredentialLeavingMovesOnlyItsOwnKeys(t *testing.T) {
	before := []Entry{
		{ID: "cred-1", Weight: 5},
		{ID: "cred-2", Weight: 5},
		{ID: "cred-3", Weight: 5},
		{ID: "cred-4", Weight: 5},
		{ID: "cred-5", Weight: 5},
	}
	// cred-3's circuit opens. Nothing else about the pool changes.
	after := append([]Entry(nil), before...)
	after[2].Circuit = credstate.CircuitOpen

	keys := stickyKeys(6000)
	var wasOnThree, movedOffThree, movedAnyway int
	for _, k := range keys {
		b := Select(append([]Entry(nil), before...), k)
		a := Select(append([]Entry(nil), after...), k)
		if b == nil || a == nil {
			t.Fatalf("Select nil for %q", k)
		}
		switch {
		case b.ID == "cred-3":
			wasOnThree++
			if a.ID != b.ID {
				movedOffThree++
			}
		case a.ID != b.ID:
			movedAnyway++
		}
	}

	if wasOnThree == 0 {
		t.Fatal("no key was pinned to cred-3 — this test cannot observe what it claims to")
	}
	if movedOffThree != wasOnThree {
		t.Errorf("%d of %d keys pinned to cred-3 moved; all of them must", movedOffThree, wasOnThree)
	}
	// The property. Under the modulo this was ~80% of the remaining keys.
	if movedAnyway != 0 {
		t.Errorf("%d keys that were NOT on cred-3 were remapped by its removal (out of %d) — one circuit flip is reshuffling the fleet and discarding everyone's prompt cache",
			movedAnyway, len(keys)-wasOnThree)
	}
}

// A weight-0 entry is dropped by filterEligible, but rendezvousScore is also
// total on its own terms rather than trusting that invariant from a caller.
func TestRendezvousScore_NonPositiveWeightNeverWins(t *testing.T) {
	for _, w := range []int{0, -1} {
		if got := rendezvousScore("vk-1", "cred-x", w); !isNegInf(got) {
			t.Errorf("rendezvousScore(weight=%d) = %v, want -Inf so it can never be the maximum", w, got)
		}
	}
}

func isNegInf(f float64) bool { return f < 0 && f*2 == f }

// TestRendezvousScore_IsFiniteAndPositiveForEveryHash pins the (0,1) bound the
// score depends on.
//
// The first draft mapped the hash with `(float64(Sum64()) + 0.5) / 2^64`, which
// does not stay below 1: float64(uint64) rounds to nearest and the ULP at 2^64
// is 4096, so the top 1024 hash values land on exactly 2^64 and the +0.5
// disappears. math.Log(1) is +0 and -w/+0 is -Inf — the candidate loses every
// key. Benign, but one sign away from a credential that WINS every key, and not
// what the arithmetic claims. A review measured it; this arm keeps it measured.
func TestRendezvousScore_IsFiniteAndPositiveForEveryHash(t *testing.T) {
	for i := range 200000 {
		id := fmt.Sprintf("cred-%d", i)
		s := rendezvousScore("vk-bound-probe", id, 100)
		if math.IsInf(s, 0) || math.IsNaN(s) {
			t.Fatalf("rendezvousScore(%q) = %v — the hash mapped outside (0,1)", id, s)
		}
		if s <= 0 {
			t.Fatalf("rendezvousScore(%q) = %v, want > 0", id, s)
		}
	}
}

// The bound must hold at the arithmetic extremes too, not only at the hashes
// this keyspace happens to produce. These are the values a review's probe
// identified as the ones the first draft mapped to exactly 1.0.
func TestRendezvousScore_BoundHoldsAtTheHashExtremes(t *testing.T) {
	const mantissaBits = 52
	const denom = float64(uint64(1) << mantissaBits)
	for _, h := range []uint64{0, 1, 1 << 63, math.MaxUint64 - 1024, math.MaxUint64} {
		u := (float64(h>>(64-mantissaBits)) + 0.5) / denom
		if !(u > 0 && u < 1) {
			t.Errorf("hash %d maps to u=%v, want strictly inside (0,1)", h, u)
		}
		if score := -100.0 / math.Log(u); math.IsInf(score, 0) || math.IsNaN(score) || score <= 0 {
			t.Errorf("hash %d yields score %v, want a positive finite value", h, score)
		}
	}
}

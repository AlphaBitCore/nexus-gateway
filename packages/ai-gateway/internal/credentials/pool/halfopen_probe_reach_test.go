package credpool

import (
	"fmt"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/credstate"
)

// prodShapedPool is the shape a real deployment has: two healthy credentials at
// the DB default selectionWeight of 100 (providers.prisma @default(100)), and
// one that tripped on a 429 and has been auto-promoted to HALF_OPEN.
//
// The default matters. The existing sibling test builds its pool at weight 10,
// which asserts a 1/21 probe share where production has 1/201.
func prodShapedPool() []Entry {
	return []Entry{
		{ID: "cred-aaaaaaaa-1111-4000-8000-000000000001", Weight: 100},
		{ID: "cred-bbbbbbbb-2222-4000-8000-000000000002", Weight: 100},
		{ID: "cred-cccccccc-3333-4000-8000-000000000003", Weight: 100, Circuit: credstate.CircuitHalfOpen},
	}
}

// vkKeys returns n virtual-key-shaped sticky keys. The sticky key is a VK id
// (stage_admission.go), and a single-tenant fleet has single digits of them —
// which is the whole point: a share expressed per KEY quantises to zero at this
// scale, while a share expressed per REQUEST does not.
func vkKeys(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("vk-%08x-4000-8000-%012d", i*2654435761, i)
	}
	return out
}

// TestSelect_HalfOpenProbeIsReachedByASmallFleet pins the recovery property.
//
// The only exit from HALF_OPEN is a 2xx recorded against that credential:
// there is no background prober, no TTL on the circuit hash, and the Hub's
// circuit-flush job only touches DB rows with no live Redis circuit. A probe
// that receives nothing therefore stays HALF_OPEN forever and the pool
// permanently loses it, which an operator can only undo by hand.
func TestSelect_HalfOpenProbeIsReachedByASmallFleet(t *testing.T) {
	pool := prodShapedPool()
	keys := vkKeys(10)
	probeID := pool[2].ID

	probeHits := 0
	const rounds = 4000 // ~20 expected probe hits at the 1/201 share
	for i := range rounds {
		got := Select(pool, keys[i%len(keys)])
		if got == nil {
			t.Fatal("Select returned nil with three eligible credentials")
		}
		if got.ID == probeID {
			probeHits++
		}
	}

	if probeHits == 0 {
		t.Fatalf("the HALF_OPEN probe received nothing across %d sticky requests over %d virtual keys — "+
			"its circuit can never close and the credential is lost until an operator resets it by hand",
			rounds, len(keys))
	}
}

// TestSelect_ProbeShareMatchesTheNonStickyPath keeps the weight knob meaning one
// thing. The clamp to 1 is a statement about how much traffic a probe should
// take; if the sticky path gave it a different share than weightedRandom does,
// the same knob would mean two things depending on whether a caller is pinned —
// which is the defect stickyPick was written to remove, in a new place.
func TestSelect_ProbeShareMatchesTheNonStickyPath(t *testing.T) {
	pool := prodShapedPool()
	pool[2].Weight = 100 // the caller's weight; filterEligible clamps it to 1
	keys := vkKeys(8)
	probeID := pool[2].ID

	const rounds = 200000
	sticky, nonSticky := 0, 0
	for i := range rounds {
		if got := Select(pool, keys[i%len(keys)]); got.ID == probeID {
			sticky++
		}
		if got := Select(pool, ""); got.ID == probeID {
			nonSticky++
		}
	}

	// Expected share is 1/(100+100+1). Both paths draw per request, so they
	// agree to within sampling noise; a factor-of-two band is loose enough not
	// to flake and tight enough to catch "one path divides keys instead".
	want := float64(rounds) / 201.0
	for _, c := range []struct {
		name string
		got  int
	}{{"sticky", sticky}, {"non-sticky", nonSticky}} {
		if float64(c.got) < want/2 || float64(c.got) > want*2 {
			t.Errorf("%s path gave the probe %d of %d selections; expected about %.0f",
				c.name, c.got, rounds, want)
		}
	}
}

// TestSelect_StickinessHoldsAmongHealthyCredentials is the other half: the
// probe carve-out must not cost stickiness for the traffic that is not probing.
// Prompt-cache reuse is the entire reason the sticky path exists.
func TestSelect_StickinessHoldsAmongHealthyCredentials(t *testing.T) {
	healthy := prodShapedPool()[:2] // no probe in the pool at all
	for _, k := range vkKeys(25) {
		first := Select(healthy, k)
		for range 50 {
			if got := Select(healthy, k); got.ID != first.ID {
				t.Fatalf("key %s moved from %s to %s with no circuit change", k, first.ID, got.ID)
			}
		}
	}
}

// TestSelect_ProbeJoiningDoesNotRemapTheSteadyKeys pins the improvement the
// split buys. Under one rendezvous over all eligible entries, a probe entering
// or leaving changes the scores every key is compared against; splitting the
// steady credentials off means a probe's arrival cannot move traffic that is
// not going to it.
func TestSelect_ProbeJoiningDoesNotRemapTheSteadyKeys(t *testing.T) {
	healthy := prodShapedPool()[:2]
	withProbe := prodShapedPool()
	probeID := withProbe[2].ID

	for _, k := range vkKeys(25) {
		before := Select(healthy, k).ID
		// Sample enough draws to get past the probe's own small share.
		for range 40 {
			got := Select(withProbe, k).ID
			if got == probeID {
				continue // this draw went to the probe, which is intended
			}
			if got != before {
				t.Fatalf("key %s was pinned to %s and a HALF_OPEN probe joining moved it to %s",
					k, before, got)
			}
		}
	}
}

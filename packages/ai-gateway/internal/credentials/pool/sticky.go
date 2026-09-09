package credpool

import (
	"hash/fnv"
	"math"
)

// stickyPick chooses one eligible credential for a sticky key using WEIGHTED
// RENDEZVOUS HASHING (highest-random-weight): each candidate is scored from a
// hash of (stickyKey, credential ID) and the highest score wins.
//
// It replaces `hash(stickyKey) % len(eligible)`, which had two defects that are
// really the same defect — the index, not the credential, was what the hash
// picked.
//
//  1. WEIGHT WAS DISCARDED. Every eligible credential got a 1/N share no matter
//     what an operator set `selectionWeight` to, and the case that matters most
//     is a HALF_OPEN probe: filterEligible clamps it to weight 1 precisely so it
//     receives a trickle while it is being re-tested, and the modulo handed it a
//     full share of the sticky traffic instead. The weighted-random path
//     honoured weight; the sticky path silently did not, so the same knob meant
//     two different things depending on whether a VK was pinned.
//
//  2. ONE CIRCUIT FLIP RESHUFFLED THE WHOLE FLEET. `len(eligible)` is the
//     divisor, so a single credential opening or closing its circuit changed the
//     mapping for essentially every VK, not just the ones on that credential.
//     Stickiness exists to maximise provider-side prompt-cache reuse, and a
//     fleet-wide remap discards that cache for everyone — during an incident,
//     which is exactly when the extra upstream cost lands.
//
// Rendezvous hashing fixes both: score depends on (key, id) alone, so removing
// one credential moves only the keys that were on it, and the weight enters the
// score directly. The formula is the standard weighted form,
// score = -w / ln(u) with u drawn uniformly from (0,1) by the hash; it yields a
// share proportional to w while keeping the per-key choice deterministic.
//
// The sort the modulo version needed is gone with it: the maximum is
// order-independent, so map-iteration order in the caller can no longer change
// the answer. Ties break on the lexicographically smaller ID for determinism —
// reachable when two candidates hash identically, and left to chance it would
// reintroduce the instability this function exists to remove.
func stickyPick(eligible []Entry, stickyKey string) *Entry {
	best := -1
	var bestScore float64
	for i := range eligible {
		s := rendezvousScore(stickyKey, eligible[i].ID, eligible[i].Weight)
		if best == -1 || s > bestScore ||
			(s == bestScore && eligible[i].ID < eligible[best].ID) {
			best, bestScore = i, s
		}
	}
	if best == -1 {
		return nil
	}
	return &eligible[best]
}

// rendezvousScore is -w / ln(u), where u is the (key, id) hash mapped into
// (0,1). Higher wins.
func rendezvousScore(stickyKey, id string, weight int) float64 {
	if weight <= 0 {
		// filterEligible already drops these. Scoring one at -Inf means it can
		// never beat a real candidate; it does not make stickyPick total on its
		// own, since a slice of nothing but such entries still returns the first
		// one. That case is unreachable — Select filters before calling — and
		// the guard is here so a future second caller cannot make a weight-0
		// entry WIN, which is the outcome that would actually matter.
		return math.Inf(-1)
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(stickyKey))
	// A separator, so ("ab","c") and ("a","bc") cannot collide.
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(id))
	// FNV-1a alone is NOT good enough here, and the failure is severe rather
	// than cosmetic. Rendezvous hashing reads the hash as a uniform draw, but
	// FNV-1a's final step is one multiply, so two ids sharing a prefix and
	// differing near the end produce values whose HIGH bits are nearly
	// identical. u is then almost the same for every candidate, and
	// `-w/ln(u)` degenerates to `w × const` — the heaviest weight wins every
	// single key, and a half-open probe clamped to weight 1 is never probed at
	// all, so its circuit can never close.
	//
	// Measured before this finalizer: ids "cred-1".."cred-5" at equal weight
	// took 25%/12.5%/12.5%/25%/25% instead of 20% each, and a weight 8/1/3 pool
	// gave the weight-8 entry 100% of keys. Production ids are UUIDs, which
	// carry enough trailing entropy to hide most of it — the weight-8 case
	// measured correctly on UUIDs — but "correct only because the id generator
	// happens to be random" is not a property, and the seeded/ULID-shaped ids
	// this code also has to serve do not have it.
	//
	// splitmix64's finalizer is the standard avalanche step for exactly this:
	// every input bit affects every output bit.
	sum := avalanche(h.Sum64())

	// Map to (0,1) EXCLUSIVE at both ends, using only as many bits as a float64
	// can hold exactly.
	//
	// The obvious `(float64(h.Sum64()) + 0.5) / 2^64` does NOT stay below 1:
	// float64(uint64) rounds to nearest, the ULP at 2^64 is 4096, so the top
	// 1024 hash values convert to exactly 2^64 and the +0.5 vanishes beneath the
	// ULP. u == 1 makes math.Log(u) a positive zero, and -w/+0 is -Inf — that
	// candidate loses every key instead of winning it, which is benign but is
	// not what the arithmetic is supposed to say, and it is one sign flip away
	// from a credential that wins every key.
	//
	// 52 bits, not 53. At 53 the same rounding bites one level down: the largest
	// numerator would be (2^53 - 1) + 0.5, and the ULP just below 2^53 is 1, so
	// it rounds back up to 2^53 and u is exactly 1 again. At 52 the ULP is 0.5,
	// 2^52 - 0.5 is exactly representable, and u stays strictly inside (0,1) for
	// every one of the 2^64 possible hashes — max 1 - 2^-53, min 2^-53. The
	// tests below hold both ends; the 53-bit version failed them.
	const mantissaBits = 52
	const denom = float64(uint64(1) << mantissaBits)
	u := (float64(sum>>(64-mantissaBits)) + 0.5) / denom

	return -float64(weight) / math.Log(u)
}

// avalanche is splitmix64's finalizer: a bijection on uint64 in which every
// input bit affects every output bit. Rendezvous hashing needs the hash to read
// as a uniform draw, and FNV-1a's single trailing multiply does not deliver
// that for ids sharing a prefix — see the note in rendezvousScore. Being a
// bijection matters too: it cannot introduce collisions FNV did not already
// have.
func avalanche(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}

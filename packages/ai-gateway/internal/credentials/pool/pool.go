// Package credpool implements credential pool selection for multi-credential
// providers. It combines weighted random selection with per-VK stickiness
// (consistent hashing) and circuit breaker awareness so that:
//
//   - A given virtual key resolves to the same upstream credential (maximizing
//     provider-side prompt cache hits) — with one bounded exception: while a
//     HALF_OPEN credential is in the pool, its weight share of requests is drawn
//     per request so the probe is actually exercised. See stickyPickWithProbe.
//   - Credentials whose circuit is OPEN are excluded from selection.
//   - HALF_OPEN credentials receive a single probe slot (weight = 1).
//   - Weighted random selection is used when no sticky key is provided.
//
// All circuit state names, Redis keys, and dirty-set semantics come from
// packages/shared/schemas/credstate — the single source of truth shared with
// credstats, Control Plane, and Nexus Hub.
package credpool

import (
	"context"
	"math/rand"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/credstate"
)

// Entry is one candidate in the pool: a credential ID with its weight and
// pre-fetched circuit state. Callers must populate all fields.
type Entry struct {
	ID      string
	Weight  int    // selectionWeight from DB; 0 = skip
	Circuit string // credstate.Circuit* values; "" means closed
}

// CircuitReader reads the circuit state of a single credential from Redis.
// Returns "" (treat as closed) when Redis is unavailable or the key is
// absent. For rate_limit circuits whose next_probe_at has elapsed, the
// reader auto-transitions the circuit to half_open (fire-and-forget),
// marks the dirty set so the Hub flush job persists the new state, and
// returns half_open.
func CircuitReader(ctx context.Context, rdb redis.Cmdable, credID string) string {
	if rdb == nil {
		return ""
	}
	key := credstate.CircuitKey(credID)
	vals, err := rdb.HMGet(ctx, key,
		credstate.CircuitFieldState,
		credstate.CircuitFieldOpenReason,
		credstate.CircuitFieldNextProbe,
	).Result()
	if err != nil || len(vals) == 0 || vals[0] == nil {
		return ""
	}
	state, _ := vals[0].(string)
	openReason, _ := vals[1].(string)
	nextProbeAt, _ := vals[2].(string)

	if state == credstate.CircuitOpen && openReason == credstate.ReasonRateLimit && nextProbeAt != "" {
		if probeTime, e := time.Parse(time.RFC3339Nano, nextProbeAt); e == nil && !time.Now().UTC().Before(probeTime) {
			// Cooldown elapsed — auto-transition OPEN → HALF_OPEN and mark
			// dirty so the Hub circuit-flush job propagates the new state
			// to the Credential table.
			pipe := rdb.Pipeline()
			pipe.HSet(ctx, key, credstate.CircuitFieldState, credstate.CircuitHalfOpen)
			pipe.SAdd(ctx, credstate.CircuitDirtySet, credID)
			_, _ = pipe.Exec(ctx)
			return credstate.CircuitHalfOpen
		}
	}
	return state
}

// BulkCircuitStates fetches circuit states for a batch of credential IDs
// in a single Redis pipeline. Keys absent from Redis are returned as ""
// (closed). For rate_limit circuits whose next_probe_at has elapsed, the
// caller auto-transitions to half_open and marks dirty so the Hub
// flush job picks up the change.
func BulkCircuitStates(ctx context.Context, rdb redis.Cmdable, ids []string) map[string]string {
	out := make(map[string]string, len(ids))
	if rdb == nil || len(ids) == 0 {
		return out
	}
	now := time.Now().UTC()
	pipe := rdb.Pipeline()
	cmds := make([]*redis.SliceCmd, len(ids))
	for i, id := range ids {
		cmds[i] = pipe.HMGet(ctx, credstate.CircuitKey(id),
			credstate.CircuitFieldState,
			credstate.CircuitFieldOpenReason,
			credstate.CircuitFieldNextProbe,
		)
	}
	_, _ = pipe.Exec(ctx)

	var toPromote []string
	for i, id := range ids {
		vals, err := cmds[i].Result()
		if err != nil || len(vals) == 0 || vals[0] == nil {
			continue
		}
		state, _ := vals[0].(string)
		openReason, _ := vals[1].(string)
		nextProbeAt, _ := vals[2].(string)

		if state == credstate.CircuitOpen && openReason == credstate.ReasonRateLimit && nextProbeAt != "" {
			if probeTime, e := time.Parse(time.RFC3339Nano, nextProbeAt); e == nil && !now.Before(probeTime) {
				toPromote = append(toPromote, id)
				out[id] = credstate.CircuitHalfOpen
				continue
			}
		}
		if state != "" {
			out[id] = state
		}
	}

	if len(toPromote) > 0 {
		// Pipeline both the state change and the dirty marker so the
		// Hub circuit-flush job persists the transition.
		ppipe := rdb.Pipeline()
		dirtyArgs := make([]interface{}, len(toPromote))
		for i, id := range toPromote {
			ppipe.HSet(ctx, credstate.CircuitKey(id), credstate.CircuitFieldState, credstate.CircuitHalfOpen)
			dirtyArgs[i] = id
		}
		ppipe.SAdd(ctx, credstate.CircuitDirtySet, dirtyArgs...)
		_, _ = ppipe.Exec(ctx)
	}

	return out
}

// Select picks one Entry from candidates according to stickyKey and
// circuit state.
//
// Selection rules:
//
//  1. Entries with Circuit == credstate.CircuitOpen or Weight == 0 are
//     excluded.
//
//  2. Entries with Circuit == credstate.CircuitHalfOpen are included with
//     effective weight = 1.
//
//  3. If stickyKey is non-empty: WEIGHTED RENDEZVOUS HASHING over the eligible
//     entries (see stickyPick). The same VK routes to the same credential for
//     as long as that credential stays eligible, maximising provider-side cache
//     reuse; when it becomes ineligible only the keys that were pinned to it
//     move.
//
//     EXCEPT while a HALF_OPEN probe is present. Stickiness is then
//     probabilistic for that pool: the probe takes its weight share of REQUESTS
//     (the weight-1 clamp means ~0.5% at the default selectionWeight of 100) and
//     the remaining credentials keep the deterministic map among themselves. The
//     clamp is a statement about traffic, and rendezvous hashing divides keys —
//     at the handful of virtual keys a real fleet has, a key-share of 0.5%
//     rounds to zero and the probe is never exercised, so its circuit can never
//     close. See stickyPickWithProbe.
//
//  4. If stickyKey is empty: weighted random among eligible entries.
//
// Returns nil when no eligible candidate exists (all OPEN or pool is empty).
func Select(candidates []Entry, stickyKey string) *Entry {
	eligible := filterEligible(candidates)
	if len(eligible) == 0 {
		return nil
	}

	if stickyKey != "" {
		return stickyPickWithProbe(eligible, stickyKey)
	}

	return weightedRandom(eligible)
}

// stickyPickWithProbe is the sticky path when a HALF_OPEN probe is in the pool.
//
// The weight-1 clamp expresses a trickle of REQUESTS, and that is what the
// non-sticky path gives it: weightedRandom draws per request, so a probe at 1
// against two credentials at the DB default of 100 receives ~0.5% of calls and
// its circuit closes on the first success. Rendezvous hashing divides KEYS, not
// requests, and the sticky key is a virtual key id — a single-tenant fleet has
// a handful of those, not thousands. 0.5% of ten keys is zero keys, so the
// probe received nothing at all, and since the ONLY exit from HALF_OPEN is a
// 2xx recorded against that credential (there is no background prober and no
// TTL on the circuit hash), the pool lost a third of its capacity until an
// operator hit circuit-reset by hand. The automatic recovery the state exists
// for could not happen.
//
// So the probe is drawn per REQUEST here too, at exactly the share its weight
// asks for, and the steady credentials keep the deterministic map among
// themselves. Two things improve at once: the probe's share stops depending on
// how many virtual keys a deployment happens to have, and the steady map no
// longer reshuffles when a probe joins or leaves it — which is the fleet-wide
// remap stickyPick was written to eliminate.
//
// The cost is that ~0.5% of a pinned key's calls land off its pinned credential
// while a probe is live. That is the same 0.5% the non-sticky path has always
// paid, it lasts only until the circuit resolves, and it buys back a credential
// that would otherwise stay dark.
func stickyPickWithProbe(eligible []Entry, stickyKey string) *Entry {
	probeWeight, steadyWeight := 0, 0
	for i := range eligible {
		if eligible[i].Circuit == credstate.CircuitHalfOpen {
			probeWeight += eligible[i].Weight
			continue
		}
		steadyWeight += eligible[i].Weight
	}
	// No probe in the pool, or nothing but probes: one rendezvous over the whole
	// eligible set is both correct and the cheapest thing to do.
	if probeWeight == 0 || steadyWeight == 0 {
		return stickyPick(eligible, stickyKey)
	}
	if rand.Intn(probeWeight+steadyWeight) < probeWeight {
		return weightedRandom(filterCircuit(eligible, true))
	}
	return stickyPick(filterCircuit(eligible, false), stickyKey)
}

// filterCircuit returns the HALF_OPEN entries when halfOpen is true, and every
// other eligible entry when it is false. Callers rely on the result being
// non-empty, which stickyPickWithProbe establishes by checking both weight sums
// before splitting.
func filterCircuit(entries []Entry, halfOpen bool) []Entry {
	out := make([]Entry, 0, len(entries))
	for i := range entries {
		if (entries[i].Circuit == credstate.CircuitHalfOpen) == halfOpen {
			out = append(out, entries[i])
		}
	}
	return out
}

// filterEligible returns entries usable in selection (not OPEN, weight > 0).
// HALF_OPEN entries are included with effective weight clamped to 1 so
// they act as low-rate probes without dominating the pool.
func filterEligible(entries []Entry) []Entry {
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if e.Weight <= 0 {
			continue
		}
		if e.Circuit == credstate.CircuitOpen {
			continue
		}
		eff := e
		if e.Circuit == credstate.CircuitHalfOpen {
			eff.Weight = 1
		}
		out = append(out, eff)
	}
	return out
}

// weightedRandom picks an entry proportional to its Weight.
func weightedRandom(entries []Entry) *Entry {
	total := 0
	for _, e := range entries {
		total += e.Weight
	}
	if total == 0 {
		return &entries[0]
	}
	pick := rand.Intn(total)
	for i := range entries {
		pick -= entries[i].Weight
		if pick < 0 {
			return &entries[i]
		}
	}
	return &entries[len(entries)-1]
}

// Re-exports so callers that imported credpool.CircuitClosed / Open / HalfOpen
// in the v1 API stay source-compatible. New code should import credstate
// directly.

const (
	CircuitClosed   = credstate.CircuitClosed
	CircuitOpen     = credstate.CircuitOpen
	CircuitHalfOpen = credstate.CircuitHalfOpen
)

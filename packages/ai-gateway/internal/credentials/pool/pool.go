// Package credpool implements credential pool selection for multi-credential
// providers. It combines weighted random selection with per-VK stickiness
// (consistent hashing) and circuit breaker awareness so that:
//
//   - A given virtual key always resolves to the same upstream credential
//     (maximizing provider-side prompt cache hits).
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
	"hash/fnv"
	"math/rand"
	"sort"
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
//  1. Entries with Circuit == credstate.CircuitOpen or Weight == 0 are
//     excluded.
//  2. Entries with Circuit == credstate.CircuitHalfOpen are included with
//     effective weight = 1.
//  3. If stickyKey is non-empty: consistent FNV32a hash mod eligible count.
//     This ensures the same VK always routes to the same credential,
//     maximising provider-side cache reuse. If the sticky target is
//     ineligible (OPEN), falls through to weighted random among remaining
//     eligible entries.
//  4. If stickyKey is empty: weighted random among eligible entries.
//
// Returns nil when no eligible candidate exists (all OPEN or pool is empty).
func Select(candidates []Entry, stickyKey string) *Entry {
	eligible := filterEligible(candidates)
	if len(eligible) == 0 {
		return nil
	}

	if stickyKey != "" {
		// Sort eligible candidates by credential ID so the hash index maps
		// to a stable position regardless of the map-iteration order that
		// produced `candidates` (callers build the slice from a map range).
		// Without this sort the same VK could resolve to a different
		// credential on each call, defeating per-VK stickiness and the
		// provider-side prompt-cache reuse it exists to maximise.
		sort.Slice(eligible, func(i, j int) bool { return eligible[i].ID < eligible[j].ID })
		h := fnv.New32a()
		_, _ = h.Write([]byte(stickyKey))
		// Reduce modulo in uint32 space before converting to int: int(Sum32())
		// can be negative on 32-bit platforms (Sum32 occupies the full 32-bit
		// range), and Go's `%` follows the dividend's sign, so a negative
		// dividend would yield a negative index and panic on slice access.
		idx := int(h.Sum32() % uint32(len(eligible)))
		return &eligible[idx]
	}

	return weightedRandom(eligible)
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

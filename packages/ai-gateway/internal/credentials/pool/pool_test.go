package credpool

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/credstate"
)

func TestSelect_EmptyPoolReturnsNil(t *testing.T) {
	if got := Select(nil, ""); got != nil {
		t.Fatalf("Select(nil): got %+v, want nil", got)
	}
	if got := Select([]Entry{}, "vk-1"); got != nil {
		t.Fatalf("Select([]): got %+v, want nil", got)
	}
}

func TestSelect_AllOpenReturnsNil(t *testing.T) {
	pool := []Entry{
		{ID: "a", Weight: 10, Circuit: credstate.CircuitOpen},
		{ID: "b", Weight: 10, Circuit: credstate.CircuitOpen},
	}
	if got := Select(pool, "vk-1"); got != nil {
		t.Fatalf("all-open pool: got %+v, want nil", got)
	}
	if got := Select(pool, ""); got != nil {
		t.Fatalf("all-open pool no-sticky: got %+v, want nil", got)
	}
}

func TestSelect_ZeroWeightExcluded(t *testing.T) {
	// Weight==0 means "configured but disabled"; must be skipped even if
	// the circuit is closed. Otherwise an operator setting weight=0 to
	// take a credential out of rotation would silently still serve traffic.
	pool := []Entry{
		{ID: "disabled", Weight: 0, Circuit: ""},
		{ID: "active", Weight: 5, Circuit: ""},
	}
	for i := range 50 {
		got := Select(pool, fmt.Sprintf("vk-%d", i))
		if got == nil || got.ID != "active" {
			t.Fatalf("iter %d: zero-weight leaked into selection: %+v", i, got)
		}
	}
}

func TestSelect_NegativeWeightExcluded(t *testing.T) {
	// Defensive: filterEligible uses `Weight <= 0`; verify negative slips
	// don't silently become eligible (would happen if check were `== 0`).
	pool := []Entry{
		{ID: "bad", Weight: -1, Circuit: ""},
		{ID: "ok", Weight: 3, Circuit: ""},
	}
	got := Select(pool, "vk-x")
	if got == nil || got.ID != "ok" {
		t.Fatalf("negative weight slipped: %+v", got)
	}
}

func TestSelect_StickyKeyDeterministic(t *testing.T) {
	pool := []Entry{
		{ID: "a", Weight: 5, Circuit: ""},
		{ID: "b", Weight: 5, Circuit: ""},
		{ID: "c", Weight: 5, Circuit: ""},
	}
	first := Select(pool, "vk-stable")
	if first == nil {
		t.Fatal("first select returned nil")
	}
	for i := range 100 {
		got := Select(pool, "vk-stable")
		if got == nil || got.ID != first.ID {
			t.Fatalf("iter %d: sticky drift %+v != %+v", i, got, first)
		}
	}
}

func TestSelect_StickyKeyDeterministic_WithShuffledInput(t *testing.T) {
	// Callers build the candidate slice from a map range, so the same logical
	// pool can arrive in any order. Sticky selection MUST resolve to the same
	// credential ID regardless of input ordering — otherwise a single VK
	// bounces between credentials across requests and destroys provider-side
	// prompt-cache reuse. We compute the reference choice from a canonical
	// ordering, then assert every shuffled permutation reproduces it.
	base := []Entry{
		{ID: "cred-a", Weight: 5, Circuit: ""},
		{ID: "cred-b", Weight: 5, Circuit: ""},
		{ID: "cred-c", Weight: 5, Circuit: ""},
		{ID: "cred-d", Weight: 5, Circuit: ""},
		{ID: "cred-e", Weight: 5, Circuit: ""},
	}
	const stickyKey = "vk-shuffle-stable"

	ref := Select(append([]Entry(nil), base...), stickyKey)
	if ref == nil {
		t.Fatal("reference select returned nil")
	}

	rng := rand.New(rand.NewSource(42))
	for trial := range 200 {
		shuffled := append([]Entry(nil), base...)
		rng.Shuffle(len(shuffled), func(i, j int) {
			shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
		})
		got := Select(shuffled, stickyKey)
		if got == nil {
			t.Fatalf("trial %d: nil despite eligible entries", trial)
		}
		if got.ID != ref.ID {
			t.Fatalf("trial %d: non-deterministic sticky selection: got %q want %q (input order=%+v)",
				trial, got.ID, ref.ID, shuffled)
		}
	}
}

func TestSelect_StickyKeyVariesAcrossKeys(t *testing.T) {
	// Different sticky keys must NOT all hash to the same entry — that
	// would defeat per-VK affinity. We allow occasional same-bucket hits
	// (FNV32a mod 3 — birthday paradox) but require > 1 distinct bucket
	// across a 50-VK sweep.
	pool := []Entry{
		{ID: "a", Weight: 1, Circuit: ""},
		{ID: "b", Weight: 1, Circuit: ""},
		{ID: "c", Weight: 1, Circuit: ""},
	}
	seen := map[string]bool{}
	for i := range 50 {
		got := Select(pool, fmt.Sprintf("vk-%d", i))
		if got != nil {
			seen[got.ID] = true
		}
	}
	if len(seen) < 2 {
		t.Fatalf("sticky hash collapsed to single bucket: %+v", seen)
	}
}

func TestSelect_StickyExcludesOpenEntry(t *testing.T) {
	// A sticky key that hashed to the OPEN entry pre-filter MUST be routed
	// to a different eligible entry — never returned an OPEN one. This is
	// the structural guard against a VK being pinned to a dead cred.
	pool := []Entry{
		{ID: "open-1", Weight: 5, Circuit: credstate.CircuitOpen},
		{ID: "closed-1", Weight: 5, Circuit: ""},
	}
	for i := range 100 {
		got := Select(pool, fmt.Sprintf("vk-%d", i))
		if got == nil {
			t.Fatalf("iter %d: nil despite eligible entry", i)
		}
		if got.Circuit == credstate.CircuitOpen {
			t.Fatalf("iter %d: OPEN entry returned: %+v", i, got)
		}
	}
}

func TestSelect_HalfOpenIncludedAsLowWeightProbe(t *testing.T) {
	// HALF_OPEN credentials must be included (otherwise they could never
	// be probed and would never recover), but with effective weight=1 so
	// they don't drown out healthy ones.
	pool := []Entry{
		{ID: "healthy", Weight: 100, Circuit: ""},
		{ID: "probing", Weight: 100, Circuit: credstate.CircuitHalfOpen},
	}
	healthyHits, probingHits := 0, 0
	const trials = 10000
	for i := range trials {
		got := Select(pool, "")
		if got == nil {
			t.Fatalf("iter %d: nil", i)
		}
		switch got.ID {
		case "healthy":
			healthyHits++
		case "probing":
			probingHits++
		}
	}
	// Effective ratio is 100:1 — probing should land in roughly 1% of
	// trials. Allow 0.2%-3% to accommodate randomness. Anything above 3%
	// means HALF_OPEN weight wasn't clamped; below 0.2% means it was
	// excluded entirely.
	ratio := float64(probingHits) / float64(trials)
	if ratio < 0.002 || ratio > 0.03 {
		t.Fatalf("HALF_OPEN probe rate out of band: %.4f (healthy=%d probing=%d)",
			ratio, healthyHits, probingHits)
	}
}

func TestSelect_WeightedRandomRoughlyProportional(t *testing.T) {
	// Without a sticky key, selection is weighted random. A 1:9 weight
	// split must produce ~10% / 90% over a large trial. Tolerance ±3%.
	pool := []Entry{
		{ID: "low", Weight: 1, Circuit: ""},
		{ID: "high", Weight: 9, Circuit: ""},
	}
	const trials = 20000
	lowHits := 0
	for range trials {
		got := Select(pool, "")
		if got.ID == "low" {
			lowHits++
		}
	}
	ratio := float64(lowHits) / float64(trials)
	if ratio < 0.07 || ratio > 0.13 {
		t.Fatalf("weighted random skew: low/total=%.3f, want ~0.10", ratio)
	}
}

func TestSelect_SinglyEligibleHonoredEvenAcrossManyEntries(t *testing.T) {
	// A pool with mixed states where exactly one entry is eligible must
	// resolve to that entry regardless of sticky key.
	pool := []Entry{
		{ID: "a", Weight: 0, Circuit: ""},                    // weight 0
		{ID: "b", Weight: 5, Circuit: credstate.CircuitOpen}, // open
		{ID: "c", Weight: 5, Circuit: ""},                    // ONLY eligible
		{ID: "d", Weight: -3, Circuit: ""},                   // negative weight
		{ID: "e", Weight: 5, Circuit: credstate.CircuitOpen}, // open
	}
	for i, sticky := range []string{"", "vk-1", "vk-2", "vk-3"} {
		got := Select(pool, sticky)
		if got == nil || got.ID != "c" {
			t.Fatalf("case %d sticky=%q: got %+v want c", i, sticky, got)
		}
	}
}

func newMiniRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, rdb
}

func TestCircuitReader_NilRdb(t *testing.T) {
	if got := CircuitReader(context.Background(), nil, "any"); got != "" {
		t.Fatalf("nil rdb: got %q, want \"\"", got)
	}
}

func TestCircuitReader_MissingKey(t *testing.T) {
	_, rdb := newMiniRedis(t)
	got := CircuitReader(context.Background(), rdb, "cred-missing")
	if got != "" {
		t.Fatalf("missing key: got %q want \"\"", got)
	}
}

func TestCircuitReader_ReturnsState(t *testing.T) {
	mr, rdb := newMiniRedis(t)
	mr.HSet(credstate.CircuitKey("cred-1"), credstate.CircuitFieldState, credstate.CircuitOpen)
	mr.HSet(credstate.CircuitKey("cred-1"), credstate.CircuitFieldOpenReason, credstate.ReasonAuthFail)
	if got := CircuitReader(context.Background(), rdb, "cred-1"); got != credstate.CircuitOpen {
		t.Fatalf("got %q want %q", got, credstate.CircuitOpen)
	}
}

func TestCircuitReader_AutoPromotesRateLimitAfterCooldown(t *testing.T) {
	// Rate-limit OPEN with next_probe_at in the past must transition to
	// HALF_OPEN automatically AND mark dirty so the Hub flush job
	// persists the state change.
	mr, rdb := newMiniRedis(t)
	key := credstate.CircuitKey("cred-rl")
	past := time.Now().UTC().Add(-1 * time.Minute).Format(time.RFC3339Nano)
	mr.HSet(key, credstate.CircuitFieldState, credstate.CircuitOpen)
	mr.HSet(key, credstate.CircuitFieldOpenReason, credstate.ReasonRateLimit)
	mr.HSet(key, credstate.CircuitFieldNextProbe, past)

	got := CircuitReader(context.Background(), rdb, "cred-rl")
	if got != credstate.CircuitHalfOpen {
		t.Fatalf("got %q want %q", got, credstate.CircuitHalfOpen)
	}
	if state := mr.HGet(key, credstate.CircuitFieldState); state != credstate.CircuitHalfOpen {
		t.Fatalf("state in Redis not promoted: %q", state)
	}
	dirty, _ := rdb.SMembers(context.Background(), credstate.CircuitDirtySet).Result()
	if len(dirty) != 1 || dirty[0] != "cred-rl" {
		t.Fatalf("dirty set: got %+v want [cred-rl]", dirty)
	}
}

func TestCircuitReader_KeepsRateLimitOpenBeforeProbeTime(t *testing.T) {
	mr, rdb := newMiniRedis(t)
	key := credstate.CircuitKey("cred-rl")
	future := time.Now().UTC().Add(5 * time.Minute).Format(time.RFC3339Nano)
	mr.HSet(key, credstate.CircuitFieldState, credstate.CircuitOpen)
	mr.HSet(key, credstate.CircuitFieldOpenReason, credstate.ReasonRateLimit)
	mr.HSet(key, credstate.CircuitFieldNextProbe, future)

	got := CircuitReader(context.Background(), rdb, "cred-rl")
	if got != credstate.CircuitOpen {
		t.Fatalf("before probe time: got %q want %q", got, credstate.CircuitOpen)
	}
	if state := mr.HGet(key, credstate.CircuitFieldState); state != credstate.CircuitOpen {
		t.Fatalf("state was prematurely promoted: %q", state)
	}
	dirty, _ := rdb.SMembers(context.Background(), credstate.CircuitDirtySet).Result()
	if len(dirty) != 0 {
		t.Fatalf("dirty set should be empty: %+v", dirty)
	}
}

func TestCircuitReader_DoesNotPromoteAuthFailOpen(t *testing.T) {
	// auth_fail OPEN circuits MUST stay open until an operator manually
	// rotates the credential — there is no automatic probe schedule.
	mr, rdb := newMiniRedis(t)
	key := credstate.CircuitKey("cred-af")
	past := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339Nano)
	mr.HSet(key, credstate.CircuitFieldState, credstate.CircuitOpen)
	mr.HSet(key, credstate.CircuitFieldOpenReason, credstate.ReasonAuthFail)
	mr.HSet(key, credstate.CircuitFieldNextProbe, past)

	got := CircuitReader(context.Background(), rdb, "cred-af")
	if got != credstate.CircuitOpen {
		t.Fatalf("auth_fail OPEN: got %q want %q", got, credstate.CircuitOpen)
	}
	if state := mr.HGet(key, credstate.CircuitFieldState); state != credstate.CircuitOpen {
		t.Fatalf("auth_fail state mutated: %q", state)
	}
}

func TestBulkCircuitStates_EmptyInput(t *testing.T) {
	_, rdb := newMiniRedis(t)
	got := BulkCircuitStates(context.Background(), rdb, nil)
	if len(got) != 0 {
		t.Fatalf("nil ids: got %+v", got)
	}
	got = BulkCircuitStates(context.Background(), rdb, []string{})
	if len(got) != 0 {
		t.Fatalf("empty ids: got %+v", got)
	}
}

func TestBulkCircuitStates_NilRdb(t *testing.T) {
	got := BulkCircuitStates(context.Background(), nil, []string{"a", "b"})
	if len(got) != 0 {
		t.Fatalf("nil rdb: got %+v", got)
	}
}

func TestBulkCircuitStates_MixedStates(t *testing.T) {
	mr, rdb := newMiniRedis(t)
	mr.HSet(credstate.CircuitKey("c-closed"), credstate.CircuitFieldState, credstate.CircuitClosed)
	mr.HSet(credstate.CircuitKey("c-open"), credstate.CircuitFieldState, credstate.CircuitOpen)
	mr.HSet(credstate.CircuitKey("c-open"), credstate.CircuitFieldOpenReason, credstate.ReasonAuthFail)
	mr.HSet(credstate.CircuitKey("c-half"), credstate.CircuitFieldState, credstate.CircuitHalfOpen)
	// c-missing: no entry

	got := BulkCircuitStates(context.Background(), rdb,
		[]string{"c-closed", "c-open", "c-half", "c-missing"})

	if got["c-closed"] != credstate.CircuitClosed {
		t.Errorf("c-closed: %q", got["c-closed"])
	}
	if got["c-open"] != credstate.CircuitOpen {
		t.Errorf("c-open: %q", got["c-open"])
	}
	if got["c-half"] != credstate.CircuitHalfOpen {
		t.Errorf("c-half: %q", got["c-half"])
	}
	if _, ok := got["c-missing"]; ok {
		t.Errorf("c-missing should be absent, got %q", got["c-missing"])
	}
}

func TestBulkCircuitStates_BatchAutoPromote(t *testing.T) {
	mr, rdb := newMiniRedis(t)
	past := time.Now().UTC().Add(-30 * time.Second).Format(time.RFC3339Nano)
	future := time.Now().UTC().Add(30 * time.Second).Format(time.RFC3339Nano)

	for _, id := range []string{"rl-due-1", "rl-due-2"} {
		mr.HSet(credstate.CircuitKey(id), credstate.CircuitFieldState, credstate.CircuitOpen)
		mr.HSet(credstate.CircuitKey(id), credstate.CircuitFieldOpenReason, credstate.ReasonRateLimit)
		mr.HSet(credstate.CircuitKey(id), credstate.CircuitFieldNextProbe, past)
	}
	mr.HSet(credstate.CircuitKey("rl-pending"), credstate.CircuitFieldState, credstate.CircuitOpen)
	mr.HSet(credstate.CircuitKey("rl-pending"), credstate.CircuitFieldOpenReason, credstate.ReasonRateLimit)
	mr.HSet(credstate.CircuitKey("rl-pending"), credstate.CircuitFieldNextProbe, future)
	mr.HSet(credstate.CircuitKey("af-open"), credstate.CircuitFieldState, credstate.CircuitOpen)
	mr.HSet(credstate.CircuitKey("af-open"), credstate.CircuitFieldOpenReason, credstate.ReasonAuthFail)
	mr.HSet(credstate.CircuitKey("af-open"), credstate.CircuitFieldNextProbe, past)

	got := BulkCircuitStates(context.Background(), rdb,
		[]string{"rl-due-1", "rl-due-2", "rl-pending", "af-open"})

	for _, id := range []string{"rl-due-1", "rl-due-2"} {
		if got[id] != credstate.CircuitHalfOpen {
			t.Errorf("%s: returned %q, want half_open", id, got[id])
		}
		if state := mr.HGet(credstate.CircuitKey(id), credstate.CircuitFieldState); state != credstate.CircuitHalfOpen {
			t.Errorf("%s: Redis state %q, want half_open", id, state)
		}
	}
	if got["rl-pending"] != credstate.CircuitOpen {
		t.Errorf("rl-pending should stay open, got %q", got["rl-pending"])
	}
	if got["af-open"] != credstate.CircuitOpen {
		t.Errorf("af-open should stay open, got %q", got["af-open"])
	}
	if state := mr.HGet(credstate.CircuitKey("af-open"), credstate.CircuitFieldState); state != credstate.CircuitOpen {
		t.Errorf("af-open mutated to %q", state)
	}

	dirty, _ := rdb.SMembers(context.Background(), credstate.CircuitDirtySet).Result()
	want := map[string]bool{"rl-due-1": true, "rl-due-2": true}
	for _, id := range dirty {
		if !want[id] {
			t.Errorf("unexpected dirty entry: %s", id)
		}
		delete(want, id)
	}
	if len(want) != 0 {
		t.Errorf("missing dirty entries: %+v", want)
	}
}

func TestExportedCircuitConstantsAlignWithCredstate(t *testing.T) {
	// Source-compat aliases must continue to map 1:1 with credstate.
	// Drift here would mean callers using the credpool aliases compare
	// against stale string values.
	if CircuitClosed != credstate.CircuitClosed {
		t.Errorf("CircuitClosed drift: %q vs %q", CircuitClosed, credstate.CircuitClosed)
	}
	if CircuitOpen != credstate.CircuitOpen {
		t.Errorf("CircuitOpen drift: %q vs %q", CircuitOpen, credstate.CircuitOpen)
	}
	if CircuitHalfOpen != credstate.CircuitHalfOpen {
		t.Errorf("CircuitHalfOpen drift: %q vs %q", CircuitHalfOpen, credstate.CircuitHalfOpen)
	}
}

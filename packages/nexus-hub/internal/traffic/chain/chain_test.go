package chain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// chainTestPool returns a pgx pool for chain tests.
//
// Strategy (C) — KEPT GATED behind NEXUS_DESTRUCTIVE_TESTS=1. The chain
// integrity tests below MUST TRUNCATE the entire AdminAuditLog because
// VerifyChain walks every row in sequenceNumber order and the genesis-row
// assertions (TestNextHash_GenesisRow expects sequenceNumber == 1) cannot
// be expressed under prefix scoping. The chain itself is a global
// tamper-evident structure — there is no "per-test sub-chain" that
// VerifyChain could validate without changing the production helper
// signature, which we explicitly avoid (see refactor brief: "DO NOT modify
// prod code unless small interface seam").
//
// Why a VerifyChain WHERE-scoped variant doesn't help: the chain link
// requires the previous row's integrityHash. If the test scope skips rows
// N-1 (from other tests / live data), row N's "previousHash links to chain
// head" assertion becomes meaningless. The tamper-evident property is by
// design a property of the whole table.
//
// Operational guidance: this suite is intended for a throwaway dev DB
// (port 55532 from docker-compose dev-start). The TEST_DATABASE_URL +
// NEXUS_DESTRUCTIVE_TESTS=1 double-opt-in stops a stray run on a populated
// dev DB.
func chainTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping destructive integration test")
	}
	if os.Getenv("NEXUS_DESTRUCTIVE_TESTS") != "1" {
		t.Skip("NEXUS_DESTRUCTIVE_TESTS!=1; skipping — this test TRUNCATEs AdminAuditLog (chain integrity is a global table property, see file-level comment). Opt in on a throwaway DB only.")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("skip: DB unavailable (%v)", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Skipf("skip: DB ping failed (%v)", err)
	}
	return pool
}

// truncateAuditLog wipes AdminAuditLog so each test starts from a clean
// genesis. Chain semantics depend on global ordering, so per-test isolation
// can only be done by truncate; row-prefix scoping (used elsewhere) is not
// sufficient because VerifyChain walks every row. This is the documented
// reason the chain tests stay behind NEXUS_DESTRUCTIVE_TESTS=1.
func truncateAuditLog(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `TRUNCATE TABLE "AdminAuditLog" RESTART IDENTITY`); err != nil {
		t.Fatalf("truncate audit log: %v", err)
	}
}

// insertChainRow runs NextHash inside a tx, executes the AdminAuditLog
// INSERT with the computed hashes, and commits. Returns the (previousHash,
// integrityHash) tuple plus the assigned sequenceNumber so callers can
// assert on the ordering.
func insertChainRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, p HashPayload) (prev, integ string, seq int64) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var hashInput []byte
	prev, integ, hashInput, err = NextHash(ctx, tx, p)
	if err != nil {
		t.Fatalf("NextHash: %v", err)
	}

	id := uuid.New().String()
	var prevArg any
	if prev != "" {
		prevArg = prev
	}
	var beforeArg, afterArg any
	if len(p.BeforeState) > 0 {
		beforeArg = []byte(p.BeforeState)
	}
	if len(p.AfterState) > 0 {
		afterArg = []byte(p.AfterState)
	}

	if err := tx.QueryRow(ctx, `
        INSERT INTO "AdminAuditLog" (
            id, timestamp,
            "actorId", "actorLabel", "actorRole",
            action, "entityType", "entityId",
            "beforeState", "afterState",
            "nexusRequestId", "clientRequestId", "clientUserId", "clientSessionId",
            "previousHash", "integrityHash", "hashInput"
        ) VALUES (
            $1, to_timestamp($2 / 1000.0),
            $3, $3, NULL,
            $4, $5, $6,
            $7, $8,
            NULL, NULL, NULL, NULL,
            $9, $10, $11
        )
        RETURNING "sequenceNumber"
    `,
		id, p.TimestampMs,
		p.ActorID,
		p.Action, p.EntityType, p.EntityID,
		beforeArg, afterArg,
		prevArg, integ, hashInput,
	).Scan(&seq); err != nil {
		t.Fatalf("insert chain row: %v", err)
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit tx: %v", err)
	}
	return prev, integ, seq
}

// --- Test 1: NextHash determinism (same payload + same chain head → same
// hash). Run on a clean table so both calls see the same genesis condition.

func TestNextHash_Deterministic(t *testing.T) {
	pool := chainTestPool(t)
	defer pool.Close()
	ctx := context.Background()

	truncateAuditLog(t, ctx, pool)

	p := HashPayload{
		TimestampMs: 1745539200000,
		Action:      "test_action",
		ActorID:     "actor-1",
		EntityType:  "thing",
		EntityID:    "thing-det-1",
	}

	hash := func() string {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		_, h, _, err := NextHash(ctx, tx, p)
		if err != nil {
			t.Fatalf("NextHash: %v", err)
		}
		return h
	}

	h1 := hash()
	h2 := hash()
	if h1 != h2 {
		t.Errorf("non-deterministic: %s != %s", h1, h2)
	}
	if len(h1) != 64 {
		t.Errorf("SHA-256 hex must be 64 chars, got %d", len(h1))
	}
}

// --- Test 2: genesis row has previousHash == "" and a non-empty integrityHash.

func TestNextHash_GenesisRow(t *testing.T) {
	pool := chainTestPool(t)
	defer pool.Close()
	ctx := context.Background()

	truncateAuditLog(t, ctx, pool)

	prev, integ, seq := insertChainRow(t, ctx, pool, HashPayload{
		TimestampMs: time.Now().UTC().UnixMilli(),
		Action:      "genesis",
		ActorID:     "actor-genesis",
		EntityType:  "thing",
		EntityID:    "g-1",
	})
	if prev != "" {
		t.Errorf("genesis previousHash = %q, want empty", prev)
	}
	if integ == "" {
		t.Error("genesis integrityHash must be non-empty")
	}
	if seq != 1 {
		t.Errorf("seq = %d, want 1", seq)
	}

	// Verify the row's stored previousHash is NULL and integrityHash matches.
	var dbPrev *string
	var dbInteg string
	if err := pool.QueryRow(ctx, `
        SELECT "previousHash", "integrityHash"
        FROM "AdminAuditLog"
        WHERE "sequenceNumber" = 1
    `).Scan(&dbPrev, &dbInteg); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if dbPrev != nil {
		t.Errorf("stored previousHash = %v, want NULL", *dbPrev)
	}
	if dbInteg != integ {
		t.Errorf("stored integrityHash = %s, want %s", dbInteg, integ)
	}
}

// --- Test 3: chain continuity — second insert's previousHash equals the
// first row's integrityHash; integrityHash advances.

func TestNextHash_ChainContinuity(t *testing.T) {
	pool := chainTestPool(t)
	defer pool.Close()
	ctx := context.Background()

	truncateAuditLog(t, ctx, pool)

	_, integ1, _ := insertChainRow(t, ctx, pool, HashPayload{
		TimestampMs: 1745539200000,
		Action:      "first",
		ActorID:     "a",
		EntityType:  "thing",
		EntityID:    "t-1",
	})
	prev2, integ2, _ := insertChainRow(t, ctx, pool, HashPayload{
		TimestampMs: 1745539200001,
		Action:      "second",
		ActorID:     "a",
		EntityType:  "thing",
		EntityID:    "t-1",
	})
	if prev2 != integ1 {
		t.Errorf("row 2 previousHash = %s, want %s (row 1 integrity)", prev2, integ1)
	}
	if integ2 == integ1 {
		t.Error("row 2 integrityHash must differ from row 1")
	}
}

// --- Test 4: concurrent inserts — N goroutines each open their own tx and
// call NextHash + INSERT. The advisory lock serialises chain head reads, so
// VerifyChain must end up with badSeq == 0 even under contention.

func TestNextHash_ConcurrentInserts(t *testing.T) {
	pool := chainTestPool(t)
	defer pool.Close()
	ctx := context.Background()

	truncateAuditLog(t, ctx, pool)

	const N = 10
	var wg sync.WaitGroup
	wg.Add(N)
	errs := make(chan error, N)
	for i := range N {
		go func(i int) {
			defer wg.Done()
			tx, err := pool.Begin(ctx)
			if err != nil {
				errs <- fmt.Errorf("goroutine %d begin: %w", i, err)
				return
			}
			defer tx.Rollback(ctx) //nolint:errcheck

			p := HashPayload{
				TimestampMs: 1745539200000 + int64(i),
				Action:      "concurrent",
				ActorID:     fmt.Sprintf("actor-%d", i),
				EntityType:  "thing",
				EntityID:    fmt.Sprintf("t-%d", i),
			}
			prev, integ, hashInput, err := NextHash(ctx, tx, p)
			if err != nil {
				errs <- fmt.Errorf("goroutine %d NextHash: %w", i, err)
				return
			}
			id := uuid.New().String()
			var prevArg any
			if prev != "" {
				prevArg = prev
			}
			if _, err := tx.Exec(ctx, `
                INSERT INTO "AdminAuditLog" (
                    id, timestamp,
                    "actorId", "actorLabel", "actorRole",
                    action, "entityType", "entityId",
                    "beforeState", "afterState",
                    "nexusRequestId", "clientRequestId", "clientUserId", "clientSessionId",
                    "previousHash", "integrityHash", "hashInput"
                ) VALUES (
                    $1, to_timestamp($2 / 1000.0),
                    $3, $3, NULL,
                    $4, $5, $6,
                    NULL, NULL,
                    NULL, NULL, NULL, NULL,
                    $7, $8, $9
                )
            `, id, p.TimestampMs, p.ActorID, p.Action, p.EntityType, p.EntityID, prevArg, integ, hashInput); err != nil {
				errs <- fmt.Errorf("goroutine %d insert: %w", i, err)
				return
			}
			if err := tx.Commit(ctx); err != nil {
				errs <- fmt.Errorf("goroutine %d commit: %w", i, err)
				return
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("goroutine error: %v", err)
	}
	if t.Failed() {
		return
	}

	// All N rows should be present and the chain should verify cleanly.
	var count int64
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM "AdminAuditLog"`).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != int64(N) {
		t.Errorf("row count = %d, want %d", count, N)
	}
	badSeq, err := VerifyChain(ctx, pool)
	if err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if badSeq != 0 {
		t.Errorf("VerifyChain badSeq = %d, want 0 (chain should be intact)", badSeq)
	}
}

// --- Test 5: VerifyChain detects hashInput tampering — insert 5 rows,
// mutate one row's hashInput, then VerifyChain returns its
// sequenceNumber. hashInput is the cryptographic source of truth, so
// any change to it breaks SHA-256(prev || hashInput) == integrityHash.

func TestVerifyChain_DetectsHashInputTampering(t *testing.T) {
	pool := chainTestPool(t)
	defer pool.Close()
	ctx := context.Background()

	truncateAuditLog(t, ctx, pool)

	for i := range 5 {
		insertChainRow(t, ctx, pool, HashPayload{
			TimestampMs: 1745539200000 + int64(i),
			Action:      "step",
			ActorID:     "actor",
			EntityType:  "thing",
			EntityID:    fmt.Sprintf("t-%d", i),
			AfterState:  json.RawMessage(fmt.Sprintf(`{"i":%d}`, i)),
		})
	}

	// Tamper with row 3: append a byte to hashInput so the recomputed
	// integrityHash no longer matches.
	if _, err := pool.Exec(ctx, `
        UPDATE "AdminAuditLog"
           SET "hashInput" = "hashInput" || E'\\x00'::bytea
         WHERE "sequenceNumber" = 3
    `); err != nil {
		t.Fatalf("tamper update: %v", err)
	}

	badSeq, err := VerifyChain(ctx, pool)
	if err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if badSeq != 3 {
		t.Errorf("VerifyChain badSeq = %d, want 3", badSeq)
	}
}

// --- Test 5b: VerifyChain intentionally does NOT flag display-column
// tampering. JSONB normalises key order and whitespace on storage, so
// reconstructing the original hash input from beforeState / afterState
// columns is not possible — that's exactly why hashInput exists as a
// separate column. Display-column tamper detection is out of scope for
// the chain itself and would require a different mechanism (e.g. a
// trigger that re-derives hashInput on UPDATE and refuses if it
// diverges). This test pins the documented behaviour so a future
// refactor cannot silently re-introduce the JSONB-roundtrip false
// positives that broke the chain at every non-trivial row.

func TestVerifyChain_DisplayColumnTamperingNotDetected(t *testing.T) {
	pool := chainTestPool(t)
	defer pool.Close()
	ctx := context.Background()

	truncateAuditLog(t, ctx, pool)

	for i := range 3 {
		insertChainRow(t, ctx, pool, HashPayload{
			TimestampMs: 1745539200000 + int64(i),
			Action:      "step",
			ActorID:     "actor",
			EntityType:  "thing",
			EntityID:    fmt.Sprintf("t-%d", i),
			AfterState:  json.RawMessage(fmt.Sprintf(`{"i":%d}`, i)),
		})
	}

	// Mutate row 2's afterState column without touching hashInput.
	if _, err := pool.Exec(ctx, `
        UPDATE "AdminAuditLog"
           SET "afterState" = '{"tampered":true}'::jsonb
         WHERE "sequenceNumber" = 2
    `); err != nil {
		t.Fatalf("tamper update: %v", err)
	}

	badSeq, err := VerifyChain(ctx, pool)
	if err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if badSeq != 0 {
		t.Errorf("VerifyChain badSeq = %d, want 0 (display tamper is out of chain scope)", badSeq)
	}
}

// --- Test 6.5: NewHashPayload builder rejects empty action / actorID. Both
// are caller bugs and would corrupt the chain's keying. EntityType /
// EntityID may be empty (login-event style action with no target entity).

func TestNewHashPayload_Validation(t *testing.T) {
	if _, err := NewHashPayload("", "actor", "thing", "t-1"); err == nil {
		t.Error("empty action should error")
	} else if !errors.Is(err, ErrEmptyAction) {
		t.Errorf("empty action err = %v, want ErrEmptyAction", err)
	}
	if _, err := NewHashPayload("act", "", "thing", "t-1"); err == nil {
		t.Error("empty actorID should error")
	} else if !errors.Is(err, ErrEmptyActorID) {
		t.Errorf("empty actorID err = %v, want ErrEmptyActorID", err)
	}
	// EntityType + EntityID empty is allowed (e.g. login events).
	p, err := NewHashPayload("login", "actor", "", "")
	if err != nil {
		t.Fatalf("login-style payload: %v", err)
	}
	if p.Action != "login" || p.ActorID != "actor" {
		t.Errorf("payload = %+v, want action=login actor=actor", p)
	}
}

// --- Test 6.7: canonicalizePayload is stable across struct-field-declaration
// order. Hashing must depend on the JSON object semantics, not Go's struct
// declaration order — otherwise a future field reorder silently breaks
// VerifyChain on every row written before the reorder. We can't reorder
// HashPayload itself in the test (it's a single struct), so we verify the
// invariant directly: the canonical bytes are sorted-key, and the hash of
// equal payloads is identical regardless of which way the test fed the
// fields in.

func TestCanonicalize_StableUnderFieldReorder(t *testing.T) {
	p := HashPayload{
		TimestampMs: 12345,
		Action:      "act",
		ActorID:     "actor",
		EntityType:  "thing",
		EntityID:    "t-1",
		BeforeState: json.RawMessage(`{"x":1}`),
		AfterState:  json.RawMessage(`{"y":2}`),
	}
	cb, err := canonicalizePayload(p)
	if err != nil {
		t.Fatalf("canonicalizePayload: %v", err)
	}
	// The canonical form must be a sorted-key JSON object: scan the keys in
	// order and confirm each comes after the previous lexicographically.
	var m map[string]json.RawMessage
	if err := json.Unmarshal(cb, &m); err != nil {
		t.Fatalf("decode canonical: %v", err)
	}
	// Round-trip the canonical bytes once more — should be byte-identical.
	cb2, err := canonicalizePayload(p)
	if err != nil {
		t.Fatalf("canonicalize 2: %v", err)
	}
	if string(cb) != string(cb2) {
		t.Errorf("canonical bytes not stable: %s vs %s", cb, cb2)
	}
	// Stronger: confirm key order in the canonical bytes is lexicographic.
	// We extract keys by scanning the bytes for `"key":` patterns.
	var prev string
	for _, k := range scanCanonicalKeys(t, cb) {
		if prev != "" && k <= prev {
			t.Errorf("canonical keys not sorted: prev=%q current=%q", prev, k)
		}
		prev = k
	}
}

// scanCanonicalKeys decodes cb as a JSON object and returns the keys in the
// order they appear in the byte stream. This is the order
// canonicalizePayload emitted them — encoding/json is order-preserving for
// json.RawMessage map values.
func scanCanonicalKeys(t *testing.T, cb []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(cb))
	tok, err := dec.Token()
	if err != nil {
		t.Fatalf("decode start: %v", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		t.Fatalf("canonical not an object: %v", tok)
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("decode key: %v", err)
		}
		k, ok := tok.(string)
		if !ok {
			t.Fatalf("key not a string: %v", tok)
		}
		keys = append(keys, k)
		// Skip the value (RawMessage decode).
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			t.Fatalf("decode value for key %q: %v", k, err)
		}
	}
	return keys
}

// TestCanonicalize_ViaTamperEvidentAndHumanUnchanged is the E90 I5 load-bearing
// guard. It pins two properties of folding `via` into the canonical hash payload:
//
//  1. Tamper-evidence: a row written with via="assistant" produces DIFFERENT
//     canonical bytes (hence a different integrityHash) than the same row with no
//     via — so the AI-attribution marker cannot be stripped or forged without
//     breaking VerifyChain.
//  2. No re-anchoring: a payload with an EMPTY via canonicalises byte-identically
//     to the very same payload value (the omitempty field drops out), which is what
//     guarantees every existing row and every future human/system write hashes
//     exactly as it did before via existed — no chain break, no backfill.
func TestCanonicalize_ViaTamperEvidentAndHumanUnchanged(t *testing.T) {
	base := HashPayload{
		TimestampMs: 999, Action: "create", ActorID: "user-1",
		EntityType: "virtual-key", EntityID: "vk-1",
	}

	human, err := canonicalizePayload(base) // Via == "" (zero value)
	if err != nil {
		t.Fatalf("canonicalize human: %v", err)
	}
	// Property 2: empty via must NOT appear in the canonical bytes at all.
	if bytes.Contains(human, []byte(`"via"`)) {
		t.Fatalf("empty via leaked into canonical bytes (would re-anchor the chain): %s", human)
	}

	assistant := base
	assistant.Via = "assistant"
	aiBytes, err := canonicalizePayload(assistant)
	if err != nil {
		t.Fatalf("canonicalize assistant: %v", err)
	}
	// Property 1: the assistant row's canonical bytes differ from the human row's.
	if string(aiBytes) == string(human) {
		t.Fatal("via=assistant produced identical canonical bytes to a human row; marker is not tamper-evident")
	}
	if !bytes.Contains(aiBytes, []byte(`"via":"assistant"`)) {
		t.Fatalf("assistant canonical bytes missing via marker: %s", aiBytes)
	}

	// And the resulting hash links differ — the property VerifyChain actually relies
	// on. computeHashLink mirrors NextHash's SHA-256(prev || canonical) recipe.
	const prev = "deadbeef"
	if computeHashLink(prev, human) == computeHashLink(prev, aiBytes) {
		t.Fatal("integrityHash identical for human vs assistant row; stripping via would not break the chain")
	}
}

// --- Test 6.8: canonical bytes for the exact-same payload produce
// byte-identical output across two invocations (no map iteration leak).

func TestCanonicalize_StableAcrossInvocations(t *testing.T) {
	p := HashPayload{
		TimestampMs: 1, Action: "a", ActorID: "b", EntityType: "c", EntityID: "d",
	}
	first, _ := canonicalizePayload(p)
	for i := range 50 {
		got, _ := canonicalizePayload(p)
		if string(got) != string(first) {
			t.Fatalf("iteration %d produced different bytes: %s vs %s", i, got, first)
		}
	}
}

// --- Test 6: VerifyChain on intact chain returns 0 (sanity for the helper).

func TestVerifyChain_IntactChain(t *testing.T) {
	pool := chainTestPool(t)
	defer pool.Close()
	ctx := context.Background()

	truncateAuditLog(t, ctx, pool)

	for i := range 3 {
		insertChainRow(t, ctx, pool, HashPayload{
			TimestampMs: 1745539200000 + int64(i),
			Action:      "step",
			ActorID:     "actor",
			EntityType:  "thing",
			EntityID:    fmt.Sprintf("t-%d", i),
		})
	}
	badSeq, err := VerifyChain(ctx, pool)
	if err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if badSeq != 0 {
		t.Errorf("VerifyChain badSeq = %d, want 0", badSeq)
	}
}

// --- Test 7: VerifyChain on an empty AdminAuditLog. A freshly-bootstrapped
// Hub will run the audit-chain-verify job before any admin action has
// produced a row; the helper must treat an empty table as "no break,
// nothing to verify" instead of erroring or returning a sentinel.

func TestVerifyChain_EmptyTable(t *testing.T) {
	pool := chainTestPool(t)
	defer pool.Close()
	ctx := context.Background()

	truncateAuditLog(t, ctx, pool)

	badSeq, err := VerifyChain(ctx, pool)
	if err != nil {
		t.Fatalf("VerifyChain on empty table: err = %v, want nil", err)
	}
	if badSeq != 0 {
		t.Errorf("VerifyChain on empty table: badSeq = %d, want 0", badSeq)
	}
}

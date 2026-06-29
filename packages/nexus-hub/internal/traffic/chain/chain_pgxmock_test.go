package chain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/goccy/go-json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
)

// chain_pgxmock_test.go drives NextHash and VerifyChain through pgxmock so
// the full statement set is covered without a live PostgreSQL. The existing
// chain_test.go suite stays gated behind NEXUS_DESTRUCTIVE_TESTS=1 because
// the chain integrity assertions there require TRUNCATE on AdminAuditLog
// (the chain is a global table property). pgxmock-based tests do not touch
// any real table, so they run in the default `go test` pass and lift the
// per-package coverage above the 95% threshold.
//
// Per binding [[tests-only-own-data]]: these tests own zero real rows and
// therefore cannot violate the no-cross-test-data rule.

// computeHashLink mirrors the hash chain rule from production NextHash /
// VerifyChain: SHA-256(prev || hashInput) → integrityHash. Tests use this
// to build the exact integrityHash value pgxmock should return for the
// chain-head SELECT (so the next NextHash call's chain math is well-defined
// against a known prior link).
func computeHashLink(prev string, hashInput []byte) string {
	h := sha256.New()
	if prev != "" {
		h.Write([]byte(prev))
	}
	h.Write(hashInput)
	return hex.EncodeToString(h.Sum(nil))
}

// expectAdvisoryLock pins the advisory-lock acquisition pgxmock expects on
// every NextHash call. The constant must equal chainAdvisoryLockKey
// (0x4E455841554348 = "NEXAUCH") — if production code ever changes that
// key, this expectation should fail, surfacing the silent change as a test
// break before any chain-link collision can ship.
func expectAdvisoryLock(mock pgxmock.PgxPoolIface) {
	mock.ExpectExec(`SELECT pg_advisory_xact_lock\(\$1\)`).
		WithArgs(chainAdvisoryLockKey).
		WillReturnResult(pgconn.NewCommandTag("SELECT 1"))
}

// expectGenesisChainHead pins the "no prior row" SELECT — pgx.ErrNoRows is
// the production signal that the chain has no head and the next insert is
// the genesis row.
func expectGenesisChainHead(mock pgxmock.PgxPoolIface) {
	mock.ExpectQuery(`SELECT "integrityHash" FROM "AdminAuditLog"`).
		WillReturnError(pgx.ErrNoRows)
}

// expectChainHead pins the SELECT to return a single row with the supplied
// integrityHash as the chain head. NextHash will use this as the
// previousHash for the new row.
func expectChainHead(mock pgxmock.PgxPoolIface, prev string) {
	mock.ExpectQuery(`SELECT "integrityHash" FROM "AdminAuditLog"`).
		WillReturnRows(pgxmock.NewRows([]string{"integrityHash"}).AddRow(&prev))
}

// runNextHashInTx wraps a NextHash call inside a pgxmock transaction so
// every test path goes through Begin / Exec advisory-lock / QueryRow head /
// Commit — the same shape production callers (consumer/admin_audit.go,
// thingmgr/override.go) use.
func runNextHashInTx(t *testing.T, mock pgxmock.PgxPoolIface, p HashPayload) (prev, integ string, hashInput []byte, err error) {
	t.Helper()
	ctx := context.Background()
	tx, beginErr := mock.Begin(ctx)
	if beginErr != nil {
		t.Fatalf("mock.Begin: %v", beginErr)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	prev, integ, hashInput, err = NextHash(ctx, tx, p)
	return prev, integ, hashInput, err
}

// --- NextHash: genesis row path ---------------------------------------------

func TestNextHash_PgxMock_GenesisRow(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	mock.ExpectBegin()
	expectAdvisoryLock(mock)
	expectGenesisChainHead(mock)

	p := HashPayload{
		TimestampMs: 1745539200000,
		Action:      "genesis-action",
		ActorID:     "actor-g",
		EntityType:  "thing",
		EntityID:    "t-genesis",
	}
	prev, integ, hashInput, err := runNextHashInTx(t, mock, p)
	if err != nil {
		t.Fatalf("NextHash: %v", err)
	}
	if prev != "" {
		t.Errorf("genesis previousHash = %q, want empty", prev)
	}
	if len(integ) != 64 {
		t.Errorf("integrityHash length = %d, want 64 (hex SHA-256)", len(integ))
	}
	// integrityHash must equal SHA-256(canonical bytes) with no previousHash
	// prefix on the genesis row.
	want := computeHashLink("", hashInput)
	if integ != want {
		t.Errorf("genesis integrityHash = %s, want %s", integ, want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}

// --- NextHash: non-genesis row links into prior chain head -----------------

func TestNextHash_PgxMock_ChainContinuity(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	priorHead := strings.Repeat("a", 64) // synthetic 64-hex prior hash
	mock.ExpectBegin()
	expectAdvisoryLock(mock)
	expectChainHead(mock, priorHead)

	p := HashPayload{
		TimestampMs: 1745539200001,
		Action:      "second-row",
		ActorID:     "actor-2",
		EntityType:  "thing",
		EntityID:    "t-2",
		AfterState:  json.RawMessage(`{"k":"v"}`),
	}
	prev, integ, hashInput, err := runNextHashInTx(t, mock, p)
	if err != nil {
		t.Fatalf("NextHash: %v", err)
	}
	if prev != priorHead {
		t.Errorf("previousHash = %s, want %s", prev, priorHead)
	}
	// integrityHash on a linked row = SHA-256(priorHead || canonicalBytes).
	want := computeHashLink(priorHead, hashInput)
	if integ != want {
		t.Errorf("integrityHash = %s, want %s", integ, want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}

// --- NextHash: advisory-lock Exec failure must be wrapped + surfaced -------

func TestNextHash_PgxMock_AdvisoryLockError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	wantErr := errors.New("lock manager unavailable")
	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock\(\$1\)`).
		WithArgs(chainAdvisoryLockKey).
		WillReturnError(wantErr)

	p := HashPayload{
		TimestampMs: 1, Action: "x", ActorID: "y", EntityType: "z", EntityID: "w",
	}
	_, _, _, err = runNextHashInTx(t, mock, p)
	if err == nil {
		t.Fatal("expected error when advisory lock fails")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("err chain missing original; got %v", err)
	}
	if !strings.Contains(err.Error(), "acquire chain advisory lock") {
		t.Errorf("err = %q, want wrap with 'acquire chain advisory lock'", err.Error())
	}
}

// --- NextHash: chain-head SELECT failure (non-ErrNoRows) must be wrapped ---

func TestNextHash_PgxMock_ChainHeadQueryError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	wantErr := errors.New("connection reset")
	mock.ExpectBegin()
	expectAdvisoryLock(mock)
	mock.ExpectQuery(`SELECT "integrityHash" FROM "AdminAuditLog"`).
		WillReturnError(wantErr)

	p := HashPayload{
		TimestampMs: 1, Action: "x", ActorID: "y", EntityType: "z", EntityID: "w",
	}
	_, _, _, err = runNextHashInTx(t, mock, p)
	if err == nil {
		t.Fatal("expected error when chain-head SELECT fails")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("err chain missing original; got %v", err)
	}
	if !strings.Contains(err.Error(), "read chain head") {
		t.Errorf("err = %q, want wrap with 'read chain head'", err.Error())
	}
}

// --- NextHash: pgx.ErrNoRows MUST NOT be treated as an error --------------
//
// This pins the special-case in NextHash where `errors.Is(err, pgx.ErrNoRows)`
// is the genesis signal. A future refactor that drops the errors.Is guard
// would surface here: the caller would see "read chain head" wrapping the
// no-rows sentinel instead of a clean genesis path.

func TestNextHash_PgxMock_ErrNoRowsIsGenesisSignal(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	mock.ExpectBegin()
	expectAdvisoryLock(mock)
	expectGenesisChainHead(mock) // returns pgx.ErrNoRows

	p := HashPayload{
		TimestampMs: 99, Action: "a", ActorID: "b", EntityType: "c", EntityID: "d",
	}
	prev, integ, _, err := runNextHashInTx(t, mock, p)
	if err != nil {
		t.Fatalf("ErrNoRows leaked: %v", err)
	}
	if prev != "" {
		t.Errorf("genesis previousHash should be empty after ErrNoRows, got %q", prev)
	}
	if integ == "" {
		t.Error("integrityHash empty on genesis row")
	}
}

// --- NextHash: malformed json.RawMessage in BeforeState fails marshal -----
//
// The canonicalizePayload step calls json.Marshal on the HashPayload struct,
// which embeds two json.RawMessage fields. Invalid RawMessage content causes
// json.Marshal to error (verified: "json: error calling MarshalJSON for type
// json.RawMessage"). This is the ONLY realistic path to the
// `return "", "", nil, fmt.Errorf("marshal payload: %w", err)` branch in
// production code, so we exercise it explicitly.

func TestNextHash_PgxMock_MarshalPayloadError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	mock.ExpectBegin()
	expectAdvisoryLock(mock)
	expectGenesisChainHead(mock)

	// Invalid JSON in BeforeState → json.Marshal of the HashPayload fails.
	p := HashPayload{
		TimestampMs: 1, Action: "a", ActorID: "b", EntityType: "c", EntityID: "d",
		BeforeState: json.RawMessage(`{not valid json`),
	}
	_, _, _, err = runNextHashInTx(t, mock, p)
	if err == nil {
		t.Fatal("expected marshal error for invalid json.RawMessage")
	}
	if !strings.Contains(err.Error(), "marshal payload") {
		t.Errorf("err = %q, want wrap with 'marshal payload'", err.Error())
	}
}

// --- NextHash: malformed json.RawMessage in AfterState fails marshal ------
//
// Twin of MarshalPayloadError but exercises the second RawMessage field, so
// either field being malformed flows through the same wrapped error.

func TestNextHash_PgxMock_MarshalPayloadError_AfterState(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	mock.ExpectBegin()
	expectAdvisoryLock(mock)
	expectGenesisChainHead(mock)

	p := HashPayload{
		TimestampMs: 1, Action: "a", ActorID: "b", EntityType: "c", EntityID: "d",
		AfterState: json.RawMessage(`[invalid`),
	}
	_, _, _, err = runNextHashInTx(t, mock, p)
	if err == nil {
		t.Fatal("expected marshal error for invalid AfterState json.RawMessage")
	}
	if !strings.Contains(err.Error(), "marshal payload") {
		t.Errorf("err = %q, want 'marshal payload'", err.Error())
	}
}

// --- canonicalizePayload: invariant — keys serialised lexicographically ---
//
// Strengthens the existing TestCanonicalize_StableUnderFieldReorder by
// confirming the actual byte sequence starts with the alphabetically-first
// key. Required so a refactor that drops the sort.Strings() call is caught
// even if the input map iteration order happens to come out sorted on a
// particular Go runtime.

func TestCanonicalizePayload_KeysAreLexicographic(t *testing.T) {
	p := HashPayload{
		TimestampMs:    7,
		Action:         "act",
		ActorID:        "actor",
		EntityType:     "etype",
		EntityID:       "eid",
		BeforeState:    json.RawMessage(`{"b":1}`),
		AfterState:     json.RawMessage(`{"a":2}`),
		NexusRequestID: "nr",
	}
	cb, err := canonicalizePayload(p)
	if err != nil {
		t.Fatalf("canonicalizePayload: %v", err)
	}
	// First key in canonical bytes must be "action" (alphabetically first
	// among action / actorId / afterState / beforeState / entityId /
	// entityType / nexusRequestId / timestampMs).
	if !strings.HasPrefix(string(cb), `{"action":`) {
		t.Errorf("canonical bytes do not start with \"action\" key; got: %s", cb)
	}
	// "timestampMs" is alphabetically last among populated keys, so the
	// canonical bytes must end with its value followed by `}`.
	if !strings.Contains(string(cb), `"timestampMs":7}`) {
		t.Errorf("canonical bytes do not end with timestampMs:7}; got: %s", cb)
	}
}

// --- canonicalizePayload: omitempty fields drop out of canonical bytes ----
//
// Pins the documented behaviour that an empty optional field produces the
// SAME canonical bytes whether the field was explicitly empty or omitted.
// Required so VerifyChain stays stable across HashPayload struct evolution.

func TestCanonicalizePayload_OmitemptyDropsEmptyFields(t *testing.T) {
	p := HashPayload{
		TimestampMs: 1,
		Action:      "a",
		ActorID:     "b",
		EntityType:  "c",
		// Everything else (EntityID, BeforeState, AfterState,
		// NexusRequestID) left at zero value — must NOT appear in
		// canonical bytes.
	}
	cb, err := canonicalizePayload(p)
	if err != nil {
		t.Fatalf("canonicalizePayload: %v", err)
	}
	got := string(cb)
	omitted := []string{
		"entityId", "beforeState", "afterState",
		"nexusRequestId",
	}
	for _, k := range omitted {
		if strings.Contains(got, `"`+k+`"`) {
			t.Errorf("canonical bytes contain omitted key %q: %s", k, got)
		}
	}
	// Required fields must still be present.
	for _, k := range []string{"action", "actorId", "entityType", "timestampMs"} {
		if !strings.Contains(got, `"`+k+`"`) {
			t.Errorf("canonical bytes missing required key %q: %s", k, got)
		}
	}
}

// --- canonicalizePayload: malformed RawMessage propagates marshal error ---
//
// This exercises the json.Marshal(p) error branch at the top of
// canonicalizePayload directly (without going through NextHash) — pins the
// behaviour for any future callers of the helper.

func TestCanonicalizePayload_MarshalErrorOnInvalidRawMessage(t *testing.T) {
	p := HashPayload{
		TimestampMs: 1, Action: "a", ActorID: "b",
		BeforeState: json.RawMessage(`{bad`),
	}
	_, err := canonicalizePayload(p)
	if err == nil {
		t.Fatal("expected marshal error for invalid RawMessage")
	}
	// Concrete json.Marshal failure surface — must not be swallowed.
	if !strings.Contains(err.Error(), "MarshalJSON") && !strings.Contains(err.Error(), "invalid character") {
		t.Errorf("err = %q, want marshal failure", err.Error())
	}
}

// --- VerifyChain: empty table returns (0, nil) ----------------------------

func TestVerifyChain_PgxMock_EmptyTable(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(`FROM "AdminAuditLog"`).
		WillReturnRows(pgxmock.NewRows([]string{"sequenceNumber", "previousHash", "integrityHash", "hashInput"}))

	bad, err := VerifyChain(context.Background(), mock)
	if err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if bad != 0 {
		t.Errorf("badSeq = %d, want 0 on empty table", bad)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}

// --- VerifyChain: intact 3-row chain returns (0, nil) --------------------

func TestVerifyChain_PgxMock_IntactChain(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	rows := buildSyntheticChain(3)
	mock.ExpectQuery(`FROM "AdminAuditLog"`).WillReturnRows(syntheticRowsToPgxMock(rows))

	bad, err := VerifyChain(context.Background(), mock)
	if err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if bad != 0 {
		t.Errorf("badSeq = %d, want 0", bad)
	}
}

// --- VerifyChain: query failure surfaces wrapped error -------------------

func TestVerifyChain_PgxMock_QueryError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	wantErr := errors.New("postgres down")
	mock.ExpectQuery(`FROM "AdminAuditLog"`).WillReturnError(wantErr)

	_, err = VerifyChain(context.Background(), mock)
	if err == nil {
		t.Fatal("expected error when chain query fails")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("err chain missing original; got %v", err)
	}
	if !strings.Contains(err.Error(), "query chain") {
		t.Errorf("err = %q, want wrap with 'query chain'", err.Error())
	}
}

// --- VerifyChain: row scan failure surfaces wrapped error ----------------
//
// Forces the scan path to fail by returning a row whose column types do not
// match the Scan destinations (seq is declared int64; here we feed a
// non-numeric string). pgxmock surfaces the scan error verbatim, which
// VerifyChain must wrap as "scan row: %w".

func TestVerifyChain_PgxMock_ScanError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	rows := pgxmock.NewRows([]string{"sequenceNumber", "previousHash", "integrityHash", "hashInput"}).
		AddRow("not-an-int64", (*string)(nil), strPtr("abc"), []byte("x"))
	mock.ExpectQuery(`FROM "AdminAuditLog"`).WillReturnRows(rows)

	_, err = VerifyChain(context.Background(), mock)
	if err == nil {
		t.Fatal("expected scan error")
	}
	if !strings.Contains(err.Error(), "scan row") {
		t.Errorf("err = %q, want 'scan row'", err.Error())
	}
}

// Note: the `rows.Err()` (= "iterate chain") branch in VerifyChain is
// structurally unreachable via pgxmock — RowError surfaces inside Scan,
// not from a clean iteration that errors out afterwards. The branch is
// preserved in production because pgx itself can surface late-iteration
// errors from real connections; that path needs a live pgx integration
// test to cover, which the package already gates behind
// NEXUS_DESTRUCTIVE_TESTS=1. Covered conceptually by the same code as
// query-error wrapping.

// --- VerifyChain: previousHash mismatch on non-genesis row ----------------
//
// Build a 3-row chain, then overwrite row #2's previousHash to a wrong
// 64-hex value. VerifyChain MUST return seq=2 because the linkage check
// fails before the SHA-256 recompute.

func TestVerifyChain_PgxMock_PreviousHashMismatch(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	rows := buildSyntheticChain(3)
	// Row index 1 is sequenceNumber 2 — overwrite its stored previousHash.
	wrongPrev := strings.Repeat("0", 64)
	rows[1].previousHash = &wrongPrev
	mock.ExpectQuery(`FROM "AdminAuditLog"`).WillReturnRows(syntheticRowsToPgxMock(rows))

	bad, err := VerifyChain(context.Background(), mock)
	if err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if bad != 2 {
		t.Errorf("badSeq = %d, want 2", bad)
	}
}

// --- VerifyChain: integrityHash NULL on a row is treated as tamper -------
//
// A row whose integrityHash is NULL cannot satisfy SHA-256(prev||hashInput)
// == integrityHash, so VerifyChain MUST surface that row's sequenceNumber.
// Pins the storedInteg == nil branch.

func TestVerifyChain_PgxMock_NullIntegrityHash(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	rows := buildSyntheticChain(2)
	rows[1].integrityHash = nil // null out the stored integrityHash on row #2
	mock.ExpectQuery(`FROM "AdminAuditLog"`).WillReturnRows(syntheticRowsToPgxMock(rows))

	bad, err := VerifyChain(context.Background(), mock)
	if err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if bad != 2 {
		t.Errorf("badSeq = %d, want 2 on null integrityHash", bad)
	}
}

// --- VerifyChain: integrityHash mismatch (SHA-256 recompute fails) -------
//
// Twin of the chain_test.go destructive test, but driven entirely through
// pgxmock — keeps the recompute branch covered in the default test pass.

func TestVerifyChain_PgxMock_IntegrityRecomputeFails(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	rows := buildSyntheticChain(3)
	// Mutate row #2's hashInput AFTER its integrityHash was computed →
	// recompute will not match stored hash.
	rows[1].hashInput = append(rows[1].hashInput, 0xFF)
	mock.ExpectQuery(`FROM "AdminAuditLog"`).WillReturnRows(syntheticRowsToPgxMock(rows))

	bad, err := VerifyChain(context.Background(), mock)
	if err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if bad != 2 {
		t.Errorf("badSeq = %d, want 2", bad)
	}
}

// --- VerifyChain: genesis-row mismatch (stored previousHash non-NULL) ----
//
// Genesis row MUST have stored previousHash NULL. If it's non-NULL,
// VerifyChain's running prevHash (nil) and storedPrev (non-nil) differ in
// nullness — first branch of the linkage check — and VerifyChain MUST
// return seq=1.

func TestVerifyChain_PgxMock_GenesisRowHasUnexpectedPreviousHash(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	rows := buildSyntheticChain(1)
	wrong := strings.Repeat("e", 64)
	rows[0].previousHash = &wrong
	mock.ExpectQuery(`FROM "AdminAuditLog"`).WillReturnRows(syntheticRowsToPgxMock(rows))

	bad, err := VerifyChain(context.Background(), mock)
	if err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if bad != 1 {
		t.Errorf("badSeq = %d, want 1", bad)
	}
}

// --- VerifyChain: non-genesis row with NULL previousHash is tamper -------
//
// Symmetric case of the previous test: a NON-genesis row's stored
// previousHash is NULL (chain head should have a prior hash). VerifyChain
// must catch this asymmetric nullness.

func TestVerifyChain_PgxMock_NonGenesisRowHasNullPreviousHash(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	rows := buildSyntheticChain(2)
	rows[1].previousHash = nil
	mock.ExpectQuery(`FROM "AdminAuditLog"`).WillReturnRows(syntheticRowsToPgxMock(rows))

	bad, err := VerifyChain(context.Background(), mock)
	if err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if bad != 2 {
		t.Errorf("badSeq = %d, want 2", bad)
	}
}

// --- helpers --------------------------------------------------------------

// syntheticRow mirrors the AdminAuditLog columns VerifyChain SELECTs. Used
// to build synthetic chain fixtures driven through pgxmock.
type syntheticRow struct {
	seq           int64
	previousHash  *string
	integrityHash *string
	hashInput     []byte
}

// buildSyntheticChain returns n rows wired into a valid SHA-256 chain.
// Row 1 has previousHash=nil (genesis); each subsequent row's previousHash
// is the prior row's integrityHash.
func buildSyntheticChain(n int) []syntheticRow {
	out := make([]syntheticRow, 0, n)
	var prev string
	for i := range n {
		hi := fmt.Appendf(nil, "synthetic-row-%d", i)
		integ := computeHashLink(prev, hi)
		integCopy := integ
		var prevPtr *string
		if prev != "" {
			cp := prev
			prevPtr = &cp
		}
		out = append(out, syntheticRow{
			seq:           int64(i + 1),
			previousHash:  prevPtr,
			integrityHash: &integCopy,
			hashInput:     hi,
		})
		prev = integ
	}
	return out
}

// syntheticRowsToPgxMock converts the test fixture rows into a pgxmock
// rowset matching the column order VerifyChain expects.
func syntheticRowsToPgxMock(rows []syntheticRow) *pgxmock.Rows {
	pgxRows := pgxmock.NewRows([]string{"sequenceNumber", "previousHash", "integrityHash", "hashInput"})
	for _, r := range rows {
		pgxRows = pgxRows.AddRow(r.seq, r.previousHash, r.integrityHash, r.hashInput)
	}
	return pgxRows
}

// strPtr returns a pointer to its string argument — convenience for fixtures
// that need *string column values.
func strPtr(s string) *string { return &s }

// --- VerifyChainAcked: acknowledged orphan skip + re-anchor ---------------

// buildOrphanReanchorChain mirrors the prod incident: seq 1-2 normal, seq 3 a
// chainless orphan (nil previousHash, "" integrityHash, nil hashInput — what a
// pre-fix job wrote), seq 4-5 a re-anchored genesis chain.
func buildOrphanReanchorChain() []syntheticRow {
	rows := buildSyntheticChain(2)
	empty := ""
	rows = append(rows, syntheticRow{seq: 3, previousHash: nil, integrityHash: &empty, hashInput: nil})
	var prev string
	for i, seq := range []int64{4, 5} {
		hi := fmt.Appendf(nil, "reanchor-%d", i)
		integ := computeHashLink(prev, hi)
		ic := integ
		var pp *string
		if prev != "" {
			cp := prev
			pp = &cp
		}
		rows = append(rows, syntheticRow{seq: seq, previousHash: pp, integrityHash: &ic, hashInput: hi})
		prev = integ
	}
	return rows
}

func TestVerifyChainAcked_PgxMock_SkipsOrphanReanchors(t *testing.T) {
	rows := buildOrphanReanchorChain()

	// Without acks: the orphan at seq 3 is reported as the first break.
	m1, _ := pgxmock.NewPool()
	defer m1.Close()
	m1.ExpectQuery(`FROM "AdminAuditLog"`).WillReturnRows(syntheticRowsToPgxMock(rows))
	bad, err := VerifyChain(context.Background(), m1)
	if err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if bad != 3 {
		t.Fatalf("without acks: badSeq=%d, want 3 (orphan)", bad)
	}

	// With seq 3 acknowledged: orphan skipped, seq 4 re-anchors as genesis,
	// seq 5 links to seq 4 → intact.
	m2, _ := pgxmock.NewPool()
	defer m2.Close()
	m2.ExpectQuery(`FROM "AdminAuditLog"`).WillReturnRows(syntheticRowsToPgxMock(rows))
	bad, err = VerifyChainAcked(context.Background(), m2, map[int64]struct{}{3: {}})
	if err != nil {
		t.Fatalf("VerifyChainAcked: %v", err)
	}
	if bad != 0 {
		t.Fatalf("with ack{3}: badSeq=%d, want 0 (intact)", bad)
	}
}

func TestVerifyChainAcked_PgxMock_TamperAfterOrphanStillCaught(t *testing.T) {
	rows := buildOrphanReanchorChain()
	// Tamper seq 5's hashInput so its stored integrityHash no longer matches.
	rows[4].hashInput = append(rows[4].hashInput, 0x00)

	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`FROM "AdminAuditLog"`).WillReturnRows(syntheticRowsToPgxMock(rows))
	bad, err := VerifyChainAcked(context.Background(), mock, map[int64]struct{}{3: {}})
	if err != nil {
		t.Fatalf("VerifyChainAcked: %v", err)
	}
	if bad != 5 {
		t.Errorf("badSeq=%d, want 5 — tampering after an acknowledged orphan must still be caught", bad)
	}
}

func TestVerifyChainAcked_PgxMock_NonEmptyHeadAdopted(t *testing.T) {
	// Acking a normal mid-chain row exercises normalizeHead's non-empty branch:
	// the row's (valid, non-empty) integrityHash is adopted as the running head,
	// so its successor still links → intact.
	rows := buildSyntheticChain(4)
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`FROM "AdminAuditLog"`).WillReturnRows(syntheticRowsToPgxMock(rows))
	bad, err := VerifyChainAcked(context.Background(), mock, map[int64]struct{}{2: {}})
	if err != nil {
		t.Fatalf("VerifyChainAcked: %v", err)
	}
	if bad != 0 {
		t.Errorf("badSeq=%d, want 0 — adopting a non-empty acked head must keep the chain intact", bad)
	}
}

package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/prometheus/client_golang/prometheus"

	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// The audit chain verify job walks the ENTIRE AdminAuditLog table in
// sequenceNumber order — there is no way to scope it to test-owned rows
// (a prefix DELETE would leave orphan previousHash links and the
// recompute would flag the surviving genesis row as broken). The prior
// version of this suite TRUNCATEd the table on every run, which (a)
// destroyed a developer's real audit history on every pre-commit hook
// and (b) was gated behind NEXUS_DESTRUCTIVE_TESTS=1 as a band-aid.
//
// The fix: AuditChainVerify.pool is now typed against the narrow
// audit.Queryer interface (just Query). pgxmock satisfies it directly,
// so we drive Run() with hand-rolled synthetic chain rows and never
// touch any real DB.

// chainHashLink computes the SHA-256(prev || hashInput) integrity hash
// that VerifyChain expects in the integrityHash column. Mirrors the
// production audit.NextHash logic for the test's hand-rolled rows.
func chainHashLink(prev string, hashInput []byte) string {
	h := sha256.New()
	if prev != "" {
		h.Write([]byte(prev))
	}
	h.Write(hashInput)
	return hex.EncodeToString(h.Sum(nil))
}

// fakeChainRow models a row VerifyChain SELECTs. previousHash + integrityHash
// follow the genesis-row convention: nil for the first row, otherwise the
// prior row's integrityHash.
type fakeChainRow struct {
	seq           int64
	previousHash  *string
	integrityHash string
	hashInput     []byte
}

// buildIntactChain returns n rows wired into a valid SHA-256 chain
// starting from a NULL previousHash. seqStart is the sequenceNumber of
// the first row.
func buildIntactChain(seqStart int64, n int) []fakeChainRow {
	out := make([]fakeChainRow, 0, n)
	var prev string
	for i := range n {
		hi := fmt.Appendf(nil, "row-%d-payload", i)
		integ := chainHashLink(prev, hi)
		var prevPtr *string
		if prev != "" {
			cp := prev
			prevPtr = &cp
		}
		out = append(out, fakeChainRow{
			seq:           seqStart + int64(i),
			previousHash:  prevPtr,
			integrityHash: integ,
			hashInput:     hi,
		})
		prev = integ
	}
	return out
}

// expectChainQuery wires the pgxmock to return `rows` as the result of
// the chain SELECT.
func expectChainQuery(mock pgxmock.PgxPoolIface, rows []fakeChainRow) {
	pgxRows := pgxmock.NewRows([]string{"sequenceNumber", "previousHash", "integrityHash", "hashInput"})
	for _, r := range rows {
		pgxRows = pgxRows.AddRow(r.seq, r.previousHash, &r.integrityHash, r.hashInput)
	}
	mock.ExpectQuery(`FROM "AdminAuditLog"`).WillReturnRows(pgxRows)
}

// expectNoAckedOrphans wires the system_metadata read that Run() performs
// before walking the chain to return ErrNoRows → nil ack set (verify full
// chain). Must be queued BEFORE the chain query (pgxmock is ordered).
func expectNoAckedOrphans(mock pgxmock.PgxPoolIface) {
	mock.ExpectQuery(`FROM system_metadata`).
		WithArgs(ackedOrphansKey).
		WillReturnError(pgx.ErrNoRows)
}

// expectAckedOrphans wires the system_metadata read to return the given JSON
// blob (the value column). Queue BEFORE the chain query.
func expectAckedOrphans(mock pgxmock.PgxPoolIface, valueJSON string) {
	mock.ExpectQuery(`FROM system_metadata`).
		WithArgs(ackedOrphansKey).
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow([]byte(valueJSON)))
}

// orphanChainWithReanchor builds a chain where row at index `orphanIdx` is a
// chainless orphan (NULL previousHash, empty integrityHash, empty hashInput —
// exactly what a pre-fix job wrote), and the row after it re-anchors as a fresh
// genesis (NULL previousHash, integrityHash = SHA256(hashInput)). Returns the
// rows plus the orphan's sequenceNumber.
func orphanChainWithReanchor() (rows []fakeChainRow, orphanSeq int64) {
	// seq 1,2: a normal opening chain.
	rows = buildIntactChain(1, 2)
	// seq 3: the orphan — empty everything, NULL previousHash.
	orphanSeq = 3
	rows = append(rows, fakeChainRow{seq: orphanSeq, previousHash: nil, integrityHash: "", hashInput: nil})
	// seq 4,5: re-anchored genesis chain (prev NULL → integ = SHA256(hashInput)).
	var prev string
	for i, seq := range []int64{4, 5} {
		hi := fmt.Appendf(nil, "reanchor-%d", i)
		integ := chainHashLink(prev, hi)
		var prevPtr *string
		if prev != "" {
			cp := prev
			prevPtr = &cp
		}
		rows = append(rows, fakeChainRow{seq: seq, previousHash: prevPtr, integrityHash: integ, hashInput: hi})
		prev = integ
	}
	return rows, orphanSeq
}

// TestAuditChainVerify_LoadAckedOrphansError pins the DB-failure path: a
// non-ErrNoRows error reading the acked-orphans checkpoint must surface as a
// job error (Run returns it) rather than silently falling through to a full
// verify — an unreadable ack set is ambiguous, and erroring is louder than
// silently re-alerting on a chain that may already be acknowledged.
func TestAuditChainVerify_LoadAckedOrphansError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(`FROM system_metadata`).
		WithArgs(ackedOrphansKey).
		WillReturnError(errors.New("connection refused"))

	job := NewAuditChainVerify(nil, 1*time.Hour, nil, testLogger())
	job.pool = mock

	err = job.Run(context.Background())
	if err == nil {
		t.Fatal("Run: want error when the acked-orphans read fails, got nil")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("Run error = %v, want it to surface the DB read failure", err)
	}
	if e := mock.ExpectationsWereMet(); e != nil {
		t.Errorf("unmet expectations: %v", e)
	}
}

func TestAuditChainVerify_Identity(t *testing.T) {
	job := NewAuditChainVerify(nil, 17*time.Minute, nil, testLogger())
	if job.ID() != "audit-chain-verify" {
		t.Errorf("ID = %q", job.ID())
	}
	if job.Name() == "" {
		t.Errorf("Name must not be empty")
	}
	if job.Description() == "" {
		t.Errorf("Description must not be empty")
	}
	if job.Interval() != 17*time.Minute {
		t.Errorf("Interval = %v", job.Interval())
	}
	if !job.RunOnStart() {
		t.Errorf("RunOnStart = false; expected true so the chain is checked at process start")
	}
}

func TestAuditChainVerify_IntactChain(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	// 3 rows wired into a valid chain.
	rows := buildIntactChain(1, 3)
	expectNoAckedOrphans(mock)
	expectChainQuery(mock, rows)

	job := NewAuditChainVerify(nil, 1*time.Hour, nil, testLogger())
	job.pool = mock
	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Re-run to confirm idempotency on intact chain (independent mock
	// expectation so the second Query is matched separately).
	expectNoAckedOrphans(mock)
	expectChainQuery(mock, rows)
	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("Run (second): %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}

func TestAuditChainVerify_BreakDetected(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	// 4 rows, then flip a byte in row #2's hashInput so the recomputed
	// SHA-256 no longer matches the stored integrityHash on that row.
	// VerifyChain must surface row #2's sequenceNumber as the break.
	rows := buildIntactChain(1, 4)
	rows[1].hashInput = append(rows[1].hashInput, 0x00)
	expectNoAckedOrphans(mock)
	expectChainQuery(mock, rows)

	job := NewAuditChainVerify(nil, 1*time.Hour, nil, testLogger())
	job.pool = mock

	// Run must NOT return an error — chain breaks are operational
	// signals, not job failures.
	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("Run after tamper: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}

// TestAuditChainVerify_BreakObservability pins both surfaces an SRE
// actually watches: the `audit_chain.break_detected_total` counter and
// the structured `event=audit_chain_break` slog line. The plain
// TestAuditChainVerify_BreakDetected only proves Run doesn't error on a
// tampered chain — it leaves the alert path uninstrumented.
func TestAuditChainVerify_BreakObservability(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	rows := buildIntactChain(1, 3)
	// Tamper with row #2 (seq=2): flip a byte in hashInput.
	tamperedSeq := rows[1].seq
	rows[1].hashInput = append(rows[1].hashInput, 0x00)
	expectNoAckedOrphans(mock)
	expectChainQuery(mock, rows)

	opsReg := opsmetrics.NewRegistry(prometheus.NewRegistry())
	var buf bytes.Buffer
	bufLogger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	job := NewAuditChainVerify(nil, 1*time.Hour, opsReg, bufLogger)
	job.pool = mock
	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Assert the break counter advanced by exactly 1.
	var breakValue float64
	var breakSeen bool
	for _, s := range opsReg.Collect() {
		if s.Name == "audit_chain.break_detected_total" {
			breakSeen = true
			breakValue += s.Value
		}
	}
	if !breakSeen {
		t.Fatalf("audit_chain.break_detected_total counter never observed")
	}
	if breakValue != 1 {
		t.Errorf("audit_chain.break_detected_total = %v, want 1", breakValue)
	}

	logs := buf.String()
	if !strings.Contains(logs, "event=audit_chain_break") {
		t.Errorf("slog buffer missing event=audit_chain_break; got:\n%s", logs)
	}
	wantSeq := fmt.Sprintf("first_bad_sequence_number=%d", tamperedSeq)
	if !strings.Contains(logs, wantSeq) {
		t.Errorf("slog buffer missing %q; got:\n%s", wantSeq, logs)
	}
	if !strings.Contains(logs, "level=ERROR") {
		t.Errorf("slog buffer missing level=ERROR; got:\n%s", logs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}

// TestAuditChainVerify_EmptyChain covers the boundary case: no rows in
// AdminAuditLog at all. VerifyChain returns 0 (no break), Run must log
// the `audit_chain_verified` event at INFO and Inc only the verified
// counter — not the break counter.
func TestAuditChainVerify_EmptyChain(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	expectNoAckedOrphans(mock)
	expectChainQuery(mock, nil)

	opsReg := opsmetrics.NewRegistry(prometheus.NewRegistry())
	var buf bytes.Buffer
	bufLogger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	job := NewAuditChainVerify(nil, 1*time.Hour, opsReg, bufLogger)
	job.pool = mock
	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	logs := buf.String()
	if !strings.Contains(logs, "event=audit_chain_verified") {
		t.Errorf("expected event=audit_chain_verified; got:\n%s", logs)
	}
	// Break counter must NOT have moved on an empty chain.
	for _, s := range opsReg.Collect() {
		if s.Name == "audit_chain.break_detected_total" && s.Value != 0 {
			t.Errorf("break_detected_total = %v on empty chain, want 0", s.Value)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}

// TestAuditChainVerify_AckedOrphanIntact: a chain whose only break is an
// acknowledged chainless orphan verifies as intact — no break alert fires.
// This is the prod incident's fix path (seq-1180-style orphan).
func TestAuditChainVerify_AckedOrphanIntact(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	rows, orphanSeq := orphanChainWithReanchor()
	expectAckedOrphans(mock, fmt.Sprintf(
		`[{"seq":%d,"reason":"pre-fix semantic-cache reindex job wrote a chainless row","ackedBy":"sre","ackedAt":"2026-05-28"}]`, orphanSeq))
	expectChainQuery(mock, rows)

	opsReg := opsmetrics.NewRegistry(prometheus.NewRegistry())
	var buf bytes.Buffer
	bufLogger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	job := NewAuditChainVerify(nil, time.Hour, opsReg, bufLogger)
	job.pool = mock
	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !strings.Contains(buf.String(), "event=audit_chain_verified") {
		t.Errorf("expected audit_chain_verified with acked orphan; got:\n%s", buf.String())
	}
	for _, s := range opsReg.Collect() {
		if s.Name == "audit_chain.break_detected_total" && s.Value != 0 {
			t.Errorf("break_detected_total=%v with acked orphan, want 0", s.Value)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}

// TestAuditChainVerify_AckedOrphan_TamperAfterDetected: acking the orphan must
// NOT blind the verifier to tampering AFTER it — the whole point of resuming
// verification past an acknowledged break.
func TestAuditChainVerify_AckedOrphan_TamperAfterDetected(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	rows, orphanSeq := orphanChainWithReanchor()
	tamperedSeq := rows[len(rows)-1].seq
	rows[len(rows)-1].hashInput = append(rows[len(rows)-1].hashInput, 0x00)
	expectAckedOrphans(mock, fmt.Sprintf(`[{"seq":%d}]`, orphanSeq))
	expectChainQuery(mock, rows)

	opsReg := opsmetrics.NewRegistry(prometheus.NewRegistry())
	var buf bytes.Buffer
	bufLogger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	job := NewAuditChainVerify(nil, time.Hour, opsReg, bufLogger)
	job.pool = mock
	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	logs := buf.String()
	if !strings.Contains(logs, "event=audit_chain_break") {
		t.Errorf("expected audit_chain_break for post-orphan tamper; got:\n%s", logs)
	}
	if !strings.Contains(logs, fmt.Sprintf("first_bad_sequence_number=%d", tamperedSeq)) {
		t.Errorf("expected first_bad_sequence_number=%d; got:\n%s", tamperedSeq, logs)
	}
}

// TestAuditChainVerify_CorruptAckBlob: a corrupt acked-orphans blob falls back
// to full verification (fail toward detection) and warns — it must NOT silently
// skip rows from an unparseable blob.
func TestAuditChainVerify_CorruptAckBlob(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	rows, orphanSeq := orphanChainWithReanchor()
	expectAckedOrphans(mock, `{not-valid-json`)
	expectChainQuery(mock, rows)

	var buf bytes.Buffer
	bufLogger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	job := NewAuditChainVerify(nil, time.Hour, nil, bufLogger)
	job.pool = mock
	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	logs := buf.String()
	if !strings.Contains(logs, "corrupt acked-orphans") {
		t.Errorf("expected corrupt-blob warning; got:\n%s", logs)
	}
	// Full verification ran → the orphan break is reported, not skipped.
	if !strings.Contains(logs, fmt.Sprintf("first_bad_sequence_number=%d", orphanSeq)) {
		t.Errorf("expected break at orphan seq %d after corrupt blob; got:\n%s", orphanSeq, logs)
	}
}

// TestAuditChainVerify_AckLoadError: a real DB error reading the acked-orphans
// key surfaces as a job error (Run returns it; the chain is not walked).
func TestAuditChainVerify_AckLoadError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(`FROM system_metadata`).
		WithArgs(ackedOrphansKey).
		WillReturnError(errors.New("db down"))
	// No chain query expected — Run must bail before walking the chain.
	// (We assert on the returned error rather than ExpectationsWereMet: pgxmock
	// does not mark an ExpectQuery "fulfilled" when QueryRow surfaces the error
	// via Scan — the same idiom the semantic-cache job tests use.)

	job := NewAuditChainVerify(nil, time.Hour, nil, testLogger())
	job.pool = mock
	err = job.Run(context.Background())
	if err == nil {
		t.Fatal("expected Run to return the ack-load DB error")
	}
	if !strings.Contains(err.Error(), "db down") {
		t.Errorf("Run error = %q, want it to carry the underlying 'db down'", err.Error())
	}
}

// TestAuditChainVerify_ChainQueryError locks the job-failure contract for a DB
// error during the chain walk itself: with a readable (empty) ack set, a failure
// reading AdminAuditLog is surfaced to the scheduler as an error so the run
// retries next tick rather than silently reporting the chain intact.
func TestAuditChainVerify_ChainQueryError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	expectNoAckedOrphans(mock)
	dbErr := errors.New("admin audit log unreadable")
	mock.ExpectQuery(`FROM "AdminAuditLog"`).WillReturnError(dbErr)

	job := NewAuditChainVerify(nil, 1*time.Hour, nil, testLogger())
	job.pool = mock

	if runErr := job.Run(context.Background()); !errors.Is(runErr, dbErr) {
		t.Fatalf("Run must surface the chain-query error, got %v", runErr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}

// TestAuditChainVerify_EmptyAckedOrphansVerifiesFullChain locks that an empty
// acked-orphans blob ("[]") is treated as "no acknowledgements" — the full
// chain is verified, not skipped — and an intact chain reports success.
func TestAuditChainVerify_EmptyAckedOrphansVerifiesFullChain(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	expectAckedOrphans(mock, "[]")
	expectChainQuery(mock, buildIntactChain(1, 3))

	job := NewAuditChainVerify(nil, 1*time.Hour, nil, testLogger())
	job.pool = mock

	if runErr := job.Run(context.Background()); runErr != nil {
		t.Fatalf("Run with empty ack set must verify the full chain without error, got %v", runErr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}

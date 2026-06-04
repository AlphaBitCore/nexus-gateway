// Pgxmock + miniredis-driven coverage for CredentialCircuitFlushJob.
// Supplements the DB-backed credential_circuit_flush_db_test.go which
// skips without Postgres.

package retention

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/credstate"
)

func TestCredentialCircuitFlush_Identity(t *testing.T) {
	j := NewCredentialCircuitFlush(nil, nil, "hub-1", 0, testLogger(), nil)
	if j.ID() != credCircuitFlushJobID {
		t.Errorf("ID = %q", j.ID())
	}
	if j.Name() != credCircuitFlushJobName {
		t.Errorf("Name = %q", j.Name())
	}
	if j.Description() == "" {
		t.Errorf("Description empty")
	}
	if j.Interval() != 30*time.Second {
		t.Errorf("default Interval = %v", j.Interval())
	}
	j2 := NewCredentialCircuitFlush(nil, nil, "hub-1", 90*time.Second, testLogger(), nil)
	if j2.Interval() != 90*time.Second {
		t.Errorf("custom Interval = %v", j2.Interval())
	}
}

func TestCredentialCircuitFlush_Run_NilRedis(t *testing.T) {
	j := &CredentialCircuitFlushJob{rdb: nil, hubID: "hub-1", logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestCredentialCircuitFlush_Run_EmptyDirtySet(t *testing.T) {
	_, rdb := newMiniredisRdb(t)

	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// Rehydrate runs once on first cycle — empty DB → no UPDATEs.
	mock.ExpectQuery(`FROM "Credential"`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "circuitState", "circuitReason", "circuitOpenedAt"}))
	// First cycle is also reconcile-due (zero lastReconcile) → orphan SELECT, empty.
	mock.ExpectQuery(`SELECT id FROM "Credential"`).WithArgs(credstate.CircuitClosed).
		WillReturnRows(pgxmock.NewRows([]string{"id"}))

	j := &CredentialCircuitFlushJob{
		pool:           mock,
		rdb:            rdb,
		hubID:          "hub-1",
		logger:         testLogger(),
		reconcileEvery: 5 * time.Minute,
	}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestCredentialCircuitFlush_AtomicClaim_EmptySource(t *testing.T) {
	_, rdb := newMiniredisRdb(t)
	j := &CredentialCircuitFlushJob{rdb: rdb, hubID: "hub-1", logger: testLogger()}
	moved, err := j.atomicClaim(context.Background(), "src-set", "dst-set")
	if err != nil {
		t.Fatalf("atomicClaim: %v", err)
	}
	if moved != nil {
		t.Errorf("expected nil moved, got %v", moved)
	}
}

func TestCredentialCircuitFlush_AtomicClaim_Moves(t *testing.T) {
	mini, rdb := newMiniredisRdb(t)
	mini.SAdd("src-set", "a", "b", "c")
	j := &CredentialCircuitFlushJob{rdb: rdb, hubID: "hub-1", logger: testLogger()}

	moved, err := j.atomicClaim(context.Background(), "src-set", "dst-set")
	if err != nil {
		t.Fatalf("atomicClaim: %v", err)
	}
	if len(moved) != 3 {
		t.Errorf("moved = %d, want 3", len(moved))
	}
	dst, _ := mini.SMembers("dst-set")
	if len(dst) != 3 {
		t.Errorf("dst size = %d, want 3", len(dst))
	}
}

func TestCredentialCircuitFlush_ReclaimInFlight_Empty(t *testing.T) {
	_, rdb := newMiniredisRdb(t)
	j := &CredentialCircuitFlushJob{rdb: rdb, hubID: "hub-1", logger: testLogger()}
	if err := j.reclaimInFlight(context.Background()); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
}

func TestCredentialCircuitFlush_ReclaimInFlight_MovesBack(t *testing.T) {
	mini, rdb := newMiniredisRdb(t)
	inFlightKey := credstate.InFlightSet("hub-1")
	mini.SAdd(inFlightKey, "cred-a", "cred-b")

	j := &CredentialCircuitFlushJob{rdb: rdb, hubID: "hub-1", logger: testLogger()}
	if err := j.reclaimInFlight(context.Background()); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	dirty, _ := mini.SMembers(credstate.CircuitDirtySet)
	if len(dirty) != 2 {
		t.Errorf("dirty size = %d, want 2", len(dirty))
	}
}

func TestCredentialCircuitFlush_FlushOne_ClosedPath(t *testing.T) {
	_, rdb := newMiniredisRdb(t)
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// Empty hash → CLOSED branch → single UPDATE reset.
	mock.ExpectExec(`UPDATE "Credential"`).WithArgs("cred-1", credstate.CircuitClosed).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	j := &CredentialCircuitFlushJob{pool: mock, rdb: rdb, hubID: "hub-1", logger: testLogger()}
	if err := j.flushOne(context.Background(), "cred-1"); err != nil {
		t.Fatalf("flushOne: %v", err)
	}
}

func TestCredentialCircuitFlush_FlushOne_TransitionPath(t *testing.T) {
	mini, rdb := newMiniredisRdb(t)
	mini.HSet(credstate.CircuitKey("cred-2"), credstate.CircuitFieldState, "open")
	mini.HSet(credstate.CircuitKey("cred-2"), credstate.CircuitFieldOpenReason, "rate_limit")

	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectExec(`UPDATE "Credential"`).
		WithArgs("cred-2", "open", pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	j := &CredentialCircuitFlushJob{pool: mock, rdb: rdb, hubID: "hub-1", logger: testLogger()}
	if err := j.flushOne(context.Background(), "cred-2"); err != nil {
		t.Fatalf("flushOne: %v", err)
	}
}

func TestCredentialCircuitFlush_RehydrateFromDB_Empty(t *testing.T) {
	_, rdb := newMiniredisRdb(t)
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`FROM "Credential"\s+WHERE "circuitState" != 'closed'`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "circuitState", "reason", "circuitOpenedAt", "circuitNextProbeAt"}))

	j := &CredentialCircuitFlushJob{pool: mock, rdb: rdb, hubID: "hub-1", logger: testLogger()}
	if err := j.rehydrateFromDB(context.Background(), credstate.InFlightSet("hub-1")); err != nil {
		t.Fatalf("rehydrate: %v", err)
	}
}

func TestCircuitFlushMetrics_NilSafe(t *testing.T) {
	var m *CircuitFlushMetrics // nil
	// Should not panic on nil receiver.
	m.cycle("any")
	m.flushed(5)
	m.reclaimed(3)
	m.reconciled(4)
	m.rehydrate("any")
	m.setDirty(7)
	m.observe(time.Millisecond)
	m.transition("a", "b")
}

func TestCredentialCircuitFlush_ReconcileDue(t *testing.T) {
	j := &CredentialCircuitFlushJob{reconcileEvery: 5 * time.Minute}
	// Zero lastReconcile → first cycle is always due.
	if !j.reconcileDue() {
		t.Fatal("expected reconcile due on zero lastReconcile")
	}
	j.lastReconcile = time.Now()
	if j.reconcileDue() {
		t.Fatal("expected reconcile NOT due immediately after a run")
	}
	j.lastReconcile = time.Now().Add(-6 * time.Minute)
	if !j.reconcileDue() {
		t.Fatal("expected reconcile due after the interval elapsed")
	}
}

// An orphan — non-closed DB row with NO live Redis hash — is force-closed.
func TestCredentialCircuitFlush_ReconcileOrphans_ClosesOrphan(t *testing.T) {
	_, rdb := newMiniredisRdb(t) // no circuit key set → orphan
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`SELECT id FROM "Credential"`).
		WithArgs(credstate.CircuitClosed).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("cred-orphan"))
	mock.ExpectExec(`UPDATE "Credential"`).
		WithArgs("cred-orphan", credstate.CircuitClosed).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	j := &CredentialCircuitFlushJob{pool: mock, rdb: rdb, hubID: "hub-1", logger: testLogger()}
	if err := j.reconcileOrphans(context.Background(), credstate.InFlightSet("hub-1")); err != nil {
		t.Fatalf("reconcileOrphans: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// A row whose live Redis circuit is still present is legitimately open and must
// NOT be closed — no UPDATE is issued.
func TestCredentialCircuitFlush_ReconcileOrphans_SkipsRedisPresent(t *testing.T) {
	mini, rdb := newMiniredisRdb(t)
	mini.HSet(credstate.CircuitKey("cred-live"), credstate.CircuitFieldState, "open")

	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`SELECT id FROM "Credential"`).
		WithArgs(credstate.CircuitClosed).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("cred-live"))
	// No ExpectExec — a stray UPDATE would fail ExpectationsWereMet.

	j := &CredentialCircuitFlushJob{pool: mock, rdb: rdb, hubID: "hub-1", logger: testLogger()}
	if err := j.reconcileOrphans(context.Background(), credstate.InFlightSet("hub-1")); err != nil {
		t.Fatalf("reconcileOrphans: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestCredentialCircuitFlush_ReconcileOrphans_QueryError(t *testing.T) {
	_, rdb := newMiniredisRdb(t)
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`SELECT id FROM "Credential"`).WithArgs(credstate.CircuitClosed).
		WillReturnError(errors.New("planner boom"))

	j := &CredentialCircuitFlushJob{pool: mock, rdb: rdb, hubID: "hub-1", logger: testLogger()}
	if err := j.reconcileOrphans(context.Background(), ""); err == nil {
		t.Fatal("expected the SELECT error to surface")
	}
}

func TestCredentialCircuitFlush_ReconcileOrphans_RowError(t *testing.T) {
	_, rdb := newMiniredisRdb(t)
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// A row-level error surfaces via rows.Err() after iteration.
	mock.ExpectQuery(`SELECT id FROM "Credential"`).WithArgs(credstate.CircuitClosed).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("cred-x").RowError(0, errors.New("row boom")))

	j := &CredentialCircuitFlushJob{pool: mock, rdb: rdb, hubID: "hub-1", logger: testLogger()}
	if err := j.reconcileOrphans(context.Background(), ""); err == nil {
		t.Fatal("expected the row error to surface")
	}
}

// A writeClosed failure on one orphan is logged and skipped (best-effort);
// reconcileOrphans still returns nil so the cycle continues.
func TestCredentialCircuitFlush_ReconcileOrphans_WriteError_Continues(t *testing.T) {
	_, rdb := newMiniredisRdb(t) // no key → orphan
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`SELECT id FROM "Credential"`).WithArgs(credstate.CircuitClosed).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("cred-orphan"))
	mock.ExpectExec(`UPDATE "Credential"`).WithArgs("cred-orphan", credstate.CircuitClosed).
		WillReturnError(errors.New("update boom"))

	j := &CredentialCircuitFlushJob{pool: mock, rdb: rdb, hubID: "hub-1", logger: testLogger()}
	if err := j.reconcileOrphans(context.Background(), ""); err != nil {
		t.Fatalf("write error should be swallowed, got: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// When Redis itself errors on the EXISTS probe, the row is skipped (best-effort)
// rather than wrongly closed.
func TestCredentialCircuitFlush_ReconcileOrphans_RedisExistsError(t *testing.T) {
	mini, rdb := newMiniredisRdb(t)
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`SELECT id FROM "Credential"`).WithArgs(credstate.CircuitClosed).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("cred-x"))
	// No ExpectExec — a closed Redis makes EXISTS error, so we must NOT close.
	mini.Close()

	j := &CredentialCircuitFlushJob{pool: mock, rdb: rdb, hubID: "hub-1", logger: testLogger()}
	if err := j.reconcileOrphans(context.Background(), ""); err != nil {
		t.Fatalf("redis error should be swallowed, got: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// A row claimed in the in-flight working set is being flushed concurrently; the
// reconcile must defer to the flush and not touch it.
func TestCredentialCircuitFlush_ReconcileOrphans_SkipsInFlight(t *testing.T) {
	mini, rdb := newMiniredisRdb(t)
	inFlightKey := credstate.InFlightSet("hub-1")
	mini.SAdd(inFlightKey, "cred-inflight") // claimed by the flush

	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`SELECT id FROM "Credential"`).
		WithArgs(credstate.CircuitClosed).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("cred-inflight"))
	// No ExpectExec — in-flight rows are skipped.

	j := &CredentialCircuitFlushJob{pool: mock, rdb: rdb, hubID: "hub-1", logger: testLogger()}
	if err := j.reconcileOrphans(context.Background(), inFlightKey); err != nil {
		t.Fatalf("reconcileOrphans: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

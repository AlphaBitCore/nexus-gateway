package enrollstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
)

// auditChainAdvisoryLockKey is duplicated from
// packages/nexus-hub/internal/traffic/chain — the lock key is the
// canonical "NEXAUCH" int64 every audit-chain caller acquires before
// computing the next hash.
const auditChainAdvisoryLockKey int64 = 0x4E4558_4155_4348

// expectAuditChainGenesis pins the chain-lock + chain-head SELECT for an
// audit insert in a brand-new chain (no prior rows). Mirrors the same
// helper in packages/nexus-hub/internal/fleet/manager/manager_pgxmock_test.go.
func expectAuditChainGenesis(mock pgxmock.PgxPoolIface) {
	mock.ExpectExec(`pg_advisory_xact_lock`).
		WithArgs(auditChainAdvisoryLockKey).
		WillReturnResult(pgconn.NewCommandTag("SELECT 1"))
	mock.ExpectQuery(`SELECT "integrityHash" FROM "AdminAuditLog"`).
		WillReturnError(pgx.ErrNoRows)
}

// expectInsertAdminAudit pins the AdminAuditLog INSERT — args are
// opaque (uuid + timestamp + hash + etc.). 13 placeholder args.
func expectInsertAdminAudit(mock pgxmock.PgxPoolIface) {
	mock.ExpectExec(`INSERT INTO "AdminAuditLog"`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
}

// TestWriteDeviceAssignmentAudit_HappyPath locks the canonical
// genesis-chain insert path: the helper acquires the advisory lock,
// reads the chain head (empty → genesis), then INSERTs one row.
func TestWriteDeviceAssignmentAudit_HappyPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectBegin()
	expectAuditChainGenesis(mock)
	expectInsertAdminAudit(mock)
	mock.ExpectCommit()

	s := New(mock)
	err := s.WriteDeviceAssignmentAudit(context.Background(), DeviceAssignmentAuditEntry{
		ActorID:    "nexus-hub",
		ActorLabel: "alice@example.com",
		Action:     "device-assignment.update",
		EntityID:   "thing-abc",
		AfterState: map[string]any{"device_id": "thing-abc", "user_id": "user-1"},
	})
	if err != nil {
		t.Fatalf("WriteDeviceAssignmentAudit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("FAILURE_MODE: unmet pgxmock expectations: %v", err)
	}
}

// TestWriteDeviceAssignmentAudit_WithBeforeState locks the rebind
// path — when an existing active assignment exists, the audit entry
// carries BeforeState as well as AfterState.
func TestWriteDeviceAssignmentAudit_WithBeforeState(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectBegin()
	expectAuditChainGenesis(mock)
	expectInsertAdminAudit(mock)
	mock.ExpectCommit()

	s := New(mock)
	err := s.WriteDeviceAssignmentAudit(context.Background(), DeviceAssignmentAuditEntry{
		ActorID:     "nexus-hub",
		ActorLabel:  "bob@example.com",
		Action:      "device-assignment.update",
		EntityID:    "thing-abc",
		BeforeState: map[string]any{"device_id": "thing-abc", "user_id": "user-prior"},
		AfterState:  map[string]any{"device_id": "thing-abc", "user_id": "user-new"},
	})
	if err != nil {
		t.Fatalf("WriteDeviceAssignmentAudit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestWriteDeviceAssignmentAudit_RequiresActorID locks the input
// validation: an empty ActorID is a programmer error and must surface
// loudly rather than write an unattributed audit row.
func TestWriteDeviceAssignmentAudit_RequiresActorID(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// No expectations set — the helper must short-circuit before any DB call.

	s := New(mock)
	err := s.WriteDeviceAssignmentAudit(context.Background(), DeviceAssignmentAuditEntry{
		Action:   "device-assignment.update",
		EntityID: "thing-abc",
	})
	if err == nil {
		t.Fatal("FAILURE_MODE: empty ActorID must produce an error")
	}
}

// TestWriteDeviceAssignmentAudit_RequiresAction locks the input
// validation: an empty Action is a programmer error (the chain.NextHash
// hash would still compute but VerifyChain treats empty Action as
// ambiguous across mutation types).
func TestWriteDeviceAssignmentAudit_RequiresAction(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	s := New(mock)
	err := s.WriteDeviceAssignmentAudit(context.Background(), DeviceAssignmentAuditEntry{
		ActorID:  "nexus-hub",
		EntityID: "thing-abc",
	})
	if err == nil {
		t.Fatal("FAILURE_MODE: empty Action must produce an error")
	}
}

// TestWriteDeviceAssignmentAudit_BeginError locks the tx-open failure
// path: a Begin error surfaces immediately as a wrapped error.
func TestWriteDeviceAssignmentAudit_BeginError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectBegin().WillReturnError(errors.New("conn dead"))

	s := New(mock)
	err := s.WriteDeviceAssignmentAudit(context.Background(), DeviceAssignmentAuditEntry{
		ActorID:  "nexus-hub",
		Action:   "device-assignment.update",
		EntityID: "thing-abc",
	})
	if err == nil {
		t.Fatal("FAILURE_MODE: Begin error must surface")
	}
}

// TestGetActiveDeviceAssignment_HappyPath locks the canonical
// active-row snapshot path.
func TestGetActiveDeviceAssignment_HappyPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	now := time.Now().UTC()
	loginMethod := "sso"
	ipAddress := "10.0.0.1"
	mock.ExpectQuery(`FROM "DeviceAssignment"\s+WHERE "deviceId" = \$1 AND "releasedAt" IS NULL`).
		WithArgs("thing-abc").
		WillReturnRows(pgxmock.NewRows([]string{"userId", "source", "login_method", "ip_address", "assignedAt"}).
			AddRow("user-1", "sso", &loginMethod, &ipAddress, now))

	s := New(mock)
	snap, err := s.GetActiveDeviceAssignment(context.Background(), "thing-abc")
	if err != nil {
		t.Fatalf("GetActiveDeviceAssignment: %v", err)
	}
	if snap == nil {
		t.Fatal("FAILURE_MODE: expected non-nil snapshot")
	}
	if snap.UserID != "user-1" {
		t.Errorf("UserID = %q; want user-1", snap.UserID)
	}
	if snap.Source != "sso" || snap.LoginMethod != "sso" || snap.IPAddress != "10.0.0.1" {
		t.Errorf("snap = %+v; want sso/sso/10.0.0.1", snap)
	}
}

// TestGetActiveDeviceAssignment_NoRow locks the "no prior binding"
// path: (nil, nil) so the caller can branch on first-bind.
func TestGetActiveDeviceAssignment_NoRow(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`FROM "DeviceAssignment"`).
		WithArgs("thing-fresh").
		WillReturnError(pgx.ErrNoRows)

	s := New(mock)
	snap, err := s.GetActiveDeviceAssignment(context.Background(), "thing-fresh")
	if err != nil {
		t.Fatalf("GetActiveDeviceAssignment: %v", err)
	}
	if snap != nil {
		t.Errorf("FAILURE_MODE: snap = %+v; want nil on no-row", snap)
	}
}

// TestGetActiveDeviceAssignment_EmptyThingID locks the input guard:
// empty thing_id is a programmer error or a malformed request, but
// must not run a query (would fail mock expectations).
func TestGetActiveDeviceAssignment_EmptyThingID(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	s := New(mock)
	snap, err := s.GetActiveDeviceAssignment(context.Background(), "")
	if err != nil {
		t.Errorf("FAILURE_MODE: empty thing_id should be a no-op, not an error; got %v", err)
	}
	if snap != nil {
		t.Errorf("FAILURE_MODE: snap = %+v; want nil on empty thing_id", snap)
	}
}

// TestGetActiveDeviceAssignment_QueryError locks the DB-error surface:
// errors other than ErrNoRows must propagate with a wrap.
func TestGetActiveDeviceAssignment_QueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`FROM "DeviceAssignment"`).
		WithArgs("thing-abc").
		WillReturnError(errors.New("db boom"))

	s := New(mock)
	_, err := s.GetActiveDeviceAssignment(context.Background(), "thing-abc")
	if err == nil {
		t.Fatal("FAILURE_MODE: query error must surface")
	}
}

// TestNilStringIfEmpty locks the small SQL-NULL helper.
func TestNilStringIfEmpty(t *testing.T) {
	if v := nilStringIfEmpty(""); v != nil {
		t.Errorf("empty must map to nil, got %v", v)
	}
	if v := nilStringIfEmpty("x"); v != "x" {
		t.Errorf("non-empty must round-trip, got %v", v)
	}
}

// TestWriteDeviceAssignmentAudit_InsertError pins the inner-tx INSERT
// failure path: pgxmock returns an error on Exec, the helper rolls back
// (defer) and surfaces a wrapped "insert AdminAuditLog" error.
func TestWriteDeviceAssignmentAudit_InsertError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectBegin()
	expectAuditChainGenesis(mock)
	mock.ExpectExec(`INSERT INTO "AdminAuditLog"`).
		WillReturnError(errors.New("23505 duplicate key"))
	mock.ExpectRollback()

	s := New(mock)
	err := s.WriteDeviceAssignmentAudit(context.Background(), DeviceAssignmentAuditEntry{
		ActorID:    "nexus-hub",
		Action:     "device-assignment.update",
		EntityID:   "thing-abc",
		AfterState: map[string]any{"device_id": "thing-abc"},
	})
	if err == nil {
		t.Fatal("FAILURE_MODE: INSERT error must surface")
	}
}

// TestWriteDeviceAssignmentAudit_CommitError pins the tx-commit failure
// path: pgxmock returns an error on Commit, the helper wraps it.
func TestWriteDeviceAssignmentAudit_CommitError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectBegin()
	expectAuditChainGenesis(mock)
	expectInsertAdminAudit(mock)
	mock.ExpectCommit().WillReturnError(errors.New("conn lost during commit"))

	s := New(mock)
	err := s.WriteDeviceAssignmentAudit(context.Background(), DeviceAssignmentAuditEntry{
		ActorID:  "nexus-hub",
		Action:   "device-assignment.update",
		EntityID: "thing-abc",
	})
	if err == nil {
		t.Fatal("FAILURE_MODE: Commit error must surface")
	}
}

// TestWriteDeviceAssignmentAudit_WithPriorChainHash pins the prevHash !=
// "" branch in writeDeviceAssignmentAuditTx — when the chain has a prior
// row, the previous integrityHash is forwarded into the INSERT's
// "previousHash" column rather than landing as SQL NULL.
func TestWriteDeviceAssignmentAudit_WithPriorChainHash(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectBegin()
	// Advisory lock + chain head SELECT returns a prior hash (not ErrNoRows).
	mock.ExpectExec(`pg_advisory_xact_lock`).
		WithArgs(auditChainAdvisoryLockKey).
		WillReturnResult(pgconn.NewCommandTag("SELECT 1"))
	priorHash := "abc123-prior"
	mock.ExpectQuery(`SELECT "integrityHash" FROM "AdminAuditLog"`).
		WillReturnRows(pgxmock.NewRows([]string{"integrityHash"}).AddRow(&priorHash))
	expectInsertAdminAudit(mock)
	mock.ExpectCommit()

	s := New(mock)
	err := s.WriteDeviceAssignmentAudit(context.Background(), DeviceAssignmentAuditEntry{
		ActorID:  "nexus-hub",
		Action:   "device-assignment.update",
		EntityID: "thing-xyz",
	})
	if err != nil {
		t.Fatalf("WriteDeviceAssignmentAudit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

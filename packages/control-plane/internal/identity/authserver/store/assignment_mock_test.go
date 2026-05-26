package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
)

func newAssignmentMock(t *testing.T) (pgxmock.PgxPoolIface, *store.AssignmentStore) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	return mock, store.NewAssignmentStoreWithPool(mock)
}

// TestAssignmentStore_UpsertDeviceAssignment_HappyPath asserts the
// three-step sequence (release stale → insert new → sync thing_agent)
// fires in the documented order with the supplied params bound through.
func TestAssignmentStore_UpsertDeviceAssignment_HappyPath(t *testing.T) {
	mock, s := newAssignmentMock(t)
	ctx := context.Background()

	mock.ExpectExec(`UPDATE "DeviceAssignment"`).
		WithArgs("dev_1", "u_1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectExec(`INSERT INTO "DeviceAssignment"`).
		WithArgs("dev_1", "u_1", "local", "jti_x", "10.0.0.5").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(`UPDATE thing_agent`).
		WithArgs("dev_1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	err := s.UpsertDeviceAssignment(ctx, store.UpsertDeviceAssignmentParams{
		DeviceID:    "dev_1",
		UserID:      "u_1",
		OrgID:       "org_1",
		LoginMethod: "local",
		TokenJTI:    "jti_x",
		IPAddress:   "10.0.0.5",
	})
	if err != nil {
		t.Fatalf("UpsertDeviceAssignment: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestAssignmentStore_UpsertDeviceAssignment_NoOpEmptyDeviceID asserts the
// short-circuit when DeviceID is empty — no DB calls fire (token-exchange
// for non-agent OAuth flows must not insert a phantom assignment row).
func TestAssignmentStore_UpsertDeviceAssignment_NoOpEmptyDeviceID(t *testing.T) {
	mock, s := newAssignmentMock(t)
	ctx := context.Background()
	// No Expect* calls — any DB call would fail the mock's
	// ExpectationsWereMet check at cleanup.

	err := s.UpsertDeviceAssignment(ctx, store.UpsertDeviceAssignmentParams{
		DeviceID: "",
		UserID:   "u_1",
	})
	if err != nil {
		t.Fatalf("empty DeviceID must short-circuit nil-err; got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("no DB calls expected: %v", err)
	}
}

// TestAssignmentStore_UpsertDeviceAssignment_NoOpEmptyUserID mirrors the
// empty-UserID short-circuit. Both halves of the FK pair are required.
func TestAssignmentStore_UpsertDeviceAssignment_NoOpEmptyUserID(t *testing.T) {
	mock, s := newAssignmentMock(t)
	ctx := context.Background()

	err := s.UpsertDeviceAssignment(ctx, store.UpsertDeviceAssignmentParams{
		DeviceID: "dev_1",
		UserID:   "",
	})
	if err != nil {
		t.Fatalf("empty UserID must short-circuit nil-err; got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("no DB calls expected: %v", err)
	}
}

// TestAssignmentStore_UpsertDeviceAssignment_ReleaseStaleError asserts that
// when the first UPDATE (release) fails, the error is wrapped with the
// "release stale" prefix and steps 2/3 are NOT attempted (verified via
// absence of additional Expect* calls).
func TestAssignmentStore_UpsertDeviceAssignment_ReleaseStaleError(t *testing.T) {
	mock, s := newAssignmentMock(t)
	ctx := context.Background()
	boom := errors.New("conn closed")

	mock.ExpectExec(`UPDATE "DeviceAssignment"`).
		WithArgs("dev_2", "u_2").
		WillReturnError(boom)
	// No further Expect — INSERT must not fire when release fails.

	err := s.UpsertDeviceAssignment(ctx, store.UpsertDeviceAssignmentParams{
		DeviceID: "dev_2",
		UserID:   "u_2",
	})
	if !errors.Is(err, boom) {
		t.Fatalf("expected wrapped DB err; got %v", err)
	}
	if !strings.Contains(err.Error(), "release stale") {
		t.Fatalf("expected 'release stale' wrap prefix; got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestAssignmentStore_UpsertDeviceAssignment_InsertError asserts an
// INSERT failure is wrapped with the "insert" prefix. Step 3 must not
// be attempted (no Expect for the thing_agent UPDATE).
func TestAssignmentStore_UpsertDeviceAssignment_InsertError(t *testing.T) {
	mock, s := newAssignmentMock(t)
	ctx := context.Background()
	boom := errors.New("unique violation")

	mock.ExpectExec(`UPDATE "DeviceAssignment"`).
		WithArgs("dev_3", "u_3").
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectExec(`INSERT INTO "DeviceAssignment"`).
		WithArgs("dev_3", "u_3", "oidc", "jti_y", "10.0.0.6").
		WillReturnError(boom)

	err := s.UpsertDeviceAssignment(ctx, store.UpsertDeviceAssignmentParams{
		DeviceID:    "dev_3",
		UserID:      "u_3",
		LoginMethod: "oidc",
		TokenJTI:    "jti_y",
		IPAddress:   "10.0.0.6",
	})
	if !errors.Is(err, boom) {
		t.Fatalf("expected wrapped insert err; got %v", err)
	}
	if !strings.Contains(err.Error(), "insert") {
		t.Fatalf("expected 'insert' wrap prefix; got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestAssignmentStore_UpsertDeviceAssignment_Step3FailureIgnored asserts
// the documented best-effort behavior of step 3: a thing_agent UPDATE
// failure must NOT propagate to the caller — token response must not
// break when the Hub join column lags.
func TestAssignmentStore_UpsertDeviceAssignment_Step3FailureIgnored(t *testing.T) {
	mock, s := newAssignmentMock(t)
	ctx := context.Background()
	boom := errors.New("thing_agent locked")

	mock.ExpectExec(`UPDATE "DeviceAssignment"`).
		WithArgs("dev_4", "u_4").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(`INSERT INTO "DeviceAssignment"`).
		WithArgs("dev_4", "u_4", "saml", "jti_z", "10.0.0.7").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(`UPDATE thing_agent`).
		WithArgs("dev_4").
		WillReturnError(boom)

	err := s.UpsertDeviceAssignment(ctx, store.UpsertDeviceAssignmentParams{
		DeviceID:    "dev_4",
		UserID:      "u_4",
		LoginMethod: "saml",
		TokenJTI:    "jti_z",
		IPAddress:   "10.0.0.7",
	})
	if err != nil {
		t.Fatalf("step 3 failures must be swallowed; got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

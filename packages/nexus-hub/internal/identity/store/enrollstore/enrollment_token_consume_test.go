// Tests for the atomic single-use enrollment-token consume path (F-0204).
package enrollstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

// enrollmentTokenRow builds a pgxmock row matching enrollmentTokenColumns:
// id, token_hash, thing_type, thing_id, label, status, expires_at, used_at,
// metadata, created_by, created_at.
func enrollmentTokenRow(id, status string, expiresAt time.Time) *pgxmock.Rows {
	return pgxmock.NewRows([]string{
		"id", "token_hash", "thing_type", "thing_id", "label",
		"status", "expires_at", "used_at", "metadata", "created_by", "created_at",
	}).AddRow(
		id, "deadbeef", "agent", (*string)(nil), "lab",
		status, expiresAt, (*time.Time)(nil), []byte(nil), (*string)(nil), time.Now(),
	)
}

// ConsumeEnrollmentToken: the happy path returns the row that the atomic
// UPDATE...RETURNING produced, proving the pending→used transition happened.
func TestConsumeEnrollmentToken_Success(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	exp := time.Now().Add(time.Hour)
	// The single statement must be an UPDATE...RETURNING that filters on
	// pending + unexpired — that is the atomicity guarantee F-0204 relies on.
	mock.ExpectQuery(`UPDATE enrollment_token`).
		WithArgs(hashTokenSHA256("enroll-raw")).
		WillReturnRows(enrollmentTokenRow("tok-1", "used", exp))

	s := New(mock)
	et, err := s.ConsumeEnrollmentToken(context.Background(), "enroll-raw")
	if err != nil {
		t.Fatalf("ConsumeEnrollmentToken: %v", err)
	}
	if et == nil || et.ID != "tok-1" {
		t.Fatalf("got %+v, want token id tok-1", et)
	}
	if et.ThingType != "agent" {
		t.Errorf("ThingType = %q, want agent (caller relies on this to pin enroll type)", et.ThingType)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// When the atomic UPDATE affects zero rows (already used, expired, or unknown
// token), RETURNING yields no row and the method MUST surface ErrAlreadyUsed —
// this is the loser-of-the-race signal the handler maps to 401.
func TestConsumeEnrollmentToken_NoRow_ReturnsAlreadyUsed(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`UPDATE enrollment_token`).
		WithArgs(hashTokenSHA256("spent")).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "token_hash", "thing_type", "thing_id", "label",
			"status", "expires_at", "used_at", "metadata", "created_by", "created_at",
		})) // zero rows

	s := New(mock)
	_, err := s.ConsumeEnrollmentToken(context.Background(), "spent")
	if !errors.Is(err, ErrAlreadyUsed) {
		t.Fatalf("err = %v, want ErrAlreadyUsed", err)
	}
}

// A DB error during consume must propagate (not be misreported as
// ErrAlreadyUsed) so a transient blip surfaces as a 500, never a silent
// "token spent".
func TestConsumeEnrollmentToken_DBError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	sentinel := errors.New("db down")
	mock.ExpectQuery(`UPDATE enrollment_token`).
		WithArgs(hashTokenSHA256("x")).
		WillReturnError(sentinel)

	s := New(mock)
	_, err := s.ConsumeEnrollmentToken(context.Background(), "x")
	if err == nil || errors.Is(err, ErrAlreadyUsed) {
		t.Fatalf("err = %v, want a non-ErrAlreadyUsed DB error", err)
	}
}

// LinkEnrollmentTokenThing records the minted thing id on the consumed row.
func TestLinkEnrollmentTokenThing_Success(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectExec(`UPDATE enrollment_token`).
		WithArgs("tok-1", "agent-abc").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	s := New(mock)
	if err := s.LinkEnrollmentTokenThing(context.Background(), "tok-1", "agent-abc"); err != nil {
		t.Fatalf("LinkEnrollmentTokenThing: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// LinkEnrollmentTokenThing returns ErrNotFound when no row matched so the
// caller can log the (non-fatal) gap.
func TestLinkEnrollmentTokenThing_NotFound(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectExec(`UPDATE enrollment_token`).
		WithArgs("missing", "agent-abc").
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))

	s := New(mock)
	if err := s.LinkEnrollmentTokenThing(context.Background(), "missing", "agent-abc"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestLinkEnrollmentTokenThing_DBError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectExec(`UPDATE enrollment_token`).
		WithArgs("tok-1", "agent-abc").
		WillReturnError(errors.New("boom"))

	s := New(mock)
	if err := s.LinkEnrollmentTokenThing(context.Background(), "tok-1", "agent-abc"); err == nil {
		t.Fatal("expected DB error to propagate")
	}
}

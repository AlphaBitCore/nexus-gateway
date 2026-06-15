// Service-level tests for the atomic single-use consume flow (F-0204).
package enrollment

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

// ConsumeToken maps the store's atomic consume result into the API Token shape,
// preserving the authoritative ThingType the handler uses to pin enroll type.
func TestConsumeToken_Success(t *testing.T) {
	s, mock := newServiceWithMock(t)
	exp := time.Now().Add(time.Hour)
	mock.ExpectQuery(`UPDATE enrollment_token`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(enrollmentTokenCols).AddRow(
			"tok-1", "hash", "ai-gateway", (*string)(nil), "lab",
			"used", exp, (*time.Time)(nil), []byte(nil), (*string)(nil), time.Now(),
		))

	tok, err := s.ConsumeToken(context.Background(), "enroll-raw")
	if err != nil {
		t.Fatalf("ConsumeToken: %v", err)
	}
	if tok.ID != "tok-1" || tok.ThingType != "ai-gateway" {
		t.Fatalf("got {%s,%s}, want {tok-1, ai-gateway}", tok.ID, tok.ThingType)
	}
}

// A lost race (store ErrAlreadyUsed) surfaces as enrollment.ErrAlreadyUsed so
// the handler can map it to a 401 without importing the store package.
func TestConsumeToken_AlreadyUsed(t *testing.T) {
	s, mock := newServiceWithMock(t)
	mock.ExpectQuery(`UPDATE enrollment_token`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(enrollmentTokenCols)) // zero rows

	_, err := s.ConsumeToken(context.Background(), "spent")
	if !errors.Is(err, ErrAlreadyUsed) {
		t.Fatalf("err = %v, want ErrAlreadyUsed", err)
	}
}

// A genuine DB error is NOT collapsed into ErrAlreadyUsed.
func TestConsumeToken_DBError(t *testing.T) {
	s, mock := newServiceWithMock(t)
	mock.ExpectQuery(`UPDATE enrollment_token`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(errors.New("db down"))

	_, err := s.ConsumeToken(context.Background(), "x")
	if err == nil || errors.Is(err, ErrAlreadyUsed) {
		t.Fatalf("err = %v, want non-ErrAlreadyUsed DB error", err)
	}
}

func TestLinkThing_Success(t *testing.T) {
	s, mock := newServiceWithMock(t)
	mock.ExpectExec(`UPDATE enrollment_token`).
		WithArgs("tok-1", "agent-1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	if err := s.LinkThing(context.Background(), "tok-1", "agent-1"); err != nil {
		t.Fatalf("LinkThing: %v", err)
	}
}

// LinkThing translates the store's ErrNotFound into store.ErrNotFound so
// callers see a consistent sentinel.
func TestLinkThing_NotFound(t *testing.T) {
	s, mock := newServiceWithMock(t)
	mock.ExpectExec(`UPDATE enrollment_token`).
		WithArgs("missing", "agent-1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))

	if err := s.LinkThing(context.Background(), "missing", "agent-1"); err == nil {
		t.Fatal("expected ErrNotFound")
	}
}

package authstore

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
)

func TestDeleteExpiredRevokedTokens_HappyPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectExec(`DELETE FROM "RevokedToken"`).
		WillReturnResult(pgconn.NewCommandTag("DELETE 5"))

	n, err := deleteExpiredRevokedTokens(context.Background(), mock)
	if err != nil {
		t.Fatalf("deleteExpiredRevokedTokens: %v", err)
	}
	if n != 5 {
		t.Errorf("rows affected = %d; want 5", n)
	}
}

func TestDeleteExpiredRevokedTokens_ZeroDeleted(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectExec(`DELETE FROM "RevokedToken"`).
		WillReturnResult(pgconn.NewCommandTag("DELETE 0"))

	n, err := deleteExpiredRevokedTokens(context.Background(), mock)
	if err != nil {
		t.Fatalf("deleteExpiredRevokedTokens: %v", err)
	}
	if n != 0 {
		t.Errorf("rows = %d; want 0 (nothing to prune)", n)
	}
}

func TestDeleteExpiredRevokedTokens_DBError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	want := errors.New("connection reset")
	mock.ExpectExec(`DELETE FROM "RevokedToken"`).
		WillReturnError(want)

	_, err := deleteExpiredRevokedTokens(context.Background(), mock)
	if !errors.Is(err, want) {
		t.Errorf("expected wrapped error; got: %v", err)
	}
}

// TestDeleteExpiredRevokedTokens_ViaMethod tests the exported Store method
// delegates to the package-level function correctly.
func TestDeleteExpiredRevokedTokens_ViaMethod(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectExec(`DELETE FROM "RevokedToken"`).
		WillReturnResult(pgconn.NewCommandTag("DELETE 3"))

	s := New(mock)
	n, err := s.DeleteExpiredRevokedTokens(context.Background())
	if err != nil {
		t.Fatalf("Store.DeleteExpiredRevokedTokens: %v", err)
	}
	if n != 3 {
		t.Errorf("rows = %d; want 3", n)
	}
}

func TestDeleteExpiredRefreshTokens_HappyPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectExec(`DELETE FROM "RefreshToken"`).
		WillReturnResult(pgconn.NewCommandTag("DELETE 2"))

	n, err := deleteExpiredRefreshTokens(context.Background(), mock)
	if err != nil {
		t.Fatalf("deleteExpiredRefreshTokens: %v", err)
	}
	if n != 2 {
		t.Errorf("rows = %d; want 2", n)
	}
}

func TestDeleteExpiredRefreshTokens_DBError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	want := errors.New("tx aborted")
	mock.ExpectExec(`DELETE FROM "RefreshToken"`).
		WillReturnError(want)

	_, err := deleteExpiredRefreshTokens(context.Background(), mock)
	if !errors.Is(err, want) {
		t.Errorf("expected wrapped error; got: %v", err)
	}
}

func TestDeleteExpiredRefreshTokens_ViaMethod(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectExec(`DELETE FROM "RefreshToken"`).
		WillReturnResult(pgconn.NewCommandTag("DELETE 7"))

	s := New(mock)
	n, err := s.DeleteExpiredRefreshTokens(context.Background())
	if err != nil {
		t.Fatalf("Store.DeleteExpiredRefreshTokens: %v", err)
	}
	if n != 7 {
		t.Errorf("rows = %d; want 7", n)
	}
}

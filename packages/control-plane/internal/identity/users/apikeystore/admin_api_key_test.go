package apikeystore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

// TestFindAPIKeyByHash_StatusFiltering verifies that the apikeystore folds
// the row's lifecycle status into the Enabled flag the middleware reads.
// Without this fold, status='expired' or 'unavailable' keys would still be
// accepted as long as enabled=true — the binding requirement from the rotate
// migration is that auth middleware accepts ONLY status IN ('active',
// 'rotating').
func TestFindAPIKeyByHash_StatusFiltering(t *testing.T) {
	farFuture := time.Now().Add(24 * time.Hour)
	cases := []struct {
		name        string
		status      string
		dbEnabled   bool
		wantEnabled bool
	}{
		{"active accepted", "active", true, true},
		{"rotating accepted (so callers can swap keys during rotation)", "rotating", true, true},
		{"expired rejected even when enabled=true", "expired", true, false},
		{"unavailable rejected even when enabled=true", "unavailable", true, false},
		{"db enabled=false stays disabled regardless of status", "active", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock, err := pgxmock.NewPool()
			if err != nil {
				t.Fatalf("pgxmock.NewPool: %v", err)
			}
			defer mock.Close()

			rows := pgxmock.NewRows([]string{
				"id", "name", "enabled", "status", "expiresAt", "ownerUserId",
				"u_id", "u_displayName", "u_enabled",
			}).AddRow(
				"k1", "Key", tc.dbEnabled, tc.status, &farFuture, (*string)(nil),
				(*string)(nil), (*string)(nil), (*bool)(nil),
			)
			mock.ExpectQuery(`SELECT k.id, k.name, k.enabled, k.status`).
				WithArgs("hash-x").
				WillReturnRows(rows)

			s := New(mock)
			got, err := s.FindAPIKeyByHash(context.Background(), "hash-x")
			if err != nil {
				t.Fatalf("FindAPIKeyByHash: %v", err)
			}
			if got == nil {
				t.Fatal("FindAPIKeyByHash returned nil")
				return
			}
			if got.Enabled != tc.wantEnabled {
				t.Errorf("Enabled=%v want %v (status=%q dbEnabled=%v)",
					got.Enabled, tc.wantEnabled, tc.status, tc.dbEnabled)
			}
			if got.Status != tc.status {
				t.Errorf("Status=%q want %q (raw status must round-trip for audit)", got.Status, tc.status)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet expectations: %v", err)
			}
		})
	}
}

// TestFindAPIKeyByHash_PastExpiry verifies that an active+enabled key past
// its expiresAt is still treated as disabled by the middleware. This rule
// pre-dates the rotation work but lives next door to the new status fold;
// covering it here pins both branches of the same return path.
func TestFindAPIKeyByHash_PastExpiry(t *testing.T) {
	past := time.Now().Add(-1 * time.Hour)
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	rows := pgxmock.NewRows([]string{
		"id", "name", "enabled", "status", "expiresAt", "ownerUserId",
		"u_id", "u_displayName", "u_enabled",
	}).AddRow(
		"k1", "Key", true, "active", &past, (*string)(nil),
		(*string)(nil), (*string)(nil), (*bool)(nil),
	)
	mock.ExpectQuery(`SELECT k.id, k.name, k.enabled, k.status`).
		WithArgs("hash-y").
		WillReturnRows(rows)

	s := New(mock)
	got, err := s.FindAPIKeyByHash(context.Background(), "hash-y")
	if err != nil {
		t.Fatalf("FindAPIKeyByHash: %v", err)
	}
	if got == nil {
		t.Fatal("FindAPIKeyByHash returned nil")
		return
	}
	if got.Enabled {
		t.Error("Enabled=true but key is past expiresAt; expected fold to false")
	}
}

// TestFindByKeyHash_DelegatesToFindAPIKeyByHash pins the middleware
// adapter — it must call through to FindAPIKeyByHash unchanged so a
// single source-of-truth lookup query is exercised.
func TestFindByKeyHash_DelegatesToFindAPIKeyByHash(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	rows := pgxmock.NewRows([]string{
		"id", "name", "enabled", "status", "expiresAt", "ownerUserId",
		"u_id", "u_displayName", "u_enabled",
	}).AddRow(
		"k1", "MW Key", true, "active", (*time.Time)(nil), (*string)(nil),
		(*string)(nil), (*string)(nil), (*bool)(nil),
	)
	mock.ExpectQuery(`SELECT k.id, k.name, k.enabled, k.status`).
		WithArgs("mw-hash").
		WillReturnRows(rows)

	s := New(mock)
	got, err := s.FindByKeyHash(context.Background(), "mw-hash")
	if err != nil {
		t.Fatalf("FindByKeyHash: %v", err)
	}
	if got == nil || got.ID != "k1" {
		t.Fatalf("FindByKeyHash result = %+v, want id=k1", got)
	}
}

// TestFindAPIKeyByHash_QueryError covers the wrapped-error branch — a
// generic DB error must surface so ops can see the real cause instead
// of getting masked as "not found".
func TestFindAPIKeyByHash_QueryError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(`SELECT k.id, k.name, k.enabled, k.status`).
		WithArgs("h").
		WillReturnError(errors.New("conn refused"))

	s := New(mock)
	_, err = s.FindAPIKeyByHash(context.Background(), "h")
	if err == nil {
		t.Fatal("expected wrapped error from query failure")
	}
}

// TestFindAPIKeyByHash_NotFound covers the ErrNoRows branch — the store
// must return (nil, nil) so the middleware can render a clean 401 instead
// of bubbling a DB error.
func TestFindAPIKeyByHash_NotFound(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(`SELECT k.id, k.name, k.enabled, k.status`).
		WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)

	s := New(mock)
	got, err := s.FindAPIKeyByHash(context.Background(), "missing")
	if err != nil {
		t.Fatalf("err = %v, want nil on no-rows", err)
	}
	if got != nil {
		t.Fatalf("got = %+v, want nil on no-rows", got)
	}
}

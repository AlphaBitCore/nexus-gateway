package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
)

func newUserMock(t *testing.T) (pgxmock.PgxPoolIface, *store.UserStore) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	return mock, store.NewUserStoreWithPool(mock)
}

// TestUserStore_GetByEmail_HappyPath asserts the (id, passwordHash, source,
// disabledAt) tuple is returned in scan-order with disabledAt left nil
// when the column is NULL.
func TestUserStore_GetByEmail_HappyPath(t *testing.T) {
	mock, s := newUserMock(t)
	ctx := context.Background()

	mock.ExpectQuery(`SELECT id, COALESCE\("passwordHash", ''\), source, "disabledAt"`).
		WithArgs("alice@nexus.ai").
		WillReturnRows(pgxmock.NewRows([]string{"id", "passwordHash", "source", "disabledAt"}).
			AddRow("u_1", "argon2id$hash", "local", (*time.Time)(nil)))

	id, pwd, source, disabledAt, err := s.GetByEmail(ctx, "alice@nexus.ai")
	if err != nil {
		t.Fatalf("GetByEmail: %v", err)
	}
	if id != "u_1" || pwd != "argon2id$hash" || source != "local" || disabledAt != nil {
		t.Fatalf("unexpected result: id=%q pwd=%q source=%q disabledAt=%v", id, pwd, source, disabledAt)
	}
}

// TestUserStore_GetByEmail_DisabledAtPopulated asserts the disabledAt
// pointer round-trips when the user has been disabled — auth handlers
// rely on this to refuse login after admin block.
func TestUserStore_GetByEmail_DisabledAtPopulated(t *testing.T) {
	mock, s := newUserMock(t)
	ctx := context.Background()
	disabled := time.Unix(1_700_000_000, 0).UTC()

	mock.ExpectQuery(`SELECT id, COALESCE`).
		WithArgs("blocked@nexus.ai").
		WillReturnRows(pgxmock.NewRows([]string{"id", "passwordHash", "source", "disabledAt"}).
			AddRow("u_blocked", "argon2id$hash", "local", &disabled))

	_, _, _, got, err := s.GetByEmail(ctx, "blocked@nexus.ai")
	if err != nil {
		t.Fatalf("GetByEmail: %v", err)
	}
	if got == nil || !got.Equal(disabled) {
		t.Fatalf("disabledAt not round-tripped: %v", got)
	}
}

// TestUserStore_GetByEmail_NotFound asserts pgx.ErrNoRows is mapped to
// the sentinel — auth handlers depend on this to return invalid_grant.
func TestUserStore_GetByEmail_NotFound(t *testing.T) {
	mock, s := newUserMock(t)
	ctx := context.Background()

	mock.ExpectQuery(`SELECT id, COALESCE`).
		WithArgs("nobody@nexus.ai").
		WillReturnError(pgx.ErrNoRows)

	id, pwd, source, da, err := s.GetByEmail(ctx, "nobody@nexus.ai")
	if id != "" || pwd != "" || source != "" || da != nil {
		t.Fatalf("on not-found expected zero values; got id=%q pwd=%q source=%q da=%v", id, pwd, source, da)
	}
	if !errors.Is(err, store.ErrUserNotFound) {
		t.Fatalf("expected ErrUserNotFound; got %v", err)
	}
}

// TestUserStore_GetByEmail_GenericError asserts non-ErrNoRows scan
// failures are surfaced as-is so logs reveal the underlying outage
// rather than masquerading as user-not-found.
func TestUserStore_GetByEmail_GenericError(t *testing.T) {
	mock, s := newUserMock(t)
	ctx := context.Background()
	boom := errors.New("conn closed")

	mock.ExpectQuery(`SELECT id, COALESCE`).
		WithArgs("err@nexus.ai").
		WillReturnError(boom)

	id, pwd, _, _, err := s.GetByEmail(ctx, "err@nexus.ai")
	if id != "" || pwd != "" {
		t.Fatalf("on error expected zero id/pwd; got id=%q pwd=%q", id, pwd)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("expected generic error passthrough; got %v", err)
	}
	if errors.Is(err, store.ErrUserNotFound) {
		t.Fatal("generic error must not be mapped to ErrUserNotFound")
	}
}

// TestUserStore_GetByID_HappyPath asserts every column lands on the User
// struct including the optional Email and BreakGlass + LastLoginAt fields.
func TestUserStore_GetByID_HappyPath(t *testing.T) {
	mock, s := newUserMock(t)
	ctx := context.Background()
	email := "id@nexus.ai"
	last := time.Unix(1_700_000_100, 0).UTC()

	mock.ExpectQuery(`SELECT id, email, "displayName"`).
		WithArgs("u_1").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "email", "displayName", "passwordHash", "disabledAt", "breakGlass", "lastLoginAt",
		}).AddRow("u_1", &email, "ID User", "argon2id$h", (*time.Time)(nil), true, &last))

	u, err := s.GetByID(ctx, "u_1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if u.ID != "u_1" || u.DisplayName != "ID User" || u.PasswordHash != "argon2id$h" {
		t.Fatalf("unexpected user: %+v", u)
	}
	if u.Email == nil || *u.Email != email {
		t.Fatalf("email not round-tripped: %v", u.Email)
	}
	if !u.BreakGlass {
		t.Fatal("breakGlass should be true")
	}
	if u.LastLoginAt == nil || !u.LastLoginAt.Equal(last) {
		t.Fatalf("lastLoginAt not round-tripped: %v", u.LastLoginAt)
	}
}

// TestUserStore_GetByID_NotFound asserts pgx.ErrNoRows -> ErrUserNotFound.
func TestUserStore_GetByID_NotFound(t *testing.T) {
	mock, s := newUserMock(t)
	ctx := context.Background()

	mock.ExpectQuery(`SELECT id, email, "displayName"`).
		WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)

	u, err := s.GetByID(ctx, "missing")
	if u != nil {
		t.Fatalf("user should be nil on not-found; got %+v", u)
	}
	if !errors.Is(err, store.ErrUserNotFound) {
		t.Fatalf("expected ErrUserNotFound; got %v", err)
	}
}

// TestUserStore_GetByID_GenericError asserts non-ErrNoRows surfaces
// the underlying error verbatim.
func TestUserStore_GetByID_GenericError(t *testing.T) {
	mock, s := newUserMock(t)
	ctx := context.Background()
	boom := errors.New("disk full")

	mock.ExpectQuery(`SELECT id, email`).
		WithArgs("u_boom").
		WillReturnError(boom)

	u, err := s.GetByID(ctx, "u_boom")
	if u != nil {
		t.Fatalf("user should be nil on err; got %+v", u)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("expected generic err; got %v", err)
	}
	if errors.Is(err, store.ErrUserNotFound) {
		t.Fatal("generic error must not be mapped to ErrUserNotFound")
	}
}

// TestUserStore_TouchLastLogin_Success asserts the UPDATE fires with the
// correct id arg and a successful tag is treated as nil error.
func TestUserStore_TouchLastLogin_Success(t *testing.T) {
	mock, s := newUserMock(t)
	ctx := context.Background()

	mock.ExpectExec(`UPDATE "NexusUser" SET "lastLoginAt" = NOW\(\) WHERE id = \$1`).
		WithArgs("u_touch").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	if err := s.TouchLastLogin(ctx, "u_touch"); err != nil {
		t.Fatalf("TouchLastLogin: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestUserStore_TouchLastLogin_DBError asserts a DB-layer error is
// returned to the caller — token-issuance handlers log this so a silent
// nil-return would hide write-path outages.
func TestUserStore_TouchLastLogin_DBError(t *testing.T) {
	mock, s := newUserMock(t)
	ctx := context.Background()
	boom := errors.New("deadlock")

	mock.ExpectExec(`UPDATE "NexusUser"`).
		WithArgs("u_err").
		WillReturnError(boom)

	if err := s.TouchLastLogin(ctx, "u_err"); !errors.Is(err, boom) {
		t.Fatalf("expected DB err to surface; got %v", err)
	}
}

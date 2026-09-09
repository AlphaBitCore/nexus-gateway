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
// disposition) tuple is returned in scan-order, and that an ordinary active row
// is permitted. The ExpectQuery regex names BOTH account-state columns on
// purpose: pgxmock replays columns by position and never executes the SQL, so
// dropping `status` from the SELECT would otherwise leave every test here green
// while the store went back to enforcing on the column with no writer.
func TestUserStore_GetByEmail_HappyPath(t *testing.T) {
	mock, s := newUserMock(t)
	ctx := context.Background()

	mock.ExpectQuery(`SELECT id, COALESCE\("passwordHash", ''\), source, status, "disabledAt"`).
		WithArgs("alice@nexus.ai").
		WillReturnRows(pgxmock.NewRows([]string{"id", "passwordHash", "source", "status", "disabledAt"}).
			AddRow("u_1", "argon2id$hash", "local", "active", (*time.Time)(nil)))

	id, pwd, source, account, err := s.GetByEmail(ctx, "alice@nexus.ai")
	if err != nil {
		t.Fatalf("GetByEmail: %v", err)
	}
	if id != "u_1" || pwd != "argon2id$hash" || source != "local" || account.Blocked() {
		t.Fatalf("unexpected result: id=%q pwd=%q source=%q blocked=%v reason=%q",
			id, pwd, source, account.Blocked(), account.Reason())
	}
}

// TestUserStore_GetByEmail_DisabledAtBlocks asserts a non-NULL disabledAt
// resolves to a refusal. Nothing in the tree writes that column today, so this
// arm covers a row disabled by hand rather than through a product surface.
func TestUserStore_GetByEmail_DisabledAtBlocks(t *testing.T) {
	mock, s := newUserMock(t)
	ctx := context.Background()
	disabled := time.Unix(1_700_000_000, 0).UTC()

	mock.ExpectQuery(`SELECT id, COALESCE`).
		WithArgs("blocked@nexus.ai").
		WillReturnRows(pgxmock.NewRows([]string{"id", "passwordHash", "source", "status", "disabledAt"}).
			AddRow("u_blocked", "argon2id$hash", "local", "active", &disabled))

	_, _, _, got, err := s.GetByEmail(ctx, "blocked@nexus.ai")
	if err != nil {
		t.Fatalf("GetByEmail: %v", err)
	}
	if !got.Blocked() {
		t.Fatalf("a row with a disabledAt timestamp was reported as permitted")
	}
}

// TestUserStore_GetByEmail_SuspendedStatusBlocks is the production shape: every
// surface that disables an account writes status='suspended' and leaves
// disabledAt NULL. A store that reported this row as permitted is exactly how a
// suspended employee kept signing in.
func TestUserStore_GetByEmail_SuspendedStatusBlocks(t *testing.T) {
	mock, s := newUserMock(t)
	ctx := context.Background()

	mock.ExpectQuery(`SELECT id, COALESCE`).
		WithArgs("suspended@nexus.ai").
		WillReturnRows(pgxmock.NewRows([]string{"id", "passwordHash", "source", "status", "disabledAt"}).
			AddRow("u_susp", "argon2id$hash", "local", "suspended", (*time.Time)(nil)))

	_, _, _, got, err := s.GetByEmail(ctx, "suspended@nexus.ai")
	if err != nil {
		t.Fatalf("GetByEmail: %v", err)
	}
	if !got.Blocked() {
		t.Fatalf("status='suspended' with a NULL disabledAt was reported as permitted")
	}
	if got.Reason() != "suspended" {
		t.Fatalf("reason: got %q, want the status that refused it", got.Reason())
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

	id, pwd, source, account, err := s.GetByEmail(ctx, "nobody@nexus.ai")
	if id != "" || pwd != "" || source != "" || account.Blocked() || account.Reason() != "" {
		t.Fatalf("on not-found expected zero values; got id=%q pwd=%q source=%q blocked=%v reason=%q",
			id, pwd, source, account.Blocked(), account.Reason())
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

	mock.ExpectQuery(`SELECT id, email, "displayName", COALESCE\("passwordHash", ''\), status, "disabledAt"`).
		WithArgs("u_1").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "email", "displayName", "passwordHash", "status", "disabledAt", "breakGlass", "lastLoginAt",
		}).AddRow("u_1", &email, "ID User", "argon2id$h", "active", (*time.Time)(nil), true, &last))

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

package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// UserPgxPool is the minimum pgx pool surface UserStore methods need.
// The concrete *pgxpool.Pool satisfies it in production; pgxmock's
// PgxPoolIface satisfies it in tests. Mirrors the PgxPool convention
// from packages/control-plane/internal/store/db.go.
type UserPgxPool interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// User mirrors the NexusUser columns the auth server needs. IAM permissions
// are resolved at enforcement time from role-policies + directly-attached
// policies keyed by user id; this struct only carries identity fields needed
// for auth (id, email, passwordHash, account disposition).
//
// Auth is a resolved verdict, not a raw column. The struct deliberately
// exposes no status / disabledAt field: a gate cannot compare the wrong one
// against nil when neither is reachable.
type User struct {
	ID           string
	Email        *string
	DisplayName  string
	PasswordHash string
	Auth         AuthDisposition
	BreakGlass   bool
	LastLoginAt  *time.Time
}

// UserStore reads NexusUser rows for the auth server.
type UserStore struct{ db UserPgxPool }

// NewUserStore returns a UserStore backed by the supplied pool.
func NewUserStore(db *pgxpool.Pool) *UserStore { return &UserStore{db: db} }

// NewUserStoreWithPool is the test-only constructor accepting any
// UserPgxPool implementation (notably pgxmock.PgxPoolIface). Production
// callers must use NewUserStore.
func NewUserStoreWithPool(db UserPgxPool) *UserStore { return &UserStore{db: db} }

// ErrUserNotFound is returned when a NexusUser lookup misses.
var ErrUserNotFound = errors.New("nexus_user: not found")

// GetByEmail returns (userID, passwordHash, source, authDisposition, err).
// passwordHash is the empty string when the column is NULL (e.g. SSO-only
// user). source is the provisioning origin ("local" | "oidc" | "saml" | "scim") and is
// never empty — the NexusUser.source column defaults to "local". The login
// adapter uses source to fail closed for federated accounts that still carry a
// stale local hash, so it must be read here.
//
// The account state is returned as a resolved AuthDisposition rather than as
// the columns behind it, so no caller can enforce against one column while
// another surface writes the other.
func (s *UserStore) GetByEmail(ctx context.Context, email string) (string, string, string, AuthDisposition, error) {
	row := s.db.QueryRow(ctx,
		`SELECT id, COALESCE("passwordHash", ''), source, status, "disabledAt"
		   FROM "NexusUser"
		  WHERE email = $1`, email)
	var id, pwd, source, status string
	var disabledAt *time.Time
	if err := row.Scan(&id, &pwd, &source, &status, &disabledAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", "", AuthDisposition{}, ErrUserNotFound
		}
		return "", "", "", AuthDisposition{}, err
	}
	return id, pwd, source, NewAuthDisposition(status, disabledAt), nil
}

// GetByID returns the user row by primary key or ErrUserNotFound.
func (s *UserStore) GetByID(ctx context.Context, id string) (*User, error) {
	row := s.db.QueryRow(ctx,
		`SELECT id, email, "displayName", COALESCE("passwordHash", ''), status, "disabledAt", "breakGlass", "lastLoginAt"
		   FROM "NexusUser"
		  WHERE id = $1`, id)
	var u User
	var status string
	var disabledAt *time.Time
	if err := row.Scan(&u.ID, &u.Email, &u.DisplayName, &u.PasswordHash, &status, &disabledAt, &u.BreakGlass, &u.LastLoginAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}
	u.Auth = NewAuthDisposition(status, disabledAt)
	return &u, nil
}

// TouchLastLogin stamps lastLoginAt=NOW() on the user row.
func (s *UserStore) TouchLastLogin(ctx context.Context, id string) error {
	_, err := s.db.Exec(ctx, `UPDATE "NexusUser" SET "lastLoginAt" = NOW() WHERE id = $1`, id)
	return err
}

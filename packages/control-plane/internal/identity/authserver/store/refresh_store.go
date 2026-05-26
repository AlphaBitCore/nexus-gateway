package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RefreshTokenRow mirrors the RefreshToken row the auth server writes during
// token issuance and reads during refresh_token grant. TokenHash is the
// SHA-256 of the raw refresh token; the raw value is never persisted.
type RefreshTokenRow struct {
	JTI       string
	SessionID string
	ParentJTI string // empty when the row has no parent
	UserID    string
	ClientID  string
	DeviceID  *string
	TokenHash []byte
	UsedAt    *time.Time
	ExpiresAt time.Time
	CreatedAt time.Time
}

// RefreshPgxPool is the minimum pgx pool surface RefreshStore methods need.
// The concrete *pgxpool.Pool satisfies it in production; pgxmock's
// PgxPoolIface satisfies it in tests. Mirrors the PgxPool convention from
// packages/control-plane/internal/store/db.go and the revocation store.
type RefreshPgxPool interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// RefreshStore persists refresh tokens.
type RefreshStore struct{ db RefreshPgxPool }

// NewRefreshStore returns a RefreshStore backed by the supplied pool.
func NewRefreshStore(db *pgxpool.Pool) *RefreshStore { return &RefreshStore{db: db} }

// NewRefreshStoreWithPool is the test-only constructor accepting any
// RefreshPgxPool implementation (notably pgxmock.PgxPoolIface). Production
// callers must use NewRefreshStore.
func NewRefreshStoreWithPool(db RefreshPgxPool) *RefreshStore { return &RefreshStore{db: db} }

// ErrRefreshNotFound is returned when a refresh token lookup misses.
var ErrRefreshNotFound = errors.New("refresh_token: not found")

// Insert persists a refresh token row. ParentJTI="" is stored as SQL NULL.
func (s *RefreshStore) Insert(ctx context.Context, row *RefreshTokenRow) error {
	var parent any
	if row.ParentJTI != "" {
		parent = row.ParentJTI
	}
	_, err := s.db.Exec(ctx,
		`INSERT INTO "RefreshToken"(jti,"sessionId","parentJti","userId","clientId","deviceId","tokenHash","expiresAt")
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		row.JTI, row.SessionID, parent, row.UserID, row.ClientID, row.DeviceID, row.TokenHash, row.ExpiresAt)
	return err
}

// FindByTokenHash returns (row, found). Not-found is not an error.
// Caller must inspect UsedAt to detect replay.
func (s *RefreshStore) FindByTokenHash(ctx context.Context, hash []byte) (*RefreshTokenRow, bool, error) {
	row := s.db.QueryRow(ctx,
		`SELECT jti, "sessionId", COALESCE("parentJti"::text, ''),
		        "userId", "clientId", "deviceId", "tokenHash",
		        "usedAt", "expiresAt", "createdAt"
		   FROM "RefreshToken"
		  WHERE "tokenHash" = $1`, hash)
	var r RefreshTokenRow
	if err := row.Scan(
		&r.JTI, &r.SessionID, &r.ParentJTI,
		&r.UserID, &r.ClientID, &r.DeviceID, &r.TokenHash,
		&r.UsedAt, &r.ExpiresAt, &r.CreatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &r, true, nil
}

// MarkUsed stamps usedAt=NOW() iff it is currently NULL. Returns true when the
// row transitioned to used, false when the row was either unknown or already
// used. Callers treat the false case as replay and should revoke the session.
func (s *RefreshStore) MarkUsed(ctx context.Context, jti string) (bool, error) {
	ct, err := s.db.Exec(ctx,
		`UPDATE "RefreshToken" SET "usedAt" = NOW() WHERE jti = $1 AND "usedAt" IS NULL`, jti)
	if err != nil {
		return false, err
	}
	return ct.RowsAffected() == 1, nil
}

// DeleteExpired removes every refresh token whose expiresAt is in the past.
func (s *RefreshStore) DeleteExpired(ctx context.Context) error {
	_, err := s.db.Exec(ctx, `DELETE FROM "RefreshToken" WHERE "expiresAt" < NOW()`)
	return err
}

// DeleteBySessionID removes every refresh row in the given session chain. The
// /oauth/revoke handler calls this to terminate a session on user logout so
// all rotations (past and pending) are invalidated atomically at the DB
// boundary. A no-op (zero rows affected) is not an error.
func (s *RefreshStore) DeleteBySessionID(ctx context.Context, sessionID string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM "RefreshToken" WHERE "sessionId" = $1`, sessionID)
	return err
}

package authstore

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// pgxExec is the minimum pgx pool surface auth_cleanup needs. Satisfied
// by *pgxpool.Pool in production and pgxmock.PgxPoolIface in tests,
// mirroring the pgxQuerier pattern the catb_agent_* loaders use.
type pgxExec interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// DeleteExpiredRevokedTokens removes RevokedToken rows older than the spec
// §8.7 grace window (expiresAt + 1 day). The grace keeps entries queryable for
// a day past access-token expiry so a delayed RS catchup still sees them.
// Returns the number of rows deleted.
func (s *Store) DeleteExpiredRevokedTokens(ctx context.Context) (int64, error) {
	return deleteExpiredRevokedTokens(ctx, s.db)
}

// DeleteExpiredRefreshTokens removes RefreshToken rows whose expiresAt is in the past.
// Returns the number of rows deleted.
func (s *Store) DeleteExpiredRefreshTokens(ctx context.Context) (int64, error) {
	return deleteExpiredRefreshTokens(ctx, s.db)
}

// deleteExpiredRevokedTokens is the testable package-level form. The
// method on *Store delegates here so callers keep the existing API
// while tests inject a pgxmock pool to drive the Exec error branch.
func deleteExpiredRevokedTokens(ctx context.Context, db pgxExec) (int64, error) {
	tag, err := db.Exec(ctx, `DELETE FROM "RevokedToken" WHERE "expiresAt" < NOW() - INTERVAL '1 day'`)
	if err != nil {
		return 0, fmt.Errorf("delete expired revoked tokens: %w", err)
	}
	return tag.RowsAffected(), nil
}

// deleteExpiredRefreshTokens mirrors deleteExpiredRevokedTokens for the
// shorter (no-grace) refresh-token sweep.
func deleteExpiredRefreshTokens(ctx context.Context, db pgxExec) (int64, error) {
	tag, err := db.Exec(ctx, `DELETE FROM "RefreshToken" WHERE "expiresAt" < NOW()`)
	if err != nil {
		return 0, fmt.Errorf("delete expired refresh tokens: %w", err)
	}
	return tag.RowsAffected(), nil
}

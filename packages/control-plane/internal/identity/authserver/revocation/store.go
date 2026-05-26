package revocation

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Row mirrors the revoked_token table column-for-column. Callers must set
// RevokedAt; the Store binds it verbatim so the stored timestamp matches the
// one embedded in the published MQ Event.
type Row struct {
	ID              int64
	Scope           Scope
	TargetJTI       *string
	TargetUserID    *string
	TargetDeviceID  *string
	TargetSessionID *string
	RevokedAt       time.Time
	ExpiresAt       time.Time
	Reason          string
	Actor           *string
}

// PgxPool is the minimum pgx pool surface Store methods need. The concrete
// *pgxpool.Pool satisfies it in production; pgxmock's PgxPoolIface satisfies
// it in tests. Mirrors the PgxPool convention from
// packages/control-plane/internal/store/db.go.
type PgxPool interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store is a thin CRUD layer over the revoked_token table.
type Store struct {
	pool PgxPool
}

// NewStore binds the store to a pgx pool. Production callers pass the
// concrete *pgxpool.Pool from store.DB.Pool.
func NewStore(p *pgxpool.Pool) *Store { return &Store{pool: p} }

// NewStoreWithPool is the test-only constructor accepting any PgxPool
// implementation (notably pgxmock.PgxPoolIface). Production callers must
// use NewStore.
func NewStoreWithPool(p PgxPool) *Store { return &Store{pool: p} }

// Insert appends a row and returns the bigserial id. The caller owns the
// RevokedAt timestamp so the DB row agrees bit-for-bit with the published MQ
// Event (see Service.Revoke).
func (s *Store) Insert(ctx context.Context, r Row) (int64, error) {
	const q = `
		INSERT INTO "RevokedToken"
		("scope","targetJti","targetUserId","targetDeviceId","targetSessionId",
		 "revokedAt","expiresAt","reason","actor")
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		RETURNING "id"`
	var id int64
	err := s.pool.QueryRow(ctx, q,
		string(r.Scope), r.TargetJTI, r.TargetUserID, r.TargetDeviceID, r.TargetSessionID,
		r.RevokedAt, r.ExpiresAt, r.Reason, r.Actor,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("revocation: insert: %w", err)
	}
	return id, nil
}

// ListSince returns rows with id > sinceID, ascending, up to limit. The
// returned lastID is the max id seen or sinceID when empty, so callers can
// pipeline successive ListSince calls without losing the cursor.
func (s *Store) ListSince(ctx context.Context, sinceID int64, limit int) ([]Row, int64, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	const q = `
		SELECT "id","scope","targetJti","targetUserId","targetDeviceId","targetSessionId",
		       "revokedAt","expiresAt","reason","actor"
		FROM "RevokedToken"
		WHERE "id" > $1
		ORDER BY "id" ASC
		LIMIT $2`
	rows, err := s.pool.Query(ctx, q, sinceID, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("revocation: list: %w", err)
	}
	defer rows.Close()
	out := make([]Row, 0, limit)
	last := sinceID
	for rows.Next() {
		var r Row
		var scope string
		if err := rows.Scan(&r.ID, &scope, &r.TargetJTI, &r.TargetUserID, &r.TargetDeviceID,
			&r.TargetSessionID, &r.RevokedAt, &r.ExpiresAt, &r.Reason, &r.Actor); err != nil {
			return nil, 0, fmt.Errorf("revocation: scan: %w", err)
		}
		r.Scope = Scope(scope)
		out = append(out, r)
		if r.ID > last {
			last = r.ID
		}
	}
	return out, last, rows.Err()
}

// IsAccessTokenRevoked reports whether any *unexpired* RevokedToken row
// matches one of the access token's identifying claims at the scope it
// was revoked under. The four scope→target mappings cover every revoke
// path the Service supports:
//   - ScopeJTI     → exact token-id match (the /oauth/revoke default)
//   - ScopeUser    → any token issued to this user is revoked
//   - ScopeDevice  → any token bound to this device is revoked
//   - ScopeSession → any token within this session is revoked
//
// Pass empty strings for claims the token does not carry — they will be
// AND-ed with a non-NULL filter on the column so a NULL targetJti row
// can't false-match on an empty jti argument.
//
// Callers (today: IntrospectHandler) treat ok=true as "active=false"
// per RFC 7662 §2.2.
func (s *Store) IsAccessTokenRevoked(ctx context.Context, jti, userID, deviceID, sessionID string) (bool, error) {
	const q = `
		SELECT EXISTS(
			SELECT 1 FROM "RevokedToken"
			WHERE "expiresAt" > NOW()
			  AND (
			       (scope = 'jti'     AND "targetJti"      IS NOT NULL AND "targetJti"      = $1 AND $1 <> '')
			    OR (scope = 'user'    AND "targetUserId"   IS NOT NULL AND "targetUserId"   = $2 AND $2 <> '')
			    OR (scope = 'device'  AND "targetDeviceId" IS NOT NULL AND "targetDeviceId" = $3 AND $3 <> '')
			    OR (scope = 'session' AND "targetSessionId" IS NOT NULL AND "targetSessionId" = $4 AND $4 <> '')
			  )
		)`
	var ok bool
	if err := s.pool.QueryRow(ctx, q, jti, userID, deviceID, sessionID).Scan(&ok); err != nil {
		return false, fmt.Errorf("revocation: IsAccessTokenRevoked: %w", err)
	}
	return ok, nil
}

// DeleteExpired removes rows whose expires_at is older than now - 1d, matching
// the spec section 8.7 cleanup SQL. Returns the number of deleted rows.
func (s *Store) DeleteExpired(ctx context.Context) (int64, error) {
	const q = `DELETE FROM "RevokedToken" WHERE "expiresAt" < NOW() - INTERVAL '1 day'`
	tag, err := s.pool.Exec(ctx, q)
	if err != nil {
		return 0, fmt.Errorf("revocation: delete expired: %w", err)
	}
	return tag.RowsAffected(), nil
}

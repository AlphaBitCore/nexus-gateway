package scimstore

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type PgxPool interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type Store struct{ pool PgxPool }

func New(pool PgxPool) *Store { return &Store{pool: pool} }

// LinkUserToIdP creates a UserFederatedIdentity row tying a NexusUser
// to an external IdP. Used by SCIM CreateUser to stamp provenance on
// users that arrive via SCIM push. Idempotent — ON CONFLICT DO NOTHING.
func (s *Store) LinkUserToIdP(ctx context.Context, userID, idpID, externalSubject string, externalEmail *string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO "UserFederatedIdentity" (id, "userId", "idpId", "externalSubject", "externalEmail", "linkedAt")
		VALUES (gen_random_uuid(), $1, $2, $3, $4, NOW())
		ON CONFLICT ("idpId", "externalSubject") DO NOTHING
	`, userID, idpID, externalSubject, externalEmail)
	return err
}

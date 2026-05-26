// Package providerstore owns Provider persistence. Extracted from store/provider.go
// so handlers that only need provider CRUD can depend on this narrow package
// instead of the full *store.DB god object.
package providerstore

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PgxPool is the minimal pgx surface providerstore needs.
type PgxPool interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store owns Provider persistence.
type Store struct{ pool PgxPool }

// New constructs a Store from any PgxPool-compatible pool (production or test).
func New(pool PgxPool) *Store { return &Store{pool: pool} }

// ilikeEscaper escapes the 3 chars PostgreSQL LIKE/ILIKE treats as wildcards.
var ilikeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func escapeILIKE(s string) string { return ilikeEscaper.Replace(s) }

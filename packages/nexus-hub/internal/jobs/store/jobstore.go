// Package jobstore persists job definitions and run history to the `job`
// and `job_run` tables. The scheduler writes a row at the start of every
// execution and updates it on completion; the admin API reads the aggregate
// view and the per-job run list from here.
package jobstore

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a lookup matches zero rows.
var ErrNotFound = errors.New("jobstore: not found")

// PgxPool is the minimum pgx pool surface jobstore needs. *pgxpool.Pool
// satisfies it in production via New; pgxmock.PgxPoolIface satisfies it in
// tests via NewWithPgxPool, letting every statement be exercised without
// touching a live PostgreSQL. Mirrors the PgxPool convention from
// packages/nexus-hub/internal/storage/store, packages/nexus-hub/internal/fleet/manager,
// and packages/nexus-hub/internal/traffic/chain.
type PgxPool interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store provides access to the job + job_run tables.
type Store struct {
	db PgxPool
}

// New creates a Store backed by the given connection pool. Production
// constructor — every nexus-hub main wires the live *pgxpool.Pool here.
func New(db *pgxpool.Pool) *Store {
	if db == nil {
		// Preserve the historical behaviour of New(nil) returning a non-nil
		// Store (asserted by TestNew_NilPool) without storing a typed-nil
		// interface that would break runtime nil checks at the call sites.
		return &Store{}
	}
	return &Store{db: db}
}

// NewWithPgxPool is the test-only constructor that accepts any PgxPool
// (typically pgxmock.PgxPoolIface). Production code MUST go through New.
func NewWithPgxPool(db PgxPool) *Store {
	return &Store{db: db}
}

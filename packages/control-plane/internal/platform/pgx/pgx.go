// Package pgx provides the shared pgx pool primitives for the control-plane.
// All store packages use PgxPool as the minimal pool surface so that tests
// can substitute a pgxmock pool without a live Postgres connection.
package pgx

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PgxPool is the minimum pgx pool surface store methods need. The
// concrete *pgxpool.Pool satisfies it in production; pgxmock's
// PgxPoolIface satisfies it in tests. Mirrors the PgxPool convention
// from packages/nexus-hub/internal/store.
type PgxPool interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Close()
	Ping(ctx context.Context) error
}

// PoolConfig holds tuning parameters for the pgx connection pool.
type PoolConfig struct {
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
}

// New creates a *pgxpool.Pool from a connection string with optional pool
// tuning. The returned pool has been pinged to verify connectivity.
func New(ctx context.Context, dsn string, opts ...PoolConfig) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database config: %w", err)
	}

	if len(opts) > 0 {
		o := opts[0]
		if o.MaxConns > 0 {
			poolCfg.MaxConns = o.MaxConns
		}
		if o.MinConns > 0 {
			poolCfg.MinConns = o.MinConns
		}
		if o.MaxConnLifetime > 0 {
			poolCfg.MaxConnLifetime = o.MaxConnLifetime
		}
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

// Package store provides read-only database access for the AI gateway.
// Dashboard Backend owns all writes; the proxy only reads. All queries use
// hand-written SQL with pgx (no ORM, no sqlc per V2 convention).
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolConfig holds optional pgx pool tuning applied on top of the values
// parsed from the connection string. Mirrors the control-plane
// internal/platform/pgx.PoolConfig convention so every service tunes the
// pool the same way; each field is applied only when > 0 so a zero value
// keeps the pgx/connection-string default.
type PoolConfig struct {
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
}

// PgxPool is the minimum pgx pool surface store methods need. The
// concrete *pgxpool.Pool satisfies it in production; pgxmock's
// PgxPoolIface satisfies it in tests. Mirrors the PgxPool convention
// from packages/control-plane/internal/store and
// packages/nexus-hub/internal/store.
type PgxPool interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Close()
	Ping(ctx context.Context) error
}

// DB wraps a pgx connection pool for read-only queries.
//
// Pool is exposed as the concrete *pgxpool.Pool because sibling
// cachelayer code reads it directly (l.db.Pool.Query(...)) — and
// because production may need the concrete pool's AcquireFunc surface.
//
// pool is the internal interface-typed view that (db *DB) methods
// use for SQL. Tests construct *DB via NewWithPgxPool with a pgxmock
// pool — that sets pool only; Pool stays nil so any accidental
// handler path is a clear nil-deref.
type DB struct {
	Pool   *pgxpool.Pool
	pool   PgxPool
	rc     *rulesCache // routing rules cache
	rcOnce sync.Once
}

// New creates a DB from a PostgreSQL connection string with optional pool
// tuning. When a PoolConfig is supplied, its non-zero fields override the
// pgx defaults so the AI Gateway's hot-path pool is sized for concurrency
// instead of the pgx fallback of max(4, NumCPU).
func New(ctx context.Context, connString string, opts ...PoolConfig) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, fmt.Errorf("store: parse config: %w", err)
	}
	if len(opts) > 0 {
		o := opts[0]
		if o.MaxConns > 0 {
			cfg.MaxConns = o.MaxConns
		}
		if o.MinConns > 0 {
			cfg.MinConns = o.MinConns
		}
		if o.MaxConnLifetime > 0 {
			cfg.MaxConnLifetime = o.MaxConnLifetime
		}
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return &DB{Pool: pool, pool: pool}, nil
}

// NewWithPgxPool is the test-only constructor. Production callers go
// through New(); tests pass a pgxmock pool here so individual store
// methods can be unit-tested without a live Postgres. Pool stays nil
// so any handler path that demands the concrete type fails loudly
// instead of silently using the mock.
func NewWithPgxPool(pool PgxPool) *DB {
	return &DB{pool: pool}
}

// Close releases the connection pool.
func (db *DB) Close() {
	db.Pool.Close()
}

// GetSystemMetadata returns the JSON value for a system metadata key.
func (db *DB) GetSystemMetadata(ctx context.Context, key string) (json.RawMessage, error) {
	var val []byte
	err := db.pool.QueryRow(ctx, `SELECT value FROM system_metadata WHERE key = $1`, key).Scan(&val)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get system metadata %q: %w", key, err)
	}
	return val, nil
}

package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/hubstore"
)

type PgxPool interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type Store struct{ db PgxPool }

func New(db PgxPool) *Store { return &Store{db: db} }

// ErrNotFound and ErrAmbiguous are the shared sentinels from hubstore,
// re-exported so callers can use store.ErrNotFound for clarity while
// errors.Is comparisons with store.ErrNotFound still succeed.
var (
	ErrNotFound  = hubstore.ErrNotFound
	ErrAmbiguous = hubstore.ErrAmbiguous
)

const ConfigChangedChannel = "config_changed"

func notifyConfigChanged(ctx context.Context, tx pgx.Tx, thingID string) error {
	if _, err := tx.Exec(ctx, "SELECT pg_notify($1, $2)", ConfigChangedChannel, thingID); err != nil {
		return fmt.Errorf("pg_notify %s for %s: %w", ConfigChangedChannel, thingID, err)
	}
	return nil
}

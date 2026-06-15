package enrollstore

import (
	"context"
	"encoding/json"
	"errors"
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
// re-exported so callers can use enrollstore.ErrNotFound for clarity while
// errors.Is comparisons with store.ErrNotFound still succeed.
var (
	ErrNotFound  = hubstore.ErrNotFound
	ErrAmbiguous = hubstore.ErrAmbiguous
	// ErrAlreadyUsed is returned by ConsumeEnrollmentToken when the token
	// does not exist, has expired, or was already consumed (status != pending)
	// at the moment of the atomic UPDATE. It collapses the validate+mark race:
	// the row transition pending→used happens in a single statement,
	// so exactly one concurrent enrollment wins and the rest see ErrAlreadyUsed.
	ErrAlreadyUsed = errors.New("enrollment token already used, expired, or not found")
)

func decodeJSONB(raw []byte, target any, column string) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("decode %s jsonb: %w", column, err)
	}
	return nil
}

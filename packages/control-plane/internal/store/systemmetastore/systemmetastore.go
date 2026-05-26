// Package systemmetastore provides get/set operations on the system_metadata
// table. Extracted from store.DB so handlers that only need metadata reads can
// depend on this narrow package instead of the full *store.DB god object.
package systemmetastore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PgxPool is the minimal pgx surface needed. Matches store.PgxPool so the
// same mock can satisfy both interfaces in tests.
type PgxPool interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store owns system_metadata reads and writes.
type Store struct {
	pool PgxPool
}

// New constructs a Store from a concrete *pgxpool.Pool (production path).
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// NewFromPool constructs a Store from the PgxPool interface (test path).
func NewFromPool(pool PgxPool) *Store { return &Store{pool: pool} }

// GetSystemMetadata returns the JSON value for a system metadata key.
// Returns (nil, nil) when the key does not exist.
func (s *Store) GetSystemMetadata(ctx context.Context, key string) (json.RawMessage, error) {
	var val []byte
	err := s.pool.QueryRow(ctx, `SELECT value FROM system_metadata WHERE key = $1`, key).Scan(&val)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get system metadata %q: %w", key, err)
	}
	return val, nil
}

// SetSystemMetadata upserts a system metadata key/value pair.
func (s *Store) SetSystemMetadata(ctx context.Context, key string, value any, updatedBy string) error {
	valJSON, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal system metadata: %w", err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO system_metadata (key, value, updated_at, updated_by)
		VALUES ($1, $2, NOW(), $3)
		ON CONFLICT (key) DO UPDATE SET value = $2, updated_at = NOW(), updated_by = $3
	`, key, valJSON, updatedBy)
	return err
}

// Package systemmetastore provides get/set operations on the system_metadata
// table. Extracted from store.DB so handlers that only need metadata reads can
// depend on this narrow package instead of the full *store.DB god object.
package systemmetastore

import (
	"context"
	"errors"
	"fmt"
	"github.com/goccy/go-json"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configkey"
)

// PgxPool is the minimal pgx surface needed. Matches store.PgxPool so the
// same mock can satisfy both interfaces in tests. *pgxpool.Pool satisfies it
// directly, so production callers pass their pool straight to New.
type PgxPool interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store owns system_metadata reads and writes.
type Store struct {
	pool PgxPool
}

// New constructs a Store from any PgxPool (production *pgxpool.Pool or a mock).
func New(pool PgxPool) *Store { return &Store{pool: pool} }

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

// AuditUnknownKeys returns the system_metadata keys no code claims.
//
// The table has no schema and no owner: a row is a key and a JSON blob written
// by whichever service reached for it, and nothing ever checked that a key is
// one somebody still reads. `audit.payload` survived three months that way, and
// it was worse than dead weight — its value was the OPPOSITE of the setting in
// force and its timestamp kept moving, so anyone asking "is body capture on?"
// by the obvious name read false and stopped investigating.
//
// Aggregated in SQL rather than streamed, so this needs no Query on the pool
// interface — widening that interface for a once-per-boot audit would make
// every mock in the tree carry a method for it. The prefix families are
// filtered in Go because the registry is where "who claims this key" is
// answered, and answering it twice is how the two answers start disagreeing.
func (s *Store) AuditUnknownKeys(ctx context.Context) ([]string, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(json_agg(key ORDER BY key), '[]')::text
		   FROM system_metadata
		  WHERE NOT (key = ANY($1::text[]))`,
		configkey.SystemMetadataKeys,
	).Scan(&raw)
	if err != nil {
		return nil, fmt.Errorf("systemmetastore: audit keys: %w", err)
	}
	var present []string
	if err := json.Unmarshal(raw, &present); err != nil {
		return nil, fmt.Errorf("systemmetastore: audit keys decode: %w", err)
	}
	return configkey.UnknownSystemMetadataKeys(present), nil
}

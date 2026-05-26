package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

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

// SetSystemMetadata upserts a system metadata key/value.
func (db *DB) SetSystemMetadata(ctx context.Context, key string, value any, updatedBy string) error {
	valJSON, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal system metadata: %w", err)
	}
	_, err = db.pool.Exec(ctx, `
		INSERT INTO system_metadata (key, value, updated_at, updated_by)
		VALUES ($1, $2, NOW(), $3)
		ON CONFLICT (key) DO UPDATE SET value = $2, updated_at = NOW(), updated_by = $3
	`, key, valJSON, updatedBy)
	return err
}

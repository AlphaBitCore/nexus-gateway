package shadow

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/goccy/go-json"
	"time"
)

// CachedConfig holds a cached config entry.
type CachedConfig struct {
	Key       string          `json:"key"`
	State     json.RawMessage `json:"state"`
	Version   int64           `json:"version"`
	UpdatedAt time.Time       `json:"updatedAt"`
}

// Cache persists applied shadow config to SQLite.
type Cache struct {
	db *sql.DB
}

// NewCache creates a config cache backed by the given SQLite database.
// Creates the config_cache table if it doesn't exist.
func NewCache(db *sql.DB) (*Cache, error) {
	_, err := db.ExecContext(context.Background(), `
		CREATE TABLE IF NOT EXISTS config_cache (
			key        TEXT PRIMARY KEY,
			state      BLOB NOT NULL,
			version    INTEGER NOT NULL,
			updated_at TEXT NOT NULL
		)
	`)
	if err != nil {
		return nil, fmt.Errorf("create config_cache table: %w", err)
	}
	return &Cache{db: db}, nil
}

// Save stores the latest applied config for a key.
func (c *Cache) Save(key string, state json.RawMessage, version int64) error {
	_, err := c.db.ExecContext(context.Background(), `
		INSERT INTO config_cache (key, state, version, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET
			state = excluded.state,
			version = excluded.version,
			updated_at = excluded.updated_at
	`, key, []byte(state), version, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("save config cache %s: %w", key, err)
	}
	return nil
}

// Load retrieves a cached config entry.
func (c *Cache) Load(key string) (*CachedConfig, error) {
	var cc CachedConfig
	var stateBytes []byte
	var updatedAtStr string
	err := c.db.QueryRowContext(context.Background(), `
		SELECT key, state, version, updated_at FROM config_cache WHERE key = ?
	`, key).Scan(&cc.Key, &stateBytes, &cc.Version, &updatedAtStr)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("load config cache %s: %w", key, err)
	}
	cc.State = json.RawMessage(stateBytes)
	cc.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAtStr)
	return &cc, nil
}

// LoadAll returns all cached config entries.
func (c *Cache) LoadAll() ([]CachedConfig, error) {
	rows, err := c.db.QueryContext(context.Background(), `SELECT key, state, version, updated_at FROM config_cache`)
	if err != nil {
		return nil, fmt.Errorf("load all config cache: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var result []CachedConfig
	for rows.Next() {
		var cc CachedConfig
		var stateBytes []byte
		var updatedAtStr string
		if err := rows.Scan(&cc.Key, &stateBytes, &cc.Version, &updatedAtStr); err != nil {
			return nil, fmt.Errorf("scan config cache: %w", err)
		}
		cc.State = json.RawMessage(stateBytes)
		cc.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAtStr)
		result = append(result, cc)
	}
	return result, nil
}

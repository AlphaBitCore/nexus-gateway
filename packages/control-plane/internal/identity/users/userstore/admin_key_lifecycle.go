package userstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// --- Admin API Key CRUD ---

// AdminAPIKey represents a row (no hash exposed).
type AdminAPIKey struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	KeyPrefix     string     `json:"keyPrefix"`
	Enabled       bool       `json:"enabled"`
	Status        string     `json:"status"`
	LastUsedAt    *time.Time `json:"lastUsedAt"`
	ExpiresAt     *time.Time `json:"expiresAt"`
	RotatedAt     *time.Time `json:"rotatedAt"`
	RotatedFromID *string    `json:"rotatedFromId"`
	CreatedBy     string     `json:"createdBy"`
	CreatedAt     time.Time  `json:"createdAt"`
	OwnerUserID   *string    `json:"ownerUserId"`
}

// Admin API key lifecycle states. The auth middleware accepts a key only
// when status is StatusActive or StatusRotating (see authenticateAPIKey).
const (
	AdminAPIKeyStatusActive      = "active"
	AdminAPIKeyStatusRotating    = "rotating"
	AdminAPIKeyStatusExpired     = "expired"
	AdminAPIKeyStatusUnavailable = "unavailable"
)

const apiKeyListColumns = `id, name, "keyPrefix", enabled, status, "lastUsedAt", "expiresAt", "rotatedAt", "rotatedFromId", "createdBy", "createdAt", "ownerUserId"`

// scanAdminAPIKey scans the apiKeyListColumns projection into k. Centralised
// so the column list and the scan target list cannot drift apart.
func scanAdminAPIKey(row pgx.Row, k *AdminAPIKey) error {
	return row.Scan(
		&k.ID, &k.Name, &k.KeyPrefix, &k.Enabled, &k.Status,
		&k.LastUsedAt, &k.ExpiresAt, &k.RotatedAt, &k.RotatedFromID,
		&k.CreatedBy, &k.CreatedAt, &k.OwnerUserID,
	)
}

// ListAdminAPIKeys returns API keys (never includes hashes), capped at 1000.
// When ownerUserId is non-empty, only keys owned by that user are returned.
func (store *Store) ListAdminAPIKeys(ctx context.Context, ownerUserId string) ([]AdminAPIKey, error) {
	var rows pgx.Rows
	var err error
	if ownerUserId != "" {
		rows, err = store.pool.Query(ctx, fmt.Sprintf(`SELECT %s FROM "AdminApiKey" WHERE "ownerUserId" = $1 ORDER BY "createdAt" DESC LIMIT 1000`, apiKeyListColumns), ownerUserId)
	} else {
		rows, err = store.pool.Query(ctx, fmt.Sprintf(`SELECT %s FROM "AdminApiKey" ORDER BY "createdAt" DESC LIMIT 1000`, apiKeyListColumns))
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	keys := []AdminAPIKey{}
	for rows.Next() {
		var k AdminAPIKey
		if err := scanAdminAPIKey(rows, &k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// GetAdminAPIKey returns an API key by ID.
func (store *Store) GetAdminAPIKey(ctx context.Context, id string) (*AdminAPIKey, error) {
	row := store.pool.QueryRow(ctx, fmt.Sprintf(`SELECT %s FROM "AdminApiKey" WHERE id = $1`, apiKeyListColumns), id)
	var k AdminAPIKey
	err := scanAdminAPIKey(row, &k)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &k, nil
}

// CreateAdminAPIKeyParams holds fields for creating an API key.
type CreateAdminAPIKeyParams struct {
	Name        string
	KeyHash     string
	KeyPrefix   string
	CreatedBy   string
	ExpiresAt   *time.Time
	OwnerUserID *string
}

// CreateAdminAPIKey inserts a new API key.
func (store *Store) CreateAdminAPIKey(ctx context.Context, p CreateAdminAPIKeyParams) (*AdminAPIKey, error) {
	row := store.pool.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO "AdminApiKey" (id, name, "keyHash", "keyPrefix", "createdBy", "expiresAt", "ownerUserId", "createdAt", "updatedAt")
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, NOW(), NOW())
		RETURNING %s
	`, apiKeyListColumns), p.Name, p.KeyHash, p.KeyPrefix, p.CreatedBy, p.ExpiresAt, p.OwnerUserID)

	var k AdminAPIKey
	if err := scanAdminAPIKey(row, &k); err != nil {
		return nil, err
	}
	return &k, nil
}

// UpdateAdminAPIKeyParams holds optional fields for updating an API key.
type UpdateAdminAPIKeyParams struct {
	Name      *string
	Enabled   *bool
	ExpiresAt *time.Time
}

// UpdateAdminAPIKey updates an API key using COALESCE.
func (store *Store) UpdateAdminAPIKey(ctx context.Context, id string, p UpdateAdminAPIKeyParams) (*AdminAPIKey, error) {
	row := store.pool.QueryRow(ctx, fmt.Sprintf(`UPDATE "AdminApiKey" SET
		name = COALESCE($2, name), enabled = COALESCE($3, enabled),
		"expiresAt" = COALESCE($4, "expiresAt"), "updatedAt" = NOW()
	WHERE id = $1 RETURNING %s`, apiKeyListColumns),
		id, p.Name, p.Enabled, p.ExpiresAt)
	var k AdminAPIKey
	if err := scanAdminAPIKey(row, &k); err != nil {
		return nil, err
	}
	return &k, nil
}

// DeleteAdminAPIKey deletes an API key.
func (store *Store) DeleteAdminAPIKey(ctx context.Context, id string) error {
	tag, err := store.pool.Exec(ctx, `DELETE FROM "AdminApiKey" WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

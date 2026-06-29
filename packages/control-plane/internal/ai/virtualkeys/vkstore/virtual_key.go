package vkstore

import (
	"context"
	"errors"
	"fmt"
	"github.com/goccy/go-json"
	"time"

	"github.com/jackc/pgx/v5"
)

// VirtualKey represents a row from the VirtualKey table.
type VirtualKey struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	KeyHash      *string    `json:"keyHash,omitempty"`
	KeyPrefix    *string    `json:"keyPrefix,omitempty"`
	ProjectID    *string    `json:"projectId"`
	SourceApp    *string    `json:"sourceApp"`
	Enabled      bool       `json:"enabled"`
	ExpiresAt    *time.Time `json:"expiresAt"`
	RateLimitRpm *int       `json:"rateLimitRpm"`
	// Separate /v1/estimate compare-endpoint RPM cap.
	// nil → 30/min default applied at the AI gateway.
	CompareEndpointRateLimitRpm *int            `json:"compareEndpointRateLimitRpm"`
	AllowedModels               json.RawMessage `json:"allowedModels"`
	OwnerID                     *string         `json:"ownerId"`
	CreatedBy                   *string         `json:"createdBy"`
	CreatedAt                   time.Time       `json:"createdAt"`
	UpdatedAt                   time.Time       `json:"updatedAt"`
	// Quota-system fields (nullable — populated once migration adds the columns).
	VKType       *string    `json:"vkType"`
	VKStatus     *string    `json:"vkStatus"`
	ApprovedBy   *string    `json:"approvedBy"`
	ApprovedAt   *time.Time `json:"approvedAt"`
	RejectedBy   *string    `json:"rejectedBy"`
	RejectedAt   *time.Time `json:"rejectedAt"`
	RejectReason *string    `json:"rejectReason"`
}

const vkColumns = `id, name, "keyHash", "keyPrefix", "projectId", "sourceApp", enabled,
	"expiresAt", "rateLimitRpm", "compareEndpointRateLimitRpm",
	"allowedModels", "ownerId", "createdBy", "createdAt", "updatedAt",
	"vkType", "vkStatus", "approvedBy", "approvedAt", "rejectedBy", "rejectedAt", "rejectReason"`

func scanVK(row pgx.Row) (*VirtualKey, error) {
	var v VirtualKey
	err := row.Scan(
		&v.ID, &v.Name, &v.KeyHash, &v.KeyPrefix, &v.ProjectID, &v.SourceApp,
		&v.Enabled, &v.ExpiresAt, &v.RateLimitRpm,
		&v.CompareEndpointRateLimitRpm,
		&v.AllowedModels, &v.OwnerID, &v.CreatedBy, &v.CreatedAt, &v.UpdatedAt,
		&v.VKType, &v.VKStatus, &v.ApprovedBy, &v.ApprovedAt, &v.RejectedBy, &v.RejectedAt, &v.RejectReason,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// VirtualKeyListParams holds filter/pagination.
type VirtualKeyListParams struct {
	Q              string
	ProjectID      string
	OrganizationID string
	OwnerID        string
	Enabled        *bool
	VKType         string
	VKStatus       string
	Limit          int
	Offset         int
}

// ListVirtualKeys returns virtual keys with filtering.
func (store *Store) ListVirtualKeys(ctx context.Context, p VirtualKeyListParams) ([]VirtualKey, int, error) {
	where := "WHERE 1=1"
	args := []any{}
	argIdx := 1

	if p.ProjectID != "" {
		where += fmt.Sprintf(` AND v."projectId" = $%d`, argIdx)
		args = append(args, p.ProjectID)
		argIdx++
	}
	if p.Enabled != nil {
		where += fmt.Sprintf(` AND v.enabled = $%d`, argIdx)
		args = append(args, *p.Enabled)
		argIdx++
	}
	if p.OwnerID != "" {
		where += fmt.Sprintf(` AND v."ownerId" = $%d`, argIdx)
		args = append(args, p.OwnerID)
		argIdx++
	}
	if p.VKType != "" {
		where += fmt.Sprintf(` AND v."vkType" = $%d`, argIdx)
		args = append(args, p.VKType)
		argIdx++
	}
	if p.VKStatus != "" {
		where += fmt.Sprintf(` AND v."vkStatus" = $%d`, argIdx)
		args = append(args, p.VKStatus)
		argIdx++
	}
	if p.Q != "" {
		where += fmt.Sprintf(` AND (v.name ILIKE $%d OR v."sourceApp" ILIKE $%d OR v."keyPrefix" ILIKE $%d)`, argIdx, argIdx, argIdx)
		args = append(args, "%"+escapeILIKE(p.Q)+"%")
		argIdx++
	}

	var total int
	if err := store.pool.QueryRow(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM "VirtualKey" v %s`, where), args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count virtual keys: %w", err)
	}

	q := fmt.Sprintf(`SELECT v.%s FROM "VirtualKey" v %s ORDER BY v."createdAt" DESC LIMIT $%d OFFSET $%d`,
		vkColumns, where, argIdx, argIdx+1)
	args = append(args, p.Limit, p.Offset)

	rows, err := store.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list virtual keys: %w", err)
	}
	defer rows.Close()

	keys := []VirtualKey{}
	for rows.Next() {
		var v VirtualKey
		if err := rows.Scan(
			&v.ID, &v.Name, &v.KeyHash, &v.KeyPrefix, &v.ProjectID, &v.SourceApp,
			&v.Enabled, &v.ExpiresAt, &v.RateLimitRpm,
			&v.CompareEndpointRateLimitRpm,
			&v.AllowedModels, &v.OwnerID, &v.CreatedBy, &v.CreatedAt, &v.UpdatedAt,
			&v.VKType, &v.VKStatus, &v.ApprovedBy, &v.ApprovedAt, &v.RejectedBy, &v.RejectedAt, &v.RejectReason,
		); err != nil {
			return nil, 0, fmt.Errorf("scan virtual key: %w", err)
		}
		keys = append(keys, v)
	}
	return keys, total, rows.Err()
}

// GetVirtualKey returns a virtual key by ID.
func (store *Store) GetVirtualKey(ctx context.Context, id string) (*VirtualKey, error) {
	q := fmt.Sprintf(`SELECT %s FROM "VirtualKey" WHERE id = $1`, vkColumns)
	return scanVK(store.pool.QueryRow(ctx, q, id))
}

// CreateVirtualKeyParams holds fields for creating a virtual key.
type CreateVirtualKeyParams struct {
	Name    string
	KeyHash string
	// KeyVersion is the HMAC keyring version that produced KeyHash —
	// auth.CurrentKeyVersion() at issue time. Stamped so the
	// key_version column reports which version sealed each VK. VKs are NOT
	// lazy-migrated (the ai-gw admission path is read-only); they ride
	// try-all-versions and are pruned by re-issue/expiry.
	KeyVersion                  string
	KeyPrefix                   string
	ProjectID                   *string
	SourceApp                   *string
	Enabled                     bool
	RateLimitRpm                *int
	CompareEndpointRateLimitRpm *int
	AllowedModels               json.RawMessage
	OwnerID                     *string
	ExpiresAt                   *time.Time
	VKType                      string // "personal" or "application"; defaults to "personal"
	VKStatus                    string // "active" or "pending"; defaults to "active"
}

// CreateVirtualKey inserts a new virtual key.
func (store *Store) CreateVirtualKey(ctx context.Context, p CreateVirtualKeyParams) (*VirtualKey, error) {
	vkType := p.VKType
	if vkType == "" {
		vkType = "personal"
	}
	vkStatus := p.VKStatus
	if vkStatus == "" {
		vkStatus = "active"
	}
	q := fmt.Sprintf(`
		INSERT INTO "VirtualKey" (id, name, "keyHash", "key_version", "keyPrefix", "projectId", "sourceApp",
			enabled, "rateLimitRpm", "compareEndpointRateLimitRpm",
			"allowedModels", "ownerId",
			"expiresAt", "vkType", "vkStatus", "createdAt", "updatedAt")
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, NOW(), NOW())
		RETURNING %s
	`, vkColumns)
	v, err := scanVK(store.pool.QueryRow(ctx, q,
		p.Name, p.KeyHash, p.KeyVersion, p.KeyPrefix, p.ProjectID, p.SourceApp,
		p.Enabled, p.RateLimitRpm, p.CompareEndpointRateLimitRpm,
		p.AllowedModels, p.OwnerID,
		p.ExpiresAt, vkType, vkStatus,
	))
	if err != nil {
		return nil, fmt.Errorf("create virtual key: %w", err)
	}
	return v, nil
}

// UpdateVirtualKeyParams holds optional fields for updating a virtual key.
// For ExpiresAt: set UpdateExpiresAt=true to change the column;
// ExpiresAt=nil with UpdateExpiresAt=true clears it to SQL NULL.
type UpdateVirtualKeyParams struct {
	ProjectID                   *string
	SourceApp                   *string
	Enabled                     *bool
	RateLimitRpm                *int
	CompareEndpointRateLimitRpm *int
	AllowedModels               json.RawMessage // nil = no change
	OwnerID                     *string
	ExpiresAt                   *time.Time // new value; nil = clear to SQL NULL
	UpdateExpiresAt             bool       // true = update expiresAt column
}

// UpdateVirtualKey updates a virtual key using COALESCE for most fields and
// an explicit CASE WHEN toggle for nullable expiresAt so callers can clear it.
func (store *Store) UpdateVirtualKey(ctx context.Context, id string, p UpdateVirtualKeyParams) (*VirtualKey, error) {
	q := fmt.Sprintf(`UPDATE "VirtualKey" SET
		"projectId" = COALESCE($2, "projectId"),
		"sourceApp" = COALESCE($3, "sourceApp"),
		enabled = COALESCE($4, enabled),
		"rateLimitRpm" = COALESCE($5, "rateLimitRpm"),
		"compareEndpointRateLimitRpm" = COALESCE($6, "compareEndpointRateLimitRpm"),
		"allowedModels" = COALESCE($7, "allowedModels"),
		"ownerId" = COALESCE($8, "ownerId"),
		"expiresAt" = CASE WHEN $9::boolean THEN $10 ELSE "expiresAt" END,
		"updatedAt" = NOW()
	WHERE id = $1 RETURNING %s`, vkColumns)

	v, err := scanVK(store.pool.QueryRow(ctx, q, id,
		p.ProjectID, p.SourceApp, p.Enabled, p.RateLimitRpm,
		p.CompareEndpointRateLimitRpm,
		p.AllowedModels, p.OwnerID,
		p.UpdateExpiresAt, p.ExpiresAt))
	if err != nil {
		return nil, fmt.Errorf("update virtual key: %w", err)
	}
	return v, nil
}

// RegenerateVirtualKeyHash updates a virtual key's hash and prefix. keyVersion
// is the HMAC keyring version that produced keyHash
// (auth.CurrentKeyVersion()): a regenerate rewrites the hash under the current
// secret, so the key_version column is stamped current alongside it.
func (store *Store) RegenerateVirtualKeyHash(ctx context.Context, id, keyHash, keyVersion, keyPrefix string) error {
	_, err := store.pool.Exec(ctx, `
		UPDATE "VirtualKey" SET "keyHash" = $2, "key_version" = $3, "keyPrefix" = $4, "updatedAt" = NOW() WHERE id = $1
	`, id, keyHash, keyVersion, keyPrefix)
	return err
}

// DeleteVirtualKey deletes a virtual key by ID.
func (store *Store) DeleteVirtualKey(ctx context.Context, id string) error {
	tag, err := store.pool.Exec(ctx, `DELETE FROM "VirtualKey" WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete virtual key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// VirtualKey represents a virtual key record with project-joined fields.
type VirtualKey struct {
	ID        string
	Name      string
	KeyHash   *string
	KeyPrefix *string
	ProjectID *string
	// OrganizationID is the resolved org for this VK. For application
	// VKs it comes from Project.organizationId; for personal VKs it
	// falls back to Owner (NexusUser).organizationId. See vkSelectSQL
	// for the COALESCE rule.
	OrganizationID *string
	SourceApp      *string
	Enabled        bool
	ExpiresAt      *time.Time
	RateLimitRpm   *int
	// CompareEndpointRateLimitRpm is the per-VK cap on /v1/estimate compare
	// requests. nil → default 30/min applied in handler.checkCompareRateLimit.
	CompareEndpointRateLimitRpm *int
	AllowedModels               []AllowedModelRef
	OwnerID                     *string

	// VK type and approval status.
	VKType   *string // "personal" | "application"
	VKStatus *string // "active" | "pending" | "expired" | "rejected" | "revoked"

	// Denormalized names (populated via JOINs at lookup time).
	OrganizationName *string
	ProjectName      *string
	UserDisplayName  *string
	// OrganizationTimezone — IANA TZ from the joined Organization.
	// Carried onto traffic_event as origin_tz to drive org-local analytics
	// calendar windows ("yesterday" / "this month" attribution). Quota
	// period keys are computed in UTC, not org-local. Empty when no
	// project/org binding (e.g. system-level VK rows).
	OrganizationTimezone *string
}

// AllowedModelRef constrains which models a VK can access.
// ModelID supports globs (e.g. "gpt-*").
type AllowedModelRef struct {
	ProviderID string `json:"providerId"`
	ModelID    string `json:"modelId"`
}

// vkSelectSQL returns one row per virtual key with its denormalized
// project / organization / owner names attached.
//
// Org resolution precedence (binding):
//
//  1. application VK → Project → Organization
//  2. personal    VK → Owner (NexusUser) → Organization
//
// Without #2 personal VKs would always have org_id = NULL, breaking
// the audit pipeline's org_id/org_name columns on traffic_event AND
// any analytics aggregation that groups by org. Bug surfaced when a
// /v1/responses prod probe used a personal VK and its traffic_event
// row showed empty org despite the user being in an org.
//
// SQL implementation: LEFT JOIN Organization twice (one per join
// chain), then COALESCE the two results. The application path wins
// when present so the existing application-VK behaviour is preserved.
const vkSelectSQL = `
SELECT
  vk.id, vk.name, vk."keyHash", vk."keyPrefix",
  vk."projectId",
  COALESCE(p."organizationId", u."organizationId") AS organization_id,
  vk."sourceApp", vk.enabled, vk."expiresAt",
  vk."rateLimitRpm", vk."compareEndpointRateLimitRpm",
  vk."allowedModels", vk."ownerId",
  vk."vkType", vk."vkStatus",
  COALESCE(org.name, u_org.name)         AS organization_name,
  p.name,
  u."displayName",
  COALESCE(org.timezone, u_org.timezone) AS organization_timezone
FROM "VirtualKey" vk
LEFT JOIN "Project"      p     ON vk."projectId" = p.id
LEFT JOIN "Organization" org   ON p."organizationId" = org.id
LEFT JOIN "NexusUser"    u     ON vk."ownerId" = u.id
LEFT JOIN "Organization" u_org ON u."organizationId" = u_org.id
`

// GetVirtualKeyByHash looks up a VK by its HMAC-SHA256 key hash.
func (db *DB) GetVirtualKeyByHash(ctx context.Context, keyHash string) (*VirtualKey, error) {
	row := db.pool.QueryRow(ctx, vkSelectSQL+`WHERE vk."keyHash" = $1`, keyHash)
	return scanVirtualKey(row)
}

// scannable abstracts pgx.Row and pgx.Rows for shared scanning.
type scannable interface {
	Scan(dest ...any) error
}

func scanVirtualKey(row scannable) (*VirtualKey, error) {
	var vk VirtualKey
	var allowedModelsJSON []byte

	err := row.Scan(
		&vk.ID, &vk.Name, &vk.KeyHash, &vk.KeyPrefix,
		&vk.ProjectID, &vk.OrganizationID,
		&vk.SourceApp, &vk.Enabled, &vk.ExpiresAt,
		&vk.RateLimitRpm, &vk.CompareEndpointRateLimitRpm,
		&allowedModelsJSON, &vk.OwnerID,
		&vk.VKType, &vk.VKStatus,
		&vk.OrganizationName, &vk.ProjectName, &vk.UserDisplayName,
		&vk.OrganizationTimezone,
	)
	if err != nil {
		return nil, fmt.Errorf("store: scan virtual key: %w", err)
	}

	// Parse allowedModels JSON. Propagate errors to prevent a VK with
	// corrupt model restrictions from silently having unrestricted access.
	if len(allowedModelsJSON) > 0 {
		if err := json.Unmarshal(allowedModelsJSON, &vk.AllowedModels); err != nil {
			return nil, fmt.Errorf("store: parse allowedModels for vk %s: %w", vk.ID, err)
		}
	}

	return &vk, nil
}

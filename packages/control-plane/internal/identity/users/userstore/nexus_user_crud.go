package userstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// NexusUserListParams holds filter/pagination for NexusUser listing.
type NexusUserListParams struct {
	Q                     string
	Enabled               *bool
	CanAccessControlPlane *bool
	OrgID                 string
	// IncludeSubOrgs extends the OrgID filter to include all descendant orgs
	// by matching Organization.path prefix.
	IncludeSubOrgs bool
	Limit          int
	Offset         int
}

// NexusUserSafe is a NexusUser without the password hash.
type NexusUserSafe struct {
	ID                    string  `json:"id"`
	DisplayName           string  `json:"displayName"`
	Email                 *string `json:"email"`
	Status                string  `json:"status"`
	CanAccessControlPlane bool    `json:"canAccessControlPlane"`
	// Source indicates how the user was provisioned: "local" | "oidc" | "scim".
	Source            string     `json:"source"`
	LastLoginAt       *time.Time `json:"lastLoginAt"`
	PreferredTimezone *string    `json:"preferredTimezone"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
	// OrganizationID and OrganizationName are populated by ListNexusUsers via LEFT JOIN.
	OrganizationID   *string `json:"organizationId,omitempty"`
	OrganizationName *string `json:"organizationName,omitempty"`
}

const nexusUserSafeColumns = `id, "displayName", email, status, "canAccessControlPlane", source, "lastLoginAt", "preferredTimezone", "createdAt", "updatedAt"`

// ListNexusUsers returns NexusUsers (never includes password hash).
// Each row includes organizationId and organizationName via LEFT JOIN.
func (store *Store) ListNexusUsers(ctx context.Context, p NexusUserListParams) ([]NexusUserSafe, int, error) {
	where := `WHERE 1=1`
	args := []any{}
	argIdx := 1

	if p.Q != "" {
		where += fmt.Sprintf(` AND (u."displayName" ILIKE $%d OR u.email ILIKE $%d)`, argIdx, argIdx)
		args = append(args, "%"+escapeILIKE(p.Q)+"%")
		argIdx++
	}
	if p.Enabled != nil {
		if *p.Enabled {
			where += ` AND u.status = 'active'`
		} else {
			where += ` AND u.status != 'active'`
		}
	}
	if p.CanAccessControlPlane != nil {
		where += fmt.Sprintf(` AND u."canAccessControlPlane" = $%d`, argIdx)
		args = append(args, *p.CanAccessControlPlane)
		argIdx++
	}
	if p.OrgID != "" {
		if p.IncludeSubOrgs {
			where += fmt.Sprintf(` AND u."organizationId" IN (SELECT id FROM "Organization" WHERE path LIKE (SELECT path FROM "Organization" WHERE id = $%d) || '%%')`, argIdx)
		} else {
			where += fmt.Sprintf(` AND u."organizationId" = $%d`, argIdx)
		}
		args = append(args, p.OrgID)
		argIdx++
	}

	var total int
	if err := store.pool.QueryRow(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM "NexusUser" u %s`, where), args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	q := fmt.Sprintf(`
		SELECT u.id, u."displayName", u.email, u.status, u."canAccessControlPlane", u.source,
		       u."lastLoginAt", u."preferredTimezone", u."createdAt", u."updatedAt",
		       u."organizationId", o.name
		FROM "NexusUser" u
		LEFT JOIN "Organization" o ON o.id = u."organizationId"
		%s ORDER BY u."updatedAt" DESC, u."displayName" ASC LIMIT $%d OFFSET $%d`,
		where, argIdx, argIdx+1)
	args = append(args, p.Limit, p.Offset)

	rows, err := store.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	users := []NexusUserSafe{}
	for rows.Next() {
		var u NexusUserSafe
		if err := rows.Scan(&u.ID, &u.DisplayName, &u.Email, &u.Status, &u.CanAccessControlPlane, &u.Source,
			&u.LastLoginAt, &u.PreferredTimezone, &u.CreatedAt, &u.UpdatedAt,
			&u.OrganizationID, &u.OrganizationName); err != nil {
			return nil, 0, err
		}
		users = append(users, u)
	}
	return users, total, rows.Err()
}

// GetNexusUserOrgInfo returns the organization ID and name for a given user.
// Returns empty strings when the user has no organization.
func (store *Store) GetNexusUserOrgInfo(ctx context.Context, userID string) (orgID, orgName string, err error) {
	err = store.pool.QueryRow(ctx, `
		SELECT COALESCE(u."organizationId", ''), COALESCE(o.name, '')
		FROM "NexusUser" u
		LEFT JOIN "Organization" o ON o.id = u."organizationId"
		WHERE u.id = $1
	`, userID).Scan(&orgID, &orgName)
	return
}

// FindDefaultOrganizationID resolves the fallback organization for users
// who are auto-provisioned without an explicit org binding (SCIM,
// OIDC-JIT, agent enrollment). The NexusUser table has a DB-default
// `'default'::text` for organizationId but the seed never inserts a row
// with that id — so callers must look up a real org instead of relying
// on the column default. Resolution order:
//
//  1. The root Organization (parentId IS NULL) with the earliest
//     createdAt. In a typical deployment the seed creates one root org
//     ("Apex Financial Group" / customer's company) before any
//     descendants — that's the sensible owner for an externally-
//     provisioned user with no org hint.
//  2. If no root exists (degenerate state), the earliest-created org.
//  3. Empty string + nil error if the Organization table is empty,
//     leaving it to the caller to decide whether to error.
func (store *Store) FindDefaultOrganizationID(ctx context.Context) (string, error) {
	var id string
	err := store.pool.QueryRow(ctx, `
		SELECT id FROM "Organization"
		ORDER BY ("parentId" IS NOT NULL) ASC, "createdAt" ASC
		LIMIT 1
	`).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("find default organization: %w", err)
	}
	return id, nil
}

// GetNexusUserSafe returns a single NexusUser by ID without the password hash.
func (store *Store) GetNexusUserSafe(ctx context.Context, id string) (*NexusUserSafe, error) {
	row := store.pool.QueryRow(ctx, fmt.Sprintf(`SELECT %s FROM "NexusUser" WHERE id = $1`, nexusUserSafeColumns), id)
	var u NexusUserSafe
	err := row.Scan(&u.ID, &u.DisplayName, &u.Email, &u.Status, &u.CanAccessControlPlane, &u.Source, &u.LastLoginAt, &u.PreferredTimezone, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get nexus user safe: %w", err)
	}
	return &u, nil
}

// CreateNexusUserParams holds fields for creating a NexusUser.
type CreateNexusUserParams struct {
	DisplayName           string
	Email                 *string
	PasswordHash          *string // nil = no password (SSO/SCIM users)
	CanAccessControlPlane *bool   // nil = use DB default (false)
	OrganizationID        *string
	CreatedBy             string
	// Source is "local" | "oidc" | "scim". Defaults to "local" when empty.
	Source string
	// Status is "active" | "suspended". Defaults to "active" when nil.
	Status *string
}

// CreateNexusUser inserts a new NexusUser.
func (store *Store) CreateNexusUser(ctx context.Context, p CreateNexusUserParams) (*NexusUserSafe, error) {
	source := p.Source
	if source == "" {
		source = "local"
	}
	canAccess := false
	if p.CanAccessControlPlane != nil {
		canAccess = *p.CanAccessControlPlane
	}
	var pwdHash *string
	if p.PasswordHash != nil && *p.PasswordHash != "" {
		pwdHash = p.PasswordHash
	}

	row := store.pool.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO "NexusUser" (id, "displayName", email, "passwordHash", "canAccessControlPlane", "organizationId", "createdBy", source, "createdAt", "updatedAt")
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, NOW(), NOW())
		RETURNING %s
	`, nexusUserSafeColumns), p.DisplayName, p.Email, pwdHash, canAccess, p.OrganizationID, p.CreatedBy, source)

	var u NexusUserSafe
	err := row.Scan(&u.ID, &u.DisplayName, &u.Email, &u.Status, &u.CanAccessControlPlane, &u.Source, &u.LastLoginAt, &u.PreferredTimezone, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// UpdateNexusUserParams holds optional fields for updating a NexusUser.
type UpdateNexusUserParams struct {
	DisplayName  *string
	Email        *string
	Status       *string
	Enabled      *bool // maps to status active/suspended
	PasswordHash *string
	// PreferredTimezone updates the user's display TZ. Pass pointer-to-empty-string
	// to clear (revert to browser default).
	PreferredTimezone *string
	// OrganizationID sets the user's org. Pass pointer-to-empty-string to
	// clear (remove from org). nil means no change.
	OrganizationID        *string
	CanAccessControlPlane *bool
}

// UpdateNexusUser updates a NexusUser using COALESCE.
func (store *Store) UpdateNexusUser(ctx context.Context, id string, p UpdateNexusUserParams) (*NexusUserSafe, error) {
	// Resolve Enabled bool to status string
	var statusVal *string
	if p.Status != nil {
		statusVal = p.Status
	} else if p.Enabled != nil {
		if *p.Enabled {
			s := "active"
			statusVal = &s
		} else {
			s := "suspended"
			statusVal = &s
		}
	}

	// preferredTimezone uses NULLIF + COALESCE so the caller can both
	// update ("Asia/Shanghai") and clear (empty string ⇒ NULL) the
	// field via the same nullable parameter.
	row := store.pool.QueryRow(ctx, fmt.Sprintf(`UPDATE "NexusUser" SET
		"displayName" = COALESCE($2, "displayName"),
		email = COALESCE($3, email), status = COALESCE($4, status),
		"passwordHash" = COALESCE($5, "passwordHash"),
		"preferredTimezone" = CASE WHEN $6::text IS NULL THEN "preferredTimezone" ELSE NULLIF($6, '') END,
		"organizationId" = CASE WHEN $7::text IS NULL THEN "organizationId" ELSE NULLIF($7, '') END,
		"canAccessControlPlane" = COALESCE($8, "canAccessControlPlane"),
		"updatedAt" = NOW()
	WHERE id = $1 RETURNING %s`, nexusUserSafeColumns),
		id, p.DisplayName, p.Email, statusVal, p.PasswordHash, p.PreferredTimezone, p.OrganizationID, p.CanAccessControlPlane)
	var u NexusUserSafe
	err := row.Scan(&u.ID, &u.DisplayName, &u.Email, &u.Status, &u.CanAccessControlPlane, &u.Source, &u.LastLoginAt, &u.PreferredTimezone, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// DeleteNexusUser deletes a NexusUser.
func (store *Store) DeleteNexusUser(ctx context.Context, id string) error {
	tag, err := store.pool.Exec(ctx, `DELETE FROM "NexusUser" WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

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

// RegenerateAdminAPIKey replaces the key hash and prefix for an existing API key.
func (store *Store) RegenerateAdminAPIKey(ctx context.Context, id, keyHash, keyPrefix string) error {
	_, err := store.pool.Exec(ctx, `
		UPDATE "AdminApiKey" SET "keyHash" = $2, "keyPrefix" = $3, "updatedAt" = NOW() WHERE id = $1
	`, id, keyHash, keyPrefix)
	return err
}

// RotateAdminAPIKeyParams holds fields for an atomic rotation: mint a new key
// that inherits the predecessor's name + owner, mark the predecessor as
// rotating, and link successor → predecessor via rotatedFromId.
type RotateAdminAPIKeyParams struct {
	PredecessorID string
	NewKeyHash    string
	NewKeyPrefix  string
	NewCreatedBy  string
	// NewExpiresAt is the absolute expiry stamped on the successor row.
	// nil means "inherit from predecessor" — RotateAdminAPIKey resolves this
	// inside the transaction.
	NewExpiresAt *time.Time
}

// RotateAdminAPIKeyResult bundles the two affected rows: the newly-minted
// successor and the rotated-out predecessor, both reflecting their post-
// transaction state.
type RotateAdminAPIKeyResult struct {
	Successor   *AdminAPIKey
	Predecessor *AdminAPIKey
}

// RotateAdminAPIKey atomically mints a successor row for the predecessor and
// flips the predecessor's status active → rotating. Both keys remain
// acceptable to the auth middleware during the rotation window so callers can
// swap in the new value without service interruption; the operator calls
// RetireAdminAPIKey on the predecessor once the swap is complete.
//
// Errors:
//   * pgx.ErrNoRows — predecessor does not exist
//   * status / enabled invariant errors are returned as plain errors with
//     descriptive messages; the handler maps them to HTTP 409.
func (store *Store) RotateAdminAPIKey(ctx context.Context, p RotateAdminAPIKeyParams) (*RotateAdminAPIKeyResult, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the predecessor row so concurrent rotate calls serialise.
	var (
		predName        string
		predEnabled     bool
		predStatus      string
		predOwnerUserID *string
		predExpiresAt   *time.Time
	)
	err = tx.QueryRow(ctx, `
		SELECT name, enabled, status, "ownerUserId", "expiresAt"
		FROM "AdminApiKey" WHERE id = $1 FOR UPDATE
	`, p.PredecessorID).Scan(&predName, &predEnabled, &predStatus, &predOwnerUserID, &predExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, pgx.ErrNoRows
	}
	if err != nil {
		return nil, fmt.Errorf("rotate: lock predecessor: %w", err)
	}
	if predStatus != AdminAPIKeyStatusActive {
		return nil, fmt.Errorf("rotate: predecessor status is %q; only active keys can be rotated", predStatus)
	}

	// Resolve the successor's expiry: caller-supplied value wins, otherwise
	// inherit the predecessor's expiry so the rotation window does not
	// silently extend the operator's intended lifetime.
	successorExpiresAt := p.NewExpiresAt
	if successorExpiresAt == nil {
		successorExpiresAt = predExpiresAt
	}

	// Mint the successor with rotatedFromId pointing at the predecessor.
	successorRow := tx.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO "AdminApiKey" (
			id, name, "keyHash", "keyPrefix", "createdBy", "expiresAt",
			"ownerUserId", "rotatedFromId", status, "createdAt", "updatedAt"
		) VALUES (
			gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, 'active', NOW(), NOW()
		) RETURNING %s
	`, apiKeyListColumns),
		predName, p.NewKeyHash, p.NewKeyPrefix, p.NewCreatedBy,
		successorExpiresAt, predOwnerUserID, p.PredecessorID,
	)
	var successor AdminAPIKey
	if err := scanAdminAPIKey(successorRow, &successor); err != nil {
		return nil, fmt.Errorf("rotate: insert successor: %w", err)
	}

	// Flip the predecessor to rotating + stamp rotatedAt. enabled is left
	// untouched so the operator's quick-disable toggle still works.
	predRow := tx.QueryRow(ctx, fmt.Sprintf(`
		UPDATE "AdminApiKey"
		SET status = 'rotating', "rotatedAt" = NOW(), "updatedAt" = NOW()
		WHERE id = $1
		RETURNING %s
	`, apiKeyListColumns), p.PredecessorID)
	var predecessor AdminAPIKey
	if err := scanAdminAPIKey(predRow, &predecessor); err != nil {
		return nil, fmt.Errorf("rotate: update predecessor: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("rotate: commit: %w", err)
	}
	return &RotateAdminAPIKeyResult{Successor: &successor, Predecessor: &predecessor}, nil
}

// RetireAdminAPIKey transitions a key out of the accepted set. Allowed
// source states:
//   * active    — operator deciding to sunset a key
//   * rotating  — rotation window closing
// The target status MUST be either AdminAPIKeyStatusExpired (natural sunset)
// or AdminAPIKeyStatusUnavailable (active revocation / compromise).
//
// Returns pgx.ErrNoRows if the key does not exist; returns a descriptive
// error if the source state or target status is invalid.
func (store *Store) RetireAdminAPIKey(ctx context.Context, id, targetStatus string) (*AdminAPIKey, error) {
	if targetStatus != AdminAPIKeyStatusExpired && targetStatus != AdminAPIKeyStatusUnavailable {
		return nil, fmt.Errorf("retire: invalid target status %q (want %q or %q)",
			targetStatus, AdminAPIKeyStatusExpired, AdminAPIKeyStatusUnavailable)
	}
	row := store.pool.QueryRow(ctx, fmt.Sprintf(`
		UPDATE "AdminApiKey"
		SET status = $2, "updatedAt" = NOW()
		WHERE id = $1
		  AND status IN ('active', 'rotating')
		RETURNING %s
	`, apiKeyListColumns), id, targetStatus)
	var k AdminAPIKey
	err := scanAdminAPIKey(row, &k)
	if errors.Is(err, pgx.ErrNoRows) {
		// Either the row does not exist OR it is already retired. Distinguish
		// by reading the row separately so the caller can return 404 vs 409.
		var exists bool
		if e := store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM "AdminApiKey" WHERE id = $1)`, id).Scan(&exists); e != nil {
			return nil, fmt.Errorf("retire: existence probe: %w", e)
		}
		if !exists {
			return nil, pgx.ErrNoRows
		}
		return nil, fmt.Errorf("retire: key is already expired or unavailable")
	}
	if err != nil {
		return nil, fmt.Errorf("retire: %w", err)
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

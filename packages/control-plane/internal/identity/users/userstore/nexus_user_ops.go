package userstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/usercascade"
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
	// OwnedByIdP, when non-nil, restricts the result to users provisioned by /
	// federated to that identity provider (a UserFederatedIdentity link row
	// exists). The SCIM ListUsers handler sets it to the calling token's IdP so a
	// per-IdP SCIM token cannot enumerate users owned by another IdP.
	OwnedByIdP *string
	Limit      int
	Offset     int
}

// NexusUserSafe is a NexusUser without the password hash.
type NexusUserSafe struct {
	ID                    string  `json:"id"`
	DisplayName           string  `json:"displayName"`
	Email                 *string `json:"email"`
	Status                string  `json:"status"`
	CanAccessControlPlane bool    `json:"canAccessControlPlane"`
	// Source indicates how the user was provisioned: "local" | "oidc" | "saml" | "scim".
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
	if p.OwnedByIdP != nil && *p.OwnedByIdP != "" {
		// Restrict to users the given IdP provisioned/owns so a
		// per-IdP SCIM token cannot enumerate users owned by another IdP.
		where += fmt.Sprintf(` AND EXISTS (SELECT 1 FROM "UserFederatedIdentity" f WHERE f."userId" = u.id AND f."idpId" = $%d)`, argIdx)
		args = append(args, *p.OwnedByIdP)
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
	// Source is "local" | "oidc" | "saml" | "scim". Defaults to "local" when empty.
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

// DeleteNexusUser hard-deletes a NexusUser and the auth/authz artifacts that
// reference it, in one transaction. A naive
// `DELETE FROM "NexusUser"` would FAIL on the ScimToken.createdBy RESTRICT FK
// and orphan-null the user's owned VirtualKey/AdminApiKey rows; the FK-correct
// ordering lives in usercascade — the SAME ordering the GDPR Art.17 erasure
// (dsarstore.FulfillDSARErasure) uses for its account-removal stage.
//
// Semantics differ from DSAR erasure: this is a genuine HARD DELETE of the
// account plus its owned auth artifacts — it does NOT anonymise the user's
// traffic footprint (that is erasure-specific). The tamper-evident
// AdminAuditLog hash chain is never touched (see usercascade docs). Returns
// pgx.ErrNoRows when no such user exists.
func (store *Store) DeleteNexusUser(ctx context.Context, id string) error {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin delete user tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	counts, err := usercascade.DeleteUserAccount(ctx, tx, id)
	if err != nil {
		return err
	}
	if !counts.AccountDeleted {
		// No NexusUser row existed; the cascade ran as no-ops. Roll back (via the
		// deferred Rollback) and report not-found, preserving the prior contract.
		return pgx.ErrNoRows
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit delete user: %w", err)
	}
	return nil
}

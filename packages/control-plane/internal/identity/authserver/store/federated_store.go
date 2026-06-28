package store

import (
	"context"
	"errors"
	"fmt"
	"github.com/goccy/go-json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// FederatedPgxPool is the minimum pgx pool surface FederatedStore methods need.
// The concrete *pgxpool.Pool satisfies it in production; pgxmock's
// PgxPoolIface satisfies it in tests. Mirrors the PgxPool convention from
// packages/control-plane/internal/store/db.go.
type FederatedPgxPool interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// JITUser is the minimal user record returned after just-in-time provisioning.
type JITUser struct {
	ID          string
	DisplayName string
	Email       *string
	Status      string
	Source      string
}

// FederatedIdentity is one (user, IdP, subject) binding decoded from
// UserFederatedIdentity. RawClaims captures the IdP's last-seen claim blob.
type FederatedIdentity struct {
	ID              string
	UserID          string
	IdPID           string
	ExternalSubject string
	ExternalEmail   *string
	RawClaims       map[string]any
	LinkedAt        time.Time
	LastLoginAt     *time.Time
}

// FederatedStore manages UserFederatedIdentity rows.
type FederatedStore struct{ db FederatedPgxPool }

// NewFederatedStore returns a FederatedStore backed by the supplied pool.
func NewFederatedStore(db *pgxpool.Pool) *FederatedStore { return &FederatedStore{db: db} }

// NewFederatedStoreWithPool is the test-only constructor accepting any
// FederatedPgxPool implementation (notably pgxmock.PgxPoolIface).
// Production callers must use NewFederatedStore.
func NewFederatedStoreWithPool(db FederatedPgxPool) *FederatedStore {
	return &FederatedStore{db: db}
}

// FindByIdPSubject looks up a federation row by its (idpId, externalSubject)
// unique pair. Not-found is not an error; (nil, false, nil) is returned so
// callers can distinguish "missing" from "db error".
func (s *FederatedStore) FindByIdPSubject(ctx context.Context, idpID, subject string) (*FederatedIdentity, bool, error) {
	row := s.db.QueryRow(ctx,
		`SELECT id, "userId", "idpId", "externalSubject", "externalEmail", "rawClaims", "linkedAt", "lastLoginAt"
		   FROM "UserFederatedIdentity"
		  WHERE "idpId" = $1 AND "externalSubject" = $2`, idpID, subject)
	var fi FederatedIdentity
	var rawClaims []byte
	if err := row.Scan(&fi.ID, &fi.UserID, &fi.IdPID, &fi.ExternalSubject, &fi.ExternalEmail, &rawClaims, &fi.LinkedAt, &fi.LastLoginAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if len(rawClaims) > 0 {
		if err := json.Unmarshal(rawClaims, &fi.RawClaims); err != nil {
			return nil, false, err
		}
	}
	return &fi, true, nil
}

// UpsertLocalIdentity inserts a (user, IdP, externalSubject) binding if it
// does not already exist, otherwise refreshes lastLoginAt. Used when a local
// NexusUser authenticates with password and needs a federated_identity row
// auto-provisioned against the local IdP.
func (s *FederatedStore) UpsertLocalIdentity(ctx context.Context, userID, idpID, externalSubject string) error {
	_, err := s.db.Exec(ctx,
		`INSERT INTO "UserFederatedIdentity"("userId","idpId","externalSubject")
		 VALUES ($1,$2,$3)
		 ON CONFLICT ("idpId","externalSubject") DO UPDATE SET "lastLoginAt" = NOW()`,
		userID, idpID, externalSubject)
	return err
}

// TouchLastLogin stamps lastLoginAt=NOW() on the row.
func (s *FederatedStore) TouchLastLogin(ctx context.Context, id string) error {
	_, err := s.db.Exec(ctx, `UPDATE "UserFederatedIdentity" SET "lastLoginAt" = NOW() WHERE id = $1`, id)
	return err
}

// UpdateRawClaims replaces the rawClaims blob and stamps lastLoginAt=NOW()
// for an existing federation row.
func (s *FederatedStore) UpdateRawClaims(ctx context.Context, id string, claims map[string]any) error {
	b, err := json.Marshal(claims)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx,
		`UPDATE "UserFederatedIdentity" SET "rawClaims" = $2, "lastLoginAt" = NOW() WHERE id = $1`,
		id, b)
	return err
}

// RefreshUserProfile refreshes a federated user's displayName and email on
// re-login so a name the IdP only started emitting after the account was first
// provisioned (or one the admin corrected on the IdP) propagates to Nexus. Both
// fields are COALESCE(NULLIF(...,”), col): a non-empty value overwrites, an
// empty one leaves the stored value intact — so a login whose assertion happens
// to omit the name never blanks a previously-good displayName. No-op when both
// arguments are empty.
func (s *FederatedStore) RefreshUserProfile(ctx context.Context, userID, displayName, email string) error {
	if displayName == "" && email == "" {
		return nil
	}
	_, err := s.db.Exec(ctx, `
		UPDATE "NexusUser"
		   SET "displayName" = COALESCE(NULLIF($2, ''), "displayName"),
		       email         = COALESCE(NULLIF($3, ''), email),
		       "updatedAt"   = NOW()
		 WHERE id = $1
	`, userID, displayName, email)
	return err
}

// JITProvisionParams holds the inputs needed to just-in-time provision a user.
type JITProvisionParams struct {
	IdPID           string
	ExternalSubject string // JWT "sub" claim
	Email           string // may be empty if the IdP omits it
	DisplayName     string // from JWT "name" or email local-part fallback
	// Groups carries the JWT "groups" claim (or whatever claim
	// IdentityProvider.config.groupClaim points at). Each entry is
	// resolved against IdpGroupMapping(idpId, externalGroupId) to find
	// the local IamGroupID; matches become IamGroupMembership rows on
	// the JIT user. Unmapped externals are silently skipped — admins
	// only consume the mappings they opted into.
	Groups []string
	// DefaultRole names an IamGroup every JIT user from this IdP joins as a
	// baseline, on top of any Groups matches. Resolved by name inside the
	// provisioning tx; an empty or unresolvable name adds no baseline group
	// (silent skip, parity with an unmapped external group).
	DefaultRole string
	// CanAccessControlPlane stamps NexusUser.canAccessControlPlane. Carried
	// per-IdP (IdentityProvider.defaultControlPlaneAccess) because one IdP may
	// federate both CP admins and agent end-users.
	CanAccessControlPlane bool
	// CreatedBy is logged for audit; typically the IdP name.
	CreatedBy string
	// Source is the provisioning origin stamped on NexusUser.source — "oidc"
	// or "saml" depending on the federated protocol the user logged in through.
	// Defaults to "oidc" when empty (back-compat). The admin UI reads it to label
	// the account; the != "local" federated-account guards treat both as SSO.
	Source string
}

// JITProvisionUser creates a NexusUser (source from p.Source — "oidc"/"saml"),
// a UserFederatedIdentity row, and zero-or-more IamGroupMembership rows derived
// from IdpGroupMapping in a single transaction. It is idempotent for
// the (idpId, externalSubject) pair — a race where two concurrent logins both
// see "not found" will hit a unique-constraint violation on the second INSERT;
// callers should retry via FindByIdPSubject on that error.
//
// Group membership rule (parity with scim handler GroupsPOST):
//   - principalType = "nexus_user" — matches IamGroupMembership convention
//     used by the SCIM provisioner so /api/admin/users/:id/memberships and
//     IAM policy resolution see OIDC-JIT users the same way SCIM ones.
//   - INSERT uses ON CONFLICT DO NOTHING on (groupId, principalType,
//     principalId) so re-running JIT (idempotent retry path) does not
//     fail on already-attached groups.
func (s *FederatedStore) JITProvisionUser(ctx context.Context, p JITProvisionParams) (*JITUser, string, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var email *string
	if p.Email != "" {
		e := p.Email
		email = &e
	}
	displayName := p.DisplayName
	if displayName == "" {
		displayName = p.Email
	}

	// Resolve the organization the JIT user belongs to. NexusUser.organizationId
	// is NOT NULL with a FK to Organization, and its column default ('default')
	// references no real row — so the insert MUST set a valid org explicitly.
	// Same resolution order as userstore.FindDefaultOrganizationID (earliest
	// root org), which the SCIM provisioner already uses; OIDC/SAML JIT must
	// match so all auto-provisioned users land in the same place.
	var orgID string
	err = tx.QueryRow(ctx, `
		SELECT id FROM "Organization"
		ORDER BY ("parentId" IS NOT NULL) ASC, "createdAt" ASC
		LIMIT 1
	`).Scan(&orgID)
	if err != nil {
		return nil, "", fmt.Errorf("jit: resolve default organization: %w", err)
	}

	source := p.Source
	if source == "" {
		source = "oidc" // back-compat default; callers pass "oidc"/"saml" explicitly
	}
	var u JITUser
	err = tx.QueryRow(ctx, `
		INSERT INTO "NexusUser" (id, "organizationId", "displayName", email, source, "canAccessControlPlane", "createdBy", "createdAt", "updatedAt")
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, NOW(), NOW())
		RETURNING id, "displayName", email, status, source
	`, orgID, displayName, email, source, p.CanAccessControlPlane, p.CreatedBy).Scan(&u.ID, &u.DisplayName, &u.Email, &u.Status, &u.Source)
	if err != nil {
		return nil, "", err
	}

	var fiID string
	err = tx.QueryRow(ctx, `
		INSERT INTO "UserFederatedIdentity" (id, "userId", "idpId", "externalSubject", "linkedAt")
		VALUES (gen_random_uuid(), $1, $2, $3, NOW())
		RETURNING id
	`, u.ID, p.IdPID, p.ExternalSubject).Scan(&fiID)
	if err != nil {
		return nil, "", err
	}

	// Resolve each external group via IdpGroupMapping and stamp the
	// membership row inside the same tx so a partial commit cannot
	// leave a JIT user with phantom federated identity but no group
	// rows (or vice-versa).
	for _, externalGroup := range p.Groups {
		if externalGroup == "" {
			continue
		}
		var iamGroupID string
		mapErr := tx.QueryRow(ctx, `
			SELECT "iamGroupId"
			  FROM "IdpGroupMapping"
			 WHERE "identityProviderId" = $1 AND "externalGroupId" = $2
		`, p.IdPID, externalGroup).Scan(&iamGroupID)
		if mapErr != nil {
			if errors.Is(mapErr, pgx.ErrNoRows) {
				// Unmapped external group — admins did not opt into it.
				// Silent skip matches the SCIM Groups POST policy
				// (mapping miss is a no-op, not an error).
				continue
			}
			return nil, "", mapErr
		}
		if _, insErr := tx.Exec(ctx, `
			INSERT INTO "IamGroupMembership" (id, "groupId", "principalType", "principalId", "createdAt")
			VALUES (gen_random_uuid(), $1, 'nexus_user', $2, NOW())
			ON CONFLICT ("groupId", "principalType", "principalId") DO NOTHING
		`, iamGroupID, u.ID); insErr != nil {
			return nil, "", insErr
		}
	}

	// Baseline role: add the IdP's defaultRole group on top of any mapped
	// groups so a JIT user is never left with zero permissions (the previous
	// behaviour for a federated user whose external groups matched nothing).
	// Resolved by group name; an empty name or a name with no matching IamGroup
	// is a silent skip — the IdP form picks from existing groups, so a miss
	// means the group was deleted after configuration, not a typo.
	if p.DefaultRole != "" {
		var iamGroupID string
		roleErr := tx.QueryRow(ctx, `
			SELECT id FROM "IamGroup" WHERE name = $1
		`, p.DefaultRole).Scan(&iamGroupID)
		switch {
		case roleErr == nil:
			if _, insErr := tx.Exec(ctx, `
				INSERT INTO "IamGroupMembership" (id, "groupId", "principalType", "principalId", "createdAt")
				VALUES (gen_random_uuid(), $1, 'nexus_user', $2, NOW())
				ON CONFLICT ("groupId", "principalType", "principalId") DO NOTHING
			`, iamGroupID, u.ID); insErr != nil {
				return nil, "", insErr
			}
		case errors.Is(roleErr, pgx.ErrNoRows):
			// defaultRole names no live group — skip the baseline, don't fail login.
		default:
			return nil, "", roleErr
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, "", err
	}
	return &u, fiID, nil
}

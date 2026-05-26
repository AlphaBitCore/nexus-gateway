package store

import (
	"context"
	"encoding/json"
	"errors"
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
	// CreatedBy is logged for audit; typically the IdP name.
	CreatedBy string
}

// JITProvisionUser creates a NexusUser (source='oidc', canAccessControlPlane=false),
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

	var u JITUser
	err = tx.QueryRow(ctx, `
		INSERT INTO "NexusUser" (id, "displayName", email, source, "canAccessControlPlane", "createdBy", "createdAt", "updatedAt")
		VALUES (gen_random_uuid(), $1, $2, 'oidc', false, $3, NOW(), NOW())
		RETURNING id, "displayName", email, status, source
	`, displayName, email, p.CreatedBy).Scan(&u.ID, &u.DisplayName, &u.Email, &u.Status, &u.Source)
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

	if err := tx.Commit(ctx); err != nil {
		return nil, "", err
	}
	return &u, fiID, nil
}

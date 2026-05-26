package scimstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/iamstore"
)

// HashScimToken returns the SHA-256 hex digest of a SCIM bearer token.
// The token itself is cryptographically random so SHA-256 provides safe
// storage without the cost of a password-KDF (no rainbow table concern).
func HashScimToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// GenerateScimToken creates a random 32-byte token prefixed with "nxs_scim_".
func GenerateScimToken() (token, prefix string, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("generate scim token: %w", err)
	}
	token = "nxs_scim_" + hex.EncodeToString(buf)
	prefix = token[:20] // "nxs_scim_" + first 11 chars of the hex
	return token, prefix, nil
}


// ScimToken is the stored representation of a SCIM provisioner credential.
// The full token value is never stored; only the bcrypt hash is kept.
type ScimToken struct {
	ID                 string     `json:"id"`
	Name               string     `json:"name"`
	TokenHash          string     `json:"-"` // never serialised
	TokenPrefix        string     `json:"tokenPrefix"`
	IdentityProviderID *string    `json:"identityProviderId,omitempty"`
	CreatedBy          string     `json:"createdBy"`
	CreatedAt          time.Time  `json:"createdAt"`
	LastUsedAt         *time.Time `json:"lastUsedAt,omitempty"`
	RevokedAt          *time.Time `json:"revokedAt,omitempty"`
}

// CreateScimTokenParams holds the fields required to insert a new ScimToken.
type CreateScimTokenParams struct {
	Name               string
	TokenHash          string
	TokenPrefix        string
	IdentityProviderID *string
	CreatedBy          string
}

func (store *Store) CreateScimToken(ctx context.Context, p CreateScimTokenParams) (*ScimToken, error) {
	var t ScimToken
	err := store.pool.QueryRow(ctx, `
		INSERT INTO "ScimToken" (id, name, "tokenHash", "tokenPrefix", "identityProviderId", "createdBy")
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5)
		RETURNING id, name, "tokenHash", "tokenPrefix", "identityProviderId", "createdBy", "createdAt", "lastUsedAt", "revokedAt"
	`, p.Name, p.TokenHash, p.TokenPrefix, p.IdentityProviderID, p.CreatedBy).
		Scan(&t.ID, &t.Name, &t.TokenHash, &t.TokenPrefix, &t.IdentityProviderID,
			&t.CreatedBy, &t.CreatedAt, &t.LastUsedAt, &t.RevokedAt)
	if err != nil {
		return nil, fmt.Errorf("create scim token: %w", err)
	}
	return &t, nil
}

func (store *Store) ListScimTokens(ctx context.Context, idpID *string) ([]ScimToken, error) {
	q := `SELECT id, name, "tokenHash", "tokenPrefix", "identityProviderId", "createdBy", "createdAt", "lastUsedAt", "revokedAt"
		  FROM "ScimToken" WHERE "revokedAt" IS NULL`
	args := []any{}
	if idpID != nil {
		q += ` AND "identityProviderId" = $1`
		args = append(args, *idpID)
	}
	q += ` ORDER BY "createdAt" DESC`

	rows, err := store.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list scim tokens: %w", err)
	}
	defer rows.Close()

	var out []ScimToken
	for rows.Next() {
		var t ScimToken
		if err := rows.Scan(&t.ID, &t.Name, &t.TokenHash, &t.TokenPrefix, &t.IdentityProviderID,
			&t.CreatedBy, &t.CreatedAt, &t.LastUsedAt, &t.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetScimTokenByHash looks up an active (non-revoked) token by its hash for auth verification.
func (store *Store) GetScimTokenByHash(ctx context.Context, hash string) (*ScimToken, error) {
	var t ScimToken
	err := store.pool.QueryRow(ctx, `
		SELECT id, name, "tokenHash", "tokenPrefix", "identityProviderId", "createdBy", "createdAt", "lastUsedAt", "revokedAt"
		FROM "ScimToken" WHERE "tokenHash" = $1 AND "revokedAt" IS NULL
	`, hash).Scan(&t.ID, &t.Name, &t.TokenHash, &t.TokenPrefix, &t.IdentityProviderID,
		&t.CreatedBy, &t.CreatedAt, &t.LastUsedAt, &t.RevokedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get scim token: %w", err)
	}
	return &t, nil
}

// TouchScimToken updates lastUsedAt to now. Silently ignores not-found errors.
func (store *Store) TouchScimToken(ctx context.Context, id string) {
	_, _ = store.pool.Exec(ctx, `UPDATE "ScimToken" SET "lastUsedAt" = NOW() WHERE id = $1`, id)
}

// RevokeScimToken sets revokedAt to now. Returns false if the token was not found.
func (store *Store) RevokeScimToken(ctx context.Context, id string) error {
	tag, err := store.pool.Exec(ctx, `UPDATE "ScimToken" SET "revokedAt" = NOW() WHERE id = $1 AND "revokedAt" IS NULL`, id)
	if err != nil {
		return fmt.Errorf("revoke scim token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}


// IdentityProviderRecord is the admin-API view of an IdentityProvider row.
// `Config` and `RoleMapping` are returned as raw JSON; the handler layer
// masks secret fields before writing the response.
//
// This struct represents an *external* IdP (OIDC or SAML). The seed `local`
// row also lives in the same table but the admin UI filters it out; the `Type`
// field carries the distinction.
type IdentityProviderRecord struct {
	ID          string    `json:"id"`
	Type        string    `json:"type"` // "local" | "oidc" | "saml"
	Name        string    `json:"name"`
	Enabled     bool      `json:"enabled"`
	Config      []byte    `json:"-"` // raw JSONB; handler decodes + masks
	RoleMapping []byte    `json:"-"` // raw JSONB
	DefaultRole string    `json:"defaultRole"`
	JITEnabled  bool      `json:"jitEnabled"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

func (store *Store) ListIdentityProviders(ctx context.Context) ([]IdentityProviderRecord, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT id, type, name, enabled, config, "roleMapping", "defaultRole", "jitEnabled", "createdAt", "updatedAt"
		FROM "IdentityProvider"
		ORDER BY "createdAt" DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("list identity providers: %w", err)
	}
	defer rows.Close()

	var out []IdentityProviderRecord
	for rows.Next() {
		var r IdentityProviderRecord
		if err := rows.Scan(&r.ID, &r.Type, &r.Name, &r.Enabled, &r.Config, &r.RoleMapping, &r.DefaultRole, &r.JITEnabled, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if out == nil {
		out = []IdentityProviderRecord{}
	}
	return out, rows.Err()
}

// GetIdentityProvider returns one IdP by id.
func (store *Store) GetIdentityProvider(ctx context.Context, id string) (*IdentityProviderRecord, error) {
	var r IdentityProviderRecord
	err := store.pool.QueryRow(ctx, `
		SELECT id, type, name, enabled, config, "roleMapping", "defaultRole", "jitEnabled", "createdAt", "updatedAt"
		FROM "IdentityProvider"
		WHERE id = $1
	`, id).Scan(&r.ID, &r.Type, &r.Name, &r.Enabled, &r.Config, &r.RoleMapping, &r.DefaultRole, &r.JITEnabled, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// CreateIdentityProviderParams is the write input for POST. `Config` and
// `RoleMapping` are accepted as raw JSON bytes; callers are expected to
// validate + encrypt secret fields before passing them in.
type CreateIdentityProviderParams struct {
	Type        string
	Name        string
	Enabled     bool
	Config      []byte // JSONB
	RoleMapping []byte // JSONB; nil → []
	DefaultRole string
	JITEnabled  bool
}

// CreateIdentityProvider inserts a new IdP row and returns the persisted record.
func (store *Store) CreateIdentityProvider(ctx context.Context, p CreateIdentityProviderParams) (*IdentityProviderRecord, error) {
	if p.Config == nil {
		p.Config = []byte(`{}`)
	}
	if p.RoleMapping == nil {
		p.RoleMapping = []byte(`[]`)
	}
	if p.DefaultRole == "" {
		p.DefaultRole = "developer"
	}
	var r IdentityProviderRecord
	err := store.pool.QueryRow(ctx, `
		INSERT INTO "IdentityProvider" (id, type, name, enabled, config, "roleMapping", "defaultRole", "jitEnabled", "createdAt", "updatedAt")
		VALUES (gen_random_uuid(), $1, $2, $3, $4::jsonb, $5::jsonb, $6, $7, NOW(), NOW())
		RETURNING id, type, name, enabled, config, "roleMapping", "defaultRole", "jitEnabled", "createdAt", "updatedAt"
	`, p.Type, p.Name, p.Enabled, p.Config, p.RoleMapping, p.DefaultRole, p.JITEnabled).
		Scan(&r.ID, &r.Type, &r.Name, &r.Enabled, &r.Config, &r.RoleMapping, &r.DefaultRole, &r.JITEnabled, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("create identity provider: %w", err)
	}
	return &r, nil
}

// UpdateIdentityProviderParams mirrors CreateIdentityProviderParams plus the id.
type UpdateIdentityProviderParams struct {
	ID          string
	Type        string
	Name        string
	Enabled     bool
	Config      []byte
	RoleMapping []byte
	DefaultRole string
	JITEnabled  bool
}

// UpdateIdentityProvider replaces an existing row. Returns pgx.ErrNoRows if id missing.
func (store *Store) UpdateIdentityProvider(ctx context.Context, p UpdateIdentityProviderParams) (*IdentityProviderRecord, error) {
	if p.Config == nil {
		p.Config = []byte(`{}`)
	}
	if p.RoleMapping == nil {
		p.RoleMapping = []byte(`[]`)
	}
	var r IdentityProviderRecord
	err := store.pool.QueryRow(ctx, `
		UPDATE "IdentityProvider"
		SET type = $2, name = $3, enabled = $4, config = $5::jsonb, "roleMapping" = $6::jsonb,
		    "defaultRole" = $7, "jitEnabled" = $8, "updatedAt" = NOW()
		WHERE id = $1
		RETURNING id, type, name, enabled, config, "roleMapping", "defaultRole", "jitEnabled", "createdAt", "updatedAt"
	`, p.ID, p.Type, p.Name, p.Enabled, p.Config, p.RoleMapping, p.DefaultRole, p.JITEnabled).
		Scan(&r.ID, &r.Type, &r.Name, &r.Enabled, &r.Config, &r.RoleMapping, &r.DefaultRole, &r.JITEnabled, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("update identity provider: %w", err)
	}
	return &r, nil
}

// CountFederatedIdentitiesForIdP returns the number of UserFederatedIdentity
// rows linked to the given IdP — used by the DELETE handler to gate the
// destructive cascade behind ?force=true.
func (store *Store) CountFederatedIdentitiesForIdP(ctx context.Context, idpID string) (int, error) {
	var n int
	if err := store.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM "UserFederatedIdentity" WHERE "idpId" = $1`, idpID,
	).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// DeleteIdentityProvider removes the IdP row. The schema cascades
// IdpGroupMapping and SetNull's ScimToken.identityProviderId; the caller
// is responsible for fanning out session revocations to any linked users
// before invoking this (or providing `force=true` semantics).
//
// If `force` is true and linked UserFederatedIdentity rows exist, this
// pre-deletes those rows so the IdP delete is not blocked by the
// Restrict-by-default FK on UserFederatedIdentity.idpId.
func (store *Store) DeleteIdentityProvider(ctx context.Context, idpID string, force bool) error {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if force {
		if _, err := tx.Exec(ctx, `DELETE FROM "UserFederatedIdentity" WHERE "idpId" = $1`, idpID); err != nil {
			return fmt.Errorf("delete federated identities: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE "ScimToken" SET "revokedAt" = NOW() WHERE "identityProviderId" = $1 AND "revokedAt" IS NULL`,
			idpID,
		); err != nil {
			return fmt.Errorf("revoke scim tokens: %w", err)
		}
	}

	tag, err := tx.Exec(ctx, `DELETE FROM "IdentityProvider" WHERE id = $1`, idpID)
	if err != nil {
		return fmt.Errorf("delete identity provider: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return tx.Commit(ctx)
}


// IdpGroupMapping maps an external IdP group to an internal IamGroup.
// When SCIM pushes a group membership for a user, the resolved IamGroup's
// policy attachments govern what that user is permitted to do.
type IdpGroupMapping struct {
	ID                 string    `json:"id"`
	IdentityProviderID string    `json:"identityProviderId"`
	ExternalGroupID    string    `json:"externalGroupId"`
	ExternalGroupName  *string   `json:"externalGroupName,omitempty"`
	IamGroupID         string    `json:"iamGroupId"`
	CreatedAt          time.Time `json:"createdAt"`

	// Eagerly loaded for list views.
	IamGroupName *string `json:"iamGroupName,omitempty"`
}

type CreateIdpGroupMappingParams struct {
	IdentityProviderID string
	ExternalGroupID    string
	ExternalGroupName  *string
	IamGroupID         string
}

func (store *Store) CreateIdpGroupMapping(ctx context.Context, p CreateIdpGroupMappingParams) (*IdpGroupMapping, error) {
	var m IdpGroupMapping
	err := store.pool.QueryRow(ctx, `
		INSERT INTO "IdpGroupMapping" (id, "identityProviderId", "externalGroupId", "externalGroupName", "iamGroupId")
		VALUES (gen_random_uuid(), $1, $2, $3, $4)
		ON CONFLICT ("identityProviderId", "externalGroupId") DO UPDATE
		  SET "externalGroupName" = EXCLUDED."externalGroupName", "iamGroupId" = EXCLUDED."iamGroupId"
		RETURNING id, "identityProviderId", "externalGroupId", "externalGroupName", "iamGroupId", "createdAt"
	`, p.IdentityProviderID, p.ExternalGroupID, p.ExternalGroupName, p.IamGroupID).
		Scan(&m.ID, &m.IdentityProviderID, &m.ExternalGroupID, &m.ExternalGroupName, &m.IamGroupID, &m.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("create idp group mapping: %w", err)
	}
	return &m, nil
}

func (store *Store) ListIdpGroupMappings(ctx context.Context, idpID string) ([]IdpGroupMapping, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT m.id, m."identityProviderId", m."externalGroupId", m."externalGroupName", m."iamGroupId", m."createdAt", g.name
		FROM "IdpGroupMapping" m
		LEFT JOIN "IamGroup" g ON g.id = m."iamGroupId"
		WHERE m."identityProviderId" = $1
		ORDER BY m."createdAt" DESC
	`, idpID)
	if err != nil {
		return nil, fmt.Errorf("list idp group mappings: %w", err)
	}
	defer rows.Close()

	var out []IdpGroupMapping
	for rows.Next() {
		var m IdpGroupMapping
		if err := rows.Scan(&m.ID, &m.IdentityProviderID, &m.ExternalGroupID, &m.ExternalGroupName,
			&m.IamGroupID, &m.CreatedAt, &m.IamGroupName); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (store *Store) DeleteIdpGroupMapping(ctx context.Context, id string) error {
	tag, err := store.pool.Exec(ctx, `DELETE FROM "IdpGroupMapping" WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete idp group mapping: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// FindIdpGroupMappingByExternal returns the IdpGroupMapping for an
// (idpId, externalGroupId) pair, or nil if no such mapping exists.
// Used by SCIM Group provisioning to route IdP-pushed memberships
// into the admin-configured Nexus IamGroup that carries policy.
func (store *Store) FindIdpGroupMappingByExternal(ctx context.Context, idpID, externalGroupID string) (*IdpGroupMapping, error) {
	var m IdpGroupMapping
	err := store.pool.QueryRow(ctx, `
		SELECT m.id, m."identityProviderId", m."externalGroupId", m."externalGroupName", m."iamGroupId", m."createdAt", g.name
		FROM "IdpGroupMapping" m
		LEFT JOIN "IamGroup" g ON g.id = m."iamGroupId"
		WHERE m."identityProviderId" = $1 AND m."externalGroupId" = $2
	`, idpID, externalGroupID).Scan(
		&m.ID, &m.IdentityProviderID, &m.ExternalGroupID, &m.ExternalGroupName,
		&m.IamGroupID, &m.CreatedAt, &m.IamGroupName,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("find idp group mapping by external: %w", err)
	}
	return &m, nil
}

// CreateScimIamGroup inserts a new IamGroup row tagged as SCIM-managed
// (source='scim', identity_provider_id=<idpId>). Used when an external
// IdP pushes a Group whose externalGroupId has no admin-configured
// mapping yet — Nexus auto-creates a group + auto-backfills the
// mapping so subsequent PATCHes route correctly. The group is created
// with NO policy attachments; admins must attach policies before
// membership grants real permissions.
func (store *Store) CreateScimIamGroup(ctx context.Context, name string, description *string, idpID, createdBy string) (*iamstore.GroupRow, error) {
	row := store.pool.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO "IamGroup" (id, name, description, source, identity_provider_id, "createdBy", "createdAt", "updatedAt")
		VALUES (gen_random_uuid(), $1, $2, 'scim', $3, $4, NOW(), NOW())
		RETURNING %s
	`, iamstore.IamGroupColumns), name, description, idpID, createdBy)
	var g iamstore.GroupRow
	err := row.Scan(&g.ID, &g.Name, &g.Description, &g.CreatedBy, &g.CreatedAt, &g.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("create scim iam group: %w", err)
	}
	return &g, nil
}

// GetIamGroupSource returns the source/identity_provider_id columns
// for the IamGroup row. Used by SCIM PATCH/Replace to verify the
// group being mutated is actually SCIM-managed (rather than letting
// SCIM clobber a manually-configured admin group via id collision).
func (store *Store) GetIamGroupSource(ctx context.Context, id string) (source string, idpID *string, err error) {
	err = store.pool.QueryRow(ctx,
		`SELECT source, identity_provider_id FROM "IamGroup" WHERE id = $1`, id,
	).Scan(&source, &idpID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, nil
	}
	return source, idpID, err
}

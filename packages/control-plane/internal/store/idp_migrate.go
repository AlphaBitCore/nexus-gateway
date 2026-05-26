package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
)

// MigrateLegacySSOConfigToIdentityProviders converts a legacy
// `SystemMetadata["sso.config"]` blob (single-IdP shape) into rows on
// the `IdentityProvider` table. Nexus is the SP; external IdPs are
// first-class rows. The blob held at most one OIDC + one SAML entry; each
// enabled half is materialised as its own `IdentityProvider` row.
//
// Behaviour:
//   - Idempotent: if the IdentityProvider table already has any row of
//     the corresponding protocol (oidc/saml), nothing happens for that protocol.
//   - No-op when no legacy blob exists.
//   - Returns the number of rows created.
//
// Runs at CP startup; produced rows are immediately visible on
// /iam/identity-providers as editable entries.
func (db *DB) MigrateLegacySSOConfigToIdentityProviders(ctx context.Context, logger *slog.Logger) (int, error) {
	raw, err := loadSystemMetadataRaw(ctx, db, "sso.config")
	if err != nil {
		return 0, err
	}
	if raw == nil {
		// Try V1 legacy key one more time before giving up.
		raw, err = loadSystemMetadataRaw(ctx, db, "oidc.config")
		if err != nil {
			return 0, err
		}
		if raw == nil {
			return 0, nil
		}
	}

	// Parse a permissive shape — either {oidc:{}, saml:{}} (v2) or flat OIDC (v1).
	var blob struct {
		OIDC map[string]any `json:"oidc"`
		SAML map[string]any `json:"saml"`
		// v1 fallback fields
		Enabled  bool   `json:"enabled"`
		Issuer   string `json:"issuer"`
		ClientID string `json:"clientId"`
	}
	if err := json.Unmarshal(raw, &blob); err != nil {
		return 0, fmt.Errorf("parse legacy sso config: %w", err)
	}

	// If we got a flat v1 shape, lift it into the OIDC slot.
	if blob.OIDC == nil && blob.Issuer != "" {
		flat := map[string]any{}
		_ = json.Unmarshal(raw, &flat)
		blob.OIDC = flat
	}

	created := 0
	for proto, cfg := range map[string]map[string]any{"oidc": blob.OIDC, "saml": blob.SAML} {
		if cfg == nil {
			continue
		}
		// Permissive import: an entry is "worth migrating" if it has any
		// configured value the operator has typed in — even if enabled=false.
		// This preserves a draft that was saved-but-not-enabled, so the operator
		// can flip it on from the new IdP admin UI rather than retyping it.
		// Heuristic: any non-empty string field in the config blob (besides
		// `enabled` itself) qualifies as "configured".
		if !hasMeaningfulValue(cfg) {
			continue
		}
		enabled, _ := cfg["enabled"].(bool)

		// Skip if any row of this protocol already exists.
		exists, err := identityProviderExistsByType(ctx, db, proto)
		if err != nil {
			return created, err
		}
		if exists {
			if logger != nil {
				logger.Info("legacy sso config: skipping migration — row already present",
					"protocol", proto)
			}
			continue
		}
		// Synthesize a name from displayName or fall back to a vendor label.
		name, _ := cfg["displayName"].(string)
		name = strings.TrimSpace(name)
		if name == "" {
			name = "Imported " + proto + " IdP"
		}
		// Strip the legacy-only `enabled` + `displayName` from the config blob;
		// `enabled` becomes a row column and `displayName` becomes the name.
		delete(cfg, "enabled")
		delete(cfg, "displayName")
		cfgBytes, _ := json.Marshal(cfg)

		_, err = db.CreateIdentityProvider(ctx, CreateIdentityProviderParams{
			Type:        proto,
			Name:        name,
			Enabled:     enabled,
			Config:      cfgBytes,
			DefaultRole: "developer",
			JITEnabled:  proto == "oidc", // SAML JIT requires SCIM provisioning first
		})
		if err != nil {
			return created, fmt.Errorf("create %s IdP from legacy blob: %w", proto, err)
		}
		created++
		if logger != nil {
			logger.Info("legacy sso config: imported into IdentityProvider row",
				"protocol", proto, "name", name, "enabled", enabled)
		}
	}
	return created, nil
}

// hasMeaningfulValue returns true if the config map contains at least one
// non-empty string value in a non-bookkeeping field. Used by the migration
// to skip blanks (defaults that the operator never edited).
func hasMeaningfulValue(cfg map[string]any) bool {
	for k, v := range cfg {
		if k == "enabled" {
			continue
		}
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			return true
		}
	}
	return false
}

func loadSystemMetadataRaw(ctx context.Context, db *DB, key string) (json.RawMessage, error) {
	var raw json.RawMessage
	err := db.pool.QueryRow(ctx,
		`SELECT value FROM system_metadata WHERE key = $1`, key).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return raw, err
}

func identityProviderExistsByType(ctx context.Context, db *DB, idpType string) (bool, error) {
	// Idempotency check is intentionally permissive — disabled rows count.
	// Once a row of the given protocol exists (regardless of enabled
	// state), the migration is treated as already done and skipped, so
	// repeat CP restarts don't accumulate duplicates from a saved-but-
	// disabled legacy blob.
	var n int
	err := db.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM "IdentityProvider" WHERE type = $1`, idpType,
	).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// LinkUserToIdP creates a UserFederatedIdentity row tying a NexusUser
// to an external IdP. Used by SCIM CreateUser to stamp provenance on
// users that arrive via SCIM push: their parent IdP is the one the
// SCIM bearer token was scoped to.
//
// Idempotent — if a link already exists for (idpId, externalSubject),
// the call succeeds without creating a duplicate.
func (db *DB) LinkUserToIdP(ctx context.Context, userID, idpID, externalSubject string, externalEmail *string) error {
	_, err := db.pool.Exec(ctx, `
		INSERT INTO "UserFederatedIdentity" (id, "userId", "idpId", "externalSubject", "externalEmail", "linkedAt")
		VALUES (gen_random_uuid(), $1, $2, $3, $4, NOW())
		ON CONFLICT ("idpId", "externalSubject") DO NOTHING
	`, userID, idpID, externalSubject, externalEmail)
	return err
}

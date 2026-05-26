package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// Credential represents an encrypted provider credential.
type Credential struct {
	ID              string
	Name            string
	ProviderID      string
	EncryptedKey    string
	EncryptionIv    string
	EncryptionTag   string
	EncryptionKeyID string // key version used for encryption; defaults to "v1"
	Enabled         bool
	RotationState   string // none | pending_rotation | validating | rotated | completed
	SelectionWeight int    // pool selection weight; higher = preferred (default 100)
	Status          string // active | retiring | retired
	// ReliabilityOverrides carries optional per-credential reliability
	// threshold overrides. Empty bytes / nil = no override (fall back to
	// global Hub-shadow values, then credstate.DefaultThresholds). JSON
	// shape matches credstate.Thresholds with all fields optional.
	ReliabilityOverrides json.RawMessage
}

const credentialColumns = `id, name, "providerId", "encryptedKey", "encryptionIv", "encryptionTag",
       COALESCE(encryption_key_id, 'v1'), enabled, COALESCE("rotationState", 'none'),
       COALESCE("selectionWeight", 100), COALESCE(status, 'active'),
       "reliabilityOverrides"`

func scanCredential(row interface{ Scan(...any) error }, c *Credential) error {
	return row.Scan(&c.ID, &c.Name, &c.ProviderID, &c.EncryptedKey,
		&c.EncryptionIv, &c.EncryptionTag, &c.EncryptionKeyID, &c.Enabled, &c.RotationState,
		&c.SelectionWeight, &c.Status, &c.ReliabilityOverrides)
}

// GetCredentialByID fetches a credential by ID.
func (db *DB) GetCredentialByID(ctx context.Context, id string) (*Credential, error) {
	row := db.pool.QueryRow(ctx,
		`SELECT `+credentialColumns+` FROM "Credential" WHERE id = $1`, id)
	var c Credential
	if err := scanCredential(row, &c); err != nil {
		return nil, fmt.Errorf("store: get credential by id: %w", err)
	}
	return &c, nil
}

// GetCredentialForProvider returns the first enabled active credential for a provider,
// ordered by creation date (newest first).
func (db *DB) GetCredentialForProvider(ctx context.Context, providerID string) (*Credential, error) {
	row := db.pool.QueryRow(ctx, `
		SELECT `+credentialColumns+`
		FROM "Credential"
		WHERE "providerId" = $1 AND enabled = true AND COALESCE(status, 'active') = 'active'
		ORDER BY "createdAt" DESC
		LIMIT 1
	`, providerID)
	var c Credential
	if err := scanCredential(row, &c); err != nil {
		return nil, fmt.Errorf("store: get credential for provider: %w", err)
	}
	return &c, nil
}

// ListCredentialsForProvider returns all enabled, active, weight>0 credentials for a
// provider ordered newest-first. Satisfies credentials.Source so *store.DB can be
// used as a fallback when cachelayer is unavailable.
func (db *DB) ListCredentialsForProvider(ctx context.Context, providerID string) ([]Credential, error) {
	return db.ListEnabledForProvider(ctx, providerID)
}

// ListEnabledForProvider returns all enabled, active, weight>0 credentials for a
// provider ordered newest-first. Used by the credential pool selector.
func (db *DB) ListEnabledForProvider(ctx context.Context, providerID string) ([]Credential, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT `+credentialColumns+`
		FROM "Credential"
		WHERE "providerId" = $1
		  AND enabled = true
		  AND COALESCE(status, 'active') = 'active'
		  AND COALESCE("selectionWeight", 100) > 0
		ORDER BY "createdAt" DESC
	`, providerID)
	if err != nil {
		return nil, fmt.Errorf("store: list enabled credentials for provider: %w", err)
	}
	defer rows.Close()
	var out []Credential
	for rows.Next() {
		var c Credential
		if err := scanCredential(rows, &c); err != nil {
			return nil, fmt.Errorf("store: scan credential: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

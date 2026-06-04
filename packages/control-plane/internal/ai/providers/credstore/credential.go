package credstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Credential represents a row from the Credential table (metadata only — never expose encrypted fields in API).
type Credential struct {
	ID                string     `json:"id"`
	Name              string     `json:"name"`
	ProviderID        string     `json:"providerId"`
	Enabled           bool       `json:"enabled"`
	RotationState     *string    `json:"rotationState"`
	LastRotatedAt     *time.Time `json:"lastRotatedAt"`
	LastUsedAt        *time.Time `json:"lastUsedAt"`
	LastSuccessAt     *time.Time `json:"lastSuccessAt"`
	LastFailureAt     *time.Time `json:"lastFailureAt"`
	LastFailureReason *string    `json:"lastFailureReason"`
	TotalUsageCount   int        `json:"totalUsageCount"`
	ExpiresAt         *time.Time `json:"expiresAt"`
	SelectionWeight   int        `json:"selectionWeight"`
	Status            string     `json:"status"`
	RetireAt          *time.Time `json:"retireAt"`
	// Circuit breaker durable state. Live auth_fails counter stays in Redis;
	// admin handler merges via withCircuit().
	CircuitState       string     `json:"circuitState"`
	CircuitReason      *string    `json:"circuitReason,omitempty"`
	CircuitOpenedAt    *time.Time `json:"circuitOpenedAt,omitempty"`
	CircuitNextProbeAt *time.Time `json:"circuitNextProbeAt,omitempty"`
	// Health classification rolled up by Hub credential-health-rollup.
	HealthStatus          string     `json:"healthStatus"`
	HealthSuccessRate5m   *float64   `json:"healthSuccessRate5m,omitempty"`
	HealthSuccessRate1h   *float64   `json:"healthSuccessRate1h,omitempty"`
	HealthSamplesObserved int        `json:"healthSamplesObserved"`
	HealthDominantError   *string    `json:"healthDominantError,omitempty"`
	HealthTrend           *string    `json:"healthTrend,omitempty"`
	HealthStatusChangedAt *time.Time `json:"healthStatusChangedAt,omitempty"`
	HealthCheckedAt       *time.Time `json:"healthCheckedAt,omitempty"`
	// Per-credential reliability threshold overrides. Empty JSON / nil = no
	// override (fall back to Hub-shadow globals + defaults).
	ReliabilityOverrides json.RawMessage `json:"reliabilityOverrides,omitempty"`
	CreatedAt            time.Time       `json:"createdAt"`
	UpdatedAt            time.Time       `json:"updatedAt"`
}

// CredentialEncrypted includes the encrypted key fields (for internal decrypt use only).
type CredentialEncrypted struct {
	Credential
	EncryptedKey    string `json:"-"`
	EncryptionIV    string `json:"-"`
	EncryptionTag   string `json:"-"`
	EncryptionKeyID string `json:"-"`
}

const CredMetadataColumns = `id, name, "providerId", enabled, "rotationState",
	"lastRotatedAt", "lastUsedAt", "lastSuccessAt", "lastFailureAt",
	"lastFailureReason", "totalUsageCount", "expiresAt",
	"selectionWeight", status, "retireAt",
	"circuitState", "circuitReason", "circuitOpenedAt", "circuitNextProbeAt",
	"healthStatus", "healthSuccessRate5m", "healthSuccessRate1h", "healthSamplesObserved",
	"healthDominantError", "healthTrend", "healthStatusChangedAt", "healthCheckedAt",
	"reliabilityOverrides",
	"createdAt", "updatedAt"`

func scanCredential(row pgx.Row) (*Credential, error) {
	var c Credential
	if err := scanCredentialFields(row, &c); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &c, nil
}

// ScanCredentialRow binds a full CredMetadataColumns row onto a Credential. It
// is the single source of truth for the credential column↔field mapping;
// cross-package callers that run `... RETURNING ` + CredMetadataColumns (e.g.
// providerstore's atomic create) MUST use it rather than re-listing the Scan
// destinations, which silently drifts as columns are added.
func ScanCredentialRow(row pgx.Row) (*Credential, error) { return scanCredential(row) }

// scanCredentialFields binds CredMetadataColumns onto c. Shared by row-at-a-time
// and rows.Next() callers so the column order stays single-sourced.
func scanCredentialFields(row pgx.Row, c *Credential) error {
	return row.Scan(
		&c.ID, &c.Name, &c.ProviderID, &c.Enabled, &c.RotationState,
		&c.LastRotatedAt, &c.LastUsedAt, &c.LastSuccessAt, &c.LastFailureAt,
		&c.LastFailureReason, &c.TotalUsageCount, &c.ExpiresAt,
		&c.SelectionWeight, &c.Status, &c.RetireAt,
		&c.CircuitState, &c.CircuitReason, &c.CircuitOpenedAt, &c.CircuitNextProbeAt,
		&c.HealthStatus, &c.HealthSuccessRate5m, &c.HealthSuccessRate1h, &c.HealthSamplesObserved,
		&c.HealthDominantError, &c.HealthTrend, &c.HealthStatusChangedAt, &c.HealthCheckedAt,
		&c.ReliabilityOverrides,
		&c.CreatedAt, &c.UpdatedAt,
	)
}

// CredentialListParams holds filter/pagination for listing credentials.
type CredentialListParams struct {
	Q          string
	Enabled    *bool
	ProviderID string
	Limit      int
	Offset     int
}

// ListCredentials returns credential metadata with optional filtering.
func (store *Store) ListCredentials(ctx context.Context, p CredentialListParams) ([]Credential, int, error) {
	where := "WHERE 1=1"
	args := []any{}
	argIdx := 1

	if p.ProviderID != "" {
		where += fmt.Sprintf(` AND c."providerId" = $%d`, argIdx)
		args = append(args, p.ProviderID)
		argIdx++
	}
	if p.Enabled != nil {
		where += fmt.Sprintf(` AND c.enabled = $%d`, argIdx)
		args = append(args, *p.Enabled)
		argIdx++
	}
	if p.Q != "" {
		where += fmt.Sprintf(` AND (c.name ILIKE $%d)`, argIdx)
		args = append(args, "%"+escapeILIKE(p.Q)+"%")
		argIdx++
	}

	var total int
	if err := store.pool.QueryRow(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM "Credential" c %s`, where), args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count credentials: %w", err)
	}

	q := fmt.Sprintf(`SELECT c.%s FROM "Credential" c %s ORDER BY c."createdAt" DESC LIMIT $%d OFFSET $%d`,
		CredMetadataColumns, where, argIdx, argIdx+1)
	args = append(args, p.Limit, p.Offset)

	rows, err := store.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list credentials: %w", err)
	}
	defer rows.Close()

	creds := []Credential{}
	for rows.Next() {
		var c Credential
		if err := scanCredentialFields(rows, &c); err != nil {
			return nil, 0, fmt.Errorf("scan credential: %w", err)
		}
		creds = append(creds, c)
	}
	return creds, total, rows.Err()
}

// GetCredential returns credential metadata by ID.
func (store *Store) GetCredential(ctx context.Context, id string) (*Credential, error) {
	q := fmt.Sprintf(`SELECT %s FROM "Credential" WHERE id = $1`, CredMetadataColumns)
	c, err := scanCredential(store.pool.QueryRow(ctx, q, id))
	if err != nil {
		return nil, fmt.Errorf("get credential: %w", err)
	}
	return c, nil
}

// GetCredentialEncrypted returns a credential including encrypted key fields (for internal decrypt).
func (store *Store) GetCredentialEncrypted(ctx context.Context, id string) (*CredentialEncrypted, error) {
	row := store.pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT %s,
			"encryptedKey", "encryptionIv", "encryptionTag", "encryption_key_id"
		FROM "Credential"
		WHERE id = $1
	`, CredMetadataColumns), id)

	var c CredentialEncrypted
	err := row.Scan(
		&c.ID, &c.Name, &c.ProviderID, &c.Enabled, &c.RotationState,
		&c.LastRotatedAt, &c.LastUsedAt, &c.LastSuccessAt, &c.LastFailureAt,
		&c.LastFailureReason, &c.TotalUsageCount, &c.ExpiresAt,
		&c.SelectionWeight, &c.Status, &c.RetireAt,
		&c.CircuitState, &c.CircuitReason, &c.CircuitOpenedAt, &c.CircuitNextProbeAt,
		&c.HealthStatus, &c.HealthSuccessRate5m, &c.HealthSuccessRate1h, &c.HealthSamplesObserved,
		&c.HealthDominantError, &c.HealthTrend, &c.HealthStatusChangedAt, &c.HealthCheckedAt,
		&c.ReliabilityOverrides,
		&c.CreatedAt, &c.UpdatedAt,
		&c.EncryptedKey, &c.EncryptionIV, &c.EncryptionTag, &c.EncryptionKeyID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get credential encrypted: %w", err)
	}
	return &c, nil
}

// CreateCredentialParams holds fields for creating a credential.
type CreateCredentialParams struct {
	Name            string
	ProviderID      string
	EncryptedKey    string
	EncryptionIV    string
	EncryptionTag   string
	EncryptionKeyID string
	Enabled         bool
	RotationState   string
	ExpiresAt       *time.Time
	SelectionWeight int // 0 → default 100
}

// CreateCredential inserts a new credential.
func (store *Store) CreateCredential(ctx context.Context, p CreateCredentialParams) (*Credential, error) {
	keyID := p.EncryptionKeyID
	if keyID == "" {
		keyID = "v1"
	}
	weight := p.SelectionWeight
	if weight <= 0 {
		weight = 100
	}
	q := fmt.Sprintf(`
		INSERT INTO "Credential" (id, name, "providerId", "encryptedKey", "encryptionIv", "encryptionTag",
			"encryption_key_id", enabled, "rotationState", "expiresAt", "selectionWeight", status, "createdAt", "updatedAt")
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'active', NOW(), NOW())
		RETURNING %s
	`, CredMetadataColumns)
	c, err := scanCredential(store.pool.QueryRow(ctx, q,
		p.Name, p.ProviderID, p.EncryptedKey, p.EncryptionIV, p.EncryptionTag,
		keyID, p.Enabled, p.RotationState, p.ExpiresAt, weight,
	))
	if err != nil {
		return nil, fmt.Errorf("create credential: %w", err)
	}
	return c, nil
}

// UpdateCredentialEncryption updates just the encrypted key fields and key ID.
func (store *Store) UpdateCredentialEncryption(ctx context.Context, id, encKey, encIV, encTag, encryptionKeyID string) error {
	keyID := encryptionKeyID
	if keyID == "" {
		keyID = "v1"
	}
	_, err := store.pool.Exec(ctx, `
		UPDATE "Credential"
		SET "encryptedKey" = $2, "encryptionIv" = $3, "encryptionTag" = $4,
		    "encryption_key_id" = $5, "lastRotatedAt" = NOW(), "updatedAt" = NOW()
		WHERE id = $1
	`, id, encKey, encIV, encTag, keyID)
	return err
}

// ClearCircuit resets the durable circuit-breaker columns on a credential row
// to the closed state. It is the DB half of an admin circuit reset: the handler
// also deletes the live Redis hash, but the UI badge and the Hub rehydrate-on-
// restart path read these columns, so they MUST be cleared too — otherwise the
// credential shows "open" forever and a Hub restart re-arms the live circuit
// from the stale row. Idempotent; a no-op on an already-closed row.
func (store *Store) ClearCircuit(ctx context.Context, id string) error {
	_, err := store.pool.Exec(ctx, `
		UPDATE "Credential" SET
			"circuitState"       = 'closed',
			"circuitReason"      = NULL,
			"circuitOpenedAt"    = NULL,
			"circuitNextProbeAt" = NULL,
			"updatedAt"          = NOW()
		WHERE id = $1
	`, id)
	return err
}

// UpdateCredentialParams holds optional fields for updating a credential.
type UpdateCredentialParams struct {
	Name            *string
	Enabled         *bool
	RotationState   *string
	LastRotatedAt   *time.Time
	ExpiresAt       *time.Time // new value; nil = clear to SQL NULL
	UpdateExpiresAt bool       // true = write expiresAt column; false = leave unchanged
	SelectionWeight *int
	Status          *string
	RetireAt        *time.Time
	UpdateRetireAt  bool
}

// UpdateCredentialMetadata updates mutable metadata fields using COALESCE.
func (store *Store) UpdateCredentialMetadata(ctx context.Context, id string, p UpdateCredentialParams) (*Credential, error) {
	q := fmt.Sprintf(`UPDATE "Credential" SET
		name = COALESCE($2, name),
		enabled = COALESCE($3, enabled),
		"rotationState" = COALESCE($4, "rotationState"),
		"lastRotatedAt" = COALESCE($5, "lastRotatedAt"),
		"expiresAt" = CASE WHEN $6::boolean THEN $7 ELSE "expiresAt" END,
		"selectionWeight" = COALESCE($8, "selectionWeight"),
		status = COALESCE($9, status),
		"retireAt" = CASE WHEN $10::boolean THEN $11 ELSE "retireAt" END,
		"updatedAt" = NOW()
	WHERE id = $1 RETURNING %s`, CredMetadataColumns)

	c, err := scanCredential(store.pool.QueryRow(ctx, q,
		id, p.Name, p.Enabled, p.RotationState, p.LastRotatedAt,
		p.UpdateExpiresAt, p.ExpiresAt,
		p.SelectionWeight, p.Status,
		p.UpdateRetireAt, p.RetireAt,
	))
	if err != nil {
		return nil, fmt.Errorf("update credential: %w", err)
	}
	return c, nil
}

// CountCredentialsNotOnKey counts credentials that are NOT using the given key ID.
func (store *Store) CountCredentialsNotOnKey(ctx context.Context, keyID string) (int, error) {
	var count int
	err := store.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM "Credential" WHERE encryption_key_id != $1
	`, keyID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count credentials not on key: %w", err)
	}
	return count, nil
}

// ListCredentialsForRotation returns up to `limit` credentials not yet on the target key.
func (store *Store) ListCredentialsForRotation(ctx context.Context, targetKeyID string, limit int) ([]CredentialEncrypted, error) {
	rows, err := store.pool.Query(ctx, fmt.Sprintf(`
		SELECT %s,
			"encryptedKey", "encryptionIv", "encryptionTag", encryption_key_id
		FROM "Credential"
		WHERE encryption_key_id != $1
		ORDER BY "createdAt" ASC
		LIMIT $2
	`, CredMetadataColumns), targetKeyID, limit)
	if err != nil {
		return nil, fmt.Errorf("list credentials for rotation: %w", err)
	}
	defer rows.Close()

	var creds []CredentialEncrypted
	for rows.Next() {
		var c CredentialEncrypted
		if err := rows.Scan(
			&c.ID, &c.Name, &c.ProviderID, &c.Enabled, &c.RotationState,
			&c.LastRotatedAt, &c.LastUsedAt, &c.LastSuccessAt, &c.LastFailureAt,
			&c.LastFailureReason, &c.TotalUsageCount, &c.ExpiresAt,
			&c.SelectionWeight, &c.Status, &c.RetireAt,
			&c.CircuitState, &c.CircuitReason, &c.CircuitOpenedAt, &c.CircuitNextProbeAt,
			&c.HealthStatus, &c.HealthSuccessRate5m, &c.HealthSuccessRate1h, &c.HealthSamplesObserved,
			&c.HealthDominantError, &c.HealthTrend, &c.HealthStatusChangedAt, &c.HealthCheckedAt,
			&c.ReliabilityOverrides,
			&c.CreatedAt, &c.UpdatedAt,
			&c.EncryptedKey, &c.EncryptionIV, &c.EncryptionTag, &c.EncryptionKeyID,
		); err != nil {
			return nil, fmt.Errorf("scan credential for rotation: %w", err)
		}
		creds = append(creds, c)
	}
	return creds, rows.Err()
}

// SetCredentialReliabilityOverrides upserts the per-credential reliability
// threshold overrides JSONB. Pass nil bytes to clear any prior override.
func (store *Store) SetCredentialReliabilityOverrides(ctx context.Context, id string, overrides []byte) error {
	tag, err := store.pool.Exec(ctx, `
		UPDATE "Credential"
		SET    "reliabilityOverrides" = $2::jsonb,
		       "updatedAt"            = NOW()
		WHERE  id = $1
	`, id, overrides)
	if err != nil {
		return fmt.Errorf("set reliability overrides: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// DeleteCredential deletes a credential by ID.
func (store *Store) DeleteCredential(ctx context.Context, id string) error {
	tag, err := store.pool.Exec(ctx, `DELETE FROM "Credential" WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete credential: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

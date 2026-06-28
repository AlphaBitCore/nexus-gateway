package providerstore

import (
	"context"
	"errors"
	"fmt"
	"github.com/goccy/go-json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/providers/credstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/providers/modelstore"
)

// Provider represents a row from the Provider table.
//
// AdapterType is the canonical wire adapter — one of the nine
// providers.Format values (openai, anthropic, gemini, glm, deepseek,
// azure-openai, minimax, bedrock, vertex). Required and mutable:
// callers may change it after create, though doing so does not
// cascade-validate credentials / models / routing rules.
//
// Region is the authoritative deployment region ("us-east-1",
// "eu-west-1", etc.) consumed by the data-residency compliance hook.
// Nullable so existing providers keep working until an operator fills
// it in; the runtime hook treats a missing region as "unknown".
type Provider struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	DisplayName *string         `json:"displayName"`
	Description *string         `json:"description"`
	AdapterType string          `json:"adapterType"`
	BaseURL     string          `json:"baseUrl"`
	PathPrefix  string          `json:"pathPrefix"`
	APIVersion  *string         `json:"apiVersion"`
	Region      *string         `json:"region"`
	Enabled     bool            `json:"enabled"`
	Headers     json.RawMessage `json:"headers"`
	CreatedAt   time.Time       `json:"createdAt"`
	UpdatedAt   time.Time       `json:"updatedAt"`
	ModelCount  *int            `json:"modelCount,omitempty"`
}

// ListParams holds filter/pagination for listing providers.
type ListParams struct {
	Q       string
	Enabled *bool
	Limit   int
	Offset  int
}

// ListProviders returns providers with optional filtering and model counts.
func (s *Store) ListProviders(ctx context.Context, p ListParams) ([]Provider, int, error) {
	where := "WHERE 1=1"
	args := []any{}
	argIdx := 1

	if p.Q != "" {
		where += fmt.Sprintf(` AND (name ILIKE $%d OR "displayName" ILIKE $%d OR description ILIKE $%d)`, argIdx, argIdx, argIdx)
		args = append(args, "%"+escapeILIKE(p.Q)+"%")
		argIdx++
	}
	if p.Enabled != nil {
		where += fmt.Sprintf(` AND enabled = $%d`, argIdx)
		args = append(args, *p.Enabled)
		argIdx++
	}

	// Count
	var total int
	countQuery := fmt.Sprintf(`SELECT COUNT(*) FROM "Provider" %s`, where)
	if err := s.pool.QueryRow(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count providers: %w", err)
	}

	// Data with model count
	dataQuery := fmt.Sprintf(`
		SELECT p.id, p.name, p."displayName", p.description, p.adapter_type, p."baseUrl",
		       p."pathPrefix", p."apiVersion", p.region, p.enabled, p.headers,
		       p."createdAt", p."updatedAt",
		       (SELECT COUNT(*) FROM "Model" m WHERE m."providerId" = p.id) AS model_count
		FROM "Provider" p
		%s
		ORDER BY p."updatedAt" DESC, p.name ASC
		LIMIT $%d OFFSET $%d
	`, where, argIdx, argIdx+1)
	args = append(args, p.Limit, p.Offset)

	rows, err := s.pool.Query(ctx, dataQuery, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list providers: %w", err)
	}
	defer rows.Close()

	providers := []Provider{}
	for rows.Next() {
		var pr Provider
		var mc int
		if err := rows.Scan(
			&pr.ID, &pr.Name, &pr.DisplayName, &pr.Description, &pr.AdapterType, &pr.BaseURL,
			&pr.PathPrefix, &pr.APIVersion, &pr.Region, &pr.Enabled, &pr.Headers,
			&pr.CreatedAt, &pr.UpdatedAt, &mc,
		); err != nil {
			return nil, 0, fmt.Errorf("scan provider: %w", err)
		}
		pr.ModelCount = &mc
		providers = append(providers, pr)
	}
	return providers, total, rows.Err()
}

// GetProvider returns a provider by ID.
func (s *Store) GetProvider(ctx context.Context, id string) (*Provider, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, name, "displayName", description, adapter_type, "baseUrl",
		       "pathPrefix", "apiVersion", region, enabled, headers, "createdAt", "updatedAt"
		FROM "Provider"
		WHERE id = $1
	`, id)

	var p Provider
	err := row.Scan(&p.ID, &p.Name, &p.DisplayName, &p.Description, &p.AdapterType, &p.BaseURL,
		&p.PathPrefix, &p.APIVersion, &p.Region, &p.Enabled, &p.Headers, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get provider: %w", err)
	}
	return &p, nil
}

// CreateParams holds fields for creating a provider.
type CreateParams struct {
	// ID, when set, is used as the provider's primary key instead of a
	// DB-generated uuid. The create-provider-with-inline-credential
	// path generates the provider id app-side so the credential's ciphertext can
	// be AAD-bound to (credentialID, providerID) before the atomic insert.
	ID          string
	Name        string
	DisplayName string
	Description *string
	BaseURL     string
	PathPrefix  string
	AdapterType string
	APIVersion  *string
	Region      *string
	Enabled     bool
	Headers     json.RawMessage
}

// CreateProvider inserts a new provider and returns it.
func (s *Store) CreateProvider(ctx context.Context, p CreateParams) (*Provider, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO "Provider" (id, name, "displayName", description, "baseUrl", "pathPrefix", adapter_type, "apiVersion", region, enabled, headers, "createdAt", "updatedAt")
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NOW(), NOW())
		RETURNING id, name, "displayName", description, adapter_type, "baseUrl", "pathPrefix", "apiVersion", region, enabled, headers, "createdAt", "updatedAt"
	`, p.Name, p.DisplayName, p.Description, p.BaseURL, p.PathPrefix, p.AdapterType, p.APIVersion, p.Region, p.Enabled, p.Headers)

	var pr Provider
	err := row.Scan(&pr.ID, &pr.Name, &pr.DisplayName, &pr.Description, &pr.AdapterType, &pr.BaseURL,
		&pr.PathPrefix, &pr.APIVersion, &pr.Region, &pr.Enabled, &pr.Headers, &pr.CreatedAt, &pr.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("create provider: %w", err)
	}
	return &pr, nil
}

// CreateProviderWithChildren atomically creates a Provider plus an optional
// set of Model rows and a single Credential, all in one transaction.
func (s *Store) CreateProviderWithChildren(
	ctx context.Context,
	provider CreateParams,
	models []modelstore.CreateModelParams,
	credential *credstore.CreateCredentialParams,
) (*Provider, []modelstore.Model, *credstore.Credential, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("begin create provider tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	providerID := provider.ID
	if providerID == "" {
		providerID = uuid.New().String()
	}
	var pr Provider
	err = tx.QueryRow(ctx, `
		INSERT INTO "Provider" (id, name, "displayName", description, "baseUrl", "pathPrefix", adapter_type, "apiVersion", region, enabled, headers, "createdAt", "updatedAt")
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, NOW(), NOW())
		RETURNING id, name, "displayName", description, adapter_type, "baseUrl", "pathPrefix", "apiVersion", region, enabled, headers, "createdAt", "updatedAt"
	`, providerID, provider.Name, provider.DisplayName, provider.Description, provider.BaseURL, provider.PathPrefix, provider.AdapterType, provider.APIVersion, provider.Region, provider.Enabled, provider.Headers,
	).Scan(&pr.ID, &pr.Name, &pr.DisplayName, &pr.Description, &pr.AdapterType, &pr.BaseURL,
		&pr.PathPrefix, &pr.APIVersion, &pr.Region, &pr.Enabled, &pr.Headers, &pr.CreatedAt, &pr.UpdatedAt)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("insert provider: %w", err)
	}

	insertedModels := make([]modelstore.Model, 0, len(models))
	for _, p := range models {
		features := p.Features
		if features == nil {
			features = []string{}
		}
		aliases := p.Aliases
		if aliases == nil {
			aliases = []string{}
		}
		inputMod := p.InputModalities
		if inputMod == nil {
			inputMod = []string{"text"}
		}
		outputMod := p.OutputModalities
		if outputMod == nil {
			outputMod = []string{"text"}
		}
		lifecycle := p.Lifecycle
		if lifecycle == "" {
			lifecycle = "ga"
		}
		var m modelstore.Model
		err = tx.QueryRow(ctx, fmt.Sprintf(`
			INSERT INTO "Model" (id, code, name, description, "providerId", "providerModelId", type, features,
				"inputPricePerMillion", "outputPricePerMillion",
				"cachedInputReadPricePerMillion", "cachedInputWritePricePerMillion",
				"maxContextTokens", "maxOutputTokens",
				aliases, "inputModalities", "outputModalities", lifecycle, "capabilityJson",
				enabled, "createdAt", "updatedAt")
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, NOW(), NOW())
			RETURNING %s
		`, modelstore.ModelColumns),
			uuid.New().String(), p.Code, p.Name, p.Description, pr.ID, p.ProviderModelID, p.Type, features,
			p.InputPricePerMillion, p.OutputPricePerMillion,
			p.CachedInputReadPricePerMillion, p.CachedInputWritePricePerMillion,
			p.MaxContextTokens, p.MaxOutputTokens,
			aliases, inputMod, outputMod, lifecycle, p.CapabilityJson,
			p.Enabled,
		).Scan(
			&m.ID, &m.Code, &m.Name, &m.Description, &m.ProviderID, &m.ProviderModelID,
			&m.Type, &m.Features, &m.InputPricePerMillion, &m.OutputPricePerMillion,
			&m.CachedInputReadPricePerMillion, &m.CachedInputWritePricePerMillion,
			&m.MaxContextTokens, &m.MaxOutputTokens, &m.Status, &m.DeprecationDate, &m.ReplacedBy, &m.Aliases,
			&m.InputModalities, &m.OutputModalities, &m.Lifecycle, &m.CapabilityJson,
			&m.Enabled, &m.CreatedAt, &m.UpdatedAt,
		)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("insert model %q: %w", p.ProviderModelID, err)
		}
		if m.Features == nil {
			m.Features = []string{}
		}
		if m.Aliases == nil {
			m.Aliases = []string{}
		}
		insertedModels = append(insertedModels, m)
	}

	var insertedCred *credstore.Credential
	if credential != nil {
		keyID := credential.EncryptionKeyID
		if keyID == "" {
			keyID = "v1"
		}
		// Bind via credstore's single-sourced scanner: RETURNING
		// CredMetadataColumns yields all 30 credential columns, and an inline
		// Scan list here silently drifted to 14 destinations as columns were
		// added (circuit-breaker + health fields), making every credential
		// insert fail at scan and roll the whole provider create back.
		credID := credential.ID
		if credID == "" {
			credID = uuid.New().String()
		}
		insertedCred, err = credstore.ScanCredentialRow(tx.QueryRow(ctx, fmt.Sprintf(`
			INSERT INTO "Credential" (id, name, "providerId", "encryptedKey", "encryptionIv", "encryptionTag",
				"encryption_key_id", enabled, "rotationState", "createdAt", "updatedAt")
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NOW(), NOW())
			RETURNING %s
		`, credstore.CredMetadataColumns),
			credID, credential.Name, pr.ID, credential.EncryptedKey, credential.EncryptionIV, credential.EncryptionTag,
			keyID, credential.Enabled, credential.RotationState,
		))
		if err != nil {
			return nil, nil, nil, fmt.Errorf("insert credential: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, nil, nil, fmt.Errorf("commit create provider: %w", err)
	}
	return &pr, insertedModels, insertedCred, nil
}

// UpdateParams holds fields for updating a provider.
type UpdateParams struct {
	Name          *string
	DisplayName   *string
	Description   *string
	BaseURL       *string
	AdapterType   *string
	Region        **string
	APIVersion    **string // nil=no change; &nil=clear; &s=set
	UpdateHeaders bool
	Headers       json.RawMessage // used when UpdateHeaders=true; nil clears the column
	Enabled       *bool
}

// UpdateProvider updates a provider's mutable fields.
func (s *Store) UpdateProvider(ctx context.Context, id string, p UpdateParams) (*Provider, error) {
	applyRegion := p.Region != nil
	var regionVal *string
	if applyRegion {
		regionVal = *p.Region
	}
	applyAPIVersion := p.APIVersion != nil
	var apiVersionVal *string
	if applyAPIVersion {
		apiVersionVal = *p.APIVersion
	}
	row := s.pool.QueryRow(ctx, `
		UPDATE "Provider"
		SET name = COALESCE($2, name),
		    "displayName" = COALESCE($3, "displayName"),
		    description = COALESCE($4, description),
		    "baseUrl" = COALESCE($5, "baseUrl"),
		    adapter_type = COALESCE($6, adapter_type),
		    region = CASE WHEN $7::boolean THEN $8 ELSE region END,
		    enabled = COALESCE($9, enabled),
		    "pathPrefix" = CASE WHEN $2 IS NOT NULL THEN '/' || $2 ELSE "pathPrefix" END,
		    "apiVersion" = CASE WHEN $10::boolean THEN $11 ELSE "apiVersion" END,
		    headers = CASE WHEN $12::boolean THEN $13 ELSE headers END,
		    "updatedAt" = NOW()
		WHERE id = $1
		RETURNING id, name, "displayName", description, adapter_type, "baseUrl", "pathPrefix", "apiVersion", region, enabled, headers, "createdAt", "updatedAt"
	`, id, p.Name, p.DisplayName, p.Description, p.BaseURL, p.AdapterType, applyRegion, regionVal, p.Enabled,
		applyAPIVersion, apiVersionVal, p.UpdateHeaders, p.Headers)

	var pr Provider
	err := row.Scan(&pr.ID, &pr.Name, &pr.DisplayName, &pr.Description, &pr.AdapterType, &pr.BaseURL,
		&pr.PathPrefix, &pr.APIVersion, &pr.Region, &pr.Enabled, &pr.Headers, &pr.CreatedAt, &pr.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("update provider: %w", err)
	}
	return &pr, nil
}

// DeleteProvider deletes a provider and its models (cascade).
func (s *Store) DeleteProvider(ctx context.Context, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err := tx.Exec(ctx, `DELETE FROM "Model" WHERE "providerId" = $1`, id); err != nil {
		return fmt.Errorf("delete provider models: %w", err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM "Provider" WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete provider: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return tx.Commit(ctx)
}

// ProviderHealthRow holds a provider health record.
type ProviderHealthRow struct {
	ProviderID  string  `json:"providerId"`
	Provider    string  `json:"provider"`
	Status      string  `json:"status"`
	ErrorRate   float64 `json:"errorRate"`
	AvgLatency  int     `json:"avgLatencyMs"`
	SampleCount int     `json:"sampleCount"`
	LastReqAt   any     `json:"lastRequestAt"`
	LastErrAt   any     `json:"lastErrorAt"`
}

// ListProviderHealth returns all provider health records.
func (s *Store) ListProviderHealth(ctx context.Context) ([]ProviderHealthRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT "providerId", provider, status, "rollingErrorRate", "avgLatencyMs", "sampleCount", "lastRequestAt", "lastErrorAt"
		FROM "ProviderHealth" ORDER BY provider ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("list provider health: %w", err)
	}
	defer rows.Close()

	result := []ProviderHealthRow{}
	for rows.Next() {
		var r ProviderHealthRow
		if err := rows.Scan(&r.ProviderID, &r.Provider, &r.Status, &r.ErrorRate, &r.AvgLatency, &r.SampleCount, &r.LastReqAt, &r.LastErrAt); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

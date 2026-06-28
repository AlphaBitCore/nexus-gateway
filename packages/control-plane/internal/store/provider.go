package store

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
	cpgx "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/pgx"
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

// ProviderListParams holds filter/pagination for listing providers.
type ProviderListParams struct {
	Q       string
	Enabled *bool
	Limit   int
	Offset  int
}

// ListProviders returns providers with optional filtering and model counts.
func (db *DB) ListProviders(ctx context.Context, p ProviderListParams) ([]Provider, int, error) {
	where := "WHERE 1=1"
	args := []any{}
	argIdx := 1

	if p.Q != "" {
		where += fmt.Sprintf(` AND (name ILIKE $%d OR "displayName" ILIKE $%d OR description ILIKE $%d)`, argIdx, argIdx, argIdx)
		args = append(args, "%"+cpgx.EscapeILIKE(p.Q)+"%")
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
	if err := db.pool.QueryRow(ctx, countQuery, args...).Scan(&total); err != nil {
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

	rows, err := db.pool.Query(ctx, dataQuery, args...)
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
func (db *DB) GetProvider(ctx context.Context, id string) (*Provider, error) {
	row := db.pool.QueryRow(ctx, `
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

// CreateProviderParams holds fields for creating a provider.
// AdapterType must be one of the nine canonical providers.Format
// strings; validation lives in the admin handler.
type CreateProviderParams struct {
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
func (db *DB) CreateProvider(ctx context.Context, p CreateProviderParams) (*Provider, error) {
	row := db.pool.QueryRow(ctx, `
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
// set of Model rows and a single Credential, all in one transaction. The
// Wizard submit flow posts everything in one request and expects a
// rollback if any row fails to insert — e.g. a duplicate provider name,
// a model (providerId, providerModelId) collision, or a duplicate `code`
// must leave the DB unchanged, not half-populated.
//
// Caller is responsible for encrypting the credential key before calling.
// Model.id is auto-generated by gen_random_uuid() in the schema; callers
// supply only the customer-facing `code` per row.
func (db *DB) CreateProviderWithChildren(
	ctx context.Context,
	provider CreateProviderParams,
	models []CreateModelParams,
	credential *CreateCredentialParams,
) (*Provider, []Model, *Credential, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("begin create provider tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var pr Provider
	err = tx.QueryRow(ctx, `
		INSERT INTO "Provider" (id, name, "displayName", description, "baseUrl", "pathPrefix", adapter_type, "apiVersion", region, enabled, headers, "createdAt", "updatedAt")
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NOW(), NOW())
		RETURNING id, name, "displayName", description, adapter_type, "baseUrl", "pathPrefix", "apiVersion", region, enabled, headers, "createdAt", "updatedAt"
	`, provider.Name, provider.DisplayName, provider.Description, provider.BaseURL, provider.PathPrefix, provider.AdapterType, provider.APIVersion, provider.Region, provider.Enabled, provider.Headers,
	).Scan(&pr.ID, &pr.Name, &pr.DisplayName, &pr.Description, &pr.AdapterType, &pr.BaseURL,
		&pr.PathPrefix, &pr.APIVersion, &pr.Region, &pr.Enabled, &pr.Headers, &pr.CreatedAt, &pr.UpdatedAt)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("insert provider: %w", err)
	}

	insertedModels := make([]Model, 0, len(models))
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
		var m Model
		err = tx.QueryRow(ctx, fmt.Sprintf(`
			INSERT INTO "Model" (id, code, name, description, "providerId", "providerModelId", type, features,
				"inputPricePerMillion", "outputPricePerMillion", "maxContextTokens", "maxOutputTokens",
				aliases, "inputModalities", "outputModalities", lifecycle, "capabilityJson",
				enabled, "createdAt", "updatedAt")
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, NOW(), NOW())
			RETURNING %s
		`, modelstore.ModelColumns),
			uuid.New().String(), p.Code, p.Name, p.Description, pr.ID, p.ProviderModelID, p.Type, features,
			p.InputPricePerMillion, p.OutputPricePerMillion, p.MaxContextTokens, p.MaxOutputTokens,
			aliases, inputMod, outputMod, lifecycle, p.CapabilityJson,
			p.Enabled,
		).Scan(
			&m.ID, &m.Code, &m.Name, &m.Description, &m.ProviderID, &m.ProviderModelID,
			&m.Type, &m.Features, &m.InputPricePerMillion, &m.OutputPricePerMillion,
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

	var insertedCred *Credential
	if credential != nil {
		keyID := credential.EncryptionKeyID
		if keyID == "" {
			keyID = "v1"
		}
		var c Credential
		err = tx.QueryRow(ctx, fmt.Sprintf(`
			INSERT INTO "Credential" (id, name, "providerId", "encryptedKey", "encryptionIv", "encryptionTag",
				"encryption_key_id", enabled, "rotationState", "createdAt", "updatedAt")
			VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, $8, NOW(), NOW())
			RETURNING %s
		`, credstore.CredMetadataColumns),
			credential.Name, pr.ID, credential.EncryptedKey, credential.EncryptionIV, credential.EncryptionTag,
			keyID, credential.Enabled, credential.RotationState,
		).Scan(
			&c.ID, &c.Name, &c.ProviderID, &c.Enabled, &c.RotationState,
			&c.LastRotatedAt, &c.LastUsedAt, &c.LastSuccessAt, &c.LastFailureAt,
			&c.LastFailureReason, &c.TotalUsageCount, &c.ExpiresAt, &c.CreatedAt, &c.UpdatedAt,
		)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("insert credential: %w", err)
		}
		insertedCred = &c
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, nil, nil, fmt.Errorf("commit create provider: %w", err)
	}
	return &pr, insertedModels, insertedCred, nil
}

// UpdateProviderParams holds fields for updating a provider. Region uses
// a double-pointer so the handler can distinguish "not set in payload"
// (leave as-is) from "set to null" (clear the region). AdapterType is a
// plain pointer — nil means "leave as-is"; a non-nil non-empty value
// overwrites the column (the handler rejects empty / invalid values
// before calling the store). Name and BaseURL are plain pointers — nil
// leaves them unchanged; non-nil overwrites. Updating Name also derives
// a new pathPrefix ("/"+name) atomically.
type UpdateProviderParams struct {
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

// UpdateProvider updates a provider's mutable fields. A Region of nil
// leaves the column unchanged; a non-nil pointer-to-pointer sets the
// region (dereferenced pointer may itself be nil to clear it).
// APIVersion follows the same double-pointer pattern. Headers uses a
// boolean toggle: UpdateHeaders=false leaves the column unchanged;
// UpdateHeaders=true writes the value (nil clears to SQL NULL).
// Updating Name also updates pathPrefix to "/" + new name atomically.
func (db *DB) UpdateProvider(ctx context.Context, id string, p UpdateProviderParams) (*Provider, error) {
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
	row := db.pool.QueryRow(ctx, `
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
func (db *DB) DeleteProvider(ctx context.Context, id string) error {
	tx, err := db.pool.Begin(ctx)
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

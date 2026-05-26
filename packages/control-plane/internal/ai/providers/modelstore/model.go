package modelstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Model represents a row from the Model table.
type Model struct {
	ID string `json:"id"`
	// Code is the customer-facing model identifier (e.g. "gpt-4o"). Globally unique.
	Code                           string           `json:"code"`
	Name                           string           `json:"name"`
	Description                    *string          `json:"description"`
	ProviderID                     string           `json:"providerId"`
	ProviderModelID                string           `json:"providerModelId"`
	Type                           string           `json:"type"`
	Features                       []string         `json:"features"`
	InputPricePerMillion           *float64         `json:"inputPricePerMillion"`
	OutputPricePerMillion          *float64         `json:"outputPricePerMillion"`
	CachedInputReadPricePerMillion  *float64         `json:"cachedInputReadPricePerMillion,omitempty"`
	CachedInputWritePricePerMillion *float64         `json:"cachedInputWritePricePerMillion,omitempty"`
	MaxContextTokens               *int             `json:"maxContextTokens"`
	MaxOutputTokens                *int             `json:"maxOutputTokens"`
	Status                         string           `json:"status"`
	DeprecationDate                *time.Time       `json:"deprecationDate"`
	ReplacedBy                     *string          `json:"replacedBy"`
	Aliases                        []string         `json:"aliases"`
	InputModalities                []string         `json:"inputModalities"`
	OutputModalities               []string         `json:"outputModalities"`
	Lifecycle                      string           `json:"lifecycle"`
	CapabilityJson                 *json.RawMessage `json:"capabilityJson,omitempty"`
	Enabled                        bool             `json:"enabled"`
	CreatedAt                      time.Time        `json:"createdAt"`
	UpdatedAt                      time.Time        `json:"updatedAt"`
}

// ModelListParams holds filter/pagination for listing models.
type ModelListParams struct {
	Q          string
	Type       string
	Status     string
	Enabled    *bool
	ProviderID string
	Limit      int
	Offset     int
}

var ModelColumns = `id, code, name, description, "providerId", "providerModelId", type, features,
	"inputPricePerMillion", "outputPricePerMillion",
	"cachedInputReadPricePerMillion", "cachedInputWritePricePerMillion",
	"maxContextTokens", "maxOutputTokens",
	status, "deprecationDate", "replacedBy", aliases,
	"inputModalities", "outputModalities", lifecycle, "capabilityJson",
	enabled, "createdAt", "updatedAt"`

func scanModel(row pgx.Row) (*Model, error) {
	var m Model
	err := row.Scan(
		&m.ID, &m.Code, &m.Name, &m.Description, &m.ProviderID, &m.ProviderModelID,
		&m.Type, &m.Features, &m.InputPricePerMillion, &m.OutputPricePerMillion,
		&m.CachedInputReadPricePerMillion, &m.CachedInputWritePricePerMillion,
		&m.MaxContextTokens, &m.MaxOutputTokens, &m.Status, &m.DeprecationDate, &m.ReplacedBy, &m.Aliases,
		&m.InputModalities, &m.OutputModalities, &m.Lifecycle, &m.CapabilityJson,
		&m.Enabled, &m.CreatedAt, &m.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if m.Features == nil {
		m.Features = []string{}
	}
	if m.Aliases == nil {
		m.Aliases = []string{}
	}
	if m.InputModalities == nil {
		m.InputModalities = []string{}
	}
	if m.OutputModalities == nil {
		m.OutputModalities = []string{}
	}
	return &m, nil
}

func scanModels(rows pgx.Rows) ([]Model, error) {
	var models []Model
	for rows.Next() {
		var m Model
		if err := rows.Scan(
			&m.ID, &m.Code, &m.Name, &m.Description, &m.ProviderID, &m.ProviderModelID,
			&m.Type, &m.Features, &m.InputPricePerMillion, &m.OutputPricePerMillion,
			&m.CachedInputReadPricePerMillion, &m.CachedInputWritePricePerMillion,
			&m.MaxContextTokens, &m.MaxOutputTokens, &m.Status, &m.DeprecationDate, &m.ReplacedBy, &m.Aliases,
			&m.InputModalities, &m.OutputModalities, &m.Lifecycle, &m.CapabilityJson,
			&m.Enabled, &m.CreatedAt, &m.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan model: %w", err)
		}
		if m.Features == nil {
			m.Features = []string{}
		}
		if m.Aliases == nil {
			m.Aliases = []string{}
		}
		if m.InputModalities == nil {
			m.InputModalities = []string{}
		}
		if m.OutputModalities == nil {
			m.OutputModalities = []string{}
		}
		models = append(models, m)
	}
	return models, rows.Err()
}

// ListModelsFlat returns a flat paginated list of models.
func (store *Store) ListModelsFlat(ctx context.Context, p ModelListParams) ([]Model, int, error) {
	where := "WHERE 1=1"
	args := []any{}
	argIdx := 1

	if p.Q != "" {
		// `code` is the customer-facing identifier (e.g. "gpt-4o") that
		// clients send in `{model: "..."}` requests; pickers like the
		// Routing preview's Model ID typeahead match on this column.
		where += fmt.Sprintf(` AND (m.code ILIKE $%d OR m.name ILIKE $%d OR m.id ILIKE $%d OR m.description ILIKE $%d OR m."providerModelId" ILIKE $%d)`, argIdx, argIdx, argIdx, argIdx, argIdx)
		args = append(args, "%"+escapeILIKE(p.Q)+"%")
		argIdx++
	}
	if p.Type != "" {
		where += fmt.Sprintf(` AND m.type = $%d`, argIdx)
		args = append(args, p.Type)
		argIdx++
	}
	if p.Status != "" {
		where += fmt.Sprintf(` AND m.status = $%d`, argIdx)
		args = append(args, p.Status)
		argIdx++
	}
	if p.Enabled != nil {
		where += fmt.Sprintf(` AND m.enabled = $%d`, argIdx)
		args = append(args, *p.Enabled)
		argIdx++
	}
	if p.ProviderID != "" {
		where += fmt.Sprintf(` AND m."providerId" = $%d`, argIdx)
		args = append(args, p.ProviderID)
		argIdx++
	}

	var total int
	countQ := fmt.Sprintf(`SELECT COUNT(*) FROM "Model" m %s`, where)
	if err := store.pool.QueryRow(ctx, countQ, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count models: %w", err)
	}

	dataQ := fmt.Sprintf(`SELECT m.%s FROM "Model" m %s ORDER BY m."updatedAt" DESC, m.name ASC LIMIT $%d OFFSET $%d`,
		ModelColumns, where, argIdx, argIdx+1)
	args = append(args, p.Limit, p.Offset)

	rows, err := store.pool.Query(ctx, dataQ, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list models: %w", err)
	}
	defer rows.Close()

	models, err := scanModels(rows)
	if err != nil {
		return nil, 0, err
	}
	return models, total, nil
}

// GetModel returns a model by ID.
func (store *Store) GetModel(ctx context.Context, id string) (*Model, error) {
	q := fmt.Sprintf(`SELECT %s FROM "Model" WHERE id = $1`, ModelColumns)
	m, err := scanModel(store.pool.QueryRow(ctx, q, id))
	if err != nil {
		return nil, fmt.Errorf("get model: %w", err)
	}
	return m, nil
}

// ListModelsByProvider returns all models for a given provider.
func (store *Store) ListModelsByProvider(ctx context.Context, providerID string) ([]Model, error) {
	q := fmt.Sprintf(`SELECT %s FROM "Model" WHERE "providerId" = $1 ORDER BY name ASC`, ModelColumns)
	rows, err := store.pool.Query(ctx, q, providerID)
	if err != nil {
		return nil, fmt.Errorf("list models by provider: %w", err)
	}
	defer rows.Close()
	return scanModels(rows)
}

// CreateModelParams holds fields for creating a model. The DB
// primary key (id) is auto-generated UUID — callers don't supply it.
type CreateModelParams struct {
	Code                           string // customer-facing identifier; unique
	Name                           string
	Description                    *string
	ProviderID                     string
	ProviderModelID                string
	Type                           string
	Features                       []string
	InputPricePerMillion           *float64
	OutputPricePerMillion          *float64
	CachedInputReadPricePerMillion  *float64
	CachedInputWritePricePerMillion *float64
	MaxContextTokens               *int
	MaxOutputTokens                *int
	Aliases                        []string
	InputModalities                []string         // defaults to ["text"] when nil
	OutputModalities               []string         // defaults to ["text"] when nil
	Lifecycle                      string           // defaults to "ga" when empty
	CapabilityJson                 *json.RawMessage // nil = no capability data
	Enabled                        bool
}

// CreateModel inserts a new model. The id column defaults to
// gen_random_uuid() server-side; callers receive the resolved row.
func (store *Store) CreateModel(ctx context.Context, p CreateModelParams) (*Model, error) {
	if p.Features == nil {
		p.Features = []string{}
	}
	if p.Aliases == nil {
		p.Aliases = []string{}
	}
	if p.InputModalities == nil {
		p.InputModalities = []string{"text"}
	}
	if p.OutputModalities == nil {
		p.OutputModalities = []string{"text"}
	}
	if p.Lifecycle == "" {
		p.Lifecycle = "ga"
	}
	q := fmt.Sprintf(`
		INSERT INTO "Model" (id, code, name, description, "providerId", "providerModelId", type, features,
			"inputPricePerMillion", "outputPricePerMillion",
			"cachedInputReadPricePerMillion", "cachedInputWritePricePerMillion",
			"maxContextTokens", "maxOutputTokens",
			aliases, "inputModalities", "outputModalities", lifecycle, "capabilityJson",
			enabled, "createdAt", "updatedAt")
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, NOW(), NOW())
		RETURNING %s
	`, ModelColumns)
	m, err := scanModel(store.pool.QueryRow(ctx, q,
		uuid.New().String(), p.Code, p.Name, p.Description, p.ProviderID, p.ProviderModelID, p.Type, p.Features,
		p.InputPricePerMillion, p.OutputPricePerMillion,
		p.CachedInputReadPricePerMillion, p.CachedInputWritePricePerMillion,
		p.MaxContextTokens, p.MaxOutputTokens,
		p.Aliases, p.InputModalities, p.OutputModalities, p.Lifecycle, p.CapabilityJson,
		p.Enabled,
	))
	if err != nil {
		return nil, fmt.Errorf("create model: %w", err)
	}
	return m, nil
}

// UpdateModelParams holds optional fields for updating a model. nil = no change.
type UpdateModelParams struct {
	Code                           *string
	ProviderModelID                *string
	Name                           *string
	Description                    *string
	Type                           *string
	InputPricePerMillion           *float64
	OutputPricePerMillion          *float64
	CachedInputReadPricePerMillion  *float64
	CachedInputWritePricePerMillion *float64
	MaxContextTokens               *int
	MaxOutputTokens                *int
	Status                         *string
	DeprecationDate                *time.Time
	ReplacedBy                     *string
	Aliases                        []string         // nil = no change; empty = clear
	Enabled                        *bool
	Features                       []string         // nil = no change; empty = clear
	InputModalities                *[]string        // nil = no change
	OutputModalities               *[]string        // nil = no change
	Lifecycle                      *string          // nil = no change
	CapabilityJson                 *json.RawMessage // nil = no change; explicit null = clear
}

// UpdateModel updates a model using COALESCE — nil params preserve existing values.
// Arrays (Aliases, Features, InputModalities, OutputModalities) use CASE WHEN to conditionally update.
func (store *Store) UpdateModel(ctx context.Context, id string, p UpdateModelParams) (*Model, error) {
	// For arrays: pgx passes nil Go slice as SQL NULL, so we use
	// CASE WHEN $N IS NULL THEN col ELSE $N END to preserve the original.
	// For *[]string capability modalities: nil pointer → NULL → preserve existing.
	var inputMod, outputMod interface{}
	if p.InputModalities != nil {
		inputMod = *p.InputModalities
	}
	var outputModVal interface{}
	if p.OutputModalities != nil {
		outputModVal = *p.OutputModalities
	}
	outputMod = outputModVal
	q := fmt.Sprintf(`UPDATE "Model" SET
		code = COALESCE($2, code),
		"providerModelId" = COALESCE($3, "providerModelId"),
		name = COALESCE($4, name),
		description = COALESCE($5, description),
		type = COALESCE($6, type),
		"inputPricePerMillion" = COALESCE($7, "inputPricePerMillion"),
		"outputPricePerMillion" = COALESCE($8, "outputPricePerMillion"),
		"cachedInputReadPricePerMillion" = COALESCE($9, "cachedInputReadPricePerMillion"),
		"cachedInputWritePricePerMillion" = COALESCE($10, "cachedInputWritePricePerMillion"),
		"maxContextTokens" = COALESCE($11, "maxContextTokens"),
		"maxOutputTokens" = COALESCE($12, "maxOutputTokens"),
		status = COALESCE($13, status),
		enabled = COALESCE($14, enabled),
		aliases = CASE WHEN $15::text[] IS NULL THEN aliases ELSE $15 END,
		features = CASE WHEN $16::text[] IS NULL THEN features ELSE $16 END,
		"deprecationDate" = COALESCE($17, "deprecationDate"),
		"replacedBy" = COALESCE($18, "replacedBy"),
		"inputModalities" = CASE WHEN $19::text[] IS NULL THEN "inputModalities" ELSE $19 END,
		"outputModalities" = CASE WHEN $20::text[] IS NULL THEN "outputModalities" ELSE $20 END,
		lifecycle = COALESCE($21, lifecycle),
		"capabilityJson" = CASE WHEN $22::jsonb IS NULL THEN "capabilityJson" ELSE $22 END,
		"updatedAt" = NOW()
	WHERE id = $1 RETURNING %s`, ModelColumns)

	m, err := scanModel(store.pool.QueryRow(ctx, q,
		id, p.Code, p.ProviderModelID,
		p.Name, p.Description, p.Type,
		p.InputPricePerMillion, p.OutputPricePerMillion,
		p.CachedInputReadPricePerMillion, p.CachedInputWritePricePerMillion,
		p.MaxContextTokens, p.MaxOutputTokens,
		p.Status, p.Enabled,
		p.Aliases, p.Features,
		p.DeprecationDate, p.ReplacedBy,
		inputMod, outputMod,
		p.Lifecycle, p.CapabilityJson,
	))
	if err != nil {
		return nil, fmt.Errorf("update model: %w", err)
	}
	return m, nil
}

// ProviderWithModels groups a provider summary with its models.
type ProviderWithModels struct {
	Provider ProviderSummary `json:"provider"`
	Models   []Model         `json:"models"`
}

// ProviderSummary is the subset of Provider fields returned in grouped listings.
type ProviderSummary struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	DisplayName *string `json:"displayName"`
	Description *string `json:"description"`
	AdapterType string  `json:"adapterType"`
	Enabled     bool    `json:"enabled"`
	ModelCount  int     `json:"modelCount"`
}

// GroupedModelsParams holds filter options for ListModelsGroupedByProvider.
// ProviderID narrows the result to a single provider (required by the Live
// Traffic model picker, which cascades from a chosen provider). Q is a
// case-insensitive substring match on model.name OR model.providerModelId
// so the picker's typed query filters the options as the user types.
type GroupedModelsParams struct {
	IncludeEmpty bool
	ProviderID   string
	Q            string
}

// ListModelsGroupedByProvider returns providers (ordered by name) with their
// models nested under each provider. ProviderID restricts the result to a
// single provider; Q filters models by name/providerModelId (ILIKE).
// Providers with zero matching models are dropped unless IncludeEmpty is set.
func (store *Store) ListModelsGroupedByProvider(ctx context.Context, p GroupedModelsParams) ([]ProviderWithModels, error) {
	// 1. Fetch providers with model counts. Provider-level filter is applied
	// at SELECT time so the downstream join only considers the target set.
	pWhere := ""
	pArgs := []any{}
	if p.ProviderID != "" {
		pWhere = ` WHERE p.id = $1`
		pArgs = append(pArgs, p.ProviderID)
	}
	pq := fmt.Sprintf(`
		SELECT p.id, p.name, p."displayName", p.description, p.adapter_type, p.enabled,
		       (SELECT COUNT(*) FROM "Model" m WHERE m."providerId" = p.id) AS model_count
		FROM "Provider" p%s
		ORDER BY p.name ASC
	`, pWhere)
	pRows, err := store.pool.Query(ctx, pq, pArgs...)
	if err != nil {
		return nil, fmt.Errorf("list providers for grouped models: %w", err)
	}
	defer pRows.Close()

	var providers []ProviderSummary
	for pRows.Next() {
		var ps ProviderSummary
		if err := pRows.Scan(&ps.ID, &ps.Name, &ps.DisplayName, &ps.Description, &ps.AdapterType, &ps.Enabled, &ps.ModelCount); err != nil {
			return nil, fmt.Errorf("scan provider summary: %w", err)
		}
		if p.IncludeEmpty || ps.ModelCount > 0 {
			providers = append(providers, ps)
		}
	}
	if err := pRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate providers: %w", err)
	}

	// 2. Fetch models scoped to the provider set (and optional name search).
	mWhere := "WHERE 1=1"
	mArgs := []any{}
	mIdx := 1
	if p.ProviderID != "" {
		mWhere += fmt.Sprintf(` AND "providerId" = $%d`, mIdx)
		mArgs = append(mArgs, p.ProviderID)
		mIdx++
	}
	if p.Q != "" {
		mWhere += fmt.Sprintf(` AND (name ILIKE $%d OR "providerModelId" ILIKE $%d)`, mIdx, mIdx)
		mArgs = append(mArgs, "%"+escapeILIKE(p.Q)+"%")
	}
	mq := fmt.Sprintf(`SELECT %s FROM "Model" %s ORDER BY "providerId", name ASC`, ModelColumns, mWhere)
	mRows, err := store.pool.Query(ctx, mq, mArgs...)
	if err != nil {
		return nil, fmt.Errorf("list all models: %w", err)
	}
	defer mRows.Close()

	modelsByProvider := map[string][]Model{}
	models, err := scanModels(mRows)
	if err != nil {
		return nil, err
	}
	for _, m := range models {
		modelsByProvider[m.ProviderID] = append(modelsByProvider[m.ProviderID], m)
	}

	// 3. Assemble result.
	result := make([]ProviderWithModels, 0, len(providers))
	for _, ps := range providers {
		pm := modelsByProvider[ps.ID]
		if pm == nil {
			pm = []Model{}
		}
		result = append(result, ProviderWithModels{Provider: ps, Models: pm})
	}
	return result, nil
}

// DeleteModel deletes a model by ID.
func (store *Store) DeleteModel(ctx context.Context, id string) error {
	tag, err := store.pool.Exec(ctx, `DELETE FROM "Model" WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete model: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

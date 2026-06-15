package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
)

// GetModelByCode resolves a customer-supplied identifier in the
// `{"model": "..."}` slot to a Model row. Strict match on Model.code
// — UUIDs and display names are not accepted, keeping the customer
// API contract narrow and stable: rename the display name without
// breaking integrations, and never expose internal UUIDs as a valid
// API input. Only enabled models are returned.
func (db *DB) GetModelByCode(ctx context.Context, code string) (*Model, error) {
	row := db.pool.QueryRow(ctx, `
		SELECT m.id, m.code, m.name, m."providerId", p.name, p.adapter_type, p."displayName", p."baseUrl",
		       m."providerModelId", m.type, m.enabled,
		       "inputPricePerMillion", "outputPricePerMillion", "cachedInputReadPricePerMillion", "cachedInputWritePricePerMillion",
		       COALESCE(features, '{}'), "maxContextTokens", "maxOutputTokens",
		       COALESCE(aliases, '{}'),
		       COALESCE(m."inputModalities", '{}'), COALESCE(m."outputModalities", '{}'),
		       COALESCE(m.lifecycle, 'ga'), m."capabilityJson"
		FROM "Model" m
		LEFT JOIN "Provider" p ON p.id = m."providerId"
		WHERE m.code = $1 AND m.enabled = true
		LIMIT 1
	`, code)
	var m Model
	var inPrice, outPrice, cacheReadPrice, cacheWritePrice *string
	var maxCtx, maxOut pgtype.Int4
	err := row.Scan(&m.ID, &m.Code, &m.Name, &m.ProviderID, &m.ProviderName, &m.ProviderAdapterType, &m.ProviderDisplayName, &m.ProviderBaseURL,
		&m.ProviderModelID,
		&m.Type, &m.Enabled, &inPrice, &outPrice, &cacheReadPrice, &cacheWritePrice, &m.Features, &maxCtx, &maxOut, &m.Aliases,
		&m.InputModalities, &m.OutputModalities, &m.Lifecycle, &m.CapabilityJson)
	if err != nil {
		return nil, fmt.Errorf("store: get model by code: %w", err)
	}
	if f, ok := ParseDecimal(inPrice); ok {
		m.InputPricePM = &f
	}
	if f, ok := ParseDecimal(outPrice); ok {
		m.OutputPricePM = &f
	}
	if f, ok := ParseDecimal(cacheReadPrice); ok {
		m.CachedInputReadPricePM = &f
	}
	if f, ok := ParseDecimal(cacheWritePrice); ok {
		m.CachedInputWritePricePM = &f
	}
	m.MaxContextTokens = intFromPgInt4(maxCtx)
	m.MaxOutputTokens = intFromPgInt4(maxOut)
	return &m, nil
}

// ResolveModelCandidates returns every enabled Model whose `code`
// equals the given request string OR whose `aliases` array contains
// it. Empty slice + nil err means the request model is unknown to the
// catalog — the routing engine treats that as "matchConditions.models
// cannot match", which lets unmatched requests fall through to a
// catch-all rule. The request's `model` field is a customer-facing string
// (e.g. "gpt-4o"), and Match Conditions store Model.id UUIDs, so the engine
// resolves the string to a UUID set here and intersects against
// MatchConditions.Models.
func (db *DB) ResolveModelCandidates(ctx context.Context, code string) ([]Model, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT m.id, m.code, m.name, m."providerId", p.name, p.adapter_type, p."displayName", p."baseUrl",
		       m."providerModelId", m.type, m.enabled,
		       "inputPricePerMillion", "outputPricePerMillion", "cachedInputReadPricePerMillion", "cachedInputWritePricePerMillion",
		       COALESCE(features, '{}'), "maxContextTokens", "maxOutputTokens",
		       COALESCE(aliases, '{}'),
		       COALESCE(m."inputModalities", '{}'), COALESCE(m."outputModalities", '{}'),
		       COALESCE(m.lifecycle, 'ga'), m."capabilityJson"
		FROM "Model" m
		LEFT JOIN "Provider" p ON p.id = m."providerId"
		WHERE m.enabled = true
		  AND (m.code = $1 OR $1 = ANY(m.aliases))
	`, code)
	if err != nil {
		return nil, fmt.Errorf("store: resolve model candidates: %w", err)
	}
	defer rows.Close()

	var out []Model
	for rows.Next() {
		var m Model
		var inPrice, outPrice, cacheReadPrice, cacheWritePrice *string
		var maxCtx, maxOut pgtype.Int4
		if err := rows.Scan(&m.ID, &m.Code, &m.Name, &m.ProviderID, &m.ProviderName, &m.ProviderAdapterType, &m.ProviderDisplayName, &m.ProviderBaseURL,
			&m.ProviderModelID,
			&m.Type, &m.Enabled, &inPrice, &outPrice, &cacheReadPrice, &cacheWritePrice, &m.Features, &maxCtx, &maxOut, &m.Aliases,
			&m.InputModalities, &m.OutputModalities, &m.Lifecycle, &m.CapabilityJson); err != nil {
			return nil, fmt.Errorf("store: scan model candidate: %w", err)
		}
		if f, ok := ParseDecimal(inPrice); ok {
			m.InputPricePM = &f
		}
		if f, ok := ParseDecimal(outPrice); ok {
			m.OutputPricePM = &f
		}
		if f, ok := ParseDecimal(cacheReadPrice); ok {
			m.CachedInputReadPricePM = &f
		}
		if f, ok := ParseDecimal(cacheWritePrice); ok {
			m.CachedInputWritePricePM = &f
		}
		m.MaxContextTokens = intFromPgInt4(maxCtx)
		m.MaxOutputTokens = intFromPgInt4(maxOut)
		out = append(out, m)
	}
	return out, nil
}

// ListEnabledModels returns all enabled models.
func (db *DB) ListEnabledModels(ctx context.Context) ([]Model, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT m.id, m.code, m.name, m."providerId", p.name, p.adapter_type, p."displayName", p."baseUrl",
		       m."providerModelId", m.type, m.enabled,
		       "inputPricePerMillion", "outputPricePerMillion", "cachedInputReadPricePerMillion", "cachedInputWritePricePerMillion",
		       COALESCE(features, '{}'), "maxContextTokens", "maxOutputTokens",
		       COALESCE(aliases, '{}'),
		       COALESCE(m."inputModalities", '{}'), COALESCE(m."outputModalities", '{}'),
		       COALESCE(m.lifecycle, 'ga'), m."capabilityJson"
		FROM "Model" m
		LEFT JOIN "Provider" p ON p.id = m."providerId"
		WHERE m.enabled = true
		ORDER BY m."providerId", m.name
	`)
	if err != nil {
		return nil, fmt.Errorf("store: list models: %w", err)
	}
	defer rows.Close()

	var models []Model
	for rows.Next() {
		var m Model
		var inPrice, outPrice, cacheReadPrice, cacheWritePrice *string
		var maxCtx, maxOut pgtype.Int4
		if err := rows.Scan(&m.ID, &m.Code, &m.Name, &m.ProviderID, &m.ProviderName, &m.ProviderAdapterType, &m.ProviderDisplayName, &m.ProviderBaseURL,
			&m.ProviderModelID,
			&m.Type, &m.Enabled, &inPrice, &outPrice, &cacheReadPrice, &cacheWritePrice, &m.Features, &maxCtx, &maxOut, &m.Aliases,
			&m.InputModalities, &m.OutputModalities, &m.Lifecycle, &m.CapabilityJson); err != nil {
			return nil, fmt.Errorf("store: scan model: %w", err)
		}
		if f, ok := ParseDecimal(inPrice); ok {
			m.InputPricePM = &f
		}
		if f, ok := ParseDecimal(outPrice); ok {
			m.OutputPricePM = &f
		}
		if f, ok := ParseDecimal(cacheReadPrice); ok {
			m.CachedInputReadPricePM = &f
		}
		if f, ok := ParseDecimal(cacheWritePrice); ok {
			m.CachedInputWritePricePM = &f
		}
		m.MaxContextTokens = intFromPgInt4(maxCtx)
		m.MaxOutputTokens = intFromPgInt4(maxOut)
		models = append(models, m)
	}
	return models, nil
}

// ModelPricing holds pricing data for a model used by quota downgrade logic.
//
// Priced is the unambiguous "this model has a price row" signal — true when at
// least one of inputPricePerMillion / outputPricePerMillion is set (non-NULL),
// false when the model has no row in the lookup OR both price columns are NULL.
// It is distinct from a price of 0: a genuinely free model is Priced=true with
// zero rates. The float fields collapse NULL and 0 to the same 0.0, so the
// downgrade selector cannot tell an unpriced candidate (uncountable against a
// cost cap) from a free one without this flag.
type ModelPricing struct {
	ModelID       string
	InputPricePM  float64
	OutputPricePM float64
	Priced        bool
}

// FetchModelPricing reads model pricing from the database for a list of model IDs.
func (db *DB) FetchModelPricing(ctx context.Context, modelIDs []string) ([]ModelPricing, error) {
	if len(modelIDs) == 0 {
		return nil, nil
	}

	rows, err := db.pool.Query(ctx, `
		SELECT id, "inputPricePerMillion", "outputPricePerMillion"
		FROM "Model"
		WHERE id = ANY($1)
	`, modelIDs)
	if err != nil {
		return nil, fmt.Errorf("store: fetch model pricing: %w", err)
	}
	defer rows.Close()

	priceMap := make(map[string]ModelPricing)
	for rows.Next() {
		var mp ModelPricing
		var inPrice, outPrice *string
		if err := rows.Scan(&mp.ModelID, &inPrice, &outPrice); err != nil {
			return nil, fmt.Errorf("store: scan model pricing: %w", err)
		}
		if f, ok := ParseDecimal(inPrice); ok {
			mp.InputPricePM = f
			mp.Priced = true
		}
		if f, ok := ParseDecimal(outPrice); ok {
			mp.OutputPricePM = f
			mp.Priced = true
		}
		priceMap[mp.ModelID] = mp
	}

	result := make([]ModelPricing, len(modelIDs))
	for i, id := range modelIDs {
		mp := priceMap[id]
		mp.ModelID = id
		result[i] = mp
	}
	return result, nil
}

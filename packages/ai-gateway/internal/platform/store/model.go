package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
)

// servableModelPredicate is the SQL spelling of [Model.Servable], shared by
// every query that answers "may this model serve traffic". It is a compile-time
// constant spliced into the query text, never caller input. Written once so the
// SQL and the Go predicate cannot drift into disagreeing about what servable
// means — a drift that shows up as a catalog advertising models the router
// refuses.
const servableModelPredicate = `m.enabled = true AND p.enabled = true AND m.status <> '` + ModelStatusDisabled + `'`

// GetModelByCode resolves a customer-supplied identifier in the
// `{"model": "..."}` slot to a Model row. Strict match on Model.code
// — UUIDs and display names are not accepted, keeping the customer
// API contract narrow and stable: rename the display name without
// breaking integrations, and never expose internal UUIDs as a valid
// API input. Only servable models are returned (see [Model.Servable]), so a
// disabled provider — or a model an operator marked disabled — takes the row
// out of both the catalog and the passthrough routing path.
func (db *DB) GetModelByCode(ctx context.Context, code string) (*Model, error) {
	row := db.pool.QueryRow(ctx, `
		SELECT m.id, m.code, m.name, m."providerId", p.name, p.adapter_type, p."displayName", p."baseUrl",
		       m."providerModelId", m.type, m.enabled, COALESCE(p.enabled, false), COALESCE(m.status, 'active'),
		       "inputPricePerMillion", "outputPricePerMillion", "cachedInputReadPricePerMillion", "cachedInputWritePricePerMillion",
		       "audioInputPricePerMillion", "audioOutputPricePerMillion", "cachedAudioInputReadPricePerMillion",
		       COALESCE(features, '{}'), "maxContextTokens", "maxOutputTokens",
		       COALESCE(aliases, '{}'),
		       COALESCE(m."inputModalities", '{}'), COALESCE(m."outputModalities", '{}'), COALESCE(m."requiredModalities", '{}'),
		       COALESCE(m.lifecycle, 'ga'), m."capabilityJson"
		       , m."minOutputTokens", m."temperatureMin", m."temperatureMax", m.family
		FROM "Model" m
		LEFT JOIN "Provider" p ON p.id = m."providerId"
		WHERE m.code = $1 AND `+servableModelPredicate+`
		LIMIT 1
	`, code)
	var m Model
	var inPrice, outPrice, cacheReadPrice, cacheWritePrice *string
	var audioInPrice, audioOutPrice, cachedAudioReadPrice *string
	var maxCtx, maxOut pgtype.Int4
	var minOut pgtype.Int4
	var tempMin, tempMax pgtype.Float8
	err := row.Scan(&m.ID, &m.Code, &m.Name, &m.ProviderID, &m.ProviderName, &m.ProviderAdapterType, &m.ProviderDisplayName, &m.ProviderBaseURL,
		&m.ProviderModelID,
		&m.Type, &m.Enabled, &m.ProviderEnabled, &m.Status, &inPrice, &outPrice, &cacheReadPrice, &cacheWritePrice,
		&audioInPrice, &audioOutPrice, &cachedAudioReadPrice, &m.Features, &maxCtx, &maxOut, &m.Aliases,
		&m.InputModalities, &m.OutputModalities, &m.RequiredModalities, &m.Lifecycle, &m.CapabilityJson,
		&minOut, &tempMin, &tempMax, &m.Family)
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
	if f, ok := ParseDecimal(audioInPrice); ok {
		m.AudioInputPricePM = &f
	}
	if f, ok := ParseDecimal(audioOutPrice); ok {
		m.AudioOutputPricePM = &f
	}
	if f, ok := ParseDecimal(cachedAudioReadPrice); ok {
		m.CachedAudioInputReadPricePM = &f
	}
	m.MaxContextTokens = intFromPgInt4(maxCtx)
	m.MaxOutputTokens = intFromPgInt4(maxOut)
	m.MinOutputTokens = intFromPgInt4(minOut)
	m.TemperatureMin = floatFromPgFloat8(tempMin)
	m.TemperatureMax = floatFromPgFloat8(tempMax)
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
//
// Provider.enabled is deliberately NOT filtered here, unlike every other
// model lookup. This resolves the identity of the model the caller ASKED
// for, not a model that will serve the call: a rule whose whole purpose is
// to redirect requests away from a disabled provider matches on that
// provider's model UUID, and filtering it out would stop the rule from ever
// matching. Servability is decided downstream — lookupTarget rejects a
// disabled provider for rule-chosen targets, and Servable() covers the
// passthrough path.
func (db *DB) ResolveModelCandidates(ctx context.Context, code string) ([]Model, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT m.id, m.code, m.name, m."providerId", p.name, p.adapter_type, p."displayName", p."baseUrl",
		       m."providerModelId", m.type, m.enabled, COALESCE(p.enabled, false), COALESCE(m.status, 'active'),
		       "inputPricePerMillion", "outputPricePerMillion", "cachedInputReadPricePerMillion", "cachedInputWritePricePerMillion",
		       "audioInputPricePerMillion", "audioOutputPricePerMillion", "cachedAudioInputReadPricePerMillion",
		       COALESCE(features, '{}'), "maxContextTokens", "maxOutputTokens",
		       COALESCE(aliases, '{}'),
		       COALESCE(m."inputModalities", '{}'), COALESCE(m."outputModalities", '{}'), COALESCE(m."requiredModalities", '{}'),
		       COALESCE(m.lifecycle, 'ga'), m."capabilityJson"
		       , m."minOutputTokens", m."temperatureMin", m."temperatureMax", m.family
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
		var audioInPrice, audioOutPrice, cachedAudioReadPrice *string
		var maxCtx, maxOut pgtype.Int4
		var minOut pgtype.Int4
		var tempMin, tempMax pgtype.Float8
		if err := rows.Scan(&m.ID, &m.Code, &m.Name, &m.ProviderID, &m.ProviderName, &m.ProviderAdapterType, &m.ProviderDisplayName, &m.ProviderBaseURL,
			&m.ProviderModelID,
			&m.Type, &m.Enabled, &m.ProviderEnabled, &m.Status, &inPrice, &outPrice, &cacheReadPrice, &cacheWritePrice,
			&audioInPrice, &audioOutPrice, &cachedAudioReadPrice, &m.Features, &maxCtx, &maxOut, &m.Aliases,
			&m.InputModalities, &m.OutputModalities, &m.RequiredModalities, &m.Lifecycle, &m.CapabilityJson,
			&minOut, &tempMin, &tempMax, &m.Family); err != nil {
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
		if f, ok := ParseDecimal(audioInPrice); ok {
			m.AudioInputPricePM = &f
		}
		if f, ok := ParseDecimal(audioOutPrice); ok {
			m.AudioOutputPricePM = &f
		}
		if f, ok := ParseDecimal(cachedAudioReadPrice); ok {
			m.CachedAudioInputReadPricePM = &f
		}
		m.MaxContextTokens = intFromPgInt4(maxCtx)
		m.MaxOutputTokens = intFromPgInt4(maxOut)
		m.MinOutputTokens = intFromPgInt4(minOut)
		m.TemperatureMin = floatFromPgFloat8(tempMin)
		m.TemperatureMax = floatFromPgFloat8(tempMax)
		out = append(out, m)
	}
	return out, nil
}

// ListEnabledModels returns every servable model (see [Model.Servable]).
// This is the catalog GET /v1/models advertises, so a model withdrawn by
// either its provider or its own status must not appear: a client that trusts
// the catalog would otherwise select it and get an upstream failure instead of
// simply never seeing it.
func (db *DB) ListEnabledModels(ctx context.Context) ([]Model, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT m.id, m.code, m.name, m."providerId", p.name, p.adapter_type, p."displayName", p."baseUrl",
		       m."providerModelId", m.type, m.enabled, COALESCE(p.enabled, false), COALESCE(m.status, 'active'),
		       "inputPricePerMillion", "outputPricePerMillion", "cachedInputReadPricePerMillion", "cachedInputWritePricePerMillion",
		       "audioInputPricePerMillion", "audioOutputPricePerMillion", "cachedAudioInputReadPricePerMillion",
		       COALESCE(features, '{}'), "maxContextTokens", "maxOutputTokens",
		       COALESCE(aliases, '{}'),
		       COALESCE(m."inputModalities", '{}'), COALESCE(m."outputModalities", '{}'), COALESCE(m."requiredModalities", '{}'),
		       COALESCE(m.lifecycle, 'ga'), m."capabilityJson"
		       , m."minOutputTokens", m."temperatureMin", m."temperatureMax", m.family
		FROM "Model" m
		LEFT JOIN "Provider" p ON p.id = m."providerId"
		WHERE `+servableModelPredicate+`
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
		var audioInPrice, audioOutPrice, cachedAudioReadPrice *string
		var maxCtx, maxOut pgtype.Int4
		var minOut pgtype.Int4
		var tempMin, tempMax pgtype.Float8
		if err := rows.Scan(&m.ID, &m.Code, &m.Name, &m.ProviderID, &m.ProviderName, &m.ProviderAdapterType, &m.ProviderDisplayName, &m.ProviderBaseURL,
			&m.ProviderModelID,
			&m.Type, &m.Enabled, &m.ProviderEnabled, &m.Status, &inPrice, &outPrice, &cacheReadPrice, &cacheWritePrice,
			&audioInPrice, &audioOutPrice, &cachedAudioReadPrice, &m.Features, &maxCtx, &maxOut, &m.Aliases,
			&m.InputModalities, &m.OutputModalities, &m.RequiredModalities, &m.Lifecycle, &m.CapabilityJson,
			&minOut, &tempMin, &tempMax, &m.Family); err != nil {
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
		if f, ok := ParseDecimal(audioInPrice); ok {
			m.AudioInputPricePM = &f
		}
		if f, ok := ParseDecimal(audioOutPrice); ok {
			m.AudioOutputPricePM = &f
		}
		if f, ok := ParseDecimal(cachedAudioReadPrice); ok {
			m.CachedAudioInputReadPricePM = &f
		}
		m.MaxContextTokens = intFromPgInt4(maxCtx)
		m.MaxOutputTokens = intFromPgInt4(maxOut)
		m.MinOutputTokens = intFromPgInt4(minOut)
		m.TemperatureMin = floatFromPgFloat8(tempMin)
		m.TemperatureMax = floatFromPgFloat8(tempMax)
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

package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
)

// Provider represents a provider record.
//
// AdapterType is the canonical wire adapter for this provider, one of
// the nine providers.Format values. provtarget.PgResolver copies it
// into CallTarget.Format so the executor, smart router, AI Guard, and
// handler call sites read the format from one place. Enum validation
// lives in the Control Plane handler; AI Gateway treats an empty or
// unrecognised value as a provider-config error.
//
// Region is the authoritative deployment region (e.g. "us-east-1",
// "eu-west-1") used by compliance hooks — specifically data-residency —
// to decide whether traffic may be dispatched to this provider. A nil
// Region tells the hook "region is unknown"; the data-residency hook
// treats that as a reject for classified data.
type Provider struct {
	ID          string
	Name        string
	DisplayName *string
	AdapterType string
	BaseURL     string
	PathPrefix  string
	APIVersion  *string
	Region      *string
	Enabled     bool
	// ServesResponsesAPI is the per-provider override for whether this
	// upstream natively serves OpenAI /v1/responses. nil = use the adapter
	// RequestShapes default; the bridge treats an explicit value as a
	// downgrade-only signal (false forces canonical(chat); true cannot
	// exceed the adapter's declared capability).
	ServesResponsesAPI *bool
}

// Model represents a model record.
type Model struct {
	ID   string // UUID PK; held by every internal FK reference.
	Code string // Customer-facing identifier ("gpt-4o"); resolved
	// to ID at the gateway boundary. Globally unique.
	Name                string
	ProviderID          string
	ProviderName        string  // Provider.name (operator-facing slug, e.g. "openai")
	ProviderAdapterType string  // Provider.adapter_type (wire format, e.g. "anthropic", "openai")
	ProviderDisplayName *string // Provider.displayName (e.g. "OpenAI") — UI label only
	ProviderBaseURL     string  // Provider.baseUrl (origin) — used to populate target.BaseURL
	// on the passthrough-fallback path so traffic_event.target_host
	// records the real upstream domain instead of falling back
	// to the provider name.
	ProviderModelID string // String sent on the upstream wire to the provider.
	Type            string // chat | embedding | image | audio
	Enabled         bool
	InputPricePM    *float64 // per million tokens
	OutputPricePM   *float64
	// CachedInputReadPricePM is the cached input token READ price (e.g.
	// Anthropic 0.10× input, OpenAI 0.50× input, Gemini 0.25× input).
	// NULL = no discount; cost calculation falls back to InputPricePM.
	CachedInputReadPricePM *float64
	// CachedInputWritePricePM is the cached input token WRITE surcharge
	// (e.g. Anthropic 1.25×). NULL = no surcharge; cost calculation
	// falls back to InputPricePM.
	CachedInputWritePricePM *float64
	Features                []string // vision, function_calling, streaming, json_mode, thinking, ...
	MaxContextTokens        *int
	MaxOutputTokens         *int
	Aliases                 []string // Alternate request strings that resolve to this row
	// (e.g. "gpt-4o-2024-08-06" → "gpt-4o"). Read by
	// ResolveModelCandidates for code-set hydration.
	InputModalities  []string // e.g. ["text"], ["text","image"]
	OutputModalities []string // e.g. ["text"], ["embedding"]
	Lifecycle        string   // ga | preview | deprecated
	CapabilityJson   []byte   // raw JSONB bytes (nil = no capability data)
}

// GetProvider fetches a provider by ID.
func (db *DB) GetProvider(ctx context.Context, id string) (*Provider, error) {
	row := db.pool.QueryRow(ctx, `
		SELECT id, name, "displayName", adapter_type, "baseUrl", "pathPrefix", "apiVersion", region, enabled, serves_responses_api
		FROM "Provider"
		WHERE id = $1
	`, id)
	var p Provider
	err := row.Scan(&p.ID, &p.Name, &p.DisplayName, &p.AdapterType, &p.BaseURL,
		&p.PathPrefix, &p.APIVersion, &p.Region, &p.Enabled, &p.ServesResponsesAPI)
	if err != nil {
		return nil, fmt.Errorf("store: get provider: %w", err)
	}
	return &p, nil
}

// GetModel fetches a model by UUID primary key. Use [GetModelByCode]
// for resolving a customer-supplied code/name string instead.
func (db *DB) GetModel(ctx context.Context, id string) (*Model, error) {
	row := db.pool.QueryRow(ctx, `
		SELECT id, code, name, "providerId", "providerModelId", type, enabled,
		       "inputPricePerMillion", "outputPricePerMillion",
		       COALESCE(features, '{}'), "maxContextTokens", "maxOutputTokens",
		       COALESCE(aliases, '{}'),
		       COALESCE("inputModalities", '{}'), COALESCE("outputModalities", '{}'),
		       COALESCE(lifecycle, 'ga'), "capabilityJson"
		FROM "Model"
		WHERE id = $1
	`, id)
	var m Model
	var inPrice, outPrice *string
	var maxCtx, maxOut pgtype.Int4
	err := row.Scan(&m.ID, &m.Code, &m.Name, &m.ProviderID, &m.ProviderModelID,
		&m.Type, &m.Enabled, &inPrice, &outPrice, &m.Features, &maxCtx, &maxOut, &m.Aliases,
		&m.InputModalities, &m.OutputModalities, &m.Lifecycle, &m.CapabilityJson)
	if err != nil {
		return nil, fmt.Errorf("store: get model: %w", err)
	}
	if f, ok := ParseDecimal(inPrice); ok {
		m.InputPricePM = &f
	}
	if f, ok := ParseDecimal(outPrice); ok {
		m.OutputPricePM = &f
	}
	m.MaxContextTokens = intFromPgInt4(maxCtx)
	m.MaxOutputTokens = intFromPgInt4(maxOut)
	return &m, nil
}

func intFromPgInt4(v pgtype.Int4) *int {
	if !v.Valid {
		return nil
	}
	i := int(v.Int32)
	return &i
}

// GetProviderAndModel fetches both a provider and model by their IDs.
// Returns an error if either is not found.
func (db *DB) GetProviderAndModel(ctx context.Context, providerID, modelID string) (*Provider, *Model, error) {
	p, err := db.GetProvider(ctx, providerID)
	if err != nil {
		return nil, nil, err
	}
	m, err := db.GetModel(ctx, modelID)
	if err != nil {
		return nil, nil, err
	}
	return p, m, nil
}

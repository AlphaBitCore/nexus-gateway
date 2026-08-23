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
	Type            string // chat | embedding | image | audio | rerank | video | realtime
	Enabled         bool
	// Status is the operator-set catalog state: active | deprecated | preview |
	// disabled. Only `disabled` withdraws the model from service — a deprecated
	// or preview model is still callable, which is what distinguishes this from
	// [Model.Lifecycle], the maturity label shown to clients.
	Status string
	// ProviderEnabled is the owning Provider.enabled flag, denormalised onto
	// the model row so servability can be decided without a second lookup.
	// Every query that produces a Model reads it; a false here always means
	// the provider really is disabled, never "this query did not ask".
	ProviderEnabled bool
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
	// Audio-token rates — realtime models bill text and audio components of
	// one response simultaneously at different rates. NULL on non-realtime
	// models. Both primary audio rates (and both text rates) must be
	// non-NULL and > 0 for a model to be realtime-priced.
	AudioInputPricePM  *float64
	AudioOutputPricePM *float64
	// CachedAudioInputReadPricePM — cached audio input READ price. NULL = no
	// discount; falls back to AudioInputPricePM (mirrors the text cache-read
	// fallback).
	CachedAudioInputReadPricePM *float64
	// Non-modality capabilities only: function_calling, streaming, json_mode,
	// thinking, … The image question lives in InputModalities; `vision` is
	// derived back onto the /v1/models response and never stored.
	Features         []string
	MaxContextTokens *int
	MaxOutputTokens  *int
	Aliases          []string // Alternate request strings that resolve to this row
	// (e.g. "gpt-4o-2024-08-06" → "gpt-4o"). Read by
	// ResolveModelCandidates for code-set hydration.
	InputModalities  []string // e.g. ["text"], ["text","image"]
	OutputModalities []string // e.g. ["text"], ["embedding"]
	// RequiredModalities is the model's floor — what a request MUST carry for
	// it to serve it at all. Empty is the normal case. InputModalities is the
	// ceiling; neither derives the other.
	RequiredModalities []string
	Lifecycle          string // ga | preview | deprecated
	CapabilityJson     []byte // raw JSONB bytes (nil = no capability data)
	// AP-2: public model-detail parameter constraints + family. All nullable;
	// MinOutputTokens nil ⟹ the universal floor of 1 at the response layer.
	MinOutputTokens *int
	TemperatureMin  *float64
	TemperatureMax  *float64
	Family          *string
}

// Servable reports whether this model can serve traffic right now. Three
// independent switches can withdraw a model, and a catalog that reads only
// one of them advertises models the router will refuse — the client then
// picks one and the call dies upstream instead of at the catalog:
//
//   - the model row's own enabled flag;
//   - the enabled flag of the provider that fulfils it, since disabling a
//     provider takes every model it owns out of service;
//   - status == "disabled", the operator's per-model catalog state. Only that
//     one value withdraws the model: `deprecated` and `preview` models stay
//     callable and merely carry a different label to clients.
func (m Model) Servable() bool {
	return m.Enabled && m.ProviderEnabled && m.Status != ModelStatusDisabled
}

// ModelStatusDisabled is the one Model.status value that takes a model out of
// service. Declared once so the Go predicate and the SQL that mirrors it
// cannot drift apart.
const ModelStatusDisabled = "disabled"

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
//
// Unlike the code lookups this returns the row whatever its state — the
// accounting paths that resolve a model by UUID (cost stamping, quota
// pricing, metering) must still price a model that has just been taken out
// of service. The provider join is here only to fill [Model.ProviderEnabled],
// so [Model.Servable] answers truthfully on this row too rather than
// reporting a servable model as unservable because nothing asked.
func (db *DB) GetModel(ctx context.Context, id string) (*Model, error) {
	row := db.pool.QueryRow(ctx, `
		SELECT m.id, m.code, m.name, m."providerId", m."providerModelId", m.type, m.enabled,
		       COALESCE(p.enabled, false), COALESCE(m.status, 'active'),
		       m."inputPricePerMillion", m."outputPricePerMillion",
		       COALESCE(m.features, '{}'), m."maxContextTokens", m."maxOutputTokens",
		       COALESCE(m.aliases, '{}'),
		       COALESCE(m."inputModalities", '{}'), COALESCE(m."outputModalities", '{}'), COALESCE(m."requiredModalities", '{}'),
		       COALESCE(m.lifecycle, 'ga'), m."capabilityJson"
		FROM "Model" m
		LEFT JOIN "Provider" p ON p.id = m."providerId"
		WHERE m.id = $1
	`, id)
	var m Model
	var inPrice, outPrice *string
	var maxCtx, maxOut pgtype.Int4
	err := row.Scan(&m.ID, &m.Code, &m.Name, &m.ProviderID, &m.ProviderModelID,
		&m.Type, &m.Enabled, &m.ProviderEnabled, &m.Status,
		&inPrice, &outPrice, &m.Features, &maxCtx, &maxOut, &m.Aliases,
		&m.InputModalities, &m.OutputModalities, &m.RequiredModalities, &m.Lifecycle, &m.CapabilityJson)
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

// floatFromPgFloat8 converts a nullable double-precision column to *float64
// (nil when SQL NULL). Mirrors intFromPgInt4 for the AP-2 temperature range.
func floatFromPgFloat8(v pgtype.Float8) *float64 {
	if !v.Valid {
		return nil
	}
	f := v.Float64
	return &f
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

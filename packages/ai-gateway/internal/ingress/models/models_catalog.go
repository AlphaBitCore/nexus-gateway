package models

import (
	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

// catalogPricing is the per-model price block for the catalog shape.
// Amounts are USD per million tokens (the catalog's native unit); the
// unit/currency are labeled explicitly so consumers never guess the scale.
type catalogPricing struct {
	Input    *float64 `json:"input,omitempty"`
	Output   *float64 `json:"output,omitempty"`
	Currency string   `json:"currency,omitempty"`
	Unit     string   `json:"unit,omitempty"`
}

// catalogEntry is the public catalog representation of one model. It is a
// self-describing superset: a consumer can pick a model (id, pricing,
// context window, modalities, capabilities, lifecycle status) without a
// second round-trip. `modalities` and `capabilities` are always present
// (never null) so consumers can index them unconditionally.
type catalogEntry struct {
	ID            string          `json:"id"`
	Provider      string          `json:"provider"`
	Name          string          `json:"name"`
	ContextLength *int            `json:"context_length,omitempty"`
	Modalities    []string        `json:"modalities"`
	Pricing       *catalogPricing `json:"pricing,omitempty"`
	Capabilities  map[string]any  `json:"capabilities"`
	Status        string          `json:"status,omitempty"`
}

// buildCatalogPricing returns the price block, or nil when the model has no
// input/output price configured (so the `pricing` key is omitted entirely).
func buildCatalogPricing(m store.Model) *catalogPricing {
	if m.InputPricePM == nil && m.OutputPricePM == nil {
		return nil
	}
	return &catalogPricing{
		Input:    m.InputPricePM,
		Output:   m.OutputPricePM,
		Currency: "USD",
		Unit:     "per_million_tokens",
	}
}

// unionModalities returns the distinct union of input and output modalities,
// input entries first, preserving first-seen order.
func unionModalities(in, out []string) []string {
	seen := make(map[string]bool, len(in)+len(out))
	res := make([]string, 0, len(in)+len(out))
	for _, group := range [][]string{in, out} {
		for _, s := range group {
			if !seen[s] {
				seen[s] = true
				res = append(res, s)
			}
		}
	}
	return res
}

// buildCatalogEntry maps a catalog row to the public catalog entry. The
// `capabilities` object starts from the raw CapabilityJson blob (when
// present, best-effort) and always carries a `features` array.
func buildCatalogEntry(m store.Model) catalogEntry {
	id := m.Code
	if m.ProviderName != "" {
		id = m.ProviderName + "/" + m.Code
	}

	caps := map[string]any{}
	if len(m.CapabilityJson) > 0 {
		_ = json.Unmarshal(m.CapabilityJson, &caps) // best-effort; leave {} on error
		if caps == nil {
			// CapabilityJson held the JSON literal "null" (common in seeded
			// data), which Unmarshal turns into a nil map, not an error.
			caps = map[string]any{}
		}
	}
	feats := m.Features
	if feats == nil {
		feats = []string{}
	}
	caps["features"] = feats

	return catalogEntry{
		ID:            id,
		Provider:      m.ProviderName,
		Name:          m.Name,
		ContextLength: m.MaxContextTokens,
		Modalities:    unionModalities(m.InputModalities, m.OutputModalities),
		Pricing:       buildCatalogPricing(m),
		Capabilities:  caps,
		Status:        m.Lifecycle,
	}
}

// catalogPricingDetail is the detail endpoint's full price block: the list's
// input/output plus cached-token and audio-token rates. Null rates are omitted
// so a consumer distinguishes "no such rate" from a real zero. Amounts share
// the catalog's per-million-tokens unit.
type catalogPricingDetail struct {
	Input            *float64 `json:"input,omitempty"`
	Output           *float64 `json:"output,omitempty"`
	CachedInput      *float64 `json:"cached_input,omitempty"`
	CachedInputWrite *float64 `json:"cached_input_write,omitempty"`
	AudioInput       *float64 `json:"audio_input,omitempty"`
	AudioOutput      *float64 `json:"audio_output,omitempty"`
	CachedAudioInput *float64 `json:"cached_audio_input,omitempty"`
	Currency         string   `json:"currency"`
	Unit             string   `json:"unit"`
}

// intRange / floatRange are the {min,max} accepted-value blocks in
// parameter_constraints. Bounds omit when unknown.
type intRange struct {
	Min *int `json:"min,omitempty"`
	Max *int `json:"max,omitempty"`
}
type floatRange struct {
	Min *float64 `json:"min,omitempty"`
	Max *float64 `json:"max,omitempty"`
}
type parameterConstraints struct {
	MaxTokens   intRange   `json:"max_tokens"`
	Temperature floatRange `json:"temperature"`
}

// catalogDetail is the public single-model detail shape: the list entry
// (embedded — its fields promote to top level) plus full pricing, a
// named-capability matrix, parameter constraints, and family.
type catalogDetail struct {
	catalogEntry
	PricingDetail        catalogPricingDetail `json:"pricing_detail"`
	CapabilityMatrix     map[string]bool      `json:"capability_matrix"`
	ParameterConstraints parameterConstraints `json:"parameter_constraints"`
	Family               *string              `json:"family,omitempty"`
}

// matrixFeatures is the fixed set of named capabilities surfaced as booleans in
// capability_matrix, derived from the Features slice (no schema of their own).
//
// `reasoning` and `structured_outputs` are here because the ROUTER refuses to
// route around them: `auto` will not hand a json_schema request to a model that
// answers prose, nor a thinking request to a model that does not think. An
// eligibility fact the gateway acts on is the caller's business, and this is
// the one surface built to answer "what can this model do". Adding a key is
// additive — an existing consumer reading the older keys is unaffected.
var matrixFeatures = []string{
	"tools", "vision", "json_mode", "streaming", "function_calling",
	"reasoning", "structured_outputs",
}

func buildCapabilityMatrix(features []string) map[string]bool {
	have := make(map[string]bool, len(features))
	for _, f := range features {
		have[f] = true
	}
	m := make(map[string]bool, len(matrixFeatures))
	for _, k := range matrixFeatures {
		m[k] = have[k]
	}
	// The catalog tags tool-calling as "function_calling" (no model carries a
	// literal "tools" feature string), but consumers expect the modern
	// tools-API name. Same capability — derive it so `tools` isn't a
	// permanently-false key.
	m["tools"] = m["tools"] || have["function_calling"]
	return m
}

func buildPricingDetail(m store.Model) catalogPricingDetail {
	return catalogPricingDetail{
		Input:            m.InputPricePM,
		Output:           m.OutputPricePM,
		CachedInput:      m.CachedInputReadPricePM,
		CachedInputWrite: m.CachedInputWritePricePM,
		AudioInput:       m.AudioInputPricePM,
		AudioOutput:      m.AudioOutputPricePM,
		CachedAudioInput: m.CachedAudioInputReadPricePM,
		Currency:         "USD",
		Unit:             "per_million_tokens",
	}
}

func buildParameterConstraints(m store.Model) parameterConstraints {
	minTok := 1
	if m.MinOutputTokens != nil {
		minTok = *m.MinOutputTokens
	}
	return parameterConstraints{
		MaxTokens:   intRange{Min: &minTok, Max: m.MaxOutputTokens},
		Temperature: floatRange{Min: m.TemperatureMin, Max: m.TemperatureMax},
	}
}

// buildCatalogDetail maps a model row to the public detail shape. Shared fields
// stay in lockstep with the list via the embedded buildCatalogEntry.
func buildCatalogDetail(m store.Model) catalogDetail {
	return catalogDetail{
		catalogEntry:         buildCatalogEntry(m),
		PricingDetail:        buildPricingDetail(m),
		CapabilityMatrix:     buildCapabilityMatrix(m.Features),
		ParameterConstraints: buildParameterConstraints(m),
		Family:               m.Family,
	}
}

// buildCatalogResponse wraps a page of entries in the catalog envelope.
// total_count is the full pre-pagination count; the page is entries
// [offset, offset+limit) clamped to bounds and never nil (serializes []).
func buildCatalogResponse(entries []catalogEntry, limit, offset int) map[string]any {
	if offset < 0 {
		offset = 0
	}
	if limit < 0 {
		limit = 0
	}
	total := len(entries)
	lo := offset
	if lo > total {
		lo = total
	}
	hi := lo + limit
	if hi > total {
		hi = total
	}
	page := entries[lo:hi]
	if page == nil {
		page = []catalogEntry{}
	}
	return map[string]any{
		"data":        page,
		"total_count": total,
		"limit":       limit,
		"offset":      offset,
	}
}

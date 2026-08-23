package models

import (
	"strings"
	"testing"

	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

func f64(v float64) *float64 { return &v }
func i(v int) *int           { return &v }

func sampleModel() store.Model {
	return store.Model{
		Code:             "gpt-4o",
		Name:             "GPT-4o",
		ProviderName:     "openai",
		MaxContextTokens: i(128000),
		InputModalities:  []string{"text", "image"},
		OutputModalities: []string{"text"},
		InputPricePM:     f64(2.5),
		OutputPricePM:    f64(10),
		Features:         []string{"vision", "function_calling"},
		CapabilityJson:   []byte(`{"maxBatchSize":2048}`),
		Lifecycle:        "ga",
	}
}

func TestBuildCatalogEntry_mapsAllFields(t *testing.T) {
	e := buildCatalogEntry(sampleModel())
	if e.ID != "openai/gpt-4o" {
		t.Errorf("id: got %q, want openai/gpt-4o", e.ID)
	}
	if e.Provider != "openai" {
		t.Errorf("provider: got %q", e.Provider)
	}
	if e.ContextLength == nil || *e.ContextLength != 128000 {
		t.Errorf("context_length: got %v", e.ContextLength)
	}
	if e.Status != "ga" {
		t.Errorf("status: got %q", e.Status)
	}
	if e.Pricing == nil || e.Pricing.Input == nil || *e.Pricing.Input != 2.5 ||
		e.Pricing.Unit != "per_million_tokens" || e.Pricing.Currency != "USD" {
		t.Errorf("pricing: got %+v", e.Pricing)
	}
	// modalities: distinct union, input order first
	if len(e.Modalities) != 2 || e.Modalities[0] != "text" || e.Modalities[1] != "image" {
		t.Errorf("modalities: got %v, want [text image]", e.Modalities)
	}
	// capabilities: capabilityJson merged + features present
	if e.Capabilities["features"] == nil {
		t.Error("capabilities.features missing")
	}
	if _, ok := e.Capabilities["maxBatchSize"]; !ok {
		t.Error("capabilities.maxBatchSize (from capabilityJson) missing")
	}
}

func TestBuildCatalogEntry_omitsPricingWhenUnpriced(t *testing.T) {
	m := sampleModel()
	m.InputPricePM = nil
	m.OutputPricePM = nil
	if e := buildCatalogEntry(m); e.Pricing != nil {
		t.Errorf("pricing should be nil when unpriced, got %+v", e.Pricing)
	}
}

func TestBuildCatalogEntry_idWithoutProvider(t *testing.T) {
	m := sampleModel()
	m.ProviderName = ""
	if e := buildCatalogEntry(m); e.ID != "gpt-4o" {
		t.Errorf("id without provider: got %q, want gpt-4o", e.ID)
	}
}

func TestBuildCatalogEntry_capabilitiesEmptyWhenNoJSON(t *testing.T) {
	m := sampleModel()
	m.CapabilityJson = nil
	m.Features = nil
	e := buildCatalogEntry(m)
	if e.Capabilities == nil {
		t.Fatal("capabilities must never be nil")
	}
	feats, ok := e.Capabilities["features"].([]string)
	if !ok || len(feats) != 0 {
		t.Errorf("features should be empty slice, got %v", e.Capabilities["features"])
	}
}

func TestBuildCatalogEntry_capabilityJSONLiteralNullDoesNotPanic(t *testing.T) {
	// Seeded/real DB rows commonly store the JSON scalar `null` (4 bytes,
	// len>0) rather than SQL NULL for capabilityJson. json.Unmarshal into a
	// map pointer turns that into a nil map, not an error — buildCatalogEntry
	// must not panic assigning "features" into it.
	m := sampleModel()
	m.CapabilityJson = []byte("null")
	e := buildCatalogEntry(m) // must not panic
	if e.Capabilities == nil {
		t.Fatal("capabilities must never be nil")
	}
	if _, ok := e.Capabilities["features"]; !ok {
		t.Error("capabilities.features missing after null capabilityJson")
	}
}

func TestBuildCatalogResponse_paginationSlices(t *testing.T) {
	entries := make([]catalogEntry, 5)
	for n := range entries {
		entries[n] = catalogEntry{ID: string(rune('a' + n))}
	}
	resp := buildCatalogResponse(entries, 2, 1)
	if resp["total_count"].(int) != 5 {
		t.Errorf("total_count: got %v, want 5", resp["total_count"])
	}
	page := resp["data"].([]catalogEntry)
	if len(page) != 2 || page[0].ID != "b" || page[1].ID != "c" {
		t.Errorf("page: got %+v, want ids b,c", page)
	}
	if resp["limit"].(int) != 2 || resp["offset"].(int) != 1 {
		t.Errorf("limit/offset echo wrong: %v %v", resp["limit"], resp["offset"])
	}
}

func TestBuildCatalogResponse_offsetPastEndEmptyData(t *testing.T) {
	entries := []catalogEntry{{ID: "a"}}
	resp := buildCatalogResponse(entries, 50, 10)
	page := resp["data"].([]catalogEntry)
	if len(page) != 0 {
		t.Errorf("data should be empty, got %d", len(page))
	}
	// must serialize as [] not null
	b, _ := json.Marshal(resp)
	if !containsSub(string(b), `"data":[]`) {
		t.Errorf("empty data must serialize as [], got %s", b)
	}
	if resp["total_count"].(int) != 1 {
		t.Errorf("total_count: got %v, want 1", resp["total_count"])
	}
}

func TestBuildCatalogResponse_negativeParamsNoPanic(t *testing.T) {
	entries := []catalogEntry{{ID: "a"}}
	resp := buildCatalogResponse(entries, -5, -3) // must not panic
	if len(resp["data"].([]catalogEntry)) != 0 {
		t.Errorf("negative params should yield empty page, got %d", len(resp["data"].([]catalogEntry)))
	}
	if resp["total_count"].(int) != 1 {
		t.Errorf("total_count: got %v, want 1", resp["total_count"])
	}
}

func containsSub(s, sub string) bool {
	for n := 0; n+len(sub) <= len(s); n++ {
		if s[n:n+len(sub)] == sub {
			return true
		}
	}
	return false
}

func sampleRealtimeModel() store.Model {
	m := sampleModel()
	m.Code = "gpt-realtime"
	m.CachedInputReadPricePM = f64(1.25)
	m.CachedInputWritePricePM = f64(3.125)
	m.AudioInputPricePM = f64(40)
	m.AudioOutputPricePM = f64(80)
	m.CachedAudioInputReadPricePM = f64(2.5)
	m.MaxOutputTokens = i(4096)
	m.TemperatureMin = f64(0)
	m.TemperatureMax = f64(2)
	fam := "gpt-realtime"
	m.Family = &fam
	m.Features = []string{"vision", "function_calling", "streaming"}
	return m
}

func TestBuildCatalogDetail_pricingAndConstraints(t *testing.T) {
	d := buildCatalogDetail(sampleRealtimeModel())

	// Inherited list-entry fields are present (embedded, promoted to top level).
	if d.ID != "openai/gpt-realtime" {
		t.Errorf("id: got %q", d.ID)
	}
	// pricing_detail carries every configured rate.
	if d.PricingDetail.AudioInput == nil || *d.PricingDetail.AudioInput != 40 {
		t.Errorf("audio_input: got %v", d.PricingDetail.AudioInput)
	}
	if d.PricingDetail.CachedInput == nil || *d.PricingDetail.CachedInput != 1.25 {
		t.Errorf("cached_input: got %v", d.PricingDetail.CachedInput)
	}
	if d.PricingDetail.Currency != "USD" || d.PricingDetail.Unit != "per_million_tokens" {
		t.Errorf("currency/unit: %+v", d.PricingDetail)
	}
	// capability_matrix reflects Features membership for the 5 named keys;
	// `tools` derives from function_calling (catalog never tags "tools").
	if !d.CapabilityMatrix["vision"] || !d.CapabilityMatrix["streaming"] || !d.CapabilityMatrix["function_calling"] {
		t.Errorf("matrix true-keys: %+v", d.CapabilityMatrix)
	}
	if !d.CapabilityMatrix["tools"] {
		t.Errorf("tools should derive true from function_calling: %+v", d.CapabilityMatrix)
	}
	if d.CapabilityMatrix["json_mode"] {
		t.Errorf("json_mode should be false: %+v", d.CapabilityMatrix)
	}
	// parameter_constraints: max_tokens.max = MaxOutputTokens, temperature from cols.
	if d.ParameterConstraints.MaxTokens.Max == nil || *d.ParameterConstraints.MaxTokens.Max != 4096 {
		t.Errorf("max_tokens.max: got %v", d.ParameterConstraints.MaxTokens.Max)
	}
	if d.ParameterConstraints.Temperature.Max == nil || *d.ParameterConstraints.Temperature.Max != 2 {
		t.Errorf("temperature.max: got %v", d.ParameterConstraints.Temperature.Max)
	}
	if d.Family == nil || *d.Family != "gpt-realtime" {
		t.Errorf("family: got %v", d.Family)
	}
}

func TestBuildCatalogDetail_defaultsAndOmission(t *testing.T) {
	d := buildCatalogDetail(sampleModel()) // no audio, no temp cols, no minOutputTokens

	// max_tokens.min defaults to 1 when MinOutputTokens is nil.
	if d.ParameterConstraints.MaxTokens.Min == nil || *d.ParameterConstraints.MaxTokens.Min != 1 {
		t.Errorf("max_tokens.min default: got %v, want 1", d.ParameterConstraints.MaxTokens.Min)
	}
	// Null audio rates omit from JSON.
	b, _ := json.Marshal(d)
	if strings.Contains(string(b), "audio_input") {
		t.Errorf("audio_input should be omitted when nil: %s", b)
	}
	// temperature object present but empty (min/max omitted) when unseeded.
	if strings.Contains(string(b), "\"temperature\":{\"min\"") {
		t.Errorf("temperature min/max should omit when nil: %s", b)
	}
}

func TestBuildCapabilityMatrix_toolsFalseWithoutFunctionCalling(t *testing.T) {
	m := buildCapabilityMatrix([]string{"streaming", "vision"})
	if m["tools"] || m["function_calling"] {
		t.Errorf("tools/function_calling should be false without the tag: %+v", m)
	}
}

// Every capability the ROUTER refuses to route around must be visible in the
// public matrix.
//
// capability_matrix is what an SDK caller reads to find out what a model can
// do. When `auto` declines to send a json_schema request to a model, or a
// reasoning request to a model that does not reason, that eligibility fact is
// the caller's business — and until this test, neither key existed, so the one
// surface built to answer "what can this model do" could not answer the two
// questions routing actually acts on.
//
// The list is written as the routing dimensions rather than as literals, so a
// dimension added to the filter without a matrix key fails here instead of
// shipping a capability nobody outside the router can observe.
func TestBuildCapabilityMatrix_carriesEveryRoutingDimension(t *testing.T) {
	// The tags filterByCapability treats as eligibility, not preference.
	routingDimensions := []string{"function_calling", "reasoning", "structured_outputs"}

	for _, dim := range routingDimensions {
		t.Run(dim, func(t *testing.T) {
			on := buildCapabilityMatrix([]string{dim})
			got, present := on[dim]
			if !present {
				t.Fatalf("capability_matrix has no %q key, so a client cannot see a fact the "+
					"router routes on: %+v", dim, on)
			}
			if !got {
				t.Errorf("%q declared in features but the matrix reports false: %+v", dim, on)
			}
			off := buildCapabilityMatrix([]string{"streaming"})
			if off[dim] {
				t.Errorf("%q not declared, yet the matrix reports true: %+v", dim, off)
			}
		})
	}
}

// The matrix must not invent a capability from a neighbouring one. Probed
// 2026-08-19: gpt-4-turbo carries json_mode and answers 400 to a json_schema,
// so deriving one from the other would tell a caller the opposite of the wire.
// `tools` from `function_calling` is the ONE sanctioned derivation, and it is
// a rename of the same capability rather than an inference across two.
func TestBuildCapabilityMatrix_jsonModeDoesNotImplyStructuredOutputs(t *testing.T) {
	m := buildCapabilityMatrix([]string{"json_mode", "streaming"})
	if m["structured_outputs"] {
		t.Errorf("json_mode made structured_outputs true — gpt-4-turbo carries json_mode and "+
			"REFUSES a json_schema, so this reports the reverse of the measurement: %+v", m)
	}
	if !m["json_mode"] {
		t.Errorf("json_mode itself was lost: %+v", m)
	}
}

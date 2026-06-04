// Package models — models_handler_test.go covers ModelsHandler and
// ModelDetailHandler via httptest.
//
// Named failure modes:
//   - nil ModelLookup → 500 JSON error
//   - nil VKAuthenticator → 500 JSON error (authenticator not configured)
//   - VK auth failure (missing/invalid) → 401
//   - ListEnabledModels error → 500 JSON error
//   - No anthropic-version header → OpenAI shape
//   - anthropic-version header → Anthropic shape (type:"model", display_name, first_id/last_id)
//   - VK with AllowedModels → filtered list
//   - VK with no AllowedModels → full list
//   - Enriched fields (aliases, features, pricing, context window, lifecycle) surfaced in both shapes
//   - ModelDetailHandler: nil store → 500; nil vkAuth → 500; auth failure → 401
//   - ModelDetailHandler: missing path param → 400; not found → 404
//   - ModelDetailHandler: model outside VK AllowedModels → 404 (hidden, not 403)
//   - ModelDetailHandler: OpenAI shape vs Anthropic shape + enriched fields
package models

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"log/slog"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

// stubModelLookup implements ModelLookup.
type stubModelLookup struct {
	models  []store.Model
	model   *store.Model
	listErr error
	getErr  error
}

func (s *stubModelLookup) GetModel(_ context.Context, _ string) (*store.Model, error) {
	return s.model, s.getErr
}

func (s *stubModelLookup) GetModelByCode(_ context.Context, _ string) (*store.Model, error) {
	return s.model, s.getErr
}

func (s *stubModelLookup) ListEnabledModels(_ context.Context) ([]store.Model, error) {
	return s.models, s.listErr
}

// stubVKAuth implements VKAuthenticator.
type stubVKAuth struct {
	meta    *vkauth.VKMeta
	authErr error
}

func (s *stubVKAuth) Authenticate(_ context.Context, _ *http.Request) (*vkauth.VKMeta, error) {
	return s.meta, s.authErr
}

// okVKAuth returns an authenticator that accepts any request with an
// unrestricted VK (no AllowedModels filter).
func okVKAuth() *stubVKAuth { return &stubVKAuth{meta: &vkauth.VKMeta{}} }

var devLogger = slog.Default()

// newReq builds a test GET request to /v1/models.
func newReq(headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestModelsHandler_nilModels_returns500(t *testing.T) {
	h := ModelsHandler(nil, okVKAuth(), devLogger)
	w := httptest.NewRecorder()
	h(w, newReq(nil))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 500", w.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["error"] == nil {
		t.Error("expected error in body")
	}
}

func TestModelsHandler_nilVKAuth_returns500(t *testing.T) {
	lk := &stubModelLookup{models: []store.Model{{ID: "m1", Code: "gpt-5"}}}
	h := ModelsHandler(lk, nil, devLogger)
	w := httptest.NewRecorder()
	h(w, newReq(nil))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 500 (authenticator not configured)", w.Code)
	}
}

func TestModelsHandler_authFailure_returns401(t *testing.T) {
	lk := &stubModelLookup{models: []store.Model{{ID: "m1", Code: "gpt-5"}}}
	vkAuth := &stubVKAuth{authErr: errors.New("vkauth: virtual key missing")}
	h := ModelsHandler(lk, vkAuth, devLogger)
	w := httptest.NewRecorder()
	h(w, newReq(nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", w.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["error"] == nil {
		t.Error("expected error envelope on 401")
	}
}

func TestModelsHandler_listError_returns500(t *testing.T) {
	lk := &stubModelLookup{listErr: errors.New("db down")}
	h := ModelsHandler(lk, okVKAuth(), devLogger)
	w := httptest.NewRecorder()
	h(w, newReq(nil))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 500", w.Code)
	}
}

func TestModelsHandler_openAIShape_noAnthropicHeader(t *testing.T) {
	models := []store.Model{
		{ID: "m1", Code: "gpt-5", Name: "GPT-5", ProviderID: "p1", ProviderName: "openai"},
	}
	h := ModelsHandler(&stubModelLookup{models: models}, okVKAuth(), devLogger)
	w := httptest.NewRecorder()
	h(w, newReq(nil))
	if w.Code != http.StatusOK {
		t.Errorf("status: got %d", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["object"] != "list" {
		t.Errorf("object: got %v", resp["object"])
	}
	data, _ := resp["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("data: got %d, want 1", len(data))
	}
	entry, _ := data[0].(map[string]any)
	if entry["id"] != "gpt-5" {
		t.Errorf("id: got %v", entry["id"])
	}
	if entry["owned_by"] != "openai" {
		t.Errorf("owned_by: got %v", entry["owned_by"])
	}
	if entry["object"] != "model" {
		t.Errorf("entry.object: got %v", entry["object"])
	}
}

func TestModelsHandler_anthropicShape_withAnthropicVersionHeader(t *testing.T) {
	maxCtx := 200000
	maxOut := 8192
	models := []store.Model{
		{
			ID: "m1", Code: "claude-sonnet-4-6", Name: "Claude Sonnet 4.6",
			ProviderID: "p1", ProviderName: "anthropic",
			MaxContextTokens: &maxCtx, MaxOutputTokens: &maxOut,
		},
	}
	h := ModelsHandler(&stubModelLookup{models: models}, okVKAuth(), devLogger)
	w := httptest.NewRecorder()
	h(w, newReq(map[string]string{"anthropic-version": "2023-06-01"}))
	if w.Code != http.StatusOK {
		t.Errorf("status: got %d", w.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	data, _ := resp["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("data: got %d", len(data))
	}
	entry, _ := data[0].(map[string]any)
	if entry["type"] != "model" {
		t.Errorf("type: got %v, want model", entry["type"])
	}
	if entry["id"] != "claude-sonnet-4-6" {
		t.Errorf("id: got %v", entry["id"])
	}
	if entry["display_name"] != "Claude Sonnet 4.6" {
		t.Errorf("display_name: got %v", entry["display_name"])
	}
	if resp["first_id"] != "claude-sonnet-4-6" {
		t.Errorf("first_id: got %v", resp["first_id"])
	}
	if resp["last_id"] != "claude-sonnet-4-6" {
		t.Errorf("last_id: got %v", resp["last_id"])
	}
	if resp["has_more"] != false {
		t.Errorf("has_more: got %v", resp["has_more"])
	}
}

func TestModelsHandler_vkFilterApplied(t *testing.T) {
	models := []store.Model{
		{ID: "m1", Code: "gpt-5", ProviderID: "prov-openai", ProviderModelID: "gpt-5"},
		{ID: "m2", Code: "claude-sonnet-4-6", ProviderID: "prov-anthropic", ProviderModelID: "claude-sonnet-4-6"},
	}
	vkAuth := &stubVKAuth{
		meta: &vkauth.VKMeta{
			AllowedModels: []store.AllowedModelRef{
				{ProviderID: "prov-openai", ModelID: "gpt-5"},
			},
		},
	}
	h := ModelsHandler(&stubModelLookup{models: models}, vkAuth, devLogger)
	w := httptest.NewRecorder()
	h(w, newReq(nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d", w.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	data, _ := resp["data"].([]any)
	if len(data) != 1 {
		t.Errorf("filtered list: got %d, want 1", len(data))
	}
	entry, _ := data[0].(map[string]any)
	if entry["id"] != "gpt-5" {
		t.Errorf("id: got %v, want gpt-5", entry["id"])
	}
}

func TestModelsHandler_vkWithNoAllowedModels_fullList(t *testing.T) {
	models := []store.Model{
		{ID: "m1", Code: "gpt-5", ProviderID: "p1", ProviderName: "openai"},
		{ID: "m2", Code: "claude-3", ProviderID: "p2", ProviderName: "anthropic"},
	}
	vkAuth := &stubVKAuth{meta: &vkauth.VKMeta{AllowedModels: nil}}
	h := ModelsHandler(&stubModelLookup{models: models}, vkAuth, devLogger)
	w := httptest.NewRecorder()
	h(w, newReq(nil))
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	data, _ := resp["data"].([]any)
	if len(data) != 2 {
		t.Errorf("unrestricted VK: got %d, want 2", len(data))
	}
}

func TestModelsHandler_enrichedFields_openAIShape(t *testing.T) {
	inP, outP, cacheR := 2.5, 10.0, 1.25
	maxCtx, maxOut := 128000, 16384
	models := []store.Model{
		{
			ID: "m1", Code: "gpt-4o", Name: "GPT-4o", ProviderID: "p1", ProviderName: "openai",
			Type:             "chat",
			Aliases:          []string{"gpt-4o-2024-08-06"},
			Features:         []string{"vision", "function_calling"},
			InputModalities:  []string{"text", "image"},
			OutputModalities: []string{"text"},
			MaxContextTokens: &maxCtx, MaxOutputTokens: &maxOut,
			Lifecycle:              "ga",
			InputPricePM:           &inP,
			OutputPricePM:          &outP,
			CachedInputReadPricePM: &cacheR,
		},
		// Second model with no price configured → pricing key omitted.
		{ID: "m2", Code: "free-model", ProviderID: "p1", ProviderName: "openai"},
	}
	h := ModelsHandler(&stubModelLookup{models: models}, okVKAuth(), devLogger)
	w := httptest.NewRecorder()
	h(w, newReq(nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d", w.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	data, _ := resp["data"].([]any)
	entry, _ := data[0].(map[string]any)

	if got := toStrings(entry["aliases"]); len(got) != 1 || got[0] != "gpt-4o-2024-08-06" {
		t.Errorf("aliases: got %v", entry["aliases"])
	}
	if got := toStrings(entry["features"]); len(got) != 2 {
		t.Errorf("features: got %v", entry["features"])
	}
	if entry["type"] != "chat" {
		t.Errorf("type: got %v", entry["type"])
	}
	if entry["maxContextTokens"] != float64(128000) {
		t.Errorf("maxContextTokens: got %v", entry["maxContextTokens"])
	}
	if entry["maxOutputTokens"] != float64(16384) {
		t.Errorf("maxOutputTokens: got %v", entry["maxOutputTokens"])
	}
	if entry["lifecycle"] != "ga" {
		t.Errorf("lifecycle: got %v", entry["lifecycle"])
	}
	pricing, ok := entry["pricing"].(map[string]any)
	if !ok {
		t.Fatalf("pricing block missing: %v", entry["pricing"])
	}
	if pricing["inputPerMillion"] != 2.5 {
		t.Errorf("inputPerMillion: got %v", pricing["inputPerMillion"])
	}
	if pricing["outputPerMillion"] != 10.0 {
		t.Errorf("outputPerMillion: got %v", pricing["outputPerMillion"])
	}
	if pricing["cachedInputReadPerMillion"] != 1.25 {
		t.Errorf("cachedInputReadPerMillion: got %v", pricing["cachedInputReadPerMillion"])
	}
	if pricing["currency"] != "USD" {
		t.Errorf("currency: got %v", pricing["currency"])
	}
	if pricing["unit"] != "per_million_tokens" {
		t.Errorf("unit: got %v", pricing["unit"])
	}
	// cachedInputWritePerMillion was not configured → omitted.
	if _, present := pricing["cachedInputWritePerMillion"]; present {
		t.Error("cachedInputWritePerMillion should be omitted when unset")
	}

	// Second model: no price → no pricing key at all.
	entry2, _ := data[1].(map[string]any)
	if _, present := entry2["pricing"]; present {
		t.Error("pricing key should be omitted when the model has no configured price")
	}
}

func TestModelsHandler_enrichedFields_anthropicShape(t *testing.T) {
	inP := 3.0
	models := []store.Model{
		{
			ID: "m1", Code: "claude-sonnet-4-6", Name: "Claude Sonnet 4.6",
			ProviderID: "p1", ProviderName: "anthropic",
			Aliases:      []string{"claude-sonnet-latest"},
			Features:     []string{"thinking"},
			Lifecycle:    "preview",
			InputPricePM: &inP,
		},
	}
	h := ModelsHandler(&stubModelLookup{models: models}, okVKAuth(), devLogger)
	w := httptest.NewRecorder()
	h(w, newReq(map[string]string{"anthropic-version": "2023-06-01"}))
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	data, _ := resp["data"].([]any)
	entry, _ := data[0].(map[string]any)
	if got := toStrings(entry["aliases"]); len(got) != 1 || got[0] != "claude-sonnet-latest" {
		t.Errorf("aliases: got %v", entry["aliases"])
	}
	if got := toStrings(entry["features"]); len(got) != 1 || got[0] != "thinking" {
		t.Errorf("features: got %v", entry["features"])
	}
	if entry["lifecycle"] != "preview" {
		t.Errorf("lifecycle: got %v", entry["lifecycle"])
	}
	pricing, ok := entry["pricing"].(map[string]any)
	if !ok || pricing["inputPerMillion"] != 3.0 {
		t.Errorf("pricing: got %v", entry["pricing"])
	}
}

func TestModelDetailHandler_nilModels_returns500(t *testing.T) {
	h := ModelDetailHandler(nil, okVKAuth(), devLogger)
	r := httptest.NewRequest(http.MethodGet, "/v1/models/gpt-5", nil)
	r.SetPathValue("model", "gpt-5")
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 500", w.Code)
	}
}

func TestModelDetailHandler_nilVKAuth_returns500(t *testing.T) {
	lk := &stubModelLookup{model: &store.Model{ID: "m1", Code: "gpt-5"}}
	h := ModelDetailHandler(lk, nil, devLogger)
	r := httptest.NewRequest(http.MethodGet, "/v1/models/gpt-5", nil)
	r.SetPathValue("model", "gpt-5")
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 500", w.Code)
	}
}

func TestModelDetailHandler_authFailure_returns401(t *testing.T) {
	lk := &stubModelLookup{model: &store.Model{ID: "m1", Code: "gpt-5"}}
	vkAuth := &stubVKAuth{authErr: errors.New("vkauth: virtual key invalid")}
	h := ModelDetailHandler(lk, vkAuth, devLogger)
	r := httptest.NewRequest(http.MethodGet, "/v1/models/gpt-5", nil)
	r.SetPathValue("model", "gpt-5")
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", w.Code)
	}
}

func TestModelDetailHandler_emptyModelID_returns400(t *testing.T) {
	lk := &stubModelLookup{}
	h := ModelDetailHandler(lk, okVKAuth(), devLogger)
	r := httptest.NewRequest(http.MethodGet, "/v1/models/", nil)
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", w.Code)
	}
}

func TestModelDetailHandler_notFound_returns404(t *testing.T) {
	lk := &stubModelLookup{getErr: errors.New("not found")}
	h := ModelDetailHandler(lk, okVKAuth(), devLogger)
	r := httptest.NewRequest(http.MethodGet, "/v1/models/unknown", nil)
	r.SetPathValue("model", "unknown")
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", w.Code)
	}
}

func TestModelDetailHandler_disallowedModel_returns404(t *testing.T) {
	// VK scoped to an openai model; requested model belongs to anthropic →
	// must be hidden as 404, not revealed.
	m := &store.Model{ID: "m2", Code: "claude-sonnet-4-6", ProviderID: "prov-anthropic", ProviderModelID: "claude-sonnet-4-6"}
	lk := &stubModelLookup{model: m}
	vkAuth := &stubVKAuth{
		meta: &vkauth.VKMeta{
			AllowedModels: []store.AllowedModelRef{{ProviderID: "prov-openai", ModelID: "gpt-5"}},
		},
	}
	h := ModelDetailHandler(lk, vkAuth, devLogger)
	r := httptest.NewRequest(http.MethodGet, "/v1/models/claude-sonnet-4-6", nil)
	r.SetPathValue("model", "claude-sonnet-4-6")
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404 (disallowed model hidden)", w.Code)
	}
}

func TestModelDetailHandler_openAIShape_enriched(t *testing.T) {
	inP := 2.5
	maxCtx := 128000
	m := &store.Model{
		ID: "m1", Code: "gpt-4o", Name: "GPT-4o", ProviderName: "openai",
		Type: "chat", Aliases: []string{"gpt-4o-2024-08-06"}, Features: []string{"vision"},
		MaxContextTokens: &maxCtx, Lifecycle: "ga", InputPricePM: &inP,
	}
	lk := &stubModelLookup{model: m}
	h := ModelDetailHandler(lk, okVKAuth(), devLogger)
	r := httptest.NewRequest(http.MethodGet, "/v1/models/gpt-4o", nil)
	r.SetPathValue("model", "gpt-4o")
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d", w.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["id"] != "gpt-4o" {
		t.Errorf("id: got %v", resp["id"])
	}
	if resp["object"] != "model" {
		t.Errorf("object: got %v", resp["object"])
	}
	if resp["owned_by"] != "openai" {
		t.Errorf("owned_by: got %v", resp["owned_by"])
	}
	if got := toStrings(resp["aliases"]); len(got) != 1 {
		t.Errorf("aliases: got %v", resp["aliases"])
	}
	if resp["maxContextTokens"] != float64(128000) {
		t.Errorf("maxContextTokens: got %v", resp["maxContextTokens"])
	}
	if resp["lifecycle"] != "ga" {
		t.Errorf("lifecycle: got %v", resp["lifecycle"])
	}
	if _, ok := resp["pricing"].(map[string]any); !ok {
		t.Errorf("pricing block missing on detail: %v", resp["pricing"])
	}
}

func TestModelDetailHandler_anthropicShape_enriched(t *testing.T) {
	maxCtx := 100000
	maxOut := 4096
	m := &store.Model{
		ID: "m1", Code: "claude-sonnet-4-6", Name: "Claude Sonnet 4.6",
		MaxContextTokens: &maxCtx, MaxOutputTokens: &maxOut,
		Aliases: []string{"claude-latest"},
	}
	lk := &stubModelLookup{model: m}
	h := ModelDetailHandler(lk, okVKAuth(), devLogger)
	r := httptest.NewRequest(http.MethodGet, "/v1/models/claude-sonnet-4-6", nil)
	r.SetPathValue("model", "claude-sonnet-4-6")
	r.Header.Set("anthropic-version", "2023-06-01")
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d", w.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["type"] != "model" {
		t.Errorf("type: got %v, want model", resp["type"])
	}
	if resp["display_name"] != "Claude Sonnet 4.6" {
		t.Errorf("display_name: got %v", resp["display_name"])
	}
	if resp["max_input_tokens"] == nil {
		t.Error("max_input_tokens should be present")
	}
	if got := toStrings(resp["aliases"]); len(got) != 1 || got[0] != "claude-latest" {
		t.Errorf("aliases: got %v", resp["aliases"])
	}
}

func TestModelsHandler_errorResponseIsValidJSON(t *testing.T) {
	h := ModelsHandler(nil, okVKAuth(), devLogger)
	w := httptest.NewRecorder()
	h(w, newReq(nil))
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type: got %q", ct)
	}
	if !json.Valid(w.Body.Bytes()) {
		t.Errorf("response is not valid JSON: %s", w.Body.Bytes())
	}
}

// toStrings converts a decoded JSON array of strings to []string.
func toStrings(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

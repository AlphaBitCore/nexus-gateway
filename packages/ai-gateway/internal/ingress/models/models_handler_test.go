// Package models — models_handler_test.go covers ModelsHandler and
// ModelDetailHandler via httptest.
//
// Named failure modes:
//   - nil ModelLookup → 500 JSON error
//   - ListEnabledModels error → 500 JSON error
//   - No anthropic-version header → OpenAI shape
//   - anthropic-version header → Anthropic shape (type:"model", display_name, first_id/last_id)
//   - VK with AllowedModels → filtered list
//   - VK auth error (or nil vkAuth) → unfiltered list
//   - ModelDetailHandler: nil store → 500
//   - ModelDetailHandler: missing path param → 400
//   - ModelDetailHandler: not found → 404
//   - ModelDetailHandler: OpenAI shape vs Anthropic shape
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
	h := ModelsHandler(nil, nil, devLogger)
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

func TestModelsHandler_listError_returns500(t *testing.T) {
	lk := &stubModelLookup{listErr: errors.New("db down")}
	h := ModelsHandler(lk, nil, devLogger)
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
	h := ModelsHandler(&stubModelLookup{models: models}, nil, devLogger)
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
	h := ModelsHandler(&stubModelLookup{models: models}, nil, devLogger)
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

func TestModelsHandler_vkAuthError_unfilteredList(t *testing.T) {
	models := []store.Model{
		{ID: "m1", Code: "gpt-5", ProviderID: "p1", ProviderName: "openai"},
	}
	vkAuth := &stubVKAuth{authErr: errors.New("bad key")}
	h := ModelsHandler(&stubModelLookup{models: models}, vkAuth, devLogger)
	w := httptest.NewRecorder()
	h(w, newReq(nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d", w.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	data, _ := resp["data"].([]any)
	// Auth error → VK allowed models not applied → unfiltered
	if len(data) != 1 {
		t.Errorf("unfiltered list: got %d, want 1", len(data))
	}
}

func TestModelsHandler_vkWithNoAllowedModels_unfilteredList(t *testing.T) {
	models := []store.Model{
		{ID: "m1", Code: "gpt-5", ProviderID: "p1", ProviderName: "openai"},
		{ID: "m2", Code: "claude-3", ProviderID: "p2", ProviderName: "anthropic"},
	}
	// VK has no allowed models restriction
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


func TestModelDetailHandler_nilModels_returns500(t *testing.T) {
	h := ModelDetailHandler(nil, devLogger)
	r := httptest.NewRequest(http.MethodGet, "/v1/models/gpt-5", nil)
	r.SetPathValue("model", "gpt-5")
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 500", w.Code)
	}
}

func TestModelDetailHandler_emptyModelID_returns400(t *testing.T) {
	lk := &stubModelLookup{}
	h := ModelDetailHandler(lk, devLogger)
	// No path value set → PathValue returns ""
	r := httptest.NewRequest(http.MethodGet, "/v1/models/", nil)
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", w.Code)
	}
}

func TestModelDetailHandler_notFound_returns404(t *testing.T) {
	lk := &stubModelLookup{getErr: errors.New("not found")}
	h := ModelDetailHandler(lk, devLogger)
	r := httptest.NewRequest(http.MethodGet, "/v1/models/unknown", nil)
	r.SetPathValue("model", "unknown")
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", w.Code)
	}
}

func TestModelDetailHandler_openAIShape(t *testing.T) {
	m := &store.Model{ID: "m1", Code: "gpt-5", Name: "GPT-5", ProviderName: "openai"}
	lk := &stubModelLookup{model: m}
	h := ModelDetailHandler(lk, devLogger)
	r := httptest.NewRequest(http.MethodGet, "/v1/models/gpt-5", nil)
	r.SetPathValue("model", "gpt-5")
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d", w.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["id"] != "gpt-5" {
		t.Errorf("id: got %v", resp["id"])
	}
	if resp["object"] != "model" {
		t.Errorf("object: got %v", resp["object"])
	}
	if resp["owned_by"] != "openai" {
		t.Errorf("owned_by: got %v", resp["owned_by"])
	}
}

func TestModelDetailHandler_anthropicShape(t *testing.T) {
	maxCtx := 100000
	maxOut := 4096
	m := &store.Model{
		ID: "m1", Code: "claude-sonnet-4-6", Name: "Claude Sonnet 4.6",
		MaxContextTokens: &maxCtx, MaxOutputTokens: &maxOut,
	}
	lk := &stubModelLookup{model: m}
	h := ModelDetailHandler(lk, devLogger)
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
}

// writeJSONError / jsonString (via handler responses)

func TestModelsHandler_errorResponseIsValidJSON(t *testing.T) {
	h := ModelsHandler(nil, nil, devLogger)
	w := httptest.NewRecorder()
	h(w, newReq(nil))
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type: got %q", ct)
	}
	if !json.Valid(w.Body.Bytes()) {
		t.Errorf("response is not valid JSON: %s", w.Body.Bytes())
	}
}

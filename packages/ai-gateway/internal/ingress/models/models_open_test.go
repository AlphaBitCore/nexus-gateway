package models

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

// openReq builds a GET request to the public catalog endpoint.
func openReq(query string) *http.Request {
	return httptest.NewRequest(http.MethodGet, "/api/v1/open/models"+query, nil)
}

func TestOpenModelsHandler_publicNoAuth_returns200(t *testing.T) {
	lookup := &stubModelLookup{models: []store.Model{sampleModel()}}
	h := OpenModelsHandler(lookup, nil, devLogger)
	w := httptest.NewRecorder()
	h(w, openReq("")) // no virtual key at all
	if w.Code != http.StatusOK {
		t.Fatalf("public catalog status: got %d, want 200", w.Code)
	}
}

func TestOpenModelsHandler_catalogShape(t *testing.T) {
	lookup := &stubModelLookup{models: []store.Model{sampleModel()}}
	h := OpenModelsHandler(lookup, nil, devLogger)
	w := httptest.NewRecorder()
	h(w, openReq(""))

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["total_count"] == nil {
		t.Error("catalog envelope missing total_count")
	}
	data := body["data"].([]any)
	first := data[0].(map[string]any)
	if first["id"] != "openai/gpt-4o" {
		t.Errorf("catalog id: got %v, want openai/gpt-4o", first["id"])
	}
	if first["provider"] != "openai" {
		t.Errorf("catalog provider: got %v, want openai", first["provider"])
	}
	if first["context_length"] == nil {
		t.Error("catalog entry missing context_length")
	}
	if first["capabilities"] == nil {
		t.Error("catalog entry missing capabilities")
	}
}

func TestOpenModelsHandler_nilModels_returns500(t *testing.T) {
	h := OpenModelsHandler(nil, nil, devLogger)
	w := httptest.NewRecorder()
	h(w, openReq(""))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 500", w.Code)
	}
}

func TestOpenModelsHandler_paginationLimitClampAndOffset(t *testing.T) {
	models := make([]store.Model, 5)
	for n := range models {
		models[n] = sampleModel()
		models[n].Code = string(rune('a' + n))
	}
	lookup := &stubModelLookup{models: models}
	h := OpenModelsHandler(lookup, nil, devLogger)

	// limit over max clamps to 200 (returns all 5), offset 3 -> 2 remaining
	w := httptest.NewRecorder()
	h(w, openReq("?limit=999&offset=3"))

	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if int(body["total_count"].(float64)) != 5 {
		t.Errorf("total_count: got %v, want 5", body["total_count"])
	}
	if int(body["limit"].(float64)) != 200 {
		t.Errorf("limit clamp: got %v, want 200", body["limit"])
	}
	if got := len(body["data"].([]any)); got != 2 {
		t.Errorf("offset=3 of 5: got %d rows, want 2", got)
	}
}

func TestOpenModelsHandler_defaultLimit(t *testing.T) {
	w := httptest.NewRecorder()
	lookup := &stubModelLookup{models: []store.Model{sampleModel()}}
	OpenModelsHandler(lookup, nil, devLogger)(w, openReq(""))
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if int(body["limit"].(float64)) != 50 {
		t.Errorf("default limit: got %v, want 50", body["limit"])
	}
	if int(body["offset"].(float64)) != 0 {
		t.Errorf("default offset: got %v, want 0", body["offset"])
	}
}

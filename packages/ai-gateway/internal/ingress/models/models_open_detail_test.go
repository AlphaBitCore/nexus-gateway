package models

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

// detailLookup is a stub ModelLookup: returns `model` for a matching code,
// or an error (not-found) otherwise.
type detailLookup struct {
	model *store.Model
	err   error
}

func (d detailLookup) GetModel(_ context.Context, _ string) (*store.Model, error) {
	return d.model, d.err
}
func (d detailLookup) GetModelByCode(_ context.Context, code string) (*store.Model, error) {
	if d.err != nil {
		return nil, d.err
	}
	if d.model != nil && d.model.Code == code {
		return d.model, nil
	}
	return nil, errors.New("not found")
}
func (d detailLookup) ListEnabledModels(_ context.Context) ([]store.Model, error) {
	return nil, nil
}

func doDetail(t *testing.T, lookup ModelLookup, pathModelID string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/open/models/"+pathModelID, nil)
	r.SetPathValue("model_id", pathModelID)
	w := httptest.NewRecorder()
	OpenModelDetailHandler(lookup, slog.Default()).ServeHTTP(w, r)
	return w
}

func TestOpenModelDetail_ok(t *testing.T) {
	m := sampleRealtimeModel()
	w := doDetail(t, detailLookup{model: &m}, "gpt-realtime")
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, key := range []string{`"pricing_detail"`, `"cached_input"`, `"audio_input"`, `"audio_output"`,
		`"capability_matrix"`, `"parameter_constraints"`, `"max_tokens"`, `"temperature"`, `"family"`, `"openai/gpt-realtime"`} {
		if !strings.Contains(body, key) {
			t.Errorf("body missing %s: %s", key, body)
		}
	}
	// parameter_constraints has the nested bounds required by acceptance.
	var got struct {
		ParameterConstraints struct {
			MaxTokens   map[string]int     `json:"max_tokens"`
			Temperature map[string]float64 `json:"temperature"`
		} `json:"parameter_constraints"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got.ParameterConstraints.MaxTokens["min"]; !ok {
		t.Error("max_tokens.min missing")
	}
	if _, ok := got.ParameterConstraints.Temperature["max"]; !ok {
		t.Error("temperature.max missing")
	}
}

func TestOpenModelDetail_providerPrefixedID(t *testing.T) {
	m := sampleModel() // Code == "gpt-4o"
	w := doDetail(t, detailLookup{model: &m}, "openai/gpt-4o")
	if w.Code != http.StatusOK {
		t.Fatalf("prefixed id should resolve: got %d, body=%s", w.Code, w.Body.String())
	}
}

func TestOpenModelDetail_notFound(t *testing.T) {
	w := doDetail(t, detailLookup{err: errors.New("no rows")}, "nope")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", w.Code)
	}
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("error envelope: %v", err)
	}
	if !strings.Contains(env.Error.Message, "model not found") {
		t.Errorf("message: got %q", env.Error.Message)
	}
}

func TestOpenModelDetail_nilLookup(t *testing.T) {
	w := doDetail(t, nil, "gpt-4o")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("nil lookup: got %d, want 500", w.Code)
	}
}

func TestOpenModelDetail_emptyID(t *testing.T) {
	// PathValue("model_id") returns "" when unset (no real mux match); the
	// handler must reject this before ever calling the lookup.
	r := httptest.NewRequest(http.MethodGet, "/api/v1/open/models/", nil)
	w := httptest.NewRecorder()
	OpenModelDetailHandler(detailLookup{}, slog.Default()).ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty id: got %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

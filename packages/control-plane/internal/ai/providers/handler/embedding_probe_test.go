package providers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"
)

// RegisterEmbeddingProbeRoutes — wires the route

func TestRegisterEmbeddingProbeRoutes_WiresRoute(t *testing.T) {
	h := newHandler(nil, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	e := echo.New()
	g := e.Group("/api/admin")
	iamMW := func(_ string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	}
	h.RegisterEmbeddingProbeRoutes(g, iamMW)
	found := false
	for _, r := range e.Routes() {
		if r.Method == http.MethodPost && r.Path == "/api/admin/providers/:id/embedding-probe" {
			found = true
		}
	}
	if !found {
		t.Error("route POST /api/admin/providers/:id/embedding-probe not registered")
	}
}

// ProviderEmbeddingProbe — provider not found

func TestProviderEmbeddingProbe_ProviderNotFound_Returns404(t *testing.T) {
	mock, db := newMockStore(t)
	mock.ExpectQuery(`FROM "Provider"`).
		WithArgs("missing-id").
		WillReturnRows(pgxmock.NewRows(providerCols))

	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("missing-id")

	if err := h.ProviderEmbeddingProbe(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code = %d; want 404", rec.Code)
	}
}

// ProviderEmbeddingProbe — provider found but no embedding model

func TestProviderEmbeddingProbe_NoEmbeddingModel_Returns400(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()

	// GetProvider succeeds.
	mock.ExpectQuery(`FROM "Provider"`).
		WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows(providerCols).AddRow(makeProviderRow(now)...))

	// ListModelsByProvider returns only a chat model.
	mock.ExpectQuery(`FROM "Model"`).
		WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(makeModelRow(now)...))

	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("prov-1")

	if err := h.ProviderEmbeddingProbe(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil || errObj["type"] != "no_embedding_model" {
		t.Errorf("expected no_embedding_model error; got %v", body)
	}
}

// ProviderEmbeddingProbe — model list DB error

func TestProviderEmbeddingProbe_ModelListError_Returns500(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()

	mock.ExpectQuery(`FROM "Provider"`).
		WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows(providerCols).AddRow(makeProviderRow(now)...))

	mock.ExpectQuery(`FROM "Model"`).
		WithArgs("prov-1").
		WillReturnError(errors.New("db error"))

	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("prov-1")

	if err := h.ProviderEmbeddingProbe(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d; want 500", rec.Code)
	}
}

// ProviderEmbeddingProbe — AI Gateway unreachable (returns 200 + ok=false)

func TestProviderEmbeddingProbe_GatewayUnreachable_Returns200WithFalse(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()

	mock.ExpectQuery(`FROM "Provider"`).
		WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows(providerCols).AddRow(makeProviderRow(now)...))

	// Return an embedding model row.
	mock.ExpectQuery(`FROM "Model"`).
		WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(makeEmbeddingModelRow(now)...))

	// ListCredentials COUNT — return 0 so getFirstCredentialKey returns "".
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "Credential"`).
		WithArgs("prov-1", true).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`FROM "Credential" c`).
		WithArgs("prov-1", true, 1, 0).
		WillReturnRows(pgxmock.NewRows(credentialMetadataCols))

	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{
		AIGatewayURL: "http://127.0.0.1:1", // no listener
	})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("prov-1")

	if err := h.ProviderEmbeddingProbe(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200 (gateway unreachable → 200 + ok=false)", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["ok"] != false {
		t.Errorf("ok should be false on unreachable gateway; got %v", body)
	}
}

// ProviderEmbeddingProbe — AI Gateway responds with probe result

func TestProviderEmbeddingProbe_GatewayResponds_PassesThrough(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()

	mock.ExpectQuery(`FROM "Provider"`).
		WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows(providerCols).AddRow(makeProviderRow(now)...))

	mock.ExpectQuery(`FROM "Model"`).
		WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(makeEmbeddingModelRow(now)...))

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "Credential"`).
		WithArgs("prov-1", true).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`FROM "Credential" c`).
		WithArgs("prov-1", true, 1, 0).
		WillReturnRows(pgxmock.NewRows(credentialMetadataCols))

	gwResp := `{"ok":true,"providerId":"prov-1","modelId":"model-emb-1","modelName":"text-embedding-3-small","dimension":1536,"latencyMs":142,"promptTokens":3,"sampleEmbeddingFirst10":[0.1,0.2,0.3]}`
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(gwResp))
	}))
	defer gw.Close()

	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{AIGatewayURL: gw.URL})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("prov-1")

	if err := h.ProviderEmbeddingProbe(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["ok"] != true {
		t.Errorf("ok should be true; got %v", body)
	}
	if body["dimension"] != float64(1536) {
		t.Errorf("dimension = %v; want 1536", body["dimension"])
	}
}

// forwardEmbeddingProbe — direct unit test

func TestForwardEmbeddingProbe_GatewayUnreachable_Returns200WithFalse(t *testing.T) {
	h := newHandler(nil, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{
		AIGatewayURL: "http://127.0.0.1:1",
	})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")

	if err := h.forwardEmbeddingProbe(c, "p-1", "m-1", "Test Model",
		"text-embedding-3-small", "https://api.openai.com", "sk-x", 1536); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["ok"] != false {
		t.Errorf("ok should be false on unreachable; got %v", body)
	}
}

func TestForwardEmbeddingProbe_BadGatewayURL_Returns200WithFalse(t *testing.T) {
	h := newHandler(nil, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{
		AIGatewayURL: "://bad-url",
	})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")

	if err := h.forwardEmbeddingProbe(c, "p-1", "m-1", "Test Model",
		"text-embedding-3-small", "https://api.openai.com", "sk-x", 1536); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["ok"] != false {
		t.Errorf("ok should be false for bad gateway URL; got %v", body)
	}
}

func TestForwardEmbeddingProbe_GatewayResponds_PassesThrough(t *testing.T) {
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"dimension":384,"latencyMs":55}`))
	}))
	defer gw.Close()

	h := newHandler(nil, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{AIGatewayURL: gw.URL})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")

	if err := h.forwardEmbeddingProbe(c, "p-1", "m-1", "local-bge-small",
		"local-bge-small", "http://localhost:9001/v1", "", 384); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["ok"] != true {
		t.Errorf("ok should be true; got %v", body)
	}
}

// makeEmbeddingModelRow helper

// makeEmbeddingModelRow returns a model fixture row with type="embedding"
// and enabled=true. modelCols column order matches modelCols in helpers_test.go:
// id, code, name, description, providerId, providerModelId, type, features,
// inputPricePerMillion, outputPricePerMillion, maxContextTokens, maxOutputTokens,
// status, deprecationDate, replacedBy, aliases, enabled, createdAt, updatedAt.
func makeEmbeddingModelRow(now time.Time) []any {
	row := makeModelRow(now)
	// Clone the row so we don't mutate the original.
	clone := make([]any, len(row))
	copy(clone, row)
	// Override fields to describe an embedding model.
	// Index 1 = code, 2 = name, 6 = type.
	clone[0] = "model-emb-1"
	clone[1] = "text-embedding-3-small"
	clone[2] = "Text Embedding 3 Small"
	clone[6] = "embedding"
	return clone
}

// packages/control-plane/internal/ai/cache/handler/semanticcache_test.go —
// unit tests for the SemanticCacheHandler GET/PUT endpoints.
//
// Pattern mirrors governance/aiguard/handler/aiguard_test.go:
//   - stubSemanticStore: in-memory double for SemanticCacheStore
//   - fakeSemanticHub: captures InvalidateConfig calls
//   - semanticAuditSpy: captures audit emissions
//
// All tests use Echo's httptest pattern without requiring a live DB or Hub.
package cache_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	cache "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/cache/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/configstore"
)

// test doubles

// stubSemanticStore is an in-memory double for cache.SemanticCacheStore.
type stubSemanticStore struct {
	mu      sync.Mutex
	current *configstore.SemanticCacheConfigRow
	getErr  error
	saveErr error
	saved   *configstore.SaveInput
}

func (s *stubSemanticStore) Get(_ context.Context) (*configstore.SemanticCacheConfigRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.current == nil {
		now := time.Now().UTC()
		return &configstore.SemanticCacheConfigRow{
			ID:             "singleton",
			RedisIndexName: "nexus:semantic-cache:v1",
			Enabled:        false,
			UpdatedAt:      now,
		}, nil
	}
	return s.current, nil
}

func (s *stubSemanticStore) Save(_ context.Context, in configstore.SaveInput) (*configstore.SemanticCacheConfigRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return nil, s.saveErr
	}
	s.saved = &in
	fp := ""
	if in.EmbeddingProviderID != nil && in.EmbeddingModelID != nil && in.EmbeddingDimension != nil {
		fp = "computed-fp"
	}
	return &configstore.SemanticCacheConfigRow{
		ID:                   "singleton",
		EmbeddingProviderID:  in.EmbeddingProviderID,
		EmbeddingModelID:     in.EmbeddingModelID,
		EmbeddingDimension:   in.EmbeddingDimension,
		EmbeddingFingerprint: fp,
		RedisIndexName:       "nexus:semantic-cache:v1",
		Enabled:              in.Enabled,
		UpdatedAt:            time.Now().UTC(),
	}, nil
}

// fakeSemanticHub captures NotifyConfigChange calls.
type fakeSemanticHub struct {
	mu            sync.Mutex
	hits          int
	lastThingType string
	lastConfigKey string
	lastState     any
	err           error // when set, NotifyConfigChange fails (drives the 502 path)
}

func (f *fakeSemanticHub) NotifyConfigChange(_ context.Context, req hub.ConfigChangeRequest) (*hub.ConfigChangeResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits++
	f.lastThingType = req.ThingType
	f.lastConfigKey = req.ConfigKey
	f.lastState = req.State
	if f.err != nil {
		return nil, f.err
	}
	return &hub.ConfigChangeResponse{OK: true, Version: int64(f.hits)}, nil
}

// semanticAuditSpy captures LogObserved calls.
type semanticAuditSpy struct {
	mu    sync.Mutex
	calls [][]byte
}

func (a *semanticAuditSpy) Publish(context.Context, string, []byte) error { return nil }
func (a *semanticAuditSpy) Enqueue(_ context.Context, _ string, data []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	copied := make([]byte, len(data))
	copy(copied, data)
	a.calls = append(a.calls, copied)
	return nil
}
func (a *semanticAuditSpy) Close() error { return nil }

type capturedEntry struct {
	Action     string         `json:"action"`
	EntityType string         `json:"entityType"`
	EntityID   string         `json:"entityId"`
	AfterState map[string]any `json:"afterState"`
}

func (a *semanticAuditSpy) captured() []capturedEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]capturedEntry, 0, len(a.calls))
	for _, raw := range a.calls {
		var c capturedEntry
		_ = json.Unmarshal(raw, &c)
		out = append(out, c)
	}
	return out
}

func newSemanticTestAudit(spy *semanticAuditSpy) *audit.Writer {
	return audit.NewWriter(spy, "audit", slog.Default())
}

func newSemanticHandler(t *testing.T, store cache.SemanticCacheStore, hub cache.SemanticCacheHubInvalidator, aw *audit.Writer) *cache.SemanticCacheHandler {
	t.Helper()
	return cache.NewSemanticCacheHandler(cache.SemanticCacheHandlerDeps{
		Store:  store,
		Hub:    hub,
		Audit:  aw,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// GET tests

// TestSemanticGet_ReturnsCurrent locks the happy-path GET: the handler
// forwards the store's current singleton verbatim as JSON with 200.
func TestSemanticGet_ReturnsCurrent(t *testing.T) {
	e := echo.New()
	store := &stubSemanticStore{}
	h := newSemanticHandler(t, store, nil, nil)
	e.GET("/api/admin/semantic-cache/config", h.GetConfig)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/semantic-cache/config", nil)
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code: %d body: %s", rec.Code, rec.Body.String())
	}
	var got configstore.SemanticCacheConfigRow
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != "singleton" {
		t.Errorf("ID: %q", got.ID)
	}
	if got.RedisIndexName != "nexus:semantic-cache:v1" {
		t.Errorf("RedisIndexName: %q", got.RedisIndexName)
	}
}

// TestSemanticGet_StoreError_500 maps a store Get failure to 500.
func TestSemanticGet_StoreError_500(t *testing.T) {
	e := echo.New()
	store := &stubSemanticStore{getErr: errors.New("pg down")}
	h := newSemanticHandler(t, store, nil, nil)
	e.GET("/cfg", h.GetConfig)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/cfg", nil)
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "pg down") {
		t.Errorf("body missing store error: %s", rec.Body.String())
	}
}

// PUT tests

// TestSemanticPut_HappyPath_WithModel locks the full PUT happy path:
// valid (provider, model, dimension, enabled) → 200 + hub invalidated + audit.
func TestSemanticPut_HappyPath_WithModel(t *testing.T) {
	e := echo.New()
	store := &stubSemanticStore{}
	hub := &fakeSemanticHub{}
	spy := &semanticAuditSpy{}
	h := newSemanticHandler(t, store, hub, newSemanticTestAudit(spy))
	e.PUT("/cfg", h.PutConfig)

	body := map[string]any{
		"embeddingProviderId": "prov-uuid",
		"embeddingModelId":    "model-uuid",
		"embeddingDimension":  1536,
		"enabled":             true,
	}
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, "/cfg", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code: %d body: %s", rec.Code, rec.Body.String())
	}
	if store.saved == nil {
		t.Fatal("Save not called")
	}
	if hub.hits != 1 {
		t.Errorf("hub invalidation: want 1 hit, got %d", hub.hits)
	}
	if hub.lastThingType != "ai-gateway" {
		t.Errorf("hub thingType: %q", hub.lastThingType)
	}
	if hub.lastConfigKey != "semantic_cache.config" {
		t.Errorf("hub configKey: %q (want semantic_cache.config)", hub.lastConfigKey)
	}
	// Verify audit emission.
	msgs := spy.captured()
	if len(msgs) != 1 {
		t.Fatalf("want 1 audit msg, got %d", len(msgs))
	}
	if msgs[0].Action != "update" || msgs[0].EntityType != "semantic-cache" || msgs[0].EntityID != "singleton" {
		t.Errorf("audit: action=%q entityType=%q entityId=%q",
			msgs[0].Action, msgs[0].EntityType, msgs[0].EntityID)
	}
}

// A dropped Hub push escalates to 502 (propagation_error) instead of silently
// returning 200, so the admin sees the failure immediately while the DB write
// still commits (Save was called). The configreconcile watch for this key
// (F-0102/F-0345) additionally heals it within one cycle.
func TestSemanticPut_HubError_Returns502(t *testing.T) {
	e := echo.New()
	store := &stubSemanticStore{}
	hub := &fakeSemanticHub{err: errors.New("hub down")}
	spy := &semanticAuditSpy{}
	h := newSemanticHandler(t, store, hub, newSemanticTestAudit(spy))
	e.PUT("/cfg", h.PutConfig)

	body := map[string]any{
		"embeddingProviderId": "prov-uuid",
		"embeddingModelId":    "model-uuid",
		"embeddingDimension":  1536,
		"enabled":             true,
	}
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, "/cfg", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "propagation_error") {
		t.Errorf("missing propagation_error envelope; body=%s", rec.Body.String())
	}
	if store.saved == nil {
		t.Error("DB write should still have committed before the push failure")
	}
	// No success audit row on a propagation failure (matches cache.go).
	if msgs := spy.captured(); len(msgs) != 0 {
		t.Errorf("want 0 audit msgs on 502, got %d", len(msgs))
	}
}

// TestSemanticPut_DisableOnly_NoModelRequired locks the kill-switch path:
// enabled=false without provider/model is valid — fleet-wide disable.
func TestSemanticPut_DisableOnly_NoModelRequired(t *testing.T) {
	e := echo.New()
	store := &stubSemanticStore{}
	h := newSemanticHandler(t, store, nil, nil)
	e.PUT("/cfg", h.PutConfig)

	body := `{"enabled":false}`
	req := httptest.NewRequest(http.MethodPut, "/cfg", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code: %d body: %s", rec.Code, rec.Body.String())
	}
	if store.saved == nil {
		t.Fatal("Save not called")
	}
	if store.saved.Enabled {
		t.Error("Enabled should be false")
	}
}

// TestSemanticPut_EnableWithoutModel_400 locks the validation: enabling
// semantic cache without (providerID + modelID) is a 400.
func TestSemanticPut_EnableWithoutModel_400(t *testing.T) {
	e := echo.New()
	h := newSemanticHandler(t, &stubSemanticStore{}, nil, nil)
	e.PUT("/cfg", h.PutConfig)

	body := `{"enabled":true}`
	req := httptest.NewRequest(http.MethodPut, "/cfg", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "embeddingProviderId") {
		t.Errorf("body missing field reference: %s", rec.Body.String())
	}
}

// TestSemanticPut_ModelSetNoDimension_NoDefault_400 covers the new contract:
// a nil dimension is allowed at the handler (the store derives the model's
// default_dimension), but when the model declares no default the store returns
// ErrEmbeddingDimensionRequired — which the handler maps to 400, not 500.
func TestSemanticPut_ModelSetNoDimension_NoDefault_400(t *testing.T) {
	e := echo.New()
	store := &stubSemanticStore{saveErr: configstore.ErrEmbeddingDimensionRequired}
	h := newSemanticHandler(t, store, nil, nil)
	e.PUT("/cfg", h.PutConfig)

	body := `{"embeddingProviderId":"prov","embeddingModelId":"model","enabled":true}`
	req := httptest.NewRequest(http.MethodPut, "/cfg", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "dimension") {
		t.Errorf("body missing dimension reference: %s", rec.Body.String())
	}
}

// TestSemanticPut_UnsupportedDimension_400 maps the store's capability
// validation failure (dimension not in the model's supported_dimensions) to 400.
func TestSemanticPut_UnsupportedDimension_400(t *testing.T) {
	e := echo.New()
	store := &stubSemanticStore{saveErr: configstore.ErrUnsupportedEmbeddingDimension}
	h := newSemanticHandler(t, store, nil, nil)
	e.PUT("/cfg", h.PutConfig)

	body := `{"embeddingProviderId":"prov","embeddingModelId":"model","embeddingDimension":3072,"enabled":true}`
	req := httptest.NewRequest(http.MethodPut, "/cfg", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestSemanticPut_ZeroDimension_400 locks the positive-dimension guard:
// dimension=0 is treated the same as missing — 400.
func TestSemanticPut_ZeroDimension_400(t *testing.T) {
	e := echo.New()
	h := newSemanticHandler(t, &stubSemanticStore{}, nil, nil)
	e.PUT("/cfg", h.PutConfig)

	body := `{"embeddingProviderId":"prov","embeddingModelId":"model","embeddingDimension":0,"enabled":true}`
	req := httptest.NewRequest(http.MethodPut, "/cfg", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestSemanticPut_MalformedJSON_400 returns 400 with malformed_json classifier.
func TestSemanticPut_MalformedJSON_400(t *testing.T) {
	e := echo.New()
	h := newSemanticHandler(t, &stubSemanticStore{}, nil, nil)
	e.PUT("/cfg", h.PutConfig)

	req := httptest.NewRequest(http.MethodPut, "/cfg", bytes.NewBufferString(`{not json`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "malformed_json") {
		t.Errorf("missing malformed_json: %s", rec.Body.String())
	}
}

// TestSemanticPut_SaveError_500 maps a store Save failure to 500.
func TestSemanticPut_SaveError_500(t *testing.T) {
	e := echo.New()
	store := &stubSemanticStore{saveErr: errors.New("save boom")}
	h := newSemanticHandler(t, store, nil, nil)
	e.PUT("/cfg", h.PutConfig)

	body := `{"enabled":false}`
	req := httptest.NewRequest(http.MethodPut, "/cfg", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "save boom") {
		t.Errorf("body missing save error: %s", rec.Body.String())
	}
}

// TestSemanticPut_HubNil_NoPanic locks the nil-Hub guard: when Hub is not
// wired the PUT must still succeed (200) without panicking.
func TestSemanticPut_HubNil_NoPanic(t *testing.T) {
	e := echo.New()
	store := &stubSemanticStore{}
	h := newSemanticHandler(t, store, nil /* hub */, nil)
	e.PUT("/cfg", h.PutConfig)

	body := `{"enabled":false}`
	req := httptest.NewRequest(http.MethodPut, "/cfg", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestSemanticPut_AuditNotEmittedOnValidationError locks the success-only
// audit contract: a 400 must not emit an audit event.
func TestSemanticPut_AuditNotEmittedOnValidationError(t *testing.T) {
	e := echo.New()
	spy := &semanticAuditSpy{}
	h := newSemanticHandler(t, &stubSemanticStore{}, nil, newSemanticTestAudit(spy))
	e.PUT("/cfg", h.PutConfig)

	// Trigger a validation error: enabled=true without model.
	body := `{"enabled":true}`
	req := httptest.NewRequest(http.MethodPut, "/cfg", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
	if n := len(spy.captured()); n != 0 {
		t.Errorf("expected no audit msgs on validation error, got %d", n)
	}
}

// TestRegisterSemanticCacheRoutes_MountsTwo confirms the handler wires
// exactly the two expected endpoints.
func TestRegisterSemanticCacheRoutes_MountsTwo(t *testing.T) {
	h := newSemanticHandler(t, &stubSemanticStore{}, nil, nil)
	e := echo.New()
	g := e.Group("/api/admin")
	noop := func(string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	}
	h.RegisterSemanticCacheRoutes(g, noop)

	want := map[string]string{
		"GET /api/admin/semantic-cache/config": "",
		"PUT /api/admin/semantic-cache/config": "",
	}
	for _, r := range e.Routes() {
		key := r.Method + " " + r.Path
		delete(want, key)
	}
	if len(want) > 0 {
		t.Fatalf("missing routes: %v", want)
	}
}

// TestNewSemanticCacheHandler_NilLoggerDefaults locks the nil-logger guard:
// an empty Logger gets backed by slog.Default() so the handler is safe to
// call without an explicit logger.
func TestNewSemanticCacheHandler_NilLoggerDefaults(t *testing.T) {
	h := cache.NewSemanticCacheHandler(cache.SemanticCacheHandlerDeps{
		Store: &stubSemanticStore{},
	})
	if h == nil {
		t.Fatal("handler is nil")
	}
	// Best-effort: GET must not panic with a defaulted logger.
	e := echo.New()
	e.GET("/cfg", h.GetConfig)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/cfg", nil)
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code: %d", rec.Code)
	}
}

// TestSemanticPut_HappyPath_DisabledWithModel locks the path where a model
// is set but enabled=false — should be 200 (pre-configure before enabling).
func TestSemanticPut_HappyPath_DisabledWithModel(t *testing.T) {
	e := echo.New()
	store := &stubSemanticStore{}
	h := newSemanticHandler(t, store, nil, nil)
	e.PUT("/cfg", h.PutConfig)

	body := map[string]any{
		"embeddingProviderId": "prov-uuid",
		"embeddingModelId":    "model-uuid",
		"embeddingDimension":  768,
		"enabled":             false,
	}
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, "/cfg", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code: %d body: %s", rec.Code, rec.Body.String())
	}
	if store.saved == nil {
		t.Fatal("Save not called")
	}
	if store.saved.Enabled {
		t.Error("Enabled should be false")
	}
	if store.saved.EmbeddingProviderID == nil || *store.saved.EmbeddingProviderID != "prov-uuid" {
		t.Errorf("EmbeddingProviderID: %v", store.saved.EmbeddingProviderID)
	}
}

// TestSemanticPut_EnableWithEmptyStringProvider_400 locks the empty-string
// provider guard: enabled=true with "" providerID is a 400 (same as nil).
func TestSemanticPut_EnableWithEmptyStringProvider_400(t *testing.T) {
	e := echo.New()
	h := newSemanticHandler(t, &stubSemanticStore{}, nil, nil)
	e.PUT("/cfg", h.PutConfig)

	body := `{"embeddingProviderId":"","embeddingModelId":"model","embeddingDimension":1536,"enabled":true}`
	req := httptest.NewRequest(http.MethodPut, "/cfg", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", rec.Code, rec.Body.String())
	}
}

package aiguard_test

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

	"github.com/labstack/echo/v4"

	aiguard "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/governance/aiguard/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/configstore"
)

// stubStore is an in-memory double for aiguard.ConfigStore. Keeps the
// last Save payload so tests can assert on the full row that would
// hit Postgres.
type stubStore struct {
	saved *configstore.AIGuardConfig
	out   *configstore.AIGuardConfig
}

func (s *stubStore) Load(_ context.Context) (*configstore.AIGuardConfig, error) {
	if s.out == nil {
		s.out = &configstore.AIGuardConfig{
			ID:              "singleton",
			BackendMode:     "configured_provider",
			TimeoutMs:       5000,
			CacheTTLSeconds: 600,
			PromptTemplate:  "default",
		}
	}
	return s.out, nil
}

func (s *stubStore) Save(_ context.Context, cfg *configstore.AIGuardConfig) error {
	s.saved = cfg
	return nil
}

// auditSpy captures the JSON-encoded enqueue stream.
type auditSpy struct {
	mu    sync.Mutex
	calls [][]byte
}

func (a *auditSpy) Publish(context.Context, string, []byte) error { return nil }
func (a *auditSpy) Enqueue(_ context.Context, _ string, data []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	copied := make([]byte, len(data))
	copy(copied, data)
	a.calls = append(a.calls, copied)
	return nil
}
func (a *auditSpy) Close() error { return nil }

type captured struct {
	Action      string         `json:"action"`
	EntityType  string         `json:"entityType"`
	EntityID    string         `json:"entityId"`
	AfterState  map[string]any `json:"afterState"`
	BeforeState map[string]any `json:"beforeState"`
}

func (a *auditSpy) captured() []captured {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]captured, 0, len(a.calls))
	for _, raw := range a.calls {
		var c captured
		_ = json.Unmarshal(raw, &c)
		out = append(out, c)
	}
	return out
}

func newTestAudit(spy *auditSpy) *audit.Writer {
	return audit.NewWriter(spy, "audit", slog.Default())
}

func newHandler(t *testing.T, store aiguard.ConfigStore, hub aiguard.HubConfigChanger, disp aiguard.DryRunDispatcher, aud *audit.Writer) *aiguard.Handler {
	t.Helper()
	return aiguard.New(aiguard.Deps{
		Store:      store,
		Hub:        hub,
		Dispatcher: disp,
		Audit:      aud,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// TestGetConfig_ReturnsCurrent locks the happy-path GET: the handler
// forwards the store's current singleton verbatim as JSON.
func TestGetConfig_ReturnsCurrent(t *testing.T) {
	e := echo.New()
	s := &stubStore{}
	h := newHandler(t, s, nil, nil, nil)
	e.GET("/api/admin/ai-guard/config", h.GetConfig)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/ai-guard/config", nil)
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code: %d body: %s", rec.Code, rec.Body.String())
	}
	var got configstore.AIGuardConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.BackendMode != "configured_provider" {
		t.Errorf("backend_mode: %q", got.BackendMode)
	}
}

// TestPutConfig_RecomputesFingerprint locks the key server-side
// invariant: the handler always recomputes BackendFingerprint — the
// admin UI cannot bypass this by sending a stale value.
func TestPutConfig_RecomputesFingerprint(t *testing.T) {
	e := echo.New()
	s := &stubStore{}
	h := newHandler(t, s, nil, nil, nil)
	e.PUT("/api/admin/ai-guard/config", h.PutConfig)

	payload := map[string]any{
		"backendMode":     "external_url",
		"externalUrl":     "https://j.example.com/v1",
		"modelId":         "gpt-4o-mini",
		"promptTemplate":  "custom prompt",
		"timeoutMs":       4000,
		"cacheTtlSeconds": 120,
	}
	buf, _ := json.Marshal(payload)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/admin/ai-guard/config", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code: %d body: %s", rec.Code, rec.Body.String())
	}
	if s.saved == nil {
		t.Fatal("Save not called")
	}
	if s.saved.BackendFingerprint == "" {
		t.Errorf("fingerprint not computed")
	}
	if s.saved.BackendMode != "external_url" {
		t.Errorf("backend_mode: %q", s.saved.BackendMode)
	}
}

// TestPutConfig_RejectsMissingProviderIDInConfiguredMode locks the
// configured_provider mode validation: both providerId and modelId
// are required — missing either is a 400.
func TestPutConfig_RejectsMissingProviderIDInConfiguredMode(t *testing.T) {
	e := echo.New()
	s := &stubStore{}
	h := newHandler(t, s, nil, nil, nil)
	e.PUT("/api/admin/ai-guard/config", h.PutConfig)

	payload := `{"backendMode":"configured_provider","promptTemplate":"x"}`
	req := httptest.NewRequest(http.MethodPut, "/api/admin/ai-guard/config", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestPutConfig_RejectsMissingURLInExternalMode locks the external_url
// mode validation: externalUrl is required — empty is a 400.
func TestPutConfig_RejectsMissingURLInExternalMode(t *testing.T) {
	e := echo.New()
	s := &stubStore{}
	h := newHandler(t, s, nil, nil, nil)
	e.PUT("/api/admin/ai-guard/config", h.PutConfig)

	payload := `{"backendMode":"external_url","promptTemplate":"x"}`
	req := httptest.NewRequest(http.MethodPut, "/api/admin/ai-guard/config", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestPutConfig_RejectsUnknownBackendMode covers the default arm of
// the BackendMode switch.
func TestPutConfig_RejectsUnknownBackendMode(t *testing.T) {
	e := echo.New()
	s := &stubStore{}
	h := newHandler(t, s, nil, nil, nil)
	e.PUT("/api/admin/ai-guard/config", h.PutConfig)

	payload := `{"backendMode":"wat","promptTemplate":"x"}`
	req := httptest.NewRequest(http.MethodPut, "/api/admin/ai-guard/config", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestPutConfig_RejectsEmptyPromptTemplate locks the mandatory-template
// rule: missing prompt template fails fast before the store is
// touched.
func TestPutConfig_RejectsEmptyPromptTemplate(t *testing.T) {
	e := echo.New()
	s := &stubStore{}
	h := newHandler(t, s, nil, nil, nil)
	e.PUT("/api/admin/ai-guard/config", h.PutConfig)

	payload := `{"backendMode":"external_url","externalUrl":"https://x"}`
	req := httptest.NewRequest(http.MethodPut, "/api/admin/ai-guard/config", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
}

// stubDispatcher records the last Dispatch call and returns canned
// response/err.
type stubDispatcher struct {
	got  aiguard.DryRunRequest
	resp *aiguard.DryRunResponse
	err  error
}

func (s *stubDispatcher) Dispatch(_ context.Context, req aiguard.DryRunRequest) (*aiguard.DryRunResponse, error) {
	s.got = req
	return s.resp, s.err
}

// TestDryRun_Proxies locks the happy path: the handler hands the
// bound request to the dispatcher and returns {request, response}
// so the UI can render them side by side.
func TestDryRun_Proxies(t *testing.T) {
	e := echo.New()
	store := &stubStore{}
	dispatcher := &stubDispatcher{resp: &aiguard.DryRunResponse{
		Decision: "approve",
		Metadata: aiguard.DryRunMetadata{CacheHit: false, JudgeLatencyMs: 120},
	}}
	h := newHandler(t, store, nil, dispatcher, nil)
	e.POST("/api/admin/ai-guard/dry-run", h.DryRun)

	payload := `{"detector_type":"prompt_injection","content":"test","context":{"ingress":"AI_GATEWAY"}}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/ai-guard/dry-run", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code: %d body: %s", rec.Code, rec.Body.String())
	}
	if dispatcher.got.DetectorType != "prompt_injection" {
		t.Errorf("dispatcher got: %+v", dispatcher.got)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp, _ := body["response"].(map[string]any)
	if resp == nil || resp["decision"] != "approve" {
		t.Errorf("response: %+v", body)
	}
}

// TestDryRun_NoDispatcher_503 locks the degraded-mode response when
// the dispatcher is not wired.
func TestDryRun_NoDispatcher_503(t *testing.T) {
	e := echo.New()
	store := &stubStore{}
	h := newHandler(t, store, nil, nil, nil)
	e.POST("/api/admin/ai-guard/dry-run", h.DryRun)

	payload := `{"detector_type":"x","content":"y"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/ai-guard/dry-run", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rec.Code)
	}
}

// TestDryRun_Missing_400 locks the request validation.
func TestDryRun_Missing_400(t *testing.T) {
	e := echo.New()
	store := &stubStore{}
	dispatcher := &stubDispatcher{resp: &aiguard.DryRunResponse{Decision: "approve"}}
	h := newHandler(t, store, nil, dispatcher, nil)
	e.POST("/api/admin/ai-guard/dry-run", h.DryRun)

	payload := `{"content":"hi"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/ai-guard/dry-run", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
}

// TestDryRun_DispatcherError_502 maps an upstream dispatcher failure
// to a 502 with the request echoed back.
func TestDryRun_DispatcherError_502(t *testing.T) {
	e := echo.New()
	store := &stubStore{}
	dispatcher := &stubDispatcher{err: io.ErrUnexpectedEOF}
	h := newHandler(t, store, nil, dispatcher, nil)
	e.POST("/api/admin/ai-guard/dry-run", h.DryRun)

	payload := `{"detector_type":"x","content":"y"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/ai-guard/dry-run", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d", rec.Code)
	}
}

// TestHTTPDispatcher_PostsClassify locks the live dispatcher's wire
// format: POST /v1/ai-guard/classify with the X-RS-Token service-token
// header, body is the request verbatim, and the parsed response
// mirrors aiguard.Response.
func TestHTTPDispatcher_PostsClassify(t *testing.T) {
	var gotPath, gotToken, gotCT, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotToken = r.Header.Get("X-RS-Token")
		gotCT = r.Header.Get("Content-Type")
		buf, _ := io.ReadAll(r.Body)
		gotBody = string(buf)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"decision":"approve","metadata":{"judge_latency_ms":42,"cache_hit":false,"backend_mode":"external_url"}}`))
	}))
	defer srv.Close()

	d := &aiguard.HTTPDispatcher{
		BaseURL:    srv.URL,
		Token:      "test-token",
		HTTPClient: srv.Client(),
	}
	resp, err := d.Dispatch(context.Background(), aiguard.DryRunRequest{
		DetectorType: "prompt_injection",
		Content:      "ignore previous",
		Context:      aiguard.DryRunContext{Ingress: "AI_GATEWAY"},
	})
	if err != nil {
		t.Fatalf("Dispatch error: %v", err)
	}
	if resp.Decision != "approve" {
		t.Errorf("decision: %q", resp.Decision)
	}
	if resp.Metadata.JudgeLatencyMs != 42 {
		t.Errorf("latency: %d", resp.Metadata.JudgeLatencyMs)
	}
	if gotPath != "/v1/ai-guard/classify" {
		t.Errorf("path: %q", gotPath)
	}
	if gotToken != "test-token" {
		t.Errorf("token: %q", gotToken)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type: %q", gotCT)
	}
	if !bytes.Contains([]byte(gotBody), []byte(`"detector_type":"prompt_injection"`)) {
		t.Errorf("body missing detector_type: %s", gotBody)
	}
}

// TestHTTPDispatcher_UpstreamError surfaces a non-2xx upstream
// response as a Go error so the admin handler maps it to 502.
func TestHTTPDispatcher_UpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"backend_unavailable"}`))
	}))
	defer srv.Close()

	d := &aiguard.HTTPDispatcher{
		BaseURL:    srv.URL,
		Token:      "t",
		HTTPClient: srv.Client(),
	}
	_, err := d.Dispatch(context.Background(), aiguard.DryRunRequest{DetectorType: "x", Content: "y"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// TestAudit_PutConfig_EmitsOnSuccess locks the admin-audit contract:
// a successful PUT publishes exactly one audit event.
func TestAudit_PutConfig_EmitsOnSuccess(t *testing.T) {
	spy := &auditSpy{}
	store := &stubStore{}
	h := newHandler(t, store, nil, nil, newTestAudit(spy))
	e := echo.New()
	e.PUT("/api/admin/ai-guard/config", h.PutConfig)

	payload := map[string]any{
		"backendMode":     "external_url",
		"externalUrl":     "https://j.example.com/v1",
		"modelId":         "gpt-4o-mini",
		"promptTemplate":  "custom prompt",
		"timeoutMs":       4000,
		"cacheTtlSeconds": 120,
	}
	buf, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPut, "/api/admin/ai-guard/config", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code: %d body: %s", rec.Code, rec.Body.String())
	}
	msgs := spy.captured()
	if len(msgs) != 1 {
		t.Fatalf("want 1 audit msg, got %d", len(msgs))
	}
	if msgs[0].Action != "update" || msgs[0].EntityType != "ai-guard-config" || msgs[0].EntityID != "singleton" {
		t.Errorf("audit entry: action=%q entityType=%q entityId=%q", msgs[0].Action, msgs[0].EntityType, msgs[0].EntityID)
	}
	if msgs[0].AfterState == nil {
		t.Errorf("after state missing")
	}
}

// TestAudit_PutConfig_NoEmitOnValidationError locks the success-only
// contract: a 400 validation failure must not publish an audit event.
func TestAudit_PutConfig_NoEmitOnValidationError(t *testing.T) {
	spy := &auditSpy{}
	store := &stubStore{}
	h := newHandler(t, store, nil, nil, newTestAudit(spy))
	e := echo.New()
	e.PUT("/api/admin/ai-guard/config", h.PutConfig)

	payload := `{"backendMode":"external_url","promptTemplate":"x"}`
	req := httptest.NewRequest(http.MethodPut, "/api/admin/ai-guard/config", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
	if got := len(spy.captured()); got != 0 {
		t.Errorf("expected no audit msgs on validation error, got %d", got)
	}
}

// errStore wraps stubStore + injectable Load/Save errors.
type errStore struct {
	stubStore
	loadErr error
	saveErr error
}

func (s *errStore) Load(ctx context.Context) (*configstore.AIGuardConfig, error) {
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	return s.stubStore.Load(ctx)
}

func (s *errStore) Save(ctx context.Context, cfg *configstore.AIGuardConfig) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	return s.stubStore.Save(ctx, cfg)
}

// fakeHub captures InvalidateConfig calls. Mirrors the hooks /
// interception fakeHub pattern (Category B fire-and-forget) so the
// aiguard handler's invalidation branch is exercised without a real
// Hub WebSocket.
type fakeHub struct {
	mu            sync.Mutex
	hits          int
	lastThingType string
	lastConfigKey string
}

func (f *fakeHub) InvalidateConfig(_ context.Context, thingType, configKey string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits++
	f.lastThingType = thingType
	f.lastConfigKey = configKey
}

// TestNew_NilLoggerDefaults locks the construction-time nil-logger
// guard: an empty Deps.Logger gets backed by slog.Default() so the
// handler is safe to call without an explicit logger.
func TestNew_NilLoggerDefaults(t *testing.T) {
	h := aiguard.New(aiguard.Deps{Store: &stubStore{}})
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

// TestGetConfig_StoreError maps a store Load failure to 500.
func TestGetConfig_StoreError(t *testing.T) {
	e := echo.New()
	s := &errStore{loadErr: errors.New("pg down")}
	h := newHandler(t, s, nil, nil, nil)
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

// TestPutConfig_MalformedJSON returns 400 with the malformed_json
// classifier so the admin UI can distinguish parser failures from
// semantic validation failures.
func TestPutConfig_MalformedJSON(t *testing.T) {
	e := echo.New()
	h := newHandler(t, &stubStore{}, nil, nil, nil)
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

// TestPutConfig_DefaultsTimeoutAndClampsTTL locks both auto-correct
// branches: TimeoutMs<=0 → 30000, CacheTTLSeconds<0 → 0.
func TestPutConfig_DefaultsTimeoutAndClampsTTL(t *testing.T) {
	e := echo.New()
	s := &stubStore{}
	h := newHandler(t, s, nil, nil, nil)
	e.PUT("/cfg", h.PutConfig)
	payload := `{"backendMode":"external_url","externalUrl":"https://j","promptTemplate":"p","timeoutMs":0,"cacheTtlSeconds":-5}`
	req := httptest.NewRequest(http.MethodPut, "/cfg", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code: %d body: %s", rec.Code, rec.Body.String())
	}
	if s.saved == nil {
		t.Fatal("Save not called")
	}
	if s.saved.TimeoutMs != 30000 {
		t.Errorf("timeout: %d (want 30000 default)", s.saved.TimeoutMs)
	}
	if s.saved.CacheTTLSeconds != 0 {
		t.Errorf("cache_ttl: %d (want 0 clamped)", s.saved.CacheTTLSeconds)
	}
}

// TestPutConfig_ConfiguredProvider_FingerprintUsesProviderID drives
// the configured_provider branch through to fingerprint computation
// — exercises the `in.ProviderID != nil` providerOrURL assignment.
func TestPutConfig_ConfiguredProvider_FingerprintUsesProviderID(t *testing.T) {
	e := echo.New()
	s := &stubStore{}
	h := newHandler(t, s, nil, nil, nil)
	e.PUT("/cfg", h.PutConfig)
	payload := `{"backendMode":"configured_provider","providerId":"prov-1","modelId":"m-1","promptTemplate":"t"}`
	req := httptest.NewRequest(http.MethodPut, "/cfg", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code: %d body: %s", rec.Code, rec.Body.String())
	}
	if s.saved == nil || s.saved.BackendFingerprint == "" {
		t.Fatal("fingerprint not stamped")
	}
	if s.saved.ProviderID == nil || *s.saved.ProviderID != "prov-1" {
		t.Errorf("provider id lost on save")
	}
}

// TestPutConfig_SaveError maps a store Save failure to 500.
func TestPutConfig_SaveError(t *testing.T) {
	e := echo.New()
	s := &errStore{saveErr: errors.New("save boom")}
	h := newHandler(t, s, nil, nil, nil)
	e.PUT("/cfg", h.PutConfig)
	payload := `{"backendMode":"external_url","externalUrl":"https://x","promptTemplate":"p"}`
	req := httptest.NewRequest(http.MethodPut, "/cfg", bytes.NewBufferString(payload))
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

// TestPutConfig_HubNotified locks the success path through the hub
// invalidation branch.
func TestPutConfig_HubNotified(t *testing.T) {
	e := echo.New()
	s := &stubStore{}
	hub := &fakeHub{}
	h := newHandler(t, s, hub, nil, nil)

	e.PUT("/cfg", h.PutConfig)

	payload := `{"backendMode":"external_url","externalUrl":"https://x","promptTemplate":"p"}`
	req := httptest.NewRequest(http.MethodPut, "/cfg", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "10.0.0.5")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code: %d body: %s", rec.Code, rec.Body.String())
	}
	if hub.hits != 1 {
		t.Fatalf("hub hits: %d (want 1)", hub.hits)
	}
	if hub.lastThingType != "ai-gateway" || hub.lastConfigKey != "ai_guard" {
		t.Errorf("hub invalidation shape: thingType=%q configKey=%q",
			hub.lastThingType, hub.lastConfigKey)
	}
}

// TestPutConfig_HubInvalidateAlwaysFires locks the contract that the
// PUT always fires InvalidateConfig (fire-and-forget) and the 200
// success path is independent of any Hub-side error — the row write
// has already succeeded; the reconcile job recovers within 60s if
// Hub was transiently unreachable.
func TestPutConfig_HubInvalidateAlwaysFires(t *testing.T) {
	e := echo.New()
	s := &stubStore{}
	hub := &fakeHub{}
	h := newHandler(t, s, hub, nil, nil)
	e.PUT("/cfg", h.PutConfig)
	payload := `{"backendMode":"external_url","externalUrl":"https://x","promptTemplate":"p"}`
	req := httptest.NewRequest(http.MethodPut, "/cfg", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if hub.hits != 1 {
		t.Errorf("hub not called: %d", hub.hits)
	}
}

// TestConfigAuditSummary_NilReturnsNil locks the nil-input guard.
// Reached via PutConfig: when Load returns (nil, err) the "before"
// pointer stays nil and the `before != nil` branch in PutConfig skips
// configAuditSummary — so we cover the nil arm directly through a
// PUT against an empty store (Load returns synthesized config) by
// instead verifying through the public surface: a stubStore where Load
// returns nil + nil error. The aiguard package exposes configAuditSummary
// only through PutConfig's BeforeState. We exercise the nil-arm via a
// store that returns (nil, nil).
func TestConfigAuditSummary_NilArmViaPut(t *testing.T) {
	e := echo.New()
	// Custom store: Load returns (nil, nil) — exercises both the
	// `before, _ := store.Load(...)` site getting nil AND the
	// configAuditSummary nil-check via BeforeState being skipped.
	s := &nilLoadStore{}
	spy := &auditSpy{}
	h := newHandler(t, s, nil, nil, newTestAudit(spy))
	e.PUT("/cfg", h.PutConfig)
	payload := `{"backendMode":"external_url","externalUrl":"https://x","promptTemplate":"p"}`
	req := httptest.NewRequest(http.MethodPut, "/cfg", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code: %d body: %s", rec.Code, rec.Body.String())
	}
	msgs := spy.captured()
	if len(msgs) != 1 {
		t.Fatalf("want 1 audit msg, got %d", len(msgs))
	}
	if msgs[0].BeforeState != nil {
		t.Errorf("BeforeState should be nil when Load returns nil; got %+v", msgs[0].BeforeState)
	}
	if msgs[0].AfterState == nil {
		t.Errorf("AfterState must be populated")
	}
}

// nilLoadStore returns (nil, nil) from Load — exercises the "no prior
// row" path where BeforeState audit summary stays nil.
type nilLoadStore struct {
	saved *configstore.AIGuardConfig
}

func (n *nilLoadStore) Load(_ context.Context) (*configstore.AIGuardConfig, error) {
	return nil, nil
}

func (n *nilLoadStore) Save(_ context.Context, cfg *configstore.AIGuardConfig) error {
	n.saved = cfg
	return nil
}

// TestPutConfig_AuditPreservesProviderID drives configAuditSummary
// down the ProviderID-non-nil branch via the AfterState payload.
func TestPutConfig_AuditPreservesProviderID(t *testing.T) {
	e := echo.New()
	s := &stubStore{}
	spy := &auditSpy{}
	h := newHandler(t, s, nil, nil, newTestAudit(spy))
	e.PUT("/cfg", h.PutConfig)
	payload := `{"backendMode":"configured_provider","providerId":"prov-x","modelId":"m-x","promptTemplate":"p"}`
	req := httptest.NewRequest(http.MethodPut, "/cfg", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code: %d body: %s", rec.Code, rec.Body.String())
	}
	msgs := spy.captured()
	if len(msgs) != 1 {
		t.Fatalf("want 1 audit msg, got %d", len(msgs))
	}
	gotProvider, _ := msgs[0].AfterState["providerId"].(string)
	if gotProvider != "prov-x" {
		t.Errorf("audit providerId: %q (want prov-x)", gotProvider)
	}
}

// TestDryRun_MalformedJSON returns 400 with malformed_json classifier.
func TestDryRun_MalformedJSON(t *testing.T) {
	e := echo.New()
	dispatcher := &stubDispatcher{resp: &aiguard.DryRunResponse{}}
	h := newHandler(t, &stubStore{}, nil, dispatcher, nil)
	e.POST("/dry", h.DryRun)
	req := httptest.NewRequest(http.MethodPost, "/dry", bytes.NewBufferString(`{not json`))
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

// TestHTTPDispatcher_DefaultsHTTPClient locks the nil-client fallback:
// when no HTTPClient is supplied the dispatcher synthesises one. We
// can't talk to the synthesised client's real provider, so we point
// BaseURL at a closed (localhost:invalid) URL and just confirm the
// dispatcher reaches the network step — proving the nil-client default
// branch executed before erroring out.
func TestHTTPDispatcher_DefaultsHTTPClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"decision":"approve","metadata":{}}`))
	}))
	defer srv.Close()
	d := &aiguard.HTTPDispatcher{
		BaseURL: srv.URL,
		Token:   "t",
		// HTTPClient deliberately nil — exercises the default-client branch.
	}
	resp, err := d.Dispatch(context.Background(), aiguard.DryRunRequest{DetectorType: "x", Content: "y"})
	if err != nil {
		t.Fatalf("Dispatch with default client: %v", err)
	}
	if resp.Decision != "approve" {
		t.Errorf("decision: %q", resp.Decision)
	}
}

// TestHTTPDispatcher_BadResponseJSON locks the decoder-error path:
// upstream returns 200 with non-JSON body → dispatcher surfaces a
// decode error.
func TestHTTPDispatcher_BadResponseJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{not json`))
	}))
	defer srv.Close()
	d := &aiguard.HTTPDispatcher{
		BaseURL:    srv.URL,
		Token:      "t",
		HTTPClient: srv.Client(),
	}
	_, err := d.Dispatch(context.Background(), aiguard.DryRunRequest{DetectorType: "x", Content: "y"})
	if err == nil {
		t.Fatal("expected decode error, got nil")
	}
}

// TestHTTPDispatcher_DoError surfaces a transport failure (closed
// server) as a Go error.
func TestHTTPDispatcher_DoError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	srv.Close() // close before dispatch — Do() returns error.
	d := &aiguard.HTTPDispatcher{
		BaseURL:    srv.URL,
		Token:      "t",
		HTTPClient: srv.Client(),
	}
	_, err := d.Dispatch(context.Background(), aiguard.DryRunRequest{DetectorType: "x", Content: "y"})
	if err == nil {
		t.Fatal("expected transport error, got nil")
	}
}

// TestHTTPDispatcher_NewRequestError exercises the
// NewRequestWithContext failure branch by passing an invalid base URL
// containing a control character.
func TestHTTPDispatcher_NewRequestError(t *testing.T) {
	d := &aiguard.HTTPDispatcher{
		BaseURL:    "http://\x7f", // DEL → url.Parse rejects via http.NewRequestWithContext.
		Token:      "t",
		HTTPClient: http.DefaultClient,
	}
	_, err := d.Dispatch(context.Background(), aiguard.DryRunRequest{DetectorType: "x", Content: "y"})
	if err == nil {
		t.Fatal("expected NewRequest error, got nil")
	}
}

// TestRegisterRoutes_MountsThree confirms the aiguard group wires
// every endpoint at its canonical path.
func TestRegisterRoutes_MountsThree(t *testing.T) {
	h := newHandler(t, &stubStore{}, nil, nil, nil)
	e := echo.New()
	g := e.Group("/api/admin")
	noop := func(string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	}
	h.RegisterRoutes(g, noop)

	want := map[string]string{
		"GET /api/admin/ai-guard/config":   "",
		"PUT /api/admin/ai-guard/config":   "",
		"POST /api/admin/ai-guard/dry-run": "",
	}
	for _, r := range e.Routes() {
		key := r.Method + " " + r.Path
		delete(want, key)
	}
	if len(want) > 0 {
		t.Fatalf("missing routes: %v", want)
	}
}

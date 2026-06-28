package cache

// Tests for cache_preview.go and handler.go:New.
// All functions in cache_preview.go are currently at 0% coverage.
// New() constructor is also at 0%.

import (
	"bytes"
	"context"
	"errors"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/wirerewrite"
)

// New() constructor — requires a real *pgxpool.Pool which we can't mock
// directly, so we exercise the nil-pool branch (d.Pool == nil).

func TestNew_NilPool_DoesNotPanic(t *testing.T) {
	h := New(Deps{
		Pool:   nil,
		Hub:    nil,
		Audit:  nil,
		Logger: nil,
	})
	if h == nil {
		t.Fatal("New returned nil")
		return
	}
	if h.cache != nil {
		t.Error("nil pool must not create a cachestore")
	}
}

// simpleDiff — pure function, no DB

func TestSimpleDiff_NoDiff_ReturnsNil(t *testing.T) {
	body := []byte(`{"a":1}`)
	diff := simpleDiff(body, body)
	if diff != nil {
		t.Errorf("identical bodies should return nil diff; got %v", diff)
	}
}

func TestSimpleDiff_WithDiff_ReturnsMinus(t *testing.T) {
	before := []byte(`{"a":1,"b":2}`)
	after := []byte(`{"a":1}`)
	diff := simpleDiff(before, after)
	if len(diff) == 0 {
		t.Error("expected diff lines; got none")
	}
	hasMinus := false
	for _, l := range diff {
		if strings.HasPrefix(l, "-") {
			hasMinus = true
		}
	}
	if !hasMinus {
		t.Errorf("diff should have at least one removal line; got %v", diff)
	}
}

func TestSimpleDiff_Added_ReturnsPlusLine(t *testing.T) {
	before := []byte(`{"a":1}`)
	after := []byte(`{"a":1,"b":2}`)
	diff := simpleDiff(before, after)
	hasPlus := false
	for _, l := range diff {
		if strings.HasPrefix(l, "+") {
			hasPlus = true
		}
	}
	if !hasPlus {
		t.Errorf("diff should have at least one addition line; got %v", diff)
	}
}

// prettyJSON — pure function

func TestPrettyJSON_ValidJSON_Indented(t *testing.T) {
	input := []byte(`{"b":2,"a":1}`)
	out := prettyJSON(input)
	if !strings.Contains(out, "\n") {
		t.Errorf("expected indented output; got %q", out)
	}
}

func TestPrettyJSON_InvalidJSON_PassthroughRaw(t *testing.T) {
	input := []byte(`not-json`)
	out := prettyJSON(input)
	if out != "not-json" {
		t.Errorf("invalid JSON should passthrough; got %q", out)
	}
}

// buildPreviewConfig — pure function

func TestBuildPreviewConfig_ForcesDryRunTrue(t *testing.T) {
	enabled := true
	dryRun := false
	cfg := wirerewrite.Config{
		NormaliserEnabled: false,
		Rules: map[string]map[string]wirerewrite.RuleOverride{
			"anthropic": {
				"rule-x": {Enabled: &enabled, DryRunAlways: &dryRun},
			},
		},
	}
	preview := buildPreviewConfig(cfg)
	if !preview.NormaliserEnabled {
		t.Error("preview config must force NormaliserEnabled=true")
	}
	ro := preview.Rules["anthropic"]["rule-x"]
	if ro.DryRunAlways == nil || !*ro.DryRunAlways {
		t.Error("buildPreviewConfig must set DryRunAlways=true on existing rules")
	}
}

func TestBuildPreviewConfig_EnsuresBundledRules(t *testing.T) {
	// Even with no operator rules, the bundled anthropic strip rule must appear.
	cfg := wirerewrite.Config{
		Rules: map[string]map[string]wirerewrite.RuleOverride{},
	}
	preview := buildPreviewConfig(cfg)
	if _, ok := preview.Rules["anthropic"][wirerewrite.RuleAnthropicCchStrip]; !ok {
		t.Error("anthropic strip rule should be ensured")
	}
}

// ensureRuleEnabled — pure function

func TestEnsureRuleEnabled_AddsWhenMissing(t *testing.T) {
	cfg := &wirerewrite.Config{Rules: map[string]map[string]wirerewrite.RuleOverride{}}
	ensureRuleEnabled(cfg, "anthropic", "my-rule")
	ro, ok := cfg.Rules["anthropic"]["my-rule"]
	if !ok {
		t.Fatal("rule not added")
	}
	if ro.Enabled == nil || !*ro.Enabled {
		t.Error("enabled must be true")
	}
	if ro.DryRunAlways == nil || !*ro.DryRunAlways {
		t.Error("dry_run must be true")
	}
}

func TestEnsureRuleEnabled_DoesNotOverwriteExisting(t *testing.T) {
	disabled := false
	cfg := &wirerewrite.Config{
		Rules: map[string]map[string]wirerewrite.RuleOverride{
			"anthropic": {"my-rule": {Enabled: &disabled}},
		},
	}
	ensureRuleEnabled(cfg, "anthropic", "my-rule")
	// existing entry must not be overwritten.
	ro := cfg.Rules["anthropic"]["my-rule"]
	if ro.Enabled == nil || *ro.Enabled {
		t.Error("ensureRuleEnabled must not overwrite existing rule")
	}
}

// collectAppliedRules — pure function

func TestCollectAppliedRules_ReturnsEnabledIDs(t *testing.T) {
	enabled := true
	disabled := false
	cfg := wirerewrite.Config{
		Rules: map[string]map[string]wirerewrite.RuleOverride{
			"anthropic": {
				"rule-a": {Enabled: &enabled},
				"rule-b": {Enabled: &disabled},
				"rule-c": {Enabled: &enabled},
			},
		},
	}
	got := collectAppliedRules(cfg, "anthropic")
	if len(got) != 2 {
		t.Errorf("expected 2 enabled rules; got %v", got)
	}
}

func TestCollectAppliedRules_CaseFolded(t *testing.T) {
	enabled := true
	cfg := wirerewrite.Config{
		Rules: map[string]map[string]wirerewrite.RuleOverride{
			"anthropic": {"rule-x": {Enabled: &enabled}},
		},
	}
	// adapter_type "Anthropic" (capitalized) must still match.
	got := collectAppliedRules(cfg, "Anthropic")
	if len(got) != 1 {
		t.Errorf("expected case-folded match; got %v", got)
	}
}

func TestCollectAppliedRules_NilEnabled_NotIncluded(t *testing.T) {
	cfg := wirerewrite.Config{
		Rules: map[string]map[string]wirerewrite.RuleOverride{
			"openai": {"rule-x": {}}, // Enabled == nil
		},
	}
	got := collectAppliedRules(cfg, "openai")
	if len(got) != 0 {
		t.Errorf("nil Enabled should not count as applied; got %v", got)
	}
}

// buildRuleSummary — pure function

func TestBuildRuleSummary_ReflectsEnabledAndDryRun(t *testing.T) {
	enabled := true
	dryRun := true
	cfg := wirerewrite.Config{
		Rules: map[string]map[string]wirerewrite.RuleOverride{
			"anthropic": {
				"r1": {Enabled: &enabled, DryRunAlways: &dryRun},
			},
		},
	}
	summary := buildRuleSummary(cfg, "anthropic")
	if len(summary) != 1 {
		t.Fatalf("expected 1 summary entry; got %d", len(summary))
	}
	if !summary[0].Enabled {
		t.Error("Enabled should be true")
	}
	if !summary[0].DryRun {
		t.Error("DryRun should be true")
	}
	if summary[0].RuleID != "r1" {
		t.Errorf("RuleID = %q; want 'r1'", summary[0].RuleID)
	}
	if summary[0].AdapterType != "anthropic" {
		t.Errorf("AdapterType = %q; want 'anthropic'", summary[0].AdapterType)
	}
}

func TestBuildRuleSummary_NoRules_ReturnsNil(t *testing.T) {
	cfg := wirerewrite.Config{Rules: map[string]map[string]wirerewrite.RuleOverride{}}
	out := buildRuleSummary(cfg, "anthropic")
	if len(out) != 0 {
		t.Errorf("expected empty slice; got %v", out)
	}
}

// getTrafficEventForPreview — via mock QueryRow

func TestGetTrafficEventForPreview_Happy(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)

	mock.ExpectQuery(`SELECT COALESCE`).
		WithArgs("evt-1").
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type", "provider_id", "inline_request_body", "inline_request_encoding"}).
			AddRow("anthropic", "prov-1", []byte(`{"model":"claude-3"}`), "text"))

	got, err := h.getTrafficEventForPreview(context.Background(), "evt-1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got == nil {
		t.Fatal("expected row; got nil")
		return
	}
	if got.AdapterType != "anthropic" || got.ProviderID != "prov-1" {
		t.Errorf("got: %+v", got)
	}
}

func TestGetTrafficEventForPreview_NoRows_ReturnsNil(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)

	mock.ExpectQuery(`SELECT COALESCE`).
		WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)

	got, err := h.getTrafficEventForPreview(context.Background(), "missing")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for no-rows; got %+v", got)
	}
}

func TestGetTrafficEventForPreview_EmptyPayload_ReturnsNil(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)

	mock.ExpectQuery(`SELECT COALESCE`).
		WithArgs("evt-2").
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type", "provider_id", "inline_request_body", "inline_request_encoding"}).
			AddRow("openai", "prov-1", []byte(nil), ""))

	got, err := h.getTrafficEventForPreview(context.Background(), "evt-2")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != nil {
		t.Errorf("empty payload should return nil; got %+v", got)
	}
}

func TestGetTrafficEventForPreview_DBError_ReturnsErr(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)

	mock.ExpectQuery(`SELECT COALESCE`).
		WithArgs("x").
		WillReturnError(&pgconn.PgError{Message: "schema mismatch"})

	_, err := h.getTrafficEventForPreview(context.Background(), "x")
	if err == nil {
		t.Error("expected error; got nil")
	}
	if !strings.Contains(err.Error(), "query traffic event") {
		t.Errorf("error should be wrapped; got %v", err)
	}
}

// CachePreview handler — bind err / missing event / DB error / happy path

func TestCachePreview_BindErr_Returns400(t *testing.T) {
	_, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/cache/preview",
		bytes.NewReader([]byte("not-json")))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	if err := h.CachePreview(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestCachePreview_MissingEventID_Returns400(t *testing.T) {
	_, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/cache/preview",
		bytes.NewReader([]byte(`{}`)))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	if err := h.CachePreview(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "traffic_event_id is required") {
		t.Errorf("body: %s", rec.Body.String())
	}
}

func TestCachePreview_EventNotFound_Returns404(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)

	mock.ExpectQuery(`SELECT COALESCE`).
		WithArgs("missing-id").
		WillReturnError(pgx.ErrNoRows)

	req := httptest.NewRequest(http.MethodPost, "/api/admin/cache/preview",
		bytes.NewReader([]byte(`{"traffic_event_id":"missing-id"}`)))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	if err := h.CachePreview(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("code = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCachePreview_DBFetchError_Returns500(t *testing.T) {
	mock, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)

	mock.ExpectQuery(`SELECT COALESCE`).
		WithArgs("err-id").
		WillReturnError(errors.New("planner err"))

	req := httptest.NewRequest(http.MethodPost, "/api/admin/cache/preview",
		bytes.NewReader([]byte(`{"traffic_event_id":"err-id"}`)))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	if err := h.CachePreview(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestCachePreview_Happy_AnthropicEvent(t *testing.T) {
	// Full happy path: event found, blob assembled (may fail silently), preview runs.
	mock, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)

	// 1. Traffic event row.
	mock.ExpectQuery(`SELECT COALESCE`).
		WithArgs("evt-ok").
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type", "provider_id", "inline_request_body", "inline_request_encoding"}).
			AddRow("anthropic", "prov-1", []byte(`{"model":"claude-3","messages":[{"role":"user","content":"hello"}]}`), "text"))

	// 2. AssembleCacheConfigBlob: global + adapters + providers.
	mock.ExpectQuery(`FROM cache_global_config`).
		WillReturnRows(pgxmock.NewRows([]string{"config"}).AddRow([]byte(`{"normaliser_enabled":true}`)))
	mock.ExpectQuery(`FROM cache_adapter_config`).
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type", "config"}))
	mock.ExpectQuery(`FROM cache_provider_config`).
		WillReturnRows(pgxmock.NewRows([]string{"provider_id", "config"}))

	req := httptest.NewRequest(http.MethodPost, "/api/admin/cache/preview",
		bytes.NewReader([]byte(`{"traffic_event_id":"evt-ok"}`)))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	if err := h.CachePreview(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v body=%s", err, rec.Body.String())
	}
	if resp["traffic_event_id"] != "evt-ok" {
		t.Errorf("traffic_event_id = %v", resp["traffic_event_id"])
	}
	if resp["adapter_type"] != "anthropic" {
		t.Errorf("adapter_type = %v", resp["adapter_type"])
	}
	if resp["dry_run"] != true {
		t.Errorf("dry_run should be true; got %v", resp["dry_run"])
	}
	// body_before should be the original payload.
	if resp["body_before"] == nil {
		t.Errorf("body_before should be present")
	}
}

func TestCachePreview_Happy_BlobAssembleFailsSilently(t *testing.T) {
	// When blob assembly fails, the preview still runs with an empty config (fail-safe).
	mock, db := newMockDB(t)
	h := newHandler(t, db, nil, nil)

	mock.ExpectQuery(`SELECT COALESCE`).
		WithArgs("evt-ok").
		WillReturnRows(pgxmock.NewRows([]string{"adapter_type", "provider_id", "inline_request_body", "inline_request_encoding"}).
			AddRow("openai", "prov-1", []byte(`{"model":"gpt-4o","messages":[]}`), "text"))

	// Blob assembly error — handler swallows it and uses empty config.
	mock.ExpectQuery(`FROM cache_global_config`).WillReturnError(errors.New("db down"))

	req := httptest.NewRequest(http.MethodPost, "/api/admin/cache/preview",
		bytes.NewReader([]byte(`{"traffic_event_id":"evt-ok"}`)))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	if err := h.CachePreview(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
}

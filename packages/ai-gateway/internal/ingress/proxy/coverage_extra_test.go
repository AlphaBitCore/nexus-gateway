// coverage_extra_test.go — focused additive coverage for handler
// helpers that have no DB / executor / upstream dependencies. Lifts
// ingress context helpers, error/envelope helpers, admin endpoints
// (provider-test, model detail), and the small scalar/streaming
// helpers above the 95% threshold without standing up a full proxy
// pipeline.
package proxy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/executor"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/envelope"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	configtypes "github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/policy"
)

func TestIngress_GeminiCacheInvalidateContext(t *testing.T) {
	called := false
	ctx := withGeminiCacheInvalidate(context.Background(), func() { called = true })
	fn := GeminiCacheInvalidateFromContext(ctx)
	if fn == nil {
		t.Fatal("expected non-nil invalidate fn")
	}
	fn()
	if !called {
		t.Error("invalidate fn not called")
	}
	// Empty ctx → nil
	if GeminiCacheInvalidateFromContext(context.Background()) != nil {
		t.Error("expected nil for empty ctx")
	}
}

func TestIngress_StickyKeyFromCtx(t *testing.T) {
	ctx := withStickyKey(context.Background(), "vk-abc")
	if got := stickyKeyFromCtx(ctx); got != "vk-abc" {
		t.Errorf("stickyKey=%q want vk-abc", got)
	}
	if got := stickyKeyFromCtx(context.Background()); got != "" {
		t.Errorf("empty ctx stickyKey=%q want empty", got)
	}
}

func TestCrossFormat_WriteResponsesFeatureRejection(t *testing.T) {
	h := &Handler{deps: &Deps{}}
	w := httptest.NewRecorder()
	rec := &audit.Record{}
	rej := &ResponsesCrossFormatRejection{
		Param:   "previous_response_id",
		Message: "feature not supported",
	}
	h.writeResponsesFeatureRejection(w, rec, rej)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", w.Code)
	}
	if rec.StatusCode != http.StatusBadRequest {
		t.Errorf("rec.StatusCode=%d", rec.StatusCode)
	}
	if rec.HookReasonCode != "feature_requires_native_responses_target" {
		t.Errorf("rec.HookReasonCode=%q", rec.HookReasonCode)
	}
	body := w.Body.Bytes()
	if got := gjson.GetBytes(body, "error.code").String(); got != "feature_requires_native_responses_target" {
		t.Errorf("error.code=%q", got)
	}
	if got := gjson.GetBytes(body, "error.param").String(); got != "previous_response_id" {
		t.Errorf("error.param=%q", got)
	}
	if got := gjson.GetBytes(body, "error.type").String(); got != "unsupported_feature" {
		t.Errorf("error.type=%q", got)
	}
}

func TestCrossFormat_WriteCrossFormatStreamUnsupported(t *testing.T) {
	h := &Handler{deps: &Deps{}}
	w := httptest.NewRecorder()
	rec := &audit.Record{}
	h.writeCrossFormatStreamUnsupported(w, rec, "openai", "anthropic")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d", w.Code)
	}
	if rec.HookReasonCode != "cross_format_stream_unsupported" {
		t.Errorf("rec.HookReasonCode=%q", rec.HookReasonCode)
	}
	if got := gjson.GetBytes(w.Body.Bytes(), "error.type").String(); got != "cross_format_stream_unsupported" {
		t.Errorf("error.type=%q", got)
	}
}

func TestCrossFormat_SchemaMode_DefaultFallthrough(t *testing.T) {
	// Without a bridge, FormatAnthropic ingress with non-anthropic provider
	// must return "rejected" (default branch of the legacy switch).
	if got := schemaMode(provcore.FormatAnthropic, provcore.FormatGemini, typology.WireShapeOpenAIChat, nil); got != "rejected" {
		t.Errorf("schemaMode anthropic→gemini=%q want rejected", got)
	}
}

// Note: error_envelope.go tests (anthropicErrorType, geminiStatusForHTTPCode,
// encodeErrorEnvelopeForIngressForStream) moved to packages/ai-gateway/internal/ingress/envelope.

func TestIsValidReasoningEffort_AllCases(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		{"", true},
		{"low", true},
		{"LOW", true}, // case-insensitive
		{"medium", true},
		{"high", true},
		{"minimal", true},
		{"100", true}, // numeric budget
		{"50000", true},
		{"0", false},      // not >0
		{"100001", false}, // too big
		{"bad", false},
		{"-5", false},
	}
	for _, tc := range cases {
		if got := isValidReasoningEffort(tc.v); got != tc.want {
			t.Errorf("isValidReasoningEffort(%q)=%v want %v", tc.v, got, tc.want)
		}
	}
}

func TestProxy_EstimateTokens(t *testing.T) {
	// Empty → minimum 1
	if got := estimateTokens(nil); got != 1 {
		t.Errorf("estimateTokens(nil)=%d want 1", got)
	}
	// 3 runes → 1 token
	if got := estimateTokens([]byte("abc")); got != 1 {
		t.Errorf("estimateTokens(abc)=%d want 1", got)
	}
	// 9 runes → 3 tokens
	if got := estimateTokens([]byte("abcdefghi")); got != 3 {
		t.Errorf("estimateTokens(9 chars)=%d want 3", got)
	}
}

func TestProxy_BuildOrgPath(t *testing.T) {
	if got := buildOrgPath("", nil); got != nil {
		t.Errorf("empty org → nil; got %v", got)
	}
	if got := buildOrgPath("org-a", nil); got != nil {
		t.Errorf("nil parents → nil; got %v", got)
	}
	parents := map[string]string{
		"org-c": "org-b",
		"org-b": "org-a",
		"org-a": "", // root
	}
	got := buildOrgPath("org-c", parents)
	if len(got) != 2 || got[0] != "org-b" || got[1] != "org-a" {
		t.Errorf("buildOrgPath=%v want [org-b org-a]", got)
	}
}

func TestProxy_NewUpstreamClient(t *testing.T) {
	c := NewUpstreamClient()
	if c == nil {
		t.Fatal("nil client")
	}
	if c.Timeout == 0 {
		t.Error("zero timeout")
	}
}

func TestProxy_StreamCaptureHardCap(t *testing.T) {
	// Nil deps → default
	h := &Handler{deps: nil}
	if got := h.streamCaptureHardCap(); got != 256*1024*1024 {
		t.Errorf("nil deps cap=%d", got)
	}
	// Zero deps.StreamCaptureHardCap → default
	h = &Handler{deps: &Deps{StreamCaptureHardCap: 0}}
	if got := h.streamCaptureHardCap(); got != 256*1024*1024 {
		t.Errorf("zero cap=%d", got)
	}
	// Custom value passes through
	h = &Handler{deps: &Deps{StreamCaptureHardCap: 1024}}
	if got := h.streamCaptureHardCap(); got != 1024 {
		t.Errorf("custom cap=%d want 1024", got)
	}
}

func TestProxy_MinInt64(t *testing.T) {
	if got := minInt64(3, 5); got != 3 {
		t.Errorf("minInt64(3,5)=%d", got)
	}
	if got := minInt64(5, 3); got != 3 {
		t.Errorf("minInt64(5,3)=%d", got)
	}
	if got := minInt64(0, -1); got != -1 {
		t.Errorf("minInt64(0,-1)=%d", got)
	}
}

func TestProxy_StreamCaptureTee(t *testing.T) {
	base := httptest.NewRecorder()
	tee := newStreamCaptureTee(base, 10)
	// Write below cap
	n, err := tee.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("write1: n=%d err=%v", n, err)
	}
	if string(tee.captured()) != "hello" {
		t.Errorf("captured=%q", tee.captured())
	}
	if tee.truncatedBeyondCap() {
		t.Error("truncated too early")
	}
	// Write that crosses cap (5 in buffer, cap=10, writing 8 → buf grows to 10, rest truncated)
	n, err = tee.Write([]byte("world!!!")) // 8 bytes
	if err != nil || n != 8 {
		t.Fatalf("write2: n=%d err=%v", n, err)
	}
	if !tee.truncatedBeyondCap() {
		t.Error("expected truncated=true")
	}
	if int64(len(tee.captured())) != 10 {
		t.Errorf("captured len=%d want 10", len(tee.captured()))
	}
	// Another write — all truncated
	n, err = tee.Write([]byte("xx"))
	if err != nil || n != 2 {
		t.Fatalf("write3: n=%d err=%v", n, err)
	}
	if len(tee.captured()) != 10 {
		t.Errorf("captured grew after tail: len=%d", len(tee.captured()))
	}

	// Flush + Unwrap forwarders
	tee.Flush()
	if tee.Unwrap() != base {
		t.Error("Unwrap didn't return base writer")
	}
}

func TestProxy_StreamCaptureTee_NegativeCap(t *testing.T) {
	base := httptest.NewRecorder()
	tee := newStreamCaptureTee(base, -1)
	// Negative cap is clamped to 0; first write goes straight to tail.
	n, err := tee.Write([]byte("anything"))
	if err != nil || n != 8 {
		t.Fatalf("write: n=%d err=%v", n, err)
	}
	if !tee.truncatedBeyondCap() {
		t.Error("expected truncated=true on zero-cap tee")
	}
	if len(tee.captured()) != 0 {
		t.Errorf("captured=%v want empty", tee.captured())
	}
}

func TestProxy_GeminicacheStaleRefError(t *testing.T) {
	cases := map[string]bool{
		``:                                 false,
		`{"error":"unrelated"}`:            false,
		`CachedContent not found`:          true,
		`cached content not found`:         true,
		`cachedContents/abc-123 not found`: true,
		`permission denied on cachedContents/xyz`: true,
		`some random 500 from gemini`:             false,
	}
	for body, want := range cases {
		if got := geminicacheStaleRefError([]byte(body)); got != want {
			t.Errorf("geminicacheStaleRefError(%q)=%v want %v", body, got, want)
		}
	}
}

func TestProxy_ExtractProviderErrorMessage(t *testing.T) {
	if got := extractProviderErrorMessage(nil, 500); !strings.Contains(got, "HTTP 500") {
		t.Errorf("empty body fallback: %q", got)
	}
	if got := extractProviderErrorMessage([]byte(`{"error":{"message":"oops"}}`), 400); got != "oops" {
		t.Errorf("error.message: %q", got)
	}
	if got := extractProviderErrorMessage([]byte(`{"message":"top-level"}`), 400); got != "top-level" {
		t.Errorf("message: %q", got)
	}
	long := strings.Repeat("x", 400)
	got := extractProviderErrorMessage([]byte(long), 400)
	if !strings.HasSuffix(got, "...") || len(got) != 303 {
		t.Errorf("long fallback truncation broken: len=%d ends_with_ellipsis=%v", len(got), strings.HasSuffix(got, "..."))
	}
	if got := extractProviderErrorMessage([]byte("short raw"), 502); got != "short raw" {
		t.Errorf("short raw fallback: %q", got)
	}
}

func TestProxy_ParseRulePolicy(t *testing.T) {
	h := &Handler{deps: &Deps{Logger: slog.Default()}}
	if got := h.parseRulePolicy(nil); got != nil {
		t.Errorf("nil → non-nil: %v", got)
	}
	if got := h.parseRulePolicy([]byte("null")); got != nil {
		t.Errorf("null → non-nil: %v", got)
	}
	if got := h.parseRulePolicy([]byte("not-json")); got != nil {
		t.Errorf("bad json → non-nil: %v", got)
	}
	got := h.parseRulePolicy([]byte(`{"maxAttemptsPerTarget":5}`))
	if got == nil || got.MaxAttemptsPerTarget != 5 {
		t.Errorf("good: %v", got)
	}
}

func TestProxy_EffectiveRetryPolicy(t *testing.T) {
	// Nil h.deps default
	h := &Handler{deps: nil}
	p := h.effectiveRetryPolicy(nil, nil)
	if p.MaxAttemptsPerTarget <= 0 {
		t.Errorf("default policy not used: %+v", p)
	}

	// Wired deps with custom default
	dp := configtypes.RetryPolicy{
		MaxAttemptsPerTarget: 3,
		RetryOn:              []configtypes.ErrorClass{configtypes.ErrorClass5xx},
		BackoffInitial:       100 * time.Millisecond,
		BackoffMax:           5 * time.Second,
		BackoffJitter:        0.1,
	}
	h = &Handler{deps: &Deps{RoutingDefaultPolicy: dp, Logger: slog.Default()}}
	p = h.effectiveRetryPolicy(nil, slog.Default())
	if p.MaxAttemptsPerTarget != 3 {
		t.Errorf("dp not used: %+v", p)
	}

	// With per-rule override (clamped to [1,5])
	p = h.effectiveRetryPolicy([]byte(`{"maxAttemptsPerTarget":7}`), slog.Default())
	if p.MaxAttemptsPerTarget != 5 {
		t.Errorf("rule override clamp not applied: %+v", p)
	}
	// Below-min clamp
	p = h.effectiveRetryPolicy([]byte(`{"maxAttemptsPerTarget":0}`), slog.Default())
	if p.MaxAttemptsPerTarget < 1 {
		t.Errorf("min clamp failed: %+v", p)
	}
}

type fakeLimiter struct {
	allow      bool
	retryAfter int
}

func (f *fakeLimiter) Allow(_ string, _ int, _ int64) (bool, int) {
	return f.allow, f.retryAfter
}

func TestProxy_CheckCompareRateLimit(t *testing.T) {
	// No limiter
	h := &Handler{deps: &Deps{}}
	w := httptest.NewRecorder()
	if err := h.checkCompareRateLimit(w, &vkauth.VKMeta{}); err != nil {
		t.Errorf("no limiter: %v", err)
	}

	// Blocked
	h = &Handler{deps: &Deps{RateLimiter: &fakeLimiter{allow: false, retryAfter: 15}}}
	if err := h.checkCompareRateLimit(w, &vkauth.VKMeta{Name: "vk"}); err == nil {
		t.Error("expected rate-limit error")
	} else if w.Header().Get("Retry-After") != "15" {
		t.Errorf("Retry-After=%q", w.Header().Get("Retry-After"))
	}

	// zero limit per-VK = no limit
	zero := 0
	w = httptest.NewRecorder()
	h = &Handler{deps: &Deps{RateLimiter: &fakeLimiter{allow: true}}}
	if err := h.checkCompareRateLimit(w, &vkauth.VKMeta{Name: "vk", CompareEndpointRateLimitRpm: &zero}); err != nil {
		t.Errorf("zero limit: %v", err)
	}
}

func TestProxy_UpstreamHost(t *testing.T) {
	if got := upstreamHost(routingcore.RoutingTarget{ProviderName: "openai"}); got != "openai" {
		t.Errorf("empty BaseURL → %q", got)
	}
	if got := upstreamHost(routingcore.RoutingTarget{BaseURL: "https://api.openai.com/v1", ProviderName: "fallback"}); got != "api.openai.com" {
		t.Errorf("with URL → %q", got)
	}
	if got := upstreamHost(routingcore.RoutingTarget{BaseURL: "://invalid", ProviderName: "fb"}); got != "fb" {
		t.Errorf("invalid URL fallback → %q", got)
	}
}

func TestProxy_RoutingFallbackError_Error(t *testing.T) {
	e := &routingFallbackError{status: 500, code: "X", message: "msg", hint: "h"}
	if e.Error() != "msg" {
		t.Errorf("Error()=%q", e.Error())
	}
}

func TestProxy_WriteAuthError_AllSentinels(t *testing.T) {
	h := &Handler{deps: &Deps{}}
	tests := []struct {
		err  error
		want string
	}{
		{vkauth.ErrMissing, "AUTH_KEY_MISSING"},
		{vkauth.ErrDisabled, "AUTH_KEY_DISABLED"},
		{vkauth.ErrExpired, "AUTH_KEY_EXPIRED"},
		{errors.New("other"), "AUTH_INVALID_KEY"},
	}
	for _, tt := range tests {
		w := httptest.NewRecorder()
		rec := &audit.Record{}
		h.writeAuthError(w, rec, tt.err)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("status=%d want 401 for %v", w.Code, tt.err)
		}
		body := w.Body.String()
		if !strings.Contains(body, tt.want) {
			t.Errorf("body missing %q for %v: %s", tt.want, tt.err, body)
		}
	}
}

// Note: ModelDetailHandler and ModelsHandler tests moved to packages/ai-gateway/internal/ingress/models.

// Note: additional ModelsHandler and ProviderTestHandler tests moved to their respective packages.

// Note: ProviderTestHandler and writeJSON tests moved to packages/ai-gateway/internal/ingress/debug.

func TestProxy_PayloadCaptureConfig_NilDeps(t *testing.T) {
	h := &Handler{deps: nil}
	cfg := h.payloadCaptureConfig()
	if cfg.MaxRequestBytes <= 0 {
		t.Errorf("nil deps fallback default: %+v", cfg)
	}
	h = &Handler{deps: &Deps{}}
	cfg = h.payloadCaptureConfig()
	if cfg.MaxRequestBytes <= 0 {
		t.Errorf("empty deps fallback default: %+v", cfg)
	}
}

func TestProxy_ReadBody_Oversize(t *testing.T) {
	h := &Handler{deps: &Deps{}}
	// Force tiny max via a custom payload-capture store would require
	// internal wiring; use a body that exceeds the package default of
	// 10 MiB. Construct a 11 MiB body.
	body := bytes.Repeat([]byte("x"), 11*1024*1024)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	_, _, _, err := h.readBody(req, Ingress{BodyFormat: provcore.FormatOpenAI})
	if !errors.Is(err, errRequestTooLarge) {
		t.Errorf("err=%v want errRequestTooLarge", err)
	}
}

func TestProxy_ReadBody_ModelRequired(t *testing.T) {
	h := &Handler{deps: &Deps{}}
	body := []byte(`{}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	_, _, _, err := h.readBody(req, Ingress{BodyFormat: provcore.FormatOpenAI})
	if err == nil || !strings.Contains(err.Error(), "model is required") {
		t.Errorf("err=%v", err)
	}
}

func TestProxy_ReadBody_AutoEmbeddingsRejected(t *testing.T) {
	h := &Handler{deps: &Deps{}}
	body := []byte(`{"model":"auto"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewReader(body))
	_, _, _, err := h.readBody(req, Ingress{WireShape: typology.WireShapeOpenAIEmbeddings, BodyFormat: provcore.FormatOpenAI})
	if err == nil || !strings.Contains(err.Error(), "auto") {
		t.Errorf("err=%v want auto-rejection", err)
	}
}

func TestProxy_BuildProviderRequest_Empty(t *testing.T) {
	got := buildProviderRequest(nil, Ingress{}, nil, false, 0)
	if got.Headers != nil {
		t.Errorf("nil r → non-nil headers")
	}
}

func TestProxy_OrgParents_Nil(t *testing.T) {
	h := &Handler{deps: nil}
	if got := h.orgParents(); got != nil {
		t.Errorf("nil deps → %v", got)
	}
	h = &Handler{deps: &Deps{}}
	if got := h.orgParents(); got != nil {
		t.Errorf("no QuotaEngine → %v", got)
	}
}

func TestProxy_Authenticate_NilVKAuth(t *testing.T) {
	h := &Handler{deps: &Deps{}}
	req := httptest.NewRequest(http.MethodPost, "/v1/x", nil)
	if _, err := h.authenticate(req); err == nil {
		t.Error("expected error when VKAuth nil")
	}
}

type fakeCachePricing struct {
	row *store.ProviderPricing
}

func (f *fakeCachePricing) LookupCachePricing(_, _, _ string) *store.ProviderPricing {
	return f.row
}

func TestProxy_ComputeCacheCosts_NilLookup(t *testing.T) {
	h := &Handler{deps: &Deps{}}
	rec := &audit.Record{CacheReadTokens: 10}
	h.computeCacheCosts(rec, routingcore.RoutingTarget{})
	if rec.CacheReadSavingsUsd != 0 {
		t.Errorf("expected no-op when CachePricing nil")
	}
}

func TestProxy_ComputeCacheCosts_NoCacheTokens(t *testing.T) {
	h := &Handler{deps: &Deps{CachePricing: &fakeCachePricing{row: &store.ProviderPricing{}}}}
	rec := &audit.Record{} // no cache tokens
	h.computeCacheCosts(rec, routingcore.RoutingTarget{})
	if rec.CacheReadSavingsUsd != 0 {
		t.Errorf("expected no-op when no cache tokens")
	}
}

func TestProxy_ComputeCacheCosts_NilPricingRow(t *testing.T) {
	h := &Handler{deps: &Deps{CachePricing: &fakeCachePricing{row: nil}}}
	rec := &audit.Record{CacheReadTokens: 100}
	h.computeCacheCosts(rec, routingcore.RoutingTarget{})
	if rec.CacheReadSavingsUsd != 0 {
		t.Errorf("expected no-op when pricing row nil")
	}
}

func TestProxy_ComputeCacheCosts_AnthropicTokens(t *testing.T) {
	h := &Handler{deps: &Deps{CachePricing: &fakeCachePricing{row: &store.ProviderPricing{
		InputUSDPerM:      3.0,
		OutputUSDPerM:     15.0,
		CacheReadUSDPerM:  0.3,
		CacheWriteUSDPerM: 3.75,
	}}}}
	rec := &audit.Record{
		PromptTokens:        100,
		CompletionTokens:    50,
		CacheReadTokens:     20,
		CacheCreationTokens: 10,
	}
	h.computeCacheCosts(rec, routingcore.RoutingTarget{AdapterType: string(provcore.FormatAnthropic)})
	if rec.CacheWriteCostUsd == 0 {
		t.Error("expected non-zero CacheWriteCostUsd")
	}
	if rec.CacheReadSavingsUsd == 0 {
		t.Error("expected non-zero CacheReadSavingsUsd")
	}
	if rec.EstimatedCostUsd == 0 {
		t.Error("expected non-zero EstimatedCostUsd")
	}
}

func TestProxy_ComputeCacheCosts_OpenAITokens(t *testing.T) {
	h := &Handler{deps: &Deps{CachePricing: &fakeCachePricing{row: &store.ProviderPricing{
		InputUSDPerM:      2.5,
		OutputUSDPerM:     10.0,
		CacheReadUSDPerM:  1.25,
		CacheWriteUSDPerM: 0,
	}}}}
	// OpenAI: PromptTokens INCLUDES cached, so subtract.
	rec := &audit.Record{
		PromptTokens:     100, // includes 20 cached
		CompletionTokens: 50,
		CacheReadTokens:  20,
	}
	h.computeCacheCosts(rec, routingcore.RoutingTarget{AdapterType: string(provcore.FormatOpenAI)})
	if rec.EstimatedCostUsd == 0 {
		t.Error("expected non-zero cost")
	}
}

func TestProxy_ComputeCacheCosts_NegativeRegularInputClamped(t *testing.T) {
	h := &Handler{deps: &Deps{CachePricing: &fakeCachePricing{row: &store.ProviderPricing{
		InputUSDPerM:     2.5,
		OutputUSDPerM:    10.0,
		CacheReadUSDPerM: 1.25,
	}}}}
	// PromptTokens=10, CacheReadTokens=20 → 10-20=-10, clamp to 0.
	rec := &audit.Record{PromptTokens: 10, CacheReadTokens: 20}
	h.computeCacheCosts(rec, routingcore.RoutingTarget{AdapterType: string(provcore.FormatOpenAI)})
	// Should not panic and should produce a non-negative cost.
	if rec.EstimatedCostUsd < 0 {
		t.Errorf("EstimatedCostUsd negative: %v", rec.EstimatedCostUsd)
	}
}

func TestProxy_AigwHookOutcomeFromResult_NilOrEmpty(t *testing.T) {
	if got := aigwHookOutcomeFromResult(nil); got.Rejected != "" || len(got.Passed) != 0 {
		t.Errorf("nil: %+v", got)
	}
}

func TestProxy_Finalize_StampsLatency(t *testing.T) {
	// Use a real audit writer with nil producer (Enqueue is a no-op).
	logger := slog.Default()
	w := audit.NewWriter(nil, "topic", nil, logger)
	h := &Handler{deps: &Deps{AuditWriter: w}}
	rec := &audit.Record{}
	start := time.Now()
	h.finalize(rec, start)
	if rec.LatencyMs < 1 {
		t.Errorf("LatencyMs=%d want >=1", rec.LatencyMs)
	}
}

func TestProxy_MergeTagSets(t *testing.T) {
	if got := mergeTagSets(nil, nil); got != nil {
		t.Errorf("nil + nil = %v", got)
	}
	got := mergeTagSets([]string{"a", "c"}, []string{"b", "a"})
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("merge=%v want [a b c]", got)
	}
	// Dedup within single slice
	got = mergeTagSets([]string{"a", "a", "b"}, nil)
	if len(got) != 2 {
		t.Errorf("dedup failed: %v", got)
	}
}

func TestProxy_UsageInt(t *testing.T) {
	if got := usageInt(nil); got != 0 {
		t.Errorf("nil: %d", got)
	}
	v := 7
	if got := usageInt(&v); got != 7 {
		t.Errorf("&7: %d", got)
	}
}

func TestProxy_AppendHookTrace_EmptyAndPopulated(t *testing.T) {
	out := appendHookTrace(nil, "request", nil)
	if out != nil {
		t.Errorf("empty input → non-nil: %v", out)
	}
}

func TestProxy_WriteForwardedResponseHeaders_NilSrc(t *testing.T) {
	w := httptest.NewRecorder()
	writeForwardedResponseHeaders(w, nil, provcore.FormatOpenAI, nil, false)
	// Should not modify response or panic.
	if len(w.Header()) != 0 {
		t.Errorf("headers added unexpectedly: %v", w.Header())
	}
}

func TestProxy_AllowlistVersionFromDeps(t *testing.T) {
	if got := allowlistVersionFromDeps(nil); got != "" {
		t.Errorf("nil deps: %q", got)
	}
	if got := allowlistVersionFromDeps(&Deps{}); got != "" {
		t.Errorf("no allowlist: %q", got)
	}
}

func TestProxy_WriteError_BasicPaths(t *testing.T) {
	h := &Handler{deps: &Deps{}}
	rec := &audit.Record{}
	w := httptest.NewRecorder()
	h.writeError(w, rec, http.StatusBadRequest, "bad")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d", w.Code)
	}
	if rec.StatusCode != http.StatusBadRequest {
		t.Errorf("rec.StatusCode=%d", rec.StatusCode)
	}

	w = httptest.NewRecorder()
	rec = &audit.Record{}
	h.writeDetailedErr(w, rec, http.StatusForbidden, "CODE", "msg", "hint")
	if w.Code != http.StatusForbidden {
		t.Errorf("status=%d", w.Code)
	}
	if rec.ErrorCode != "CODE" {
		t.Errorf("rec.ErrorCode=%q", rec.ErrorCode)
	}
}

func TestProxyCache_CopyUpstreamHeaders(t *testing.T) {
	if got := copyUpstreamHeaders(nil); got != nil {
		t.Errorf("nil → %v", got)
	}
	if got := copyUpstreamHeaders(http.Header{}); got != nil {
		t.Errorf("empty → %v", got)
	}
	src := http.Header{"X-Foo": []string{"a", "b"}, "Content-Type": []string{"application/json"}}
	got := copyUpstreamHeaders(src)
	if len(got["X-Foo"]) != 2 || got["X-Foo"][0] != "a" {
		t.Errorf("copy x-foo: %v", got)
	}
	// Mutating source must not affect copy
	src["X-Foo"][0] = "MUTATED"
	if got["X-Foo"][0] == "MUTATED" {
		t.Error("copy shared memory with src")
	}
}

func TestProxyCache_JoinCSV(t *testing.T) {
	if joinCSV(nil) != "" {
		t.Error("empty → non-empty")
	}
	if joinCSV([]string{"a"}) != "a" {
		t.Error("single → wrong")
	}
	if got := joinCSV([]string{"a", "b", "c"}); got != "a,b,c" {
		t.Errorf("got=%q", got)
	}
}

func TestProxyCache_ChunkUsageHolder_RecordAndSnapshot(t *testing.T) {
	h := &chunkUsageHolder{}
	// nil receiver / nil usage no-ops
	h.record(nil)
	if snap := h.snapshot(); snap.PromptTokens != nil {
		t.Errorf("empty snap: %+v", snap)
	}

	// First record sets fields
	p1 := 100
	c1 := 50
	h.record(&provcore.Usage{PromptTokens: &p1, CompletionTokens: &c1})
	snap := h.snapshot()
	if snap.PromptTokens == nil || *snap.PromptTokens != 100 {
		t.Errorf("prompt=%v", snap.PromptTokens)
	}
	if snap.TotalTokens == nil || *snap.TotalTokens != 150 {
		t.Errorf("total=%v", snap.TotalTokens)
	}

	// Second record merges (only completion changes)
	c2 := 75
	h.record(&provcore.Usage{CompletionTokens: &c2})
	snap = h.snapshot()
	if *snap.PromptTokens != 100 {
		t.Errorf("prompt lost after merge: %v", snap.PromptTokens)
	}
	if *snap.CompletionTokens != 75 {
		t.Errorf("completion=%v", snap.CompletionTokens)
	}

	// Provider total wins over derived
	total := 999
	h.record(&provcore.Usage{TotalTokens: &total})
	snap = h.snapshot()
	if *snap.TotalTokens != 999 {
		t.Errorf("provider total ignored: %v", snap.TotalTokens)
	}

	// Cache tokens contribute to derived total
	h2 := &chunkUsageHolder{}
	p := 100
	c := 50
	cr := 10
	cw := 20
	h2.record(&provcore.Usage{PromptTokens: &p, CompletionTokens: &c, CacheReadTokens: &cr, CacheCreationTokens: &cw})
	snap2 := h2.snapshot()
	if *snap2.TotalTokens != 180 {
		t.Errorf("derived total with cache: %v want 180", snap2.TotalTokens)
	}
}

func TestProxyCache_ChunkUsageHolder_NilSafe(t *testing.T) {
	var h *chunkUsageHolder
	h.record(&provcore.Usage{}) // must not panic
	snap := h.snapshot()
	if snap.PromptTokens != nil {
		t.Errorf("nil receiver snap=%+v", snap)
	}
}

// fakeStreamSession implements provcore.StreamSession.
type fakeStreamSession struct {
	chunks  []provcore.Chunk
	idx     int
	closed  bool
	nextErr error
}

func (f *fakeStreamSession) Next(_ context.Context) (provcore.Chunk, error) {
	if f.nextErr != nil {
		return provcore.Chunk{}, f.nextErr
	}
	if f.idx >= len(f.chunks) {
		return provcore.Chunk{}, io.EOF
	}
	c := f.chunks[f.idx]
	f.idx++
	return c, nil
}

func (f *fakeStreamSession) Close() error {
	f.closed = true
	return nil
}

func TestProxyCache_DirectStreamSubscription(t *testing.T) {
	sess := &fakeStreamSession{chunks: []provcore.Chunk{{Delta: "hi"}}}
	sub := newDirectStreamSubscription(sess)

	c, err := sub.Next(context.Background())
	if err != nil || c.Delta != "hi" {
		t.Fatalf("first chunk: c=%+v err=%v", c, err)
	}
	if _, err := sub.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Errorf("EOF expected, got %v", err)
	}
	if err := sub.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
	if !sess.closed {
		t.Error("session.closed false")
	}
	// Double-close idempotent
	if err := sub.Close(); err != nil {
		t.Errorf("double close: %v", err)
	}
	// After close, Next returns EOF
	if _, err := sub.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Errorf("Next after close: %v want EOF", err)
	}
}

func TestProxyCache_DirectStreamSubscription_NilSession(t *testing.T) {
	sub := newDirectStreamSubscription(nil)
	if _, err := sub.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Errorf("nil session Next: %v", err)
	}
	if err := sub.Close(); err != nil {
		t.Errorf("nil session close: %v", err)
	}
}

func TestProxyCache_SingleChunkSession(t *testing.T) {
	res := &executor.ExecutionResult{
		Body:  []byte(`{"id":"x"}`),
		Usage: provcore.Usage{},
	}
	sess := newSingleChunkSession(res)
	c, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next err: %v", err)
	}
	if !c.Done {
		t.Error("expected Done=true")
	}
	if c.Delta != `{"id":"x"}` {
		t.Errorf("Delta=%q", c.Delta)
	}
	// Second call → EOF
	if _, err := sess.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Errorf("second Next: %v want EOF", err)
	}
	if err := sess.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// After close, EOF
	if _, err := sess.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Errorf("post-close Next: %v", err)
	}
}

// fakeChunkSub implements streamcache.ChunkSubscription with a scripted
// sequence of chunks for chunkSSEReader tests.
type fakeChunkSub struct {
	chunks []provcore.Chunk
	idx    int
	err    error
	closed bool
}

func (f *fakeChunkSub) Next(_ context.Context) (provcore.Chunk, error) {
	if f.err != nil {
		return provcore.Chunk{}, f.err
	}
	if f.idx >= len(f.chunks) {
		return provcore.Chunk{}, io.EOF
	}
	c := f.chunks[f.idx]
	f.idx++
	return c, nil
}

func (f *fakeChunkSub) Close() error {
	f.closed = true
	return nil
}

func TestProxyCache_ChunkSSEReader_RawBytesPassthrough(t *testing.T) {
	sub := &fakeChunkSub{chunks: []provcore.Chunk{
		{RawBytes: []byte("data: {\"a\":1}\n\n")},
		{Done: true, RawBytes: []byte("data: [DONE]\n\n")},
	}}
	r := newChunkSSEReaderFromSubscription(context.Background(), sub, nil, provcore.FormatOpenAI)
	r.usageSink = &chunkUsageHolder{}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !strings.Contains(string(got), `{"a":1}`) {
		t.Errorf("missing frame: %s", got)
	}
	if !strings.Contains(string(got), "[DONE]") {
		t.Errorf("missing terminator: %s", got)
	}
}

func TestProxyCache_ChunkSSEReader_DeltaFallback(t *testing.T) {
	sub := &fakeChunkSub{chunks: []provcore.Chunk{
		{Delta: "hello"},
		{Done: true},
	}}
	r := newChunkSSEReaderFromSubscription(context.Background(), sub, nil, provcore.FormatOpenAI)
	r.usageSink = &chunkUsageHolder{}
	got, _ := io.ReadAll(r)
	// Should synthesize {"choices":[{"delta":{"content":"hello"}}]}
	if !strings.Contains(string(got), `"content":"hello"`) {
		t.Errorf("delta fallback not synthesized: %s", got)
	}
}

func TestProxyCache_ChunkSSEReader_NilSub(t *testing.T) {
	r := newChunkSSEReaderFromSubscription(context.Background(), nil, nil, provcore.FormatOpenAI)
	buf := make([]byte, 10)
	n, err := r.Read(buf)
	if n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("nil sub Read: n=%d err=%v", n, err)
	}
}

func TestProxyCache_ChunkSSEReader_ProviderErrorSynthesizesFrame(t *testing.T) {
	sub := &fakeChunkSub{err: errors.New("upstream blew up")}
	r := newChunkSSEReaderFromSubscription(context.Background(), sub, nil, provcore.FormatOpenAI)
	r.usageSink = &chunkUsageHolder{}
	got, _ := io.ReadAll(r)
	// Must contain an SSE frame with the error message
	if !strings.Contains(string(got), "upstream blew up") {
		t.Errorf("missing error message in synthesized frame: %s", got)
	}
	if !strings.Contains(string(got), "data: ") {
		t.Errorf("missing data: prefix: %s", got)
	}
}

func TestProxyCache_ChunkSSEReader_ContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sub := &fakeChunkSub{err: context.Canceled}
	r := newChunkSSEReaderFromSubscription(ctx, sub, nil, provcore.FormatOpenAI)
	r.usageSink = &chunkUsageHolder{}
	buf := make([]byte, 10)
	_, err := r.Read(buf)
	if err == nil {
		t.Error("expected error on ctx cancel")
	}
}

// fakeVKLookup is a minimal in-memory VKLookup so we can construct a
// real *vkauth.Authenticator (which the usage handlers require by
// concrete type, not interface). Maps HMAC-SHA256(key) → VirtualKey row.
type fakeVKLookup struct {
	keys map[string]*store.VirtualKey
}

func (f *fakeVKLookup) GetVirtualKeyByHash(_ context.Context, keyHash string) (*store.VirtualKey, error) {
	if vk, ok := f.keys[keyHash]; ok {
		return vk, nil
	}
	return nil, nil // miss
}

func hmacHashVK(raw, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(raw))
	return hex.EncodeToString(mac.Sum(nil))
}

func TestUsageSummaryHandler_AuthMissing(t *testing.T) {
	// No DB needed; auth runs before DB. Pass a non-nil *store.DB so the
	// nil-DB short-circuit doesn't fire — pgxmock works perfectly for this.
	mock, dbErr := pgxmock.NewPool()
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	defer mock.Close()
	db := store.NewWithPgxPool(mock)

	authn := vkauth.NewAuthenticator(&fakeVKLookup{}, "test-secret", slog.Default())
	h := envelope.UsageSummaryHandler(db, authn, nil, slog.Default())

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	h(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status=%d want 401; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "AUTH_INVALID") {
		t.Errorf("body missing AUTH_INVALID: %s", w.Body.String())
	}
}

func TestUsageDailyHandler_AuthMissing(t *testing.T) {
	mock, dbErr := pgxmock.NewPool()
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	defer mock.Close()
	db := store.NewWithPgxPool(mock)

	authn := vkauth.NewAuthenticator(&fakeVKLookup{}, "test-secret", slog.Default())
	h := envelope.UsageDailyHandler(db, authn, slog.Default())

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/usage/daily", nil)
	h(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status=%d want 401", w.Code)
	}
}

func TestUsageDailyHandler_InvalidDate(t *testing.T) {
	mock, dbErr := pgxmock.NewPool()
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	defer mock.Close()
	db := store.NewWithPgxPool(mock)

	hmacSecret := "test-secret-key"
	vkKey := "nx-12345678901234567890"
	authn := vkauth.NewAuthenticator(&fakeVKLookup{
		keys: map[string]*store.VirtualKey{
			hmacHashVK(vkKey, hmacSecret): {ID: "vk-1", Name: "test-vk", Enabled: true},
		},
	}, hmacSecret, slog.Default())

	h := envelope.UsageDailyHandler(db, authn, slog.Default())
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/usage/daily?startDate=bad-date", nil)
	r.Header.Set("Authorization", "Bearer "+vkKey)
	h(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "USAGE_INVALID_DATE") {
		t.Errorf("body: %s", w.Body.String())
	}
}

func TestUsageDailyHandler_DBQueryError(t *testing.T) {
	mock, dbErr := pgxmock.NewPool()
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	defer mock.Close()
	db := store.NewWithPgxPool(mock)
	mock.ExpectQuery("FROM traffic_event").WillReturnError(errors.New("db down"))

	hmacSecret := "test-secret-key"
	vkKey := "nx-12345678901234567890"
	authn := vkauth.NewAuthenticator(&fakeVKLookup{
		keys: map[string]*store.VirtualKey{
			hmacHashVK(vkKey, hmacSecret): {ID: "vk-1", Name: "test-vk", Enabled: true},
		},
	}, hmacSecret, slog.Default())

	h := envelope.UsageDailyHandler(db, authn, slog.Default())
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/usage/daily", nil)
	r.Header.Set("Authorization", "Bearer "+vkKey)
	h(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status=%d want 500; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "USAGE_QUERY_FAILED") {
		t.Errorf("body: %s", w.Body.String())
	}
}

func TestUsageDailyHandler_HappyPath(t *testing.T) {
	mock, dbErr := pgxmock.NewPool()
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	defer mock.Close()
	db := store.NewWithPgxPool(mock)
	day := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery("FROM traffic_event").WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnRows(
		pgxmock.NewRows([]string{"day", "model_name", "provider_name", "requests", "prompt_tokens", "completion_tokens", "total_tokens", "cost_usd"}).
			AddRow(day, "gpt-4o", "openai", int64(10), int64(100), int64(50), int64(150), 0.25),
	)

	hmacSecret := "test-secret-key"
	vkKey := "nx-12345678901234567890"
	authn := vkauth.NewAuthenticator(&fakeVKLookup{
		keys: map[string]*store.VirtualKey{
			hmacHashVK(vkKey, hmacSecret): {ID: "vk-1", Name: "test-vk", Enabled: true},
		},
	}, hmacSecret, slog.Default())

	h := envelope.UsageDailyHandler(db, authn, slog.Default())
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/usage/daily", nil)
	r.Header.Set("Authorization", "Bearer "+vkKey)
	h(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := gjson.GetBytes(w.Body.Bytes(), "virtualKeyId").String(); got != "vk-1" {
		t.Errorf("virtualKeyId=%q", got)
	}
	if got := gjson.GetBytes(w.Body.Bytes(), "totals.requests").Int(); got != 10 {
		t.Errorf("totals.requests=%d", got)
	}
}

func TestUsageSummaryHandler_DBQueryError(t *testing.T) {
	mock, dbErr := pgxmock.NewPool()
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	defer mock.Close()
	db := store.NewWithPgxPool(mock)
	mock.ExpectQuery("FROM traffic_event").WillReturnError(errors.New("db down"))

	hmacSecret := "test-secret-key"
	vkKey := "nx-12345678901234567890"
	authn := vkauth.NewAuthenticator(&fakeVKLookup{
		keys: map[string]*store.VirtualKey{
			hmacHashVK(vkKey, hmacSecret): {ID: "vk-1", Name: "test-vk", Enabled: true},
		},
	}, hmacSecret, slog.Default())

	h := envelope.UsageSummaryHandler(db, authn, nil, slog.Default())
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	r.Header.Set("Authorization", "Bearer "+vkKey)
	h(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status=%d want 500", w.Code)
	}
}

func TestUsageSummaryHandler_HappyPath_NoQuota(t *testing.T) {
	mock, dbErr := pgxmock.NewPool()
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	defer mock.Close()
	db := store.NewWithPgxPool(mock)
	day := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery("FROM traffic_event").WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnRows(
		pgxmock.NewRows([]string{"day", "model_name", "provider_name", "requests", "prompt_tokens", "completion_tokens", "total_tokens", "cost_usd"}).
			AddRow(day, "gpt-4o", "openai", int64(5), int64(50), int64(20), int64(70), 0.10),
	)

	hmacSecret := "test-secret-key"
	vkKey := "nx-12345678901234567890"
	authn := vkauth.NewAuthenticator(&fakeVKLookup{
		keys: map[string]*store.VirtualKey{
			hmacHashVK(vkKey, hmacSecret): {ID: "vk-1", Name: "test-vk", Enabled: true},
		},
	}, hmacSecret, slog.Default())

	h := envelope.UsageSummaryHandler(db, authn, nil, slog.Default())
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	r.Header.Set("Authorization", "Bearer "+vkKey)
	h(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := gjson.GetBytes(w.Body.Bytes(), "virtualKeyId").String(); got != "vk-1" {
		t.Errorf("virtualKeyId=%q", got)
	}
	if got := gjson.GetBytes(w.Body.Bytes(), "usage.totalRequests").Int(); got != 5 {
		t.Errorf("totalRequests=%d", got)
	}
}

var _ = bytes.NewReader
var _ = json.NewDecoder
var _ = url.Parse

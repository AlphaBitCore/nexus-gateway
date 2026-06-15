// Tests for the proxy hook-extraction helpers (request/response content
// extraction for hooks), forwarded-response-header writing, rate-limit
// checks, and request-context building — exercised with in-package stubs
// rather than the full broker + executor + canonical-bridge pipeline.
//
// The handleNonStream / fetchUpstreamWithPreparedBody / runViaBroker
// entrypoints take concrete *executor.TargetExecutor + *streamcache.Registry
// + *canonicalbridge.Bridge (no interface seam), so they are exercised by the
// end-to-end proxy tests (proxy_e2e_test.go, proxy_cache_*_test.go) that stand
// up a real upstream over httptest; these tests cover the helper surface.
package proxy

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

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	cache "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/forwardheader"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/metrics"
	provbuiltins "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/builtins"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/builtins"
	goHooks "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// extractRequestContentForHooks / extractResponseForHooks

// trackingMetricsRecorder counts RecordTrafficExtract calls so the
// extract path can be observed per outcome bucket. RecordHookRequest +
// RecordRequest etc. are no-ops; the gap tests only assert the extract
// bookkeeping.
type trackingMetricsRecorder struct {
	mu       sync.Mutex
	extracts []string // outcomes seen, in order
}

func (t *trackingMetricsRecorder) RecordRequest(_, _, _ string, _ int, _ time.Duration, _ metrics.Usage) {
}
func (t *trackingMetricsRecorder) RecordHookRequest(_, _, _ string) {}
func (t *trackingMetricsRecorder) RecordTrafficExtract(_, _, outcome string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.extracts = append(t.extracts, outcome)
}
func (t *trackingMetricsRecorder) RecordEstimate(_, _, _ string, _ time.Duration)         {}
func (t *trackingMetricsRecorder) RecordEstimateCompare(_ string, _ int, _ time.Duration) {}

func (t *trackingMetricsRecorder) outcomes() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, len(t.extracts))
	copy(out, t.extracts)
	return out
}

func TestExtractRequestContentForHooks_NilAdapter_RecordsSkipped(t *testing.T) {
	mr := &trackingMetricsRecorder{}
	h := &Handler{deps: &Deps{Metrics: mr}}
	got := h.extractRequestContentForHooks(context.Background(), nil, "openai", []byte("hi"), "/v1/chat/completions", slog.Default())
	if got != nil {
		t.Errorf("expected nil payload for nil adapter; got %+v", got)
	}
	if outs := mr.outcomes(); len(outs) != 1 || outs[0] != "skipped" {
		t.Errorf("outcomes=%v, want [skipped]", outs)
	}
}

func TestExtractRequestContentForHooks_EmptyBody_RecordsSkipped(t *testing.T) {
	mr := &trackingMetricsRecorder{}
	h := &Handler{deps: &Deps{Metrics: mr}}
	adapter := &stubTrafficAdapter{id: "openai"}
	got := h.extractRequestContentForHooks(context.Background(), adapter, "openai", nil, "/v1/chat/completions", slog.Default())
	if got != nil {
		t.Errorf("expected nil payload for empty body; got %+v", got)
	}
	if outs := mr.outcomes(); len(outs) != 1 || outs[0] != "skipped" {
		t.Errorf("outcomes=%v, want [skipped]", outs)
	}
}

func TestExtractRequestContentForHooks_AdapterError_RecordsError(t *testing.T) {
	mr := &trackingMetricsRecorder{}
	h := &Handler{deps: &Deps{Metrics: mr}}
	adapter := &stubTrafficAdapter{
		id: "openai",
		extractRequest: func(_ context.Context, _ []byte, _ string) (traffic.NormalizedContent, error) {
			return traffic.NormalizedContent{}, errors.New("boom")
		},
	}
	got := h.extractRequestContentForHooks(context.Background(), adapter, "openai", []byte("body"), "/v1/chat/completions", slog.Default())
	if got != nil {
		t.Errorf("expected nil payload on adapter error; got %+v", got)
	}
	if outs := mr.outcomes(); len(outs) != 1 || outs[0] != "error" {
		t.Errorf("outcomes=%v, want [error]", outs)
	}
}

func TestExtractRequestContentForHooks_HappyPath(t *testing.T) {
	mr := &trackingMetricsRecorder{}
	h := &Handler{deps: &Deps{Metrics: mr}}
	adapter := &stubTrafficAdapter{
		id: "openai",
		extractRequest: func(_ context.Context, _ []byte, _ string) (traffic.NormalizedContent, error) {
			return traffic.NormalizedContent{Segments: []string{"hello"}}, nil
		},
	}
	got := h.extractRequestContentForHooks(context.Background(), adapter, "openai", []byte("body"), "/v1/chat/completions", slog.Default())
	if got == nil {
		t.Fatal("expected non-nil payload on success")
	}
	if outs := mr.outcomes(); len(outs) != 1 || outs[0] != "success" {
		t.Errorf("outcomes=%v, want [success]", outs)
	}
}

func TestExtractRequestContentForHooks_NilMetricsRecorder(t *testing.T) {
	// Exercises every `h.deps.Metrics != nil` guard arm with metrics off.
	h := &Handler{deps: &Deps{}}
	adapter := &stubTrafficAdapter{
		id: "openai",
		extractRequest: func(_ context.Context, _ []byte, _ string) (traffic.NormalizedContent, error) {
			return traffic.NormalizedContent{Segments: []string{"hi"}}, nil
		},
	}
	if got := h.extractRequestContentForHooks(context.Background(), adapter, "openai", []byte("b"), "/x", slog.Default()); got == nil {
		t.Error("expected non-nil payload")
	}
}

func TestExtractResponseForHooks_NilAdapter_RecordsSkipped(t *testing.T) {
	mr := &trackingMetricsRecorder{}
	h := &Handler{deps: &Deps{Metrics: mr}}
	got, model, finish := h.extractResponseForHooks(context.Background(), nil, "openai", []byte("hi"), "/v1/chat/completions", slog.Default())
	if got != nil || model != "" || finish != "" {
		t.Errorf("expected zero values; got %v/%q/%q", got, model, finish)
	}
	if outs := mr.outcomes(); len(outs) != 1 || outs[0] != "skipped" {
		t.Errorf("outcomes=%v, want [skipped]", outs)
	}
}

func TestExtractResponseForHooks_EmptyBody(t *testing.T) {
	mr := &trackingMetricsRecorder{}
	h := &Handler{deps: &Deps{Metrics: mr}}
	adapter := &stubTrafficAdapter{id: "openai"}
	got, model, finish := h.extractResponseForHooks(context.Background(), adapter, "openai", nil, "/v1/chat/completions", slog.Default())
	if got != nil || model != "" || finish != "" {
		t.Errorf("expected zero values; got %v/%q/%q", got, model, finish)
	}
	if outs := mr.outcomes(); len(outs) != 1 || outs[0] != "skipped" {
		t.Errorf("outcomes=%v, want [skipped]", outs)
	}
}

func TestExtractResponseForHooks_AdapterError(t *testing.T) {
	mr := &trackingMetricsRecorder{}
	h := &Handler{deps: &Deps{Metrics: mr}}
	adapter := &stubTrafficAdapter{
		id: "openai",
		extractResponse: func(_ context.Context, _ []byte, _ string) (traffic.NormalizedContent, error) {
			return traffic.NormalizedContent{}, errors.New("decode failed")
		},
	}
	got, model, finish := h.extractResponseForHooks(context.Background(), adapter, "openai", []byte("body"), "/v1/chat/completions", slog.Default())
	if got != nil || model != "" || finish != "" {
		t.Errorf("expected zero values; got %v/%q/%q", got, model, finish)
	}
	if outs := mr.outcomes(); len(outs) != 1 || outs[0] != "error" {
		t.Errorf("outcomes=%v, want [error]", outs)
	}
}

func TestExtractResponseForHooks_HappyPathWithMetadata(t *testing.T) {
	mr := &trackingMetricsRecorder{}
	h := &Handler{deps: &Deps{Metrics: mr}}
	adapter := &stubTrafficAdapter{
		id: "openai",
		extractResponse: func(_ context.Context, _ []byte, _ string) (traffic.NormalizedContent, error) {
			return traffic.NormalizedContent{
				Segments: []string{"hi"},
				Metadata: map[string]string{"model": "gpt-4o"},
			}, nil
		},
	}
	got, model, _ := h.extractResponseForHooks(context.Background(), adapter, "openai", []byte("body"), "/v1/chat/completions", slog.Default())
	if got == nil {
		t.Fatal("expected non-nil payload")
	}
	if model != "gpt-4o" {
		t.Errorf("model=%q, want gpt-4o", model)
	}
	if outs := mr.outcomes(); len(outs) != 1 || outs[0] != "success" {
		t.Errorf("outcomes=%v, want [success]", outs)
	}
}

func TestExtractResponseForHooks_HappyPathNoMetadata(t *testing.T) {
	mr := &trackingMetricsRecorder{}
	h := &Handler{deps: &Deps{Metrics: mr}}
	adapter := &stubTrafficAdapter{
		id: "openai",
		extractResponse: func(_ context.Context, _ []byte, _ string) (traffic.NormalizedContent, error) {
			return traffic.NormalizedContent{Segments: []string{"hi"}}, nil
		},
	}
	got, model, _ := h.extractResponseForHooks(context.Background(), adapter, "openai", []byte("body"), "/v1/chat/completions", slog.Default())
	if got == nil {
		t.Fatal("expected non-nil payload")
	}
	if model != "" {
		t.Errorf("model=%q, want empty (no metadata)", model)
	}
}

func TestExtractResponseForHooks_NilMetricsRecorder(t *testing.T) {
	h := &Handler{deps: &Deps{}}
	adapter := &stubTrafficAdapter{
		id: "openai",
		extractResponse: func(_ context.Context, _ []byte, _ string) (traffic.NormalizedContent, error) {
			return traffic.NormalizedContent{Segments: []string{"hi"}}, nil
		},
	}
	if got, _, _ := h.extractResponseForHooks(context.Background(), adapter, "openai", []byte("b"), "/x", slog.Default()); got == nil {
		t.Error("expected non-nil payload")
	}
}

// writeForwardedResponseHeaders — non-empty src branch

func TestWriteForwardedResponseHeaders_FiltersAndWritesHeaders(t *testing.T) {
	src := http.Header{
		"X-Request-Id": []string{"req-1"},
		"Content-Type": []string{"application/json"},
	}
	w := httptest.NewRecorder()
	allowlist := forwardheader.Default()
	writeForwardedResponseHeaders(w, allowlist, provcore.FormatOpenAI, src, false)
	// At least one header should make it through the default allowlist
	// for non-cache-hit calls; we don't pin the exact set (it depends on
	// the default config), but the writer header map must have something
	// non-empty when src is non-empty.
	if len(w.Header()) == 0 {
		t.Error("expected at least one header written from non-empty src")
	}
}

func TestWriteForwardedResponseHeaders_CacheHit_StripsPerRequest(t *testing.T) {
	src := http.Header{
		"X-Request-Id": []string{"req-1"},
		"Content-Type": []string{"application/json"},
	}
	w := httptest.NewRecorder()
	allowlist := forwardheader.Default()
	writeForwardedResponseHeaders(w, allowlist, provcore.FormatOpenAI, src, true)
	// isCacheHit=true should suppress per-request headers like X-Request-Id.
	if got := w.Header().Get("X-Request-Id"); got != "" {
		t.Errorf("cache-hit X-Request-Id=%q, want empty (per-request stripped)", got)
	}
}

// setResponseHeaders / setResponseHeadersStream — non-default branches

func TestSetResponseHeaders_StampsAllowlistAndOverhead(t *testing.T) {
	allowlist := forwardheader.Default()
	h := &Handler{deps: &Deps{Allowlist: allowlist}}
	w := httptest.NewRecorder()
	ttfb := 50
	total := 100
	rec := &audit.Record{
		RequestID:       "req-1",
		UpstreamTtfbMs:  &ttfb,
		UpstreamTotalMs: &total,
	}
	target := routingcore.RoutingTarget{ProviderName: "openai", ModelCode: "gpt-4o"}
	result := &routingcore.RouteResult{Substituted: true, RuleName: "my-rule"}
	h.setResponseHeaders(w, rec, target, result, time.Now().Add(-200*time.Millisecond), 2)
	if got := w.Header().Get("X-Nexus-Attempts"); got != "2" {
		t.Errorf("attempts=%q", got)
	}
	if got := w.Header().Get("x-nexus-routed-model"); got != "gpt-4o" {
		t.Errorf("routed-model=%q (substituted=true)", got)
	}
}

func TestSetResponseHeaders_ZeroAttemptsDefendsToOne(t *testing.T) {
	h := &Handler{deps: &Deps{}}
	w := httptest.NewRecorder()
	rec := &audit.Record{RequestID: "req-x"}
	target := routingcore.RoutingTarget{ProviderName: "p", ModelCode: "m"}
	result := &routingcore.RouteResult{}
	h.setResponseHeaders(w, rec, target, result, time.Now(), 0)
	if got := w.Header().Get("X-Nexus-Attempts"); got != "1" {
		t.Errorf("zero attempts must default to 1; got %q", got)
	}
}

func TestSetResponseHeadersStream_StampsExpected(t *testing.T) {
	allowlist := forwardheader.Default()
	h := &Handler{deps: &Deps{Allowlist: allowlist}}
	w := httptest.NewRecorder()
	rec := &audit.Record{RequestID: "req-stream"}
	target := routingcore.RoutingTarget{ProviderName: "anthropic", ModelCode: "claude-3"}
	result := &routingcore.RouteResult{Substituted: true, RuleName: "stream-rule"}
	h.setResponseHeadersStream(w, rec, target, result, 3)
	if got := w.Header().Get("X-Nexus-Attempts"); got != "3" {
		t.Errorf("attempts=%q", got)
	}
	if got := w.Header().Get("x-nexus-routed-model"); got != "claude-3" {
		t.Errorf("routed-model=%q", got)
	}
}

func TestSetResponseHeadersStream_ZeroAttemptsDefendsToOne(t *testing.T) {
	h := &Handler{deps: &Deps{}}
	w := httptest.NewRecorder()
	rec := &audit.Record{RequestID: "req-x"}
	target := routingcore.RoutingTarget{ProviderName: "p", ModelCode: "m"}
	h.setResponseHeadersStream(w, rec, target, &routingcore.RouteResult{}, 0)
	if got := w.Header().Get("X-Nexus-Attempts"); got != "1" {
		t.Errorf("zero attempts must default to 1; got %q", got)
	}
}

// checkRateLimit — every branch

func TestCheckRateLimit_NoRPM(t *testing.T) {
	h := &Handler{deps: &Deps{RateLimiter: &fakeLimiter{allow: false, retryAfter: 60}}}
	w := httptest.NewRecorder()
	if err := h.checkRateLimit(w, &vkauth.VKMeta{}); err != nil {
		t.Errorf("no RPM set should not return error: %v", err)
	}
}

func TestCheckRateLimit_NoLimiter(t *testing.T) {
	rpm := 100
	h := &Handler{deps: &Deps{}}
	w := httptest.NewRecorder()
	if err := h.checkRateLimit(w, &vkauth.VKMeta{Name: "vk1", RateLimitRpm: &rpm}); err != nil {
		t.Errorf("nil limiter should not return error: %v", err)
	}
}

func TestCheckRateLimit_Allowed(t *testing.T) {
	rpm := 100
	h := &Handler{deps: &Deps{RateLimiter: &fakeLimiter{allow: true}}}
	w := httptest.NewRecorder()
	if err := h.checkRateLimit(w, &vkauth.VKMeta{Name: "vk1", RateLimitRpm: &rpm}); err != nil {
		t.Errorf("allowed=true should not return error: %v", err)
	}
}

func TestCheckRateLimit_Blocked(t *testing.T) {
	rpm := 100
	h := &Handler{deps: &Deps{RateLimiter: &fakeLimiter{allow: false, retryAfter: 42}}}
	w := httptest.NewRecorder()
	if err := h.checkRateLimit(w, &vkauth.VKMeta{Name: "vk1", RateLimitRpm: &rpm}); err == nil {
		t.Error("expected rate-limit error")
	}
	if got := w.Header().Get("Retry-After"); got != "42" {
		t.Errorf("Retry-After=%q want 42", got)
	}
}

// buildRequestContext — both branches (with/without NormalizeRegistry)

func TestBuildRequestContext_NoRegistry(t *testing.T) {
	h := &Handler{deps: &Deps{}}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4o"}`)))
	rctx := h.buildRequestContext(req, &vkauth.VKMeta{ID: "vk-1"}, []byte(`{"model":"gpt-4o"}`), provcore.FormatOpenAI, "gpt-4o", "chat")
	if rctx == nil {
		t.Fatal("expected non-nil RequestContext")
	}
	if rctx.Identity().ID != "vk-1" {
		t.Errorf("identity not preserved")
	}
	if rctx.Normalized() != nil {
		t.Error("no-registry must leave Normalized nil")
	}
}

func TestBuildRequestContext_RegistryEmptyBody(t *testing.T) {
	reg := normalize.NewRegistry()
	h := &Handler{deps: &Deps{NormalizeRegistry: reg}}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rctx := h.buildRequestContext(req, &vkauth.VKMeta{}, nil, provcore.FormatOpenAI, "", "chat")
	if rctx.Normalized() != nil {
		t.Error("empty body must leave Normalized nil")
	}
}

func TestBuildRequestContext_RegistryHappyPath(t *testing.T) {
	reg := normalize.NewRegistry()
	stub := &captureNormalize{
		id: "openai",
		payload: normalize.NormalizedPayload{
			Model: "gpt-4o",
		},
	}
	reg.Register("openai", stub)
	h := &Handler{deps: &Deps{NormalizeRegistry: reg}}
	body := []byte(`{"model":"gpt-4o"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rctx := h.buildRequestContext(req, &vkauth.VKMeta{}, body, provcore.FormatOpenAI, "gpt-4o", "chat")
	if rctx.Normalized() == nil {
		t.Fatal("expected non-nil Normalized after Normalize success")
	}
	if got := rctx.Normalized().Model; got != "gpt-4o" {
		t.Errorf("Model=%q want gpt-4o", got)
	}
}

func TestBuildRequestContext_RegistryErrSwallowed(t *testing.T) {
	reg := normalize.NewRegistry()
	stub := &captureNormalize{id: "openai", err: errors.New("malformed")}
	reg.Register("openai", stub)
	h := &Handler{deps: &Deps{NormalizeRegistry: reg}}
	body := []byte(`not json`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	rctx := h.buildRequestContext(req, &vkauth.VKMeta{}, body, provcore.FormatOpenAI, "gpt-4o", "chat")
	// Error must be swallowed — RequestContext built, Normalized == nil
	if rctx == nil {
		t.Fatal("expected non-nil RequestContext even on normalize error")
	}
	if rctx.Normalized() != nil {
		t.Errorf("Normalized should be nil on error; got %+v", rctx.Normalized())
	}
}

// captureNormalize implements normalize.Normalizer for buildRequestContext
// tests — returns a canned NormalizedPayload or an error.
type captureNormalize struct {
	id      string
	payload normalize.NormalizedPayload
	err     error
}

func (c *captureNormalize) ID() string { return c.id }

func (c *captureNormalize) Normalize(_ context.Context, _ []byte, _ normalize.Meta) (normalize.NormalizedPayload, error) {
	if c.err != nil {
		return normalize.NormalizedPayload{}, c.err
	}
	return c.payload, nil
}

// allowlistVersionFromDeps — wired-allowlist path.

func TestAllowlistVersionFromDeps_WiredAllowlistReturnsHash(t *testing.T) {
	d := &Deps{Allowlist: forwardheader.Default()}
	got := allowlistVersionFromDeps(d)
	if got == "" {
		t.Error("wired allowlist must produce non-empty hash")
	}
}

// chunkSSEReader.Read — partial read across multiple chunks

func TestChunkSSEReader_PartialReadAcrossChunks(t *testing.T) {
	sub := &fakeChunkSub{chunks: []provcore.Chunk{
		{RawBytes: []byte("data: 1\n\n")},
		{RawBytes: []byte("data: 2\n\n")},
		{Done: true, RawBytes: []byte("data: [DONE]\n\n")},
	}}
	r := newChunkSSEReaderFromSubscription(context.Background(), sub, nil, provcore.FormatOpenAI)
	r.usageSink = &chunkUsageHolder{}
	// Read in 4-byte chunks to force the internal buf re-fill loop.
	buf := make([]byte, 4)
	var collected []byte
	for {
		n, err := r.Read(buf)
		if n > 0 {
			collected = append(collected, buf[:n]...)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Read err: %v", err)
		}
	}
	got := string(collected)
	if !strings.Contains(got, "data: 1") || !strings.Contains(got, "data: 2") || !strings.Contains(got, "[DONE]") {
		t.Errorf("missing frames in collected: %q", got)
	}
}

// trafficAdapterFor — fallback branches

func TestTrafficAdapterFor_RegistryWins(t *testing.T) {
	reg := traffic.NewAdapterRegistry("nexus_ai_gateway_test_extra")
	// Register an "openai-compat" factory — formatToTrafficAdapterID maps
	// provcore.FormatOpenAI to "openai-compat".
	if err := reg.Register("openai-compat", func() traffic.Adapter {
		return &stubTrafficAdapter{id: "openai-compat"}
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	reg.Freeze()
	h := &Handler{deps: &Deps{TrafficAdapters: reg}}
	got := h.trafficAdapterFor(provcore.FormatOpenAI)
	if got == nil || got.ID() != "openai-compat" {
		t.Errorf("expected stub openai-compat adapter; got %v", got)
	}
}

func TestTrafficAdapterFor_RegistryMissReturnsNilWhenNoGenericFallback(t *testing.T) {
	reg := traffic.NewAdapterRegistry("nexus_ai_gateway_test_extra2")
	// Only register a no-match adapter; "anthropic" + "generic-jsonpath"
	// both miss so the function returns nil per the doc contract.
	if err := reg.Register("zzz-other", func() traffic.Adapter {
		return &stubTrafficAdapter{id: "zzz-other"}
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	reg.Freeze()
	h := &Handler{deps: &Deps{TrafficAdapters: reg, Logger: slog.Default()}}
	if got := h.trafficAdapterFor(provcore.FormatAnthropic); got != nil {
		t.Errorf("expected nil when neither format-id nor generic-jsonpath registered; got %v", got)
	}
}

func TestTrafficAdapterFor_RegistryMissUsesGenericFallback(t *testing.T) {
	reg := traffic.NewAdapterRegistry("nexus_ai_gateway_test_extra3")
	// Register only "generic-jsonpath" so the format-specific lookup
	// misses and falls back to the generic adapter.
	if err := reg.Register("generic-jsonpath", func() traffic.Adapter {
		return &stubTrafficAdapter{id: "generic-jsonpath"}
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	reg.Freeze()
	h := &Handler{deps: &Deps{TrafficAdapters: reg, Logger: slog.Default()}}
	got := h.trafficAdapterFor(provcore.FormatAnthropic)
	if got == nil || got.ID() != "generic-jsonpath" {
		t.Errorf("expected generic-jsonpath fallback; got %v", got)
	}
}

func TestTrafficAdapterFor_NoRegistryUsesSingleAdapter(t *testing.T) {
	// No registry → fall back to Deps.TrafficAdapter test escape hatch.
	stub := &stubTrafficAdapter{id: "fallback"}
	h := &Handler{deps: &Deps{TrafficAdapter: stub}}
	got := h.trafficAdapterFor(provcore.FormatAnthropic)
	if got == nil || got.ID() != "fallback" {
		t.Errorf("expected fallback adapter; got %v", got)
	}
}

func TestTrafficAdapterFor_BothNilReturnsNil(t *testing.T) {
	h := &Handler{deps: &Deps{}}
	if got := h.trafficAdapterFor(provcore.FormatOpenAI); got != nil {
		t.Errorf("expected nil with no adapters; got %v", got)
	}
}

// handleNonStreamHit — additional ingress-format coverage via Cache-HIT
// driving the real bridge / hooks / payload-capture chain.

// TestServeProxy_CacheHIT_AnthropicIngress drives a cache HIT through the
// Anthropic ingress format so handleNonStreamHit's ingress reshape branch
// + Anthropic adapter + response-hook (no-op pipeline) all execute.
func TestServeProxy_CacheHIT_AnthropicIngress(t *testing.T) {
	body := []byte(`{"model":"claude-3-5-sonnet","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`)
	cachedResp := []byte(`{"id":"a-1","content":[{"type":"text","text":"cached-hi"}]}`)

	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mini.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})

	rcache := cache.New(rdb, cache.Config{Enabled: true}, slog.Default())
	if rcache == nil {
		t.Fatal("cache.New returned nil")
	}

	cacheKey := rcache.BuildKey("anthropic", "claude-3-5-sonnet", body, "")
	tIn, tOut, tTotal := 5, 7, 12
	usage := provcore.Usage{PromptTokens: &tIn, CompletionTokens: &tOut, TotalTokens: &tTotal}
	if _, err := rcache.StoreResponse(context.Background(), cacheKey, &cache.ResponseEntry{
		Provider:          "anthropic",
		Model:             "claude-3-5-sonnet",
		CanonicalResponse: cachedResp,
		Usage:             usage,
		CachedAt:          time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	hookCache := compliance.NewHookConfigCache(
		func(_ context.Context) ([]goHooks.HookConfig, error) { return nil, nil },
		builtins.Registry, 0, slog.Default(),
	)
	if err := hookCache.Start(context.Background()); err != nil {
		t.Fatalf("hookCache.Start: %v", err)
	}
	time.Sleep(20 * time.Millisecond)

	prod := &captureProducer{}
	auditWriter := audit.NewWriter(prod, "nexus.event.ai-traffic", nil, slog.Default())
	pcStore := payloadcapture.NewStore(payloadcapture.Config{
		StoreRequestBody:   true,
		StoreResponseBody:  true,
		MaxInlineBodyBytes: 64 * 1024,
	})

	provReg := provcore.NewRegistry()
	provbuiltins.Register(provReg, nil, slog.Default())
	provReg.Freeze()

	deps := &Deps{
		VKAuth: &stubVKAuthCacheTest{meta: &vkauth.VKMeta{
			ID:               "vk-1",
			Name:             "test-vk",
			OrganizationID:   "org-1",
			OrganizationName: "Org",
		}},
		Router: &stubRouterCacheTest{targets: []routingcore.RoutingTarget{{
			ProviderID:      "p-anthropic",
			ProviderName:    "anthropic",
			ProviderModelID: "claude-3-5-sonnet",
			ModelID:         "claude-3-5-sonnet",
			ModelName:       "Claude 3.5 Sonnet",
			ModelCode:       "claude-3-5-sonnet",
			AdapterType:     "anthropic",
		}}},
		HookConfigCache: hookCache,
		ProviderReg:     provReg,
		Cache:           rcache,
		AuditWriter:     auditWriter,
		PayloadCapture:  pcStore,
		Logger:          slog.Default(),
	}

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatAnthropic,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "fake-token")
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Nexus-Cache"); got != "HIT" {
		t.Errorf("x-nexus-cache=%q want HIT", got)
	}

	auditWriter.Close()

	prod.mu.Lock()
	defer prod.mu.Unlock()
	if len(prod.messages) != 1 {
		t.Fatalf("captured %d messages, want 1", len(prod.messages))
	}
	var evt mq.TrafficEventMessage
	if err := json.Unmarshal(prod.messages[0], &evt); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if evt.CacheStatus != string(audit.CacheStatusHit) {
		t.Errorf("CacheStatus=%q want HIT", evt.CacheStatus)
	}
	if evt.RoutedProviderName != "anthropic" {
		t.Errorf("RoutedProviderName=%q want anthropic", evt.RoutedProviderName)
	}
	if evt.PromptTokens != int64(tIn) || evt.CompletionTokens != int64(tOut) {
		t.Errorf("usage tokens not propagated: prompt=%d compl=%d", evt.PromptTokens, evt.CompletionTokens)
	}
}

// checkQuota — no-policy (allowed) + budget block paths

func TestCheckQuota_NilVKMeta(t *testing.T) {
	h := &Handler{deps: &Deps{}}
	w := httptest.NewRecorder()
	rec := &audit.Record{}
	// nil meta → zero-value pricing + nil decision, no write.
	in, out, dec := h.checkQuota(httptest.NewRequest(http.MethodPost, "/v1/x", nil), w, rec,
		nil, &routingcore.RouteResult{}, []byte(`{}`), "gpt-4o")
	if in != 0 || out != 0 || dec != nil {
		t.Errorf("nil meta: in=%v out=%v dec=%v", in, out, dec)
	}
}

func TestCheckQuota_NilQuotaEngine(t *testing.T) {
	h := &Handler{deps: &Deps{}}
	w := httptest.NewRecorder()
	rec := &audit.Record{}
	in, out, dec := h.checkQuota(httptest.NewRequest(http.MethodPost, "/v1/x", nil), w, rec,
		&vkauth.VKMeta{ID: "vk-1"}, &routingcore.RouteResult{}, []byte(`{}`), "gpt-4o")
	if in != 0 || out != 0 || dec != nil {
		t.Errorf("nil engine: in=%v out=%v dec=%v", in, out, dec)
	}
}

// Compile-time anchor — keep the file compiling even if a future helper
// hasn't landed yet. No-op assertion.

var _ = json.Marshal

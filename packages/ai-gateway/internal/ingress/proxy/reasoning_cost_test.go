// reasoning_cost_test.go — F-0056: reasoning_cost_usd must be stamped
// consistently with reasoning_tokens on EVERY cost-stamp path (direct
// non-stream, broker non-stream, streaming, and cache HIT), not only the
// direct non-stream path. Each path funnels through stampReasoningCost.
package proxy

import (
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	cache "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

const reasoningOutPricePM = 10.0 // USD per 1M output tokens

// wantReasoningCost is the breakdown value for `tokens` reasoning tokens at the
// fixed output price used across these tests.
func wantReasoningCost(tokens int64) float64 {
	return float64(tokens) * reasoningOutPricePM / 1_000_000
}

func approxEqual(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

func TestStampReasoningCost(t *testing.T) {
	t.Run("non-zero tokens and price", func(t *testing.T) {
		rec := &audit.Record{ReasoningTokens: 100}
		stampReasoningCost(rec, reasoningOutPricePM)
		if !approxEqual(rec.ReasoningCostUsd, wantReasoningCost(100)) {
			t.Errorf("ReasoningCostUsd=%v want %v", rec.ReasoningCostUsd, wantReasoningCost(100))
		}
	})
	t.Run("zero tokens → zero cost", func(t *testing.T) {
		rec := &audit.Record{ReasoningTokens: 0}
		stampReasoningCost(rec, reasoningOutPricePM)
		if rec.ReasoningCostUsd != 0 {
			t.Errorf("ReasoningCostUsd=%v want 0", rec.ReasoningCostUsd)
		}
	})
	t.Run("zero price → zero cost", func(t *testing.T) {
		rec := &audit.Record{ReasoningTokens: 100}
		stampReasoningCost(rec, 0)
		if rec.ReasoningCostUsd != 0 {
			t.Errorf("ReasoningCostUsd=%v want 0", rec.ReasoningCostUsd)
		}
	})
}

func openAIIngressRequest(t *testing.T) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	return r.WithContext(WithIngress(r.Context(), Ingress{
		WireShape:  "openai-chat",
		BodyFormat: provcore.FormatOpenAI,
	}))
}

// Cache HIT (non-stream): handleNonStreamHit must stamp reasoning_cost_usd
// from the cached entry's reasoning tokens.
func TestHandleNonStreamHit_StampsReasoningCost(t *testing.T) {
	reasoning := 100
	entry := &cache.ResponseEntry{
		Provider: "openai", Model: "gpt-4o",
		CanonicalResponse: json.RawMessage(`{"id":"c","choices":[{"message":{"content":"hi"}}]}`),
		Usage: provcore.Usage{
			PromptTokens: iPtr(10), CompletionTokens: iPtr(5), TotalTokens: iPtr(15),
			ReasoningTokens: &reasoning,
		},
		CachedAt: time.Now().UTC(),
	}
	h := &Handler{deps: &Deps{Logger: slog.Default(), HookConfigCache: emptyHookCache(t)}}
	rec := &audit.Record{EndpointType: "chat"}
	target := routingcore.RoutingTarget{ProviderName: "openai", ModelCode: "gpt-4o", AdapterType: "openai"}

	h.handleNonStreamHit(openAIIngressRequest(t), httptest.NewRecorder(), rec, target,
		&routingcore.RouteResult{}, nil, entry, 2.0, reasoningOutPricePM, nil,
		"chat", "req", time.Now(), slog.Default())

	if rec.ReasoningTokens != 100 {
		t.Fatalf("ReasoningTokens=%d want 100", rec.ReasoningTokens)
	}
	if !approxEqual(rec.ReasoningCostUsd, wantReasoningCost(100)) {
		t.Errorf("ReasoningCostUsd=%v want %v", rec.ReasoningCostUsd, wantReasoningCost(100))
	}
}

// Cache HIT (stream): handleStreamHit must stamp reasoning_cost_usd from the
// cached stream entry's reasoning tokens.
func TestHandleStreamHit_StampsReasoningCost(t *testing.T) {
	reasoning := 100
	entry := &cache.StreamEntry{
		Provider: "openai", Model: "gpt-4o",
		Usage: provcore.Usage{
			PromptTokens: iPtr(10), CompletionTokens: iPtr(5), TotalTokens: iPtr(15),
			ReasoningTokens: &reasoning,
		},
		Chunks:   []cache.ChunkRecord{{Delta: "hi"}, {Done: true}},
		CachedAt: time.Now().UTC(),
	}
	h := &Handler{deps: &Deps{Logger: slog.Default(), HookConfigCache: emptyHookCache(t)}}
	rec := &audit.Record{EndpointType: "chat"}
	target := routingcore.RoutingTarget{ProviderName: "openai", ModelCode: "gpt-4o", AdapterType: "openai"}

	h.handleStreamHit(openAIIngressRequest(t), httptest.NewRecorder(), rec, target,
		&routingcore.RouteResult{}, nil, entry, 2.0, reasoningOutPricePM, nil,
		"chat", "req", time.Now(), slog.Default())

	if rec.ReasoningTokens != 100 {
		t.Fatalf("ReasoningTokens=%d want 100", rec.ReasoningTokens)
	}
	if !approxEqual(rec.ReasoningCostUsd, wantReasoningCost(100)) {
		t.Errorf("ReasoningCostUsd=%v want %v", rec.ReasoningCostUsd, wantReasoningCost(100))
	}
}

// Broker non-stream MISS: handleNonStreamWithSubscription must stamp
// reasoning_cost_usd from the terminal chunk's reasoning tokens.
func TestHandleNonStreamWithSubscription_StampsReasoningCost(t *testing.T) {
	reasoning := 100
	sub := &fakeChunkSub{chunks: []provcore.Chunk{
		{
			Delta: `{"id":"x","choices":[{"message":{"content":"hi"}}]}`,
			Usage: &provcore.Usage{
				PromptTokens: iPtr(10), CompletionTokens: iPtr(5), TotalTokens: iPtr(15),
				ReasoningTokens: &reasoning,
			},
			Done: true,
		},
	}}
	h := &Handler{deps: &Deps{Logger: slog.Default(), HookConfigCache: emptyHookCache(t)}}
	rec := &audit.Record{EndpointType: "chat", GatewayCacheStatus: audit.GatewayCacheMiss}
	target := routingcore.RoutingTarget{ProviderName: "openai", ModelCode: "gpt-4o", AdapterType: "openai"}

	h.handleNonStreamWithSubscription(openAIIngressRequest(t), httptest.NewRecorder(), rec, sub,
		target, nil, 2.0, reasoningOutPricePM, nil, "chat", "req", time.Now(), slog.Default(), nil)

	if rec.ReasoningTokens != 100 {
		t.Fatalf("ReasoningTokens=%d want 100", rec.ReasoningTokens)
	}
	if !approxEqual(rec.ReasoningCostUsd, wantReasoningCost(100)) {
		t.Errorf("ReasoningCostUsd=%v want %v", rec.ReasoningCostUsd, wantReasoningCost(100))
	}
}

// Broker non-stream HIT_INFLIGHT joiner: the joiner paid nothing, so the cost
// breakdown — including reasoning_cost_usd — must be zeroed even though the
// reasoning_tokens count is preserved.
func TestHandleNonStreamWithSubscription_HitInflight_ZeroesReasoningCost(t *testing.T) {
	reasoning := 100
	sub := &fakeChunkSub{chunks: []provcore.Chunk{
		{
			Delta: `{"id":"x","choices":[{"message":{"content":"hi"}}]}`,
			Usage: &provcore.Usage{
				PromptTokens: iPtr(10), CompletionTokens: iPtr(5), TotalTokens: iPtr(15),
				ReasoningTokens: &reasoning,
			},
			Done: true,
		},
	}}
	h := &Handler{deps: &Deps{Logger: slog.Default(), HookConfigCache: emptyHookCache(t)}}
	rec := &audit.Record{EndpointType: "chat", GatewayCacheStatus: audit.GatewayCacheHitInflight}
	target := routingcore.RoutingTarget{ProviderName: "openai", ModelCode: "gpt-4o", AdapterType: "openai"}

	h.handleNonStreamWithSubscription(openAIIngressRequest(t), httptest.NewRecorder(), rec, sub,
		target, nil, 2.0, reasoningOutPricePM, nil, "chat", "req", time.Now(), slog.Default(), nil)

	if rec.EstimatedCostUsd != 0 {
		t.Errorf("HIT_INFLIGHT EstimatedCostUsd=%v want 0", rec.EstimatedCostUsd)
	}
	if rec.ReasoningCostUsd != 0 {
		t.Errorf("HIT_INFLIGHT ReasoningCostUsd=%v want 0 (joiner pays nothing)", rec.ReasoningCostUsd)
	}
}

// Streaming MISS leader: handleStreamWithSubscription must stamp
// reasoning_cost_usd from the live stream's reasoning tokens.
func TestHandleStreamWithSubscription_Live_StampsReasoningCost(t *testing.T) {
	reasoning := 100
	sub := &fakeChunkSub{chunks: []provcore.Chunk{
		{Delta: "hi"},
		{
			Done: true,
			Usage: &provcore.Usage{
				PromptTokens: iPtr(10), CompletionTokens: iPtr(5), TotalTokens: iPtr(15),
				ReasoningTokens: &reasoning,
			},
		},
	}}
	h := &Handler{deps: &Deps{Logger: slog.Default(), HookConfigCache: emptyHookCache(t)}}
	// rec carries zero tokens so the live-usage extraction block runs.
	rec := &audit.Record{EndpointType: "chat", GatewayCacheStatus: audit.GatewayCacheMiss}
	target := routingcore.RoutingTarget{ProviderName: "openai", ModelCode: "gpt-4o", AdapterType: "openai"}

	h.handleStreamWithSubscription(openAIIngressRequest(t), httptest.NewRecorder(), rec, sub,
		target, nil, 2.0, reasoningOutPricePM, nil, "chat", "req", time.Now(), slog.Default())

	if rec.ReasoningTokens != 100 {
		t.Fatalf("ReasoningTokens=%d want 100", rec.ReasoningTokens)
	}
	if !approxEqual(rec.ReasoningCostUsd, wantReasoningCost(100)) {
		t.Errorf("ReasoningCostUsd=%v want %v", rec.ReasoningCostUsd, wantReasoningCost(100))
	}
}

// F-0058: a stream that faults mid-flight (headers already flushed with
// HTTP 200) must record a queryable usage_extraction_status="streaming_error"
// + ErrorCode, distinguishing it from a clean no-usage stream.
func TestHandleStreamWithSubscription_UpstreamError_StampsStreamingError(t *testing.T) {
	sub := &fakeChunkSub{err: errors.New("upstream stream broke")}
	h := &Handler{deps: &Deps{Logger: slog.Default(), HookConfigCache: emptyHookCache(t)}}
	rec := &audit.Record{EndpointType: "chat", GatewayCacheStatus: audit.GatewayCacheMiss}
	target := routingcore.RoutingTarget{ProviderName: "openai", ModelCode: "gpt-4o", AdapterType: "openai"}

	h.handleStreamWithSubscription(openAIIngressRequest(t), httptest.NewRecorder(), rec, sub,
		target, nil, 2.0, 2.0, nil, "chat", "req", time.Now(), slog.Default())

	if rec.UsageExtractionStatus != "streaming_error" {
		t.Errorf("UsageExtractionStatus=%q want streaming_error", rec.UsageExtractionStatus)
	}
	if rec.ErrorCode != "UPSTREAM_STREAM_ERROR" {
		t.Errorf("ErrorCode=%q want UPSTREAM_STREAM_ERROR", rec.ErrorCode)
	}
	// StatusCode stays 200 (headers already flushed) — the failure is
	// queryable via the status/error_code, not the HTTP code.
	if rec.StatusCode != http.StatusOK {
		t.Errorf("StatusCode=%d want 200 (SSE headers already flushed)", rec.StatusCode)
	}
}

// Streaming HIT_INFLIGHT joiner: reasoning_cost_usd zeroed with the rest of
// the cost breakdown.
func TestHandleStreamWithSubscription_HitInflight_ZeroesReasoningCost(t *testing.T) {
	reasoning := 100
	sub := &fakeChunkSub{chunks: []provcore.Chunk{
		{Delta: "hi"},
		{
			Done: true,
			Usage: &provcore.Usage{
				PromptTokens: iPtr(10), CompletionTokens: iPtr(5), TotalTokens: iPtr(15),
				ReasoningTokens: &reasoning,
			},
		},
	}}
	h := &Handler{deps: &Deps{Logger: slog.Default(), HookConfigCache: emptyHookCache(t)}}
	rec := &audit.Record{EndpointType: "chat", GatewayCacheStatus: audit.GatewayCacheHitInflight}
	target := routingcore.RoutingTarget{ProviderName: "openai", ModelCode: "gpt-4o", AdapterType: "openai"}

	h.handleStreamWithSubscription(openAIIngressRequest(t), httptest.NewRecorder(), rec, sub,
		target, nil, 2.0, reasoningOutPricePM, nil, "chat", "req", time.Now(), slog.Default())

	if rec.EstimatedCostUsd != 0 {
		t.Errorf("HIT_INFLIGHT EstimatedCostUsd=%v want 0", rec.EstimatedCostUsd)
	}
	if rec.ReasoningCostUsd != 0 {
		t.Errorf("HIT_INFLIGHT ReasoningCostUsd=%v want 0 (joiner pays nothing)", rec.ReasoningCostUsd)
	}
}

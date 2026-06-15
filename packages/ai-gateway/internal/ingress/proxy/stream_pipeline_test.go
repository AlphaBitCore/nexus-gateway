// stream_pipeline_test.go — characterization pins for the streaming
// subscription pipeline (handleStreamWithSubscription). Each test pins
// an observable business behavior of the SSE hot path: admin streaming-
// mode dispatch, transcoder override selection on cross-ingress cache
// hits and on the Responses auto-upgrade, provider prompt-cache token
// accounting, Prometheus usage recording, and teardown tolerance.
package proxy

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/metrics"
	provbuiltins "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/builtins"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// streamTestTarget is the routed upstream every pin in this file uses.
func streamTestTarget() routingcore.RoutingTarget {
	return routingcore.RoutingTarget{
		ProviderName: "openai",
		ProviderID:   "p-openai",
		ModelID:      "gpt-4o",
		ModelCode:    "gpt-4o",
		AdapterType:  "openai",
	}
}

// Admin streaming mode "passthrough" must be honored by the gateway:
// upstream SSE bytes relay to the client verbatim — no hook pipeline,
// no synthesized frames, and no OpenAI `data: [DONE]` sentinel appended
// (the sentinel is a live-pipeline behavior; passthrough opts the
// stream out of pipeline processing entirely).
func TestHandleStreamWithSubscription_PassthroughMode_RelaysRawBytesVerbatim(t *testing.T) {
	frame1 := "data: {\"choices\":[{\"delta\":{\"content\":\"raw one\"}}]}\n\n"
	frame2 := "data: {\"choices\":[{\"delta\":{\"content\":\"raw two\"}}]}\n\n"
	sub := &fakeChunkSub{chunks: []provcore.Chunk{
		{RawBytes: []byte(frame1)},
		{RawBytes: []byte(frame2)},
		{Done: true},
	}}
	h := &Handler{deps: &Deps{
		Logger:          slog.Default(),
		HookConfigCache: emptyHookCache(t),
		StreamingPolicy: streampolicy.NewStore(streampolicy.Policy{Mode: streampolicy.ModePassThrough}),
	}}
	rec := &audit.Record{EndpointType: "chat", GatewayCacheStatus: audit.GatewayCacheMiss}
	w := httptest.NewRecorder()

	h.handleStreamWithSubscription(openAIIngressRequest(t), w, rec, sub,
		streamTestTarget(), nil, 2.0, 2.0, nil, "chat", "req", time.Now(), slog.Default())

	body := w.Body.String()
	if body != frame1+frame2 {
		t.Errorf("passthrough body must be the raw upstream frames verbatim;\n got: %q\nwant: %q", body, frame1+frame2)
	}
	if strings.Contains(body, "[DONE]") {
		t.Error("passthrough mode must not append the OpenAI [DONE] sentinel — admin opted the stream out of pipeline processing")
	}
	if rec.StatusCode != http.StatusOK {
		t.Errorf("StatusCode=%d want 200", rec.StatusCode)
	}
}

// Provider prompt-cache token accounting on the live streaming path:
// cache_read / cache_creation token counts reported in the stream's
// usage frame must land on the audit record, and ProviderCacheStatus
// must classify a positive read count as a provider cache hit.
func TestHandleStreamWithSubscription_Live_StampsProviderCacheTokens(t *testing.T) {
	sub := &fakeChunkSub{chunks: []provcore.Chunk{
		{Delta: "hi"},
		{Done: true, Usage: &provcore.Usage{
			PromptTokens: iPtr(10), CompletionTokens: iPtr(5), TotalTokens: iPtr(15),
			CacheReadTokens: iPtr(7), CacheCreationTokens: iPtr(3),
		}},
	}}
	h := &Handler{deps: &Deps{Logger: slog.Default(), HookConfigCache: emptyHookCache(t)}}
	rec := &audit.Record{EndpointType: "chat", GatewayCacheStatus: audit.GatewayCacheMiss}

	h.handleStreamWithSubscription(openAIIngressRequest(t), httptest.NewRecorder(), rec, sub,
		streamTestTarget(), nil, 2.0, 2.0, nil, "chat", "req", time.Now(), slog.Default())

	if rec.CacheReadTokens != 7 {
		t.Errorf("CacheReadTokens=%d want 7", rec.CacheReadTokens)
	}
	if rec.CacheCreationTokens != 3 {
		t.Errorf("CacheCreationTokens=%d want 3", rec.CacheCreationTokens)
	}
	if rec.ProviderCacheStatus != audit.ProviderCacheHit {
		t.Errorf("ProviderCacheStatus=%q want %q (read tokens > 0)", rec.ProviderCacheStatus, audit.ProviderCacheHit)
	}
}

// streamMetricsCapture records the RecordRequest call the streaming
// pipeline makes at stream end; other MetricsRecorder methods are no-ops.
type streamMetricsCapture struct {
	mu       sync.Mutex
	calls    int
	provider string
	model    string
	endpoint string
	status   int
	usage    metrics.Usage
}

func (m *streamMetricsCapture) RecordRequest(provider, model, endpoint string, status int, _ time.Duration, usage metrics.Usage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	m.provider, m.model, m.endpoint, m.status, m.usage = provider, model, endpoint, status, usage
}
func (m *streamMetricsCapture) RecordHookRequest(_, _, _ string)                       {}
func (m *streamMetricsCapture) RecordTrafficExtract(_, _, _ string)                    {}
func (m *streamMetricsCapture) RecordEstimate(_, _, _ string, _ time.Duration)         {}
func (m *streamMetricsCapture) RecordEstimateCompare(_ string, _ int, _ time.Duration) {}

// A completed stream must record exactly one Prometheus request sample
// carrying the routed provider/model, the endpoint type, HTTP 200, and
// the usage extracted from the stream's terminal usage frame.
func TestHandleStreamWithSubscription_RecordsRequestMetricWithStreamedUsage(t *testing.T) {
	sub := &fakeChunkSub{chunks: []provcore.Chunk{
		{Delta: "hi"},
		{Done: true, Usage: &provcore.Usage{
			PromptTokens: iPtr(10), CompletionTokens: iPtr(5), TotalTokens: iPtr(15),
		}},
	}}
	mc := &streamMetricsCapture{}
	h := &Handler{deps: &Deps{Logger: slog.Default(), HookConfigCache: emptyHookCache(t), Metrics: mc}}
	rec := &audit.Record{EndpointType: "chat", GatewayCacheStatus: audit.GatewayCacheMiss}

	h.handleStreamWithSubscription(openAIIngressRequest(t), httptest.NewRecorder(), rec, sub,
		streamTestTarget(), nil, 2.0, 2.0, nil, "chat", "req", time.Now(), slog.Default())

	mc.mu.Lock()
	defer mc.mu.Unlock()
	if mc.calls != 1 {
		t.Fatalf("RecordRequest calls=%d want 1", mc.calls)
	}
	if mc.provider != "openai" || mc.model != "gpt-4o" || mc.endpoint != "chat" {
		t.Errorf("RecordRequest labels=(%q,%q,%q) want (openai,gpt-4o,chat)", mc.provider, mc.model, mc.endpoint)
	}
	if mc.status != http.StatusOK {
		t.Errorf("RecordRequest status=%d want 200", mc.status)
	}
	if mc.usage.PromptTokens != 10 || mc.usage.CompletionTokens != 5 || mc.usage.TotalTokens != 15 {
		t.Errorf("RecordRequest usage=%+v want {10 5 15}", mc.usage)
	}
}

// Responses auto-upgrade: the client sent /v1/chat/completions but the
// upstream call was upgraded to /v1/responses, so upstream SSE frames
// arrive in Responses event grammar. The pipeline must re-encode the
// canonical chunks into chat.completion.chunk frames — never forward
// the Responses-grammar RawBytes to a chat-completions SDK.
func TestHandleStreamWithSubscription_ResponsesUpgrade_ReencodesToChatCompletionsSSE(t *testing.T) {
	rawResponsesFrame := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"
	sub := &fakeChunkSub{chunks: []provcore.Chunk{
		{Delta: "hi", RawBytes: []byte(rawResponsesFrame)},
		{Done: true, Usage: &provcore.Usage{
			PromptTokens: iPtr(2), CompletionTokens: iPtr(1), TotalTokens: iPtr(3),
		}},
	}}
	h := &Handler{deps: &Deps{Logger: slog.Default(), HookConfigCache: emptyHookCache(t)}}
	rec := &audit.Record{EndpointType: "chat", GatewayCacheStatus: audit.GatewayCacheMiss}
	r := openAIIngressRequest(t)
	r = r.WithContext(WithResponsesUpgrade(r.Context()))
	w := httptest.NewRecorder()

	h.handleStreamWithSubscription(r, w, rec, sub,
		streamTestTarget(), nil, 2.0, 2.0, nil, "chat", "req", time.Now(), slog.Default())

	body := w.Body.String()
	if !strings.Contains(body, "chat.completion.chunk") {
		t.Errorf("upgraded stream must re-encode to chat.completion.chunk frames; body=%q", body)
	}
	if !strings.Contains(body, "hi") {
		t.Errorf("delta content missing from re-encoded stream; body=%q", body)
	}
	if strings.Contains(body, "response.output_text.delta") {
		t.Errorf("Responses-grammar RawBytes leaked to a chat-completions client; body=%q", body)
	}
}

// Cross-ingress stream cache HIT where the entry's origin wire shape
// has no Format mapping (e.g. a future Gemini cache lane): the origin
// override must be skipped, falling back to the standard (ingress,
// target) transcoder selection — here OpenAI→OpenAI passthrough, so the
// cached raw frames relay verbatim instead of dispatching to an
// unconfigured codec.
func TestHandleStreamWithSubscription_UnmappedHitOrigin_KeepsDefaultTranscoderSelection(t *testing.T) {
	rawFrame := "data: {\"choices\":[{\"delta\":{\"content\":\"native\"}}]}\n\n"
	sub := &fakeChunkSub{chunks: []provcore.Chunk{
		{RawBytes: []byte(rawFrame)},
		{Done: true},
	}}
	h := &Handler{deps: &Deps{
		Logger:          slog.Default(),
		HookConfigCache: emptyHookCache(t),
		CanonicalBridge: canonicalbridge.New(provbuiltins.SchemaCodecs(slog.Default())),
	}}
	rec := &audit.Record{EndpointType: "chat", GatewayCacheStatus: audit.GatewayCacheHit}
	r := openAIIngressRequest(t)
	r = r.WithContext(WithStreamHitOrigin(r.Context(), StreamHitOrigin{
		WireShape: typology.WireShapeGeminiGenerateContent, // no Format mapping
	}))
	w := httptest.NewRecorder()

	h.handleStreamWithSubscription(r, w, rec, sub,
		streamTestTarget(), nil, 2.0, 2.0, nil, "chat", "req", time.Now(), slog.Default())

	if !strings.Contains(w.Body.String(), rawFrame) {
		t.Errorf("unmapped origin must fall back to passthrough selection (raw frames verbatim); body=%q", w.Body.String())
	}
}

// Cross-ingress stream cache HIT, OpenAI-compat family: the entry was
// written under a plain OpenAI ingress but is replayed to a client on
// an OpenAI-compat sibling format (deepseek via the body-format
// override). The (deepseek, openai) pair resolves to the family
// passthrough (nil transcoder), so without the origin override the
// cached canonical chunks would fall back to the minimal delta
// envelope. The override must force the explicit chat-completions
// encoder so the client receives full chat.completion.chunk frames.
func TestHandleStreamWithSubscription_CompatIngress_OpenAIOriginHit_ForcesChatCompletionsEncoder(t *testing.T) {
	sub := &fakeChunkSub{chunks: []provcore.Chunk{
		{Delta: "cached text"}, // replayed canonical chunk: no RawBytes
		{Done: true},
	}}
	h := &Handler{deps: &Deps{
		Logger:          slog.Default(),
		HookConfigCache: emptyHookCache(t),
		CanonicalBridge: canonicalbridge.New(provbuiltins.SchemaCodecs(slog.Default())),
	}}
	rec := &audit.Record{EndpointType: "chat", GatewayCacheStatus: audit.GatewayCacheHit}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx := WithIngress(r.Context(), Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatDeepSeek,
	})
	ctx = WithStreamHitOrigin(ctx, StreamHitOrigin{WireShape: typology.WireShapeOpenAIChat})
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()

	h.handleStreamWithSubscription(r, w, rec, sub,
		streamTestTarget(), nil, 2.0, 2.0, nil, "chat", "req", time.Now(), slog.Default())

	body := w.Body.String()
	if !strings.Contains(body, "chat.completion.chunk") {
		t.Errorf("origin override must force the chat-completions encoder (full envelope), not the minimal delta fallback; body=%q", body)
	}
	if !strings.Contains(body, "cached text") {
		t.Errorf("cached delta missing from replay; body=%q", body)
	}
}

// A failing subscription Close must never disturb the already-delivered
// response: the stream still relays, the audit row still records 200,
// and the close error is swallowed (logged at debug, not surfaced).
func TestHandleStreamWithSubscription_SubscriptionCloseError_ResponseUnaffected(t *testing.T) {
	sub := &fakeChunkSub{
		chunks: []provcore.Chunk{
			{Delta: "hi"},
			{Done: true, Usage: &provcore.Usage{
				PromptTokens: iPtr(1), CompletionTokens: iPtr(1), TotalTokens: iPtr(2),
			}},
		},
		closeErr: errors.New("subscription teardown failed"),
	}
	h := &Handler{deps: &Deps{Logger: slog.Default(), HookConfigCache: emptyHookCache(t)}}
	rec := &audit.Record{EndpointType: "chat", GatewayCacheStatus: audit.GatewayCacheMiss}
	w := httptest.NewRecorder()

	h.handleStreamWithSubscription(openAIIngressRequest(t), w, rec, sub,
		streamTestTarget(), nil, 2.0, 2.0, nil, "chat", "req", time.Now(), slog.Default())

	if !sub.closed {
		t.Error("subscription must be closed at stream end")
	}
	if rec.StatusCode != http.StatusOK {
		t.Errorf("StatusCode=%d want 200 — a close error must not change the recorded outcome", rec.StatusCode)
	}
	if !strings.Contains(w.Body.String(), "hi") {
		t.Errorf("delta missing from delivered stream; body=%q", w.Body.String())
	}
	if rec.UsageExtractionStatus != "streaming_reported" {
		t.Errorf("UsageExtractionStatus=%q want streaming_reported — usage was delivered before the failing close", rec.UsageExtractionStatus)
	}
}

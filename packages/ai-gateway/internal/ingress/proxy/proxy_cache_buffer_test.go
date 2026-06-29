package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/builtins"
	goHooks "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/openai"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// testWriter is a minimal http.ResponseWriter shim wrapping a bytes.Buffer so
// tests can assert what the streaming handlers wrote without an
// httptest.ResponseRecorder. Shared by the dispatch / live / passthrough tests.
type testWriter struct {
	*bytes.Buffer
	header     http.Header
	statusCode int
}

func (w *testWriter) Header() http.Header    { return w.header }
func (w *testWriter) WriteHeader(status int) { w.statusCode = status }

// newPiiRedactResponseHookCache builds a HookConfigCache serving one
// RESPONSE-stage PII-redact hook (email → [REDACTED_EMAIL]). It drives the
// canonical buffer through the Modify path. Mirror of newPiiRedactHookCache
// (request stage) — the only difference is Stage="response".
func newPiiRedactResponseHookCache(t *testing.T) *compliance.HookConfigCache {
	t.Helper()
	loader := func(_ context.Context) ([]goHooks.HookConfig, error) {
		return []goHooks.HookConfig{{
			ID:                "pii-resp-1",
			ImplementationID:  "pii-detector",
			Name:              "pii-detect-response",
			Priority:          10,
			Enabled:           true,
			Stage:             "response",
			FailBehavior:      "fail-closed",
			TimeoutMs:         1000,
			ApplicableIngress: []string{"ALL"},
			Config: map[string]any{
				"onMatch": map[string]any{
					"inflightAction": "redact",
					"storageAction":  "redact",
				},
				"patternDefinitions": []any{
					map[string]any{
						"id":          "email",
						"regex":       `[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}`,
						"flags":       "i",
						"replacement": "[REDACTED_EMAIL]",
					},
				},
			},
		}}, nil
	}
	cache := compliance.NewHookConfigCache(loader, builtins.Registry, 0, noopLogger())
	if err := cache.Start(context.Background()); err != nil {
		t.Fatalf("cache.Start: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	return cache
}

// newRejectingResponseHookCache wires a RESPONSE-stage hook that always returns
// RejectHard, to drive the canonical buffer's hard-block path.
func newRejectingResponseHookCache(t *testing.T) *compliance.HookConfigCache {
	t.Helper()
	reg := builtins.Registry.Clone()
	reg.Register("rejecting-resp-hook", func(_ *goHooks.HookConfig) (goHooks.Hook, error) {
		return &rejectingHook{rule: &goHooks.BlockingRule{Pack: "content-safety", RuleID: "secret", Severity: "hard"}}, nil
	})
	reg.Freeze()
	loader := func(_ context.Context) ([]goHooks.HookConfig, error) {
		return []goHooks.HookConfig{{
			ID:                "reject-resp-1",
			ImplementationID:  "rejecting-resp-hook",
			Name:              "reject-response",
			Priority:          1,
			Enabled:           true,
			Stage:             "response",
			FailBehavior:      "fail-closed",
			TimeoutMs:         1000,
			ApplicableIngress: []string{"ALL"},
			Config:            map[string]any{},
		}}, nil
	}
	cache := compliance.NewHookConfigCache(loader, reg, 0, noopLogger())
	if err := cache.Start(context.Background()); err != nil {
		t.Fatalf("cache.Start: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	return cache
}

// bufferTestState builds a streamState wired for a canonical-buffer unit test:
// a Handler with the supplied response-hook cache + OpenAI traffic adapter, a
// scripted ChunkSubscription, the requested ingress (in context + on the
// state), and a transcoder selected for that ingress.
func bufferTestState(t *testing.T, cache *compliance.HookConfigCache, ingress Ingress, transcoder canonicalbridge.StreamTranscoder, chunks []provcore.Chunk) (*streamState, *testWriter, *chunkUsageHolder) {
	t.Helper()
	h := &Handler{deps: &Deps{
		HookConfigCache: cache,
		TrafficAdapter:  &openai.Adapter{},
		Logger:          noopLogger(),
	}}
	w := &testWriter{Buffer: &bytes.Buffer{}, header: http.Header{}}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req = req.WithContext(WithIngress(req.Context(), ingress))
	s := &streamState{
		h:             h,
		w:             w,
		r:             req,
		rec:           &audit.Record{RequestID: "buf-rec"},
		sub:           &fakeChunkSub{chunks: chunks},
		target:        routingcore.RoutingTarget{ModelCode: "m-test", Region: "us-east-1"},
		transcoder:    transcoder,
		ingressFormat: ingress.BodyFormat,
		emitDone:      ingress.BodyFormat.IsOpenAIFamily(),
		requestID:     "buf-rec",
		logger:        noopLogger(),
	}
	return s, w, &chunkUsageHolder{}
}

// openAIChatIngress is an OpenAI-compat chat ingress (transcoder == nil
// passthrough; the buffer re-encodes Modify via NewChatCompletionsStreamEncoder).
var openAIChatIngress = Ingress{WireShape: typology.WireShapeOpenAIChat, BodyFormat: provcore.FormatOpenAI}

// anthropicChatIngress drives the S-canon proof: redaction on the OpenAI
// canonical waist, re-encoded to ANTHROPIC wire. WireShape stays OpenAIChat so
// the response pipeline resolves endpoint kind = chat (mirrors the non-stream
// Anthropic ingress test); BodyFormat=Anthropic selects the Anthropic encoder
// and suppresses the OpenAI [DONE] sentinel.
var anthropicChatIngress = Ingress{WireShape: typology.WireShapeOpenAIChat, BodyFormat: provcore.FormatAnthropic}

// TestCanonicalBuffer_OpenAIIngress_Modify_MasksContent proves buffer mode
// actually redacts a Modify decision on an OpenAI-ingress stream: the delivered
// wire carries the masked content and the original PII is NEVER delivered.
func TestCanonicalBuffer_OpenAIIngress_Modify_MasksContent(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	chunks := []provcore.Chunk{
		{Delta: "contact "},
		{Delta: "alice@example.com"},
		{Delta: " now", Done: true},
	}
	s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, chunks)

	s.h.runCanonicalBufferStream(context.Background(), s, w, usage)

	out := w.String()
	if strings.Contains(out, "alice@example.com") {
		t.Fatalf("original PII leaked to client: %q", out)
	}
	if !strings.Contains(out, "[REDACTED_EMAIL]") {
		t.Fatalf("expected masked content in delivered stream, got %q", out)
	}
	if !s.rec.ResponseHookRewritten {
		t.Error("rec.ResponseHookRewritten should be true after a Modify")
	}
	if s.rec.ResponseAction != goHooks.ActionRedact {
		t.Errorf("rec.ResponseAction = %q, want redact", s.rec.ResponseAction)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Errorf("OpenAI ingress must terminate with [DONE]; got tail %q", lastN(out, 40))
	}
}

// TestCanonicalBuffer_AnthropicIngress_Modify_ReEncodesToAnthropic proves
// S-canon: redaction runs on the OpenAI canonical body, then re-encodes to
// ANTHROPIC wire. The delivered Anthropic frames carry masked text and never
// the original PII — proving redaction is NOT OpenAI-wire-only.
func TestCanonicalBuffer_AnthropicIngress_Modify_ReEncodesToAnthropic(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	transcoder := canonicalbridge.New(nil).NewStreamTranscoder(provcore.FormatAnthropic, provcore.FormatOpenAI, "m-test")
	if transcoder == nil {
		t.Fatal("expected a non-nil Anthropic stream transcoder")
	}
	chunks := []provcore.Chunk{
		{Delta: "mail alice@example.com please", Done: true},
	}
	s, w, usage := bufferTestState(t, cache, anthropicChatIngress, transcoder, chunks)

	s.h.runCanonicalBufferStream(context.Background(), s, w, usage)

	out := w.String()
	if strings.Contains(out, "alice@example.com") {
		t.Fatalf("original PII leaked to Anthropic client: %q", out)
	}
	if !strings.Contains(out, "[REDACTED_EMAIL]") {
		t.Fatalf("expected masked content in Anthropic frames, got %q", out)
	}
	// Must be Anthropic wire grammar, not OpenAI chat.completion.chunk.
	if !strings.Contains(out, "event: content_block_delta") || !strings.Contains(out, "text_delta") {
		t.Errorf("expected Anthropic content_block_delta/text_delta frames, got %q", out)
	}
	if !strings.Contains(out, "message_stop") {
		t.Errorf("expected Anthropic message_stop terminator, got %q", out)
	}
	if strings.Contains(out, "[DONE]") {
		t.Errorf("Anthropic ingress must NOT emit OpenAI [DONE], got %q", out)
	}
}

// TestCanonicalBuffer_ToolArgs_Masked proves tool-call argument PII is masked in
// buffer mode (the BUG-toolcall canonical path: arguments are scanned + rewritten
// on the canonical waist, then re-encoded to the wire).
func TestCanonicalBuffer_ToolArgs_Masked(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	chunks := []provcore.Chunk{
		{ToolCallDeltas: []provcore.ToolCallDelta{{Index: 0, ID: "call_1", Name: "send_email"}}},
		{ToolCallDeltas: []provcore.ToolCallDelta{{Index: 0, Arguments: `{"to":"alice@example.com"}`}}, Done: true},
	}
	s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, chunks)

	s.h.runCanonicalBufferStream(context.Background(), s, w, usage)

	out := w.String()
	if strings.Contains(out, "alice@example.com") {
		t.Fatalf("tool-call argument PII leaked to client: %q", out)
	}
	if !strings.Contains(out, "[REDACTED_EMAIL]") {
		t.Fatalf("expected masked tool-arg in delivered stream, got %q", out)
	}
	// The tool call structure (name) must survive the rewrite.
	if !strings.Contains(out, "send_email") {
		t.Errorf("tool call name must be preserved through the redact re-encode, got %q", out)
	}
}

// TestCanonicalBuffer_HardBlock_ZeroContent proves a RejectHard delivers ONLY an
// in-band SSE error frame: zero content bytes, the original content never
// delivered, and ResponseBodyRedacted left nil so storage stores NULL (no leak).
func TestCanonicalBuffer_HardBlock_ZeroContent(t *testing.T) {
	cache := newRejectingResponseHookCache(t)
	chunks := []provcore.Chunk{
		{Delta: "the secret password is hunter2", Done: true},
	}
	s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, chunks)

	s.h.runCanonicalBufferStream(context.Background(), s, w, usage)

	out := w.String()
	if strings.Contains(out, "hunter2") {
		t.Fatalf("hard-blocked stream leaked content: %q", out)
	}
	if !strings.Contains(out, `"error"`) {
		t.Errorf("expected an SSE error frame on hard block, got %q", out)
	}
	if s.rec.ResponseAction != goHooks.ActionBlock {
		t.Errorf("rec.ResponseAction = %q, want block", s.rec.ResponseAction)
	}
	if s.rec.ResponseHookRewritten {
		t.Error("a hard block is not a rewrite; ResponseHookRewritten must stay false")
	}
	if s.rec.ResponseBodyRedacted != nil {
		t.Error("ResponseBodyRedacted must stay nil on a hard block (storage stores NULL, no leak)")
	}
}

// TestCanonicalBuffer_Approve_ForwardsContent proves the common case: no PII →
// Approve → the buffered content is delivered to the client unchanged.
func TestCanonicalBuffer_Approve_ForwardsContent(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	chunks := []provcore.Chunk{
		{Delta: "hello "},
		{Delta: "world", Done: true},
	}
	s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, chunks)

	s.h.runCanonicalBufferStream(context.Background(), s, w, usage)

	out := w.String()
	if !strings.Contains(out, "hello") || !strings.Contains(out, "world") {
		t.Fatalf("approved content must be delivered, got %q", out)
	}
	if s.rec.ResponseHookRewritten {
		t.Error("no rewrite expected on Approve")
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Errorf("OpenAI ingress must terminate with [DONE], got tail %q", lastN(out, 40))
	}
}

// TestCanonicalBuffer_UsagePreserved proves usage accounting survives the
// restructure: usage observed on the canonical chunks lands on the usageHolder
// the accounting stage reads, AND is re-emitted on the wire.
func TestCanonicalBuffer_UsagePreserved(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	pt, ct, tt := 11, 22, 33
	chunks := []provcore.Chunk{
		{Delta: "hi", Done: true, Usage: &provcore.Usage{PromptTokens: &pt, CompletionTokens: &ct, TotalTokens: &tt}},
	}
	s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, chunks)

	s.h.runCanonicalBufferStream(context.Background(), s, w, usage)

	snap := usage.snapshot()
	if snap.TotalTokens == nil || *snap.TotalTokens != 33 {
		t.Fatalf("usageHolder total = %v, want 33", snap.TotalTokens)
	}
	if snap.PromptTokens == nil || *snap.PromptTokens != 11 {
		t.Errorf("usageHolder prompt = %v, want 11", snap.PromptTokens)
	}
	if !strings.Contains(w.String(), "total_tokens") {
		t.Errorf("expected usage frame re-emitted on the wire, got %q", w.String())
	}
}

// TestCanonicalBuffer_CacheHit_ReRedacts proves R5: the streamcache stores
// PRE-redaction canonical chunks (the broker collects raw chunks); a buffer HIT
// replays them through the SAME path, so redaction re-runs on the HIT. The
// subscription source is irrelevant — a replay sub yielding pre-redaction PII is
// redacted identically to a live MISS.
func TestCanonicalBuffer_CacheHit_ReRedacts(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	// Pre-redaction canonical chunks, as a streamcache replay would yield them.
	replayChunks := []provcore.Chunk{
		{Delta: "cached reply to bob@example.com", Done: true},
	}
	s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, replayChunks)

	s.h.runCanonicalBufferStream(context.Background(), s, w, usage)

	out := w.String()
	if strings.Contains(out, "bob@example.com") {
		t.Fatalf("cache HIT must re-run redaction; PII leaked: %q", out)
	}
	if !strings.Contains(out, "[REDACTED_EMAIL]") {
		t.Fatalf("expected masked content on cache HIT, got %q", out)
	}
}

// TestCanonicalBuffer_ProviderError_SynthesizesErrorFrame proves a subscription
// fault delivers an ingress-shaped SSE error frame and classifies the stream as
// an upstream error for the accounting stage.
func TestCanonicalBuffer_ProviderError_SynthesizesErrorFrame(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, nil)
	s.sub = &fakeChunkSub{err: &provcore.ProviderError{Status: http.StatusBadGateway, Code: provcore.CodeUpstreamError, Message: "boom"}}

	term := s.h.runCanonicalBufferStream(context.Background(), s, w, usage)

	if !strings.Contains(w.String(), `"error"`) {
		t.Errorf("expected SSE error frame on provider fault, got %q", w.String())
	}
	if te := term.terminalError(); te == nil || te.code != streamErrCodeUpstream {
		t.Errorf("expected terminal classification %q, got %+v", streamErrCodeUpstream, te)
	}
}

// TestCanonicalBuffer_GenericUpstreamFault_SynthesizesFrame proves a non-typed
// subscription error (not *provcore.ProviderError) still produces an
// ingress-shaped 502 error frame and an upstream classification.
func TestCanonicalBuffer_GenericUpstreamFault_SynthesizesFrame(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, nil)
	s.sub = &fakeChunkSub{err: errors.New("raw upstream read failure")}

	term := s.h.runCanonicalBufferStream(context.Background(), s, w, usage)

	if !strings.Contains(w.String(), `"error"`) {
		t.Errorf("expected synthesized error frame, got %q", w.String())
	}
	if te := term.terminalError(); te == nil || te.code != streamErrCodeUpstream {
		t.Errorf("expected %q classification, got %+v", streamErrCodeUpstream, te)
	}
}

// TestCanonicalBuffer_NilSubOrTee_NoOp pins the defensive guard.
func TestCanonicalBuffer_NilSubOrTee_NoOp(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, nil)
	s.sub = nil
	if term := s.h.runCanonicalBufferStream(context.Background(), s, w, usage); term == nil {
		t.Fatal("expected a non-nil terminal carrier even on nil sub")
	}
	if w.Len() != 0 {
		t.Errorf("nil sub must not write to the client, got %q", w.String())
	}
}

// TestCanonicalBuffer_Concurrent_PerCallState proves accumulation is per-call:
// many buffered streams run concurrently under -race, each masking only its own
// PII with no cross-talk.
func TestCanonicalBuffer_Concurrent_PerCallState(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			chunks := []provcore.Chunk{
				{Delta: "x carol@example.com y", Done: true},
			}
			s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, chunks)
			s.h.runCanonicalBufferStream(context.Background(), s, w, usage)
			out := w.String()
			if strings.Contains(out, "carol@example.com") {
				t.Errorf("per-call leak under concurrency: %q", out)
			}
			if !strings.Contains(out, "[REDACTED_EMAIL]") {
				t.Errorf("missing mask under concurrency: %q", out)
			}
		}()
	}
	wg.Wait()
}

// TestCanonicalStreamAccumulator_BuildsCanonicalBody asserts the aggregator folds
// content + tool-call deltas into the canonical chat-completion shape the OpenAI
// extractor/rewriter consumes.
func TestCanonicalStreamAccumulator_BuildsCanonicalBody(t *testing.T) {
	acc := newCanonicalStreamAccumulator("m")
	acc.add(provcore.Chunk{Delta: "hel", ReasoningDelta: "th"})
	acc.add(provcore.Chunk{Delta: "lo", ToolCallDeltas: []provcore.ToolCallDelta{{Index: 0, ID: "c1", Name: "fn"}}})
	acc.add(provcore.Chunk{ToolCallDeltas: []provcore.ToolCallDelta{{Index: 0, Arguments: `{"a":1}`}}})

	body := acc.canonicalBody()
	if got := gjson.GetBytes(body, "choices.0.message.content").String(); got != "hello" {
		t.Errorf("content = %q, want hello", got)
	}
	if got := gjson.GetBytes(body, "choices.0.message.tool_calls.0.function.name").String(); got != "fn" {
		t.Errorf("tool name = %q, want fn", got)
	}
	if got := gjson.GetBytes(body, "choices.0.message.tool_calls.0.function.arguments").String(); got != `{"a":1}` {
		t.Errorf("tool args = %q, want {\"a\":1}", got)
	}
	if acc.reasoning.String() != "th" {
		t.Errorf("reasoning = %q, want th", acc.reasoning.String())
	}

	// syntheticChunkFromCanonical round-trips the (redacted) body back to chunk.
	ch := syntheticChunkFromCanonical(body, acc.reasoning.String())
	if ch.Delta != "hello" {
		t.Errorf("synthetic Delta = %q, want hello", ch.Delta)
	}
	if ch.ReasoningDelta != "th" {
		t.Errorf("synthetic ReasoningDelta = %q, want th", ch.ReasoningDelta)
	}
	if len(ch.ToolCallDeltas) != 1 || ch.ToolCallDeltas[0].Name != "fn" || ch.ToolCallDeltas[0].Arguments != `{"a":1}` {
		t.Errorf("synthetic tool calls = %+v", ch.ToolCallDeltas)
	}
}

// TestStreamRelayStage_BufferMode_RedactsAndStores proves the relay stage routes
// buffer mode to the canonical buffer (NOT the wire dispatch): the client gets
// the masked stream and the redacted transcript is stamped onto the audit record
// for storage (ResponseBodyRedacted), never the original PII.
func TestStreamRelayStage_BufferMode_RedactsAndStores(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	chunks := []provcore.Chunk{{Delta: "reach alice@example.com", Done: true}}
	s, _, _ := bufferTestState(t, cache, openAIChatIngress, nil, chunks)
	// Store response bodies so the relay stamps the redacted copy.
	s.h.deps.PayloadCapture = payloadcapture.NewStore(payloadcapture.Config{StoreResponseBody: true})
	rr := httptest.NewRecorder()
	s.w = rr
	s.streamMode = streampolicy.ModeBufferFullBlock

	if ok := (streamRelayStage{s: s}).run(); !ok {
		t.Fatal("relay stage returned false")
	}

	body := rr.Body.String()
	if strings.Contains(body, "alice@example.com") {
		t.Fatalf("relay buffer path leaked PII: %q", body)
	}
	if !strings.Contains(body, "[REDACTED_EMAIL]") {
		t.Fatalf("expected masked content via relay buffer path, got %q", body)
	}
	if !s.rec.ResponseHookRewritten {
		t.Error("ResponseHookRewritten should be true after the redact")
	}
	if string(s.rec.ResponseBodyRedacted) != string(s.rec.ResponseBody) || s.rec.ResponseBodyRedacted == nil {
		t.Error("redacted transcript must be stamped as the storage-safe copy")
	}
	if strings.Contains(string(s.rec.ResponseBodyRedacted), "alice@example.com") {
		t.Errorf("stored redacted copy leaked PII: %q", s.rec.ResponseBodyRedacted)
	}
}

// lastN returns the last n characters of s for readable failure tails.
func lastN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// failingWriter is an http.ResponseWriter whose Write always errors — used to
// exercise the canonical buffer's re-encode write-error classification.
type failingWriter struct{ header http.Header }

func (f *failingWriter) Header() http.Header       { return f.header }
func (f *failingWriter) WriteHeader(int)           {}
func (f *failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// failingTranscoder is a StreamTranscoder whose Write always errors — used to
// exercise the encode-error classification path.
type failingTranscoder struct{}

func (failingTranscoder) Write(context.Context, provcore.Chunk) ([]byte, error) {
	return nil, io.ErrUnexpectedEOF
}

// TestCanonicalBuffer_ClientAbort_NoFrame proves a context-cancelled
// subscription is classified as a client abort and writes nothing to the
// (already-gone) client.
func TestCanonicalBuffer_ClientAbort_NoFrame(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, nil)
	s.sub = &fakeChunkSub{err: context.Canceled}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	term := s.h.runCanonicalBufferStream(ctx, s, w, usage)

	if w.Len() != 0 {
		t.Errorf("client abort must not write to the gone peer, got %q", w.String())
	}
	if te := term.terminalError(); te == nil || te.code != streamErrCodeClientAbort {
		t.Errorf("expected %q classification, got %+v", streamErrCodeClientAbort, te)
	}
}

// TestCanonicalBuffer_ReEncodeWriteError_Classified proves a write failure during
// the forward-encode is surfaced as an upstream stream error for accounting.
func TestCanonicalBuffer_ReEncodeWriteError_Classified(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	chunks := []provcore.Chunk{{Delta: "ok", Done: true}}
	s, _, usage := bufferTestState(t, cache, openAIChatIngress, nil, chunks)
	fw := &failingWriter{header: http.Header{}}

	term := s.h.runCanonicalBufferStream(context.Background(), s, fw, usage)

	if te := term.terminalError(); te == nil || te.code != streamErrCodeUpstream {
		t.Errorf("expected %q on re-encode write error, got %+v", streamErrCodeUpstream, te)
	}
}

// TestCanonicalBuffer_EncodeError_Classified proves a transcoder failure during
// the forward-encode is surfaced as an upstream stream error.
func TestCanonicalBuffer_EncodeError_Classified(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	chunks := []provcore.Chunk{{Delta: "ok", Done: true}}
	s, w, usage := bufferTestState(t, cache, openAIChatIngress, failingTranscoder{}, chunks)

	term := s.h.runCanonicalBufferStream(context.Background(), s, w, usage)

	if te := term.terminalError(); te == nil || te.code != streamErrCodeUpstream {
		t.Errorf("expected %q on encode error, got %+v", streamErrCodeUpstream, te)
	}
}

// TestCanonicalStreamAccumulator_EmptyToolArgs_DefaultsToObject proves a tool
// call with no streamed arguments renders `{}` so the canonical body stays valid
// JSON the rewriter can parse.
func TestCanonicalStreamAccumulator_EmptyToolArgs_DefaultsToObject(t *testing.T) {
	acc := newCanonicalStreamAccumulator("m")
	acc.add(provcore.Chunk{ToolCallDeltas: []provcore.ToolCallDelta{{Index: 0, ID: "c1", Name: "fn"}}})
	body := acc.canonicalBody()
	if got := gjson.GetBytes(body, "choices.0.message.tool_calls.0.function.arguments").String(); got != "{}" {
		t.Errorf("empty tool args = %q, want {}", got)
	}
	if !gjson.ValidBytes(body) {
		t.Error("canonical body must be valid JSON")
	}
}

// modifyNoContentHook returns a Modify decision with NEITHER ModifiedContent
// nor TransformSpans — the canonical-locus fail-closed hole (HIGH-1): a Modify
// the policy demanded but that applies no rewrite must NOT deliver the original.
type modifyNoContentHook struct {
	goHooks.AnyEndpointAnyModality
}

func (modifyNoContentHook) Execute(_ context.Context, _ *goHooks.HookInput) (*goHooks.HookResult, error) {
	return &goHooks.HookResult{Decision: goHooks.Modify, Reason: "modify-no-content", ReasonCode: "TEST_NOOP"}, nil
}

// newModifyNoContentHookCache wires a RESPONSE-stage hook that always returns
// Modify with no applicable rewrite.
func newModifyNoContentHookCache(t *testing.T) *compliance.HookConfigCache {
	t.Helper()
	reg := builtins.Registry.Clone()
	reg.Register("modify-noop-hook", func(_ *goHooks.HookConfig) (goHooks.Hook, error) {
		return modifyNoContentHook{}, nil
	})
	reg.Freeze()
	loader := func(_ context.Context) ([]goHooks.HookConfig, error) {
		return []goHooks.HookConfig{{
			ID:                "modify-noop-1",
			ImplementationID:  "modify-noop-hook",
			Name:              "modify-noop",
			Priority:          1,
			Enabled:           true,
			Stage:             "response",
			FailBehavior:      "fail-closed",
			TimeoutMs:         1000,
			ApplicableIngress: []string{"ALL"},
			Config:            map[string]any{},
		}}, nil
	}
	cache := compliance.NewHookConfigCache(loader, reg, 0, noopLogger())
	if err := cache.Start(context.Background()); err != nil {
		t.Fatalf("cache.Start: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	return cache
}

// TestCanonicalBuffer_ModifyNoAppliedRewrite_FailsClosed pins HIGH-1: a Modify
// decision that produces NO applied rewrite (empty ModifiedContent + no
// applicable tool-arg spans) fails closed — zero content delivered, an in-band
// SSE error frame, and ResponseBodyRedacted left nil (storage NULL). The
// pre-fix behavior emitted the ORIGINAL unredacted chunks under a Modify label.
func TestCanonicalBuffer_ModifyNoAppliedRewrite_FailsClosed(t *testing.T) {
	cache := newModifyNoContentHookCache(t)
	chunks := []provcore.Chunk{
		{Delta: "sensitive original text", Done: true},
	}
	s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, chunks)

	s.h.runCanonicalBufferStream(context.Background(), s, w, usage)

	out := w.String()
	if strings.Contains(out, "sensitive original text") {
		t.Fatalf("Modify with no applied rewrite must fail closed, not deliver original: %q", out)
	}
	if !strings.Contains(out, `"error"`) {
		t.Errorf("expected an SSE error frame on fail-closed Modify, got %q", out)
	}
	if s.rec.ResponseHookRewritten {
		t.Error("ResponseHookRewritten must stay false on a fail-closed Modify")
	}
	if s.rec.ResponseBodyRedacted != nil {
		t.Error("ResponseBodyRedacted must stay nil (storage NULL) on a fail-closed Modify")
	}
}

// finishReasonRecorderHook records the FinishReason it received on the response
// HookInput and approves, so a test can assert the hook saw the real terminal
// reason (not a fabricated "stop").
type finishReasonRecorderHook struct {
	goHooks.AnyEndpointAnyModality
	got *string
}

func (h finishReasonRecorderHook) Execute(_ context.Context, in *goHooks.HookInput) (*goHooks.HookResult, error) {
	*h.got = in.FinishReason
	return &goHooks.HookResult{Decision: goHooks.Approve}, nil
}

func newFinishReasonRecorderCache(t *testing.T, got *string) *compliance.HookConfigCache {
	t.Helper()
	reg := builtins.Registry.Clone()
	reg.Register("finish-recorder-hook", func(_ *goHooks.HookConfig) (goHooks.Hook, error) {
		return finishReasonRecorderHook{got: got}, nil
	})
	reg.Freeze()
	loader := func(_ context.Context) ([]goHooks.HookConfig, error) {
		return []goHooks.HookConfig{{
			ID:                "finish-recorder-1",
			ImplementationID:  "finish-recorder-hook",
			Name:              "finish-recorder",
			Priority:          1,
			Enabled:           true,
			Stage:             "response",
			FailBehavior:      "fail-open",
			TimeoutMs:         1000,
			ApplicableIngress: []string{"ALL"},
			Config:            map[string]any{},
		}}, nil
	}
	cache := compliance.NewHookConfigCache(loader, reg, 0, noopLogger())
	if err := cache.Start(context.Background()); err != nil {
		t.Fatalf("cache.Start: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	return cache
}

// TestCanonicalBuffer_PreservesFinishReason pins HIGH-2: buffer mode preserves
// the real finish_reason on the delivered wire AND on the response-hook input,
// instead of collapsing it to "stop". OpenAI-style: finish_reason rides a
// trailing delta-empty chunk before the terminal Done.
func TestCanonicalBuffer_PreservesFinishReason(t *testing.T) {
	for _, fr := range []string{"tool_calls", "length"} {
		t.Run(fr, func(t *testing.T) {
			var seen string
			cache := newFinishReasonRecorderCache(t, &seen)
			chunks := []provcore.Chunk{
				{Delta: "hello world"},
				{FinishReason: fr},
				{Done: true},
			}
			s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, chunks)

			s.h.runCanonicalBufferStream(context.Background(), s, w, usage)

			out := w.String()
			if !strings.Contains(out, `"finish_reason":"`+fr+`"`) {
				t.Fatalf("delivered wire must preserve finish_reason=%s, got %q", fr, out)
			}
			if strings.Contains(out, `"finish_reason":"stop"`) {
				t.Errorf("finish_reason must not collapse to stop, got %q", out)
			}
			if seen != fr {
				t.Errorf("response hook input FinishReason = %q, want %q", seen, fr)
			}
		})
	}
}

// TestCanonicalBuffer_ExceedsBufferCap_FailsClosed pins HIGH-3: a stream whose
// accumulated canonical content exceeds the resolved buffer cap fails with an
// in-band error frame (bounded accumulation, no unbounded growth / OOM), rather
// than buffering without limit.
func TestCanonicalBuffer_ExceedsBufferCap_FailsClosed(t *testing.T) {
	cache := newPiiRedactResponseHookCache(t)
	big := strings.Repeat("x", 200)
	chunks := []provcore.Chunk{
		{Delta: big},
		{Delta: big},
		{Delta: big, Done: true},
	}
	s, w, usage := bufferTestState(t, cache, openAIChatIngress, nil, chunks)
	s.streamMaxBufferBytes = 256 // smaller than the 600 bytes of content above

	term := s.h.runCanonicalBufferStream(context.Background(), s, w, usage)

	out := w.String()
	if !strings.Contains(out, `"error"`) || !strings.Contains(out, "maximum buffer size") {
		t.Fatalf("expected a buffer-cap error frame, got %q", out)
	}
	if te := term.terminalError(); te == nil || te.code != streamErrCodeUpstream {
		t.Errorf("expected %q classification on cap exceedance, got %+v", streamErrCodeUpstream, te)
	}
	if s.rec.ResponseHookRewritten {
		t.Error("a capped stream is not a rewrite; ResponseHookRewritten must stay false")
	}
}

// TestCanonicalChunkSize_CountsAllChannels pins the byte tally used to bound the
// buffer: assistant text, reasoning text, and tool-call id/name/arguments all
// contribute so a tool-arg flood is bounded too.
func TestCanonicalChunkSize_CountsAllChannels(t *testing.T) {
	got := canonicalChunkSize(provcore.Chunk{
		Delta:          "abcd",
		ReasoningDelta: "ef",
		ToolCallDeltas: []provcore.ToolCallDelta{{ID: "g", Name: "hi", Arguments: "jkl"}},
	})
	if want := 4 + 2 + 1 + 2 + 3; got != want {
		t.Errorf("canonicalChunkSize = %d, want %d", got, want)
	}
}

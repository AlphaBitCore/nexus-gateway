package replicate

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// The tests in this file fill the coverage gaps for spec_replicate to push
// the package above the 95% statement-coverage gate (binding
// [[unit_test_coverage_95]]). They assert OBSERVABLE behavior — canonical
// OpenAI ↔ Replicate prediction wire conversion, SSE chunk projection,
// error normalization, and the Transport URL / auth / probe / Do path
// — and avoid err==nil padding.

// codec.EncodeRequest edge branches

// TestCodec_EncodeRequest_UnsupportedEndpoint pins the dispatch guard.
// Replicate's prediction API only serves chat-completions; routing the
// wrong shape (e.g. embeddings / models) to Replicate would corrupt the
// /v1/predictions envelope, so we surface a typed error.
func TestCodec_EncodeRequest_UnsupportedEndpoint(t *testing.T) {
	_, err := codec{}.EncodeRequest(typology.WireShapeOpenAIEmbeddings, []byte(`{}`), provcore.CallTarget{})
	if err == nil || !strings.Contains(err.Error(), "unsupported endpoint") {
		t.Fatalf("expected unsupported-endpoint error, got %v", err)
	}
}

// TestCodec_EncodeRequest_EmptyBodyIsNil asserts an empty canonical body
// is passed through as (nil, nil, nil) so the caller can skip the HTTP
// body entirely instead of sending `""`.
func TestCodec_EncodeRequest_EmptyBodyIsNil(t *testing.T) {
	encRes, err := codec{}.EncodeRequest(typology.WireShapeOpenAIChat, nil, provcore.CallTarget{})
	out := encRes.Body
	rewrites := encRes.Rewrites
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if out != nil {
		t.Errorf("expected nil body, got %q", out)
	}
	if rewrites != nil {
		t.Errorf("expected nil rewrites, got %v", rewrites)
	}
}

// TestCodec_EncodeRequest_InvalidJSON pins the gjson.Valid guard so the
// codec rejects malformed canonical bodies before they reach Replicate.
func TestCodec_EncodeRequest_InvalidJSON(t *testing.T) {
	_, err := codec{}.EncodeRequest(typology.WireShapeOpenAIChat, []byte(`{not json`), provcore.CallTarget{})
	if err == nil || !strings.Contains(err.Error(), "invalid canonical body") {
		t.Fatalf("expected invalid-body error, got %v", err)
	}
}

// TestCodec_EncodeRequest_MultimodalContentParts covers the IsArray
// branch of the message-content handling — OpenAI clients may send
// `content` as an array of typed parts. The codec must extract `type:
// text` parts and ignore others (image, audio) when flattening into
// Replicate's `prompt` string.
func TestCodec_EncodeRequest_MultimodalContentParts(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"Describe this."},
				{"type":"image_url","image_url":{"url":"https://example.com/a.png"}},
				{"type":"text","text":"In two sentences."}
			]}
		]
	}`)
	encRes, err := codec{}.EncodeRequest(
		typology.WireShapeOpenAIChat, body,
		provcore.CallTarget{ProviderModelID: "meta/llama-3:abc"},
	)
	encoded := encRes.Body
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	prompt := gjson.GetBytes(encoded, "input.prompt").Str
	if !strings.Contains(prompt, "Describe this.") {
		t.Errorf("prompt missing first text part: %q", prompt)
	}
	if !strings.Contains(prompt, "In two sentences.") {
		t.Errorf("prompt missing trailing text part: %q", prompt)
	}
	if strings.Contains(prompt, "example.com") {
		t.Errorf("prompt must NOT include image-URL part: %q", prompt)
	}
}

// TestCodec_EncodeRequest_StreamFlagAndOptionalParams covers the optional
// parameter forwarding: stream, top_p (in addition to max_tokens +
// temperature already covered by the legacy test). Replicate models gate
// generation on these so they must round-trip.
func TestCodec_EncodeRequest_StreamFlagAndOptionalParams(t *testing.T) {
	body := []byte(`{
		"messages":[{"role":"user","content":"hi"}],
		"stream":true,
		"top_p":0.9
	}`)
	encRes, err := codec{}.EncodeRequest(
		typology.WireShapeOpenAIChat, body,
		provcore.CallTarget{ProviderModelID: "x/y:v1"},
	)
	encoded := encRes.Body
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got := gjson.GetBytes(encoded, "stream").Bool(); !got {
		t.Errorf("stream must surface as true, got %v", got)
	}
	if got := gjson.GetBytes(encoded, "input.top_p").Float(); got != 0.9 {
		t.Errorf("input.top_p=%v want 0.9", got)
	}
	// version must always be the target ProviderModelID.
	if got := gjson.GetBytes(encoded, "version").Str; got != "x/y:v1" {
		t.Errorf("version=%q want x/y:v1", got)
	}
	// `messages` must also ride inside input verbatim.
	if !gjson.GetBytes(encoded, "input.messages").IsArray() {
		t.Errorf("input.messages must be an array: %s", encoded)
	}
}

// codec.DecodeResponse edge branches

// TestCodec_DecodeResponse_UnsupportedEndpoint pins the dispatch guard
// on the decode side — only chat-completions is valid.
func TestCodec_DecodeResponse_UnsupportedEndpoint(t *testing.T) {
	_, err := codec{}.DecodeResponse(typology.WireShapeOpenAIEmbeddings, []byte(`{}`), "", provcore.DecodeContext{})
	if err == nil || !strings.Contains(err.Error(), "unsupported endpoint") {
		t.Fatalf("expected unsupported-endpoint error, got %v", err)
	}
}

// TestCodec_DecodeResponse_EmptyBody pins the early-return guard so an
// empty native body returns (empty, zero-usage, nil) rather than
// failing validation.
func TestCodec_DecodeResponse_EmptyBody(t *testing.T) {
	decRes, err := codec{}.DecodeResponse(typology.WireShapeOpenAIChat, nil, "", provcore.DecodeContext{})
	out := decRes.CanonicalBody
	usage := decRes.Usage
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected empty output, got %q", out)
	}
	if usage.PromptTokens != nil {
		t.Errorf("expected zero usage, got %+v", usage)
	}
}

// TestCodec_DecodeResponse_InvalidJSON pins the gjson.Valid guard.
func TestCodec_DecodeResponse_InvalidJSON(t *testing.T) {
	_, err := codec{}.DecodeResponse(typology.WireShapeOpenAIChat, []byte(`{not json`), "", provcore.DecodeContext{})
	if err == nil || !strings.Contains(err.Error(), "invalid response body") {
		t.Fatalf("expected invalid-response error, got %v", err)
	}
}

// TestCodec_DecodeResponse_ObjectOutput_TextKey covers the object-output
// branch — Replicate models for some hosted prompts return
// `output: {text: "..."}`. The codec must pull the text into
// choices[0].message.content.
func TestCodec_DecodeResponse_ObjectOutput_TextKey(t *testing.T) {
	body := []byte(`{"id":"p1","status":"succeeded","output":{"text":"hello there"},"version":"x/y:v"}`)
	decRes, err := codec{}.DecodeResponse(typology.WireShapeOpenAIChat, body, "", provcore.DecodeContext{})
	out := decRes.CanonicalBody
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got := gjson.GetBytes(out, "choices.0.message.content").Str; got != "hello there" {
		t.Errorf("content=%q want 'hello there'", got)
	}
}

// TestCodec_DecodeResponse_ObjectOutput_AnswerKey covers the alternative
// key ordering — `answer` is the second key in the fallback list.
func TestCodec_DecodeResponse_ObjectOutput_AnswerKey(t *testing.T) {
	body := []byte(`{"id":"p1","status":"succeeded","output":{"answer":"forty-two"},"version":"x"}`)
	decRes, err := codec{}.DecodeResponse(typology.WireShapeOpenAIChat, body, "", provcore.DecodeContext{})
	out := decRes.CanonicalBody
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got := gjson.GetBytes(out, "choices.0.message.content").Str; got != "forty-two" {
		t.Errorf("content=%q want 'forty-two'", got)
	}
}

// TestCodec_DecodeResponse_ObjectOutput_CompletionKey covers the
// `completion` key (legacy chat models like cog-instruct).
func TestCodec_DecodeResponse_ObjectOutput_CompletionKey(t *testing.T) {
	body := []byte(`{"id":"p1","status":"succeeded","output":{"completion":"done"},"version":"x"}`)
	decRes, err := codec{}.DecodeResponse(typology.WireShapeOpenAIChat, body, "", provcore.DecodeContext{})
	out := decRes.CanonicalBody
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got := gjson.GetBytes(out, "choices.0.message.content").Str; got != "done" {
		t.Errorf("content=%q want 'done'", got)
	}
}

// TestCodec_DecodeResponse_ObjectOutput_MessageKey covers the `message`
// key (newer Replicate chat builds).
func TestCodec_DecodeResponse_ObjectOutput_MessageKey(t *testing.T) {
	body := []byte(`{"id":"p1","status":"succeeded","output":{"message":"reply"},"version":"x"}`)
	decRes, err := codec{}.DecodeResponse(typology.WireShapeOpenAIChat, body, "", provcore.DecodeContext{})
	out := decRes.CanonicalBody
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got := gjson.GetBytes(out, "choices.0.message.content").Str; got != "reply" {
		t.Errorf("content=%q want 'reply'", got)
	}
}

// TestCodec_DecodeResponse_StatusFailed maps Replicate's status=failed
// onto finish_reason=error so downstream surfaces a non-success outcome
// even when `output` is empty.
func TestCodec_DecodeResponse_StatusFailed(t *testing.T) {
	body := []byte(`{"id":"p1","status":"failed","output":null,"version":"x"}`)
	decRes, err := codec{}.DecodeResponse(typology.WireShapeOpenAIChat, body, "", provcore.DecodeContext{})
	out := decRes.CanonicalBody
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got := gjson.GetBytes(out, "choices.0.finish_reason").Str; got != "error" {
		t.Errorf("finish_reason=%q want 'error'", got)
	}
}

// TestCodec_DecodeResponse_StatusCanceled maps status=canceled onto
// finish_reason=error — Replicate reports canceled when a prediction is
// aborted server-side.
func TestCodec_DecodeResponse_StatusCanceled(t *testing.T) {
	body := []byte(`{"id":"p1","status":"canceled","output":null,"version":"x"}`)
	decRes, err := codec{}.DecodeResponse(typology.WireShapeOpenAIChat, body, "", provcore.DecodeContext{})
	out := decRes.CanonicalBody
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got := gjson.GetBytes(out, "choices.0.finish_reason").Str; got != "error" {
		t.Errorf("finish_reason=%q want 'error'", got)
	}
}

// TestCodec_DecodeResponse_ErrorFieldSurfacedIntoContent covers the
// error-text fallback: when `output` is empty and `error` is set, the
// codec lifts the error text into choices[0].message.content so callers
// always see a non-empty assistant message.
func TestCodec_DecodeResponse_ErrorFieldSurfacedIntoContent(t *testing.T) {
	body := []byte(`{"id":"p1","status":"failed","output":null,"error":"out of memory","version":"x"}`)
	decRes, err := codec{}.DecodeResponse(typology.WireShapeOpenAIChat, body, "", provcore.DecodeContext{})
	out := decRes.CanonicalBody
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got := gjson.GetBytes(out, "choices.0.message.content").Str; got != "out of memory" {
		t.Errorf("content=%q want 'out of memory'", got)
	}
	if got := gjson.GetBytes(out, "choices.0.finish_reason").Str; got != "error" {
		t.Errorf("finish_reason=%q want 'error'", got)
	}
}

// TestCodec_DecodeResponse_ErrorFieldWithOutputPreservesOutput verifies
// that when BOTH `output` and `error` are present, output text wins for
// content but finish_reason still flips to error so the caller can
// detect the failure.
func TestCodec_DecodeResponse_ErrorFieldWithOutputPreservesOutput(t *testing.T) {
	body := []byte(`{"id":"p1","status":"succeeded","output":"partial reply","error":"warning: truncated","version":"x"}`)
	decRes, err := codec{}.DecodeResponse(typology.WireShapeOpenAIChat, body, "", provcore.DecodeContext{})
	out := decRes.CanonicalBody
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got := gjson.GetBytes(out, "choices.0.message.content").Str; got != "partial reply" {
		t.Errorf("content=%q want 'partial reply'", got)
	}
	if got := gjson.GetBytes(out, "choices.0.finish_reason").Str; got != "error" {
		t.Errorf("finish_reason=%q want 'error' (error present)", got)
	}
}

// TestCodec_DecodeResponse_NoUsageOmitsUsageBlock asserts the codec
// returns zero Usage when Replicate did not surface metrics — downstream
// cost ledger treats "no usage" as unknown, not zero.
func TestCodec_DecodeResponse_NoUsageOmitsUsageBlock(t *testing.T) {
	body := []byte(`{"id":"p1","status":"succeeded","output":"hi","version":"x"}`)
	decRes, err := codec{}.DecodeResponse(typology.WireShapeOpenAIChat, body, "", provcore.DecodeContext{})
	usage := decRes.Usage
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if usage.PromptTokens != nil || usage.CompletionTokens != nil {
		t.Errorf("expected zero usage, got %+v", usage)
	}
}

// errors.Normalize / parseRetryAfter

// TestErrorNormalizer_ErrorField covers the {"error":"..."} envelope
// branch (Replicate's prediction-failed shape).
func TestErrorNormalizer_ErrorField(t *testing.T) {
	pe := errorNormalizer{}.Normalize(http.StatusInternalServerError, http.Header{}, []byte(`{"status":"failed","error":"model exploded"}`))
	if pe.Message != "model exploded" {
		t.Errorf("Message=%q want 'model exploded'", pe.Message)
	}
	if pe.Code != provcore.CodeUpstreamError {
		t.Errorf("Code=%q want upstream_error", pe.Code)
	}
}

// TestErrorNormalizer_MessageField covers the {"message":"..."}
// envelope branch — some Replicate edges return OpenAI-like shape.
func TestErrorNormalizer_MessageField(t *testing.T) {
	pe := errorNormalizer{}.Normalize(http.StatusBadRequest, http.Header{}, []byte(`{"message":"bad version"}`))
	if pe.Message != "bad version" {
		t.Errorf("Message=%q want 'bad version'", pe.Message)
	}
	if pe.Code != provcore.CodeInvalidRequest {
		t.Errorf("Code=%q want invalid_request", pe.Code)
	}
}

// TestErrorNormalizer_EmptyBodyFallback pins the http.StatusText fallback
// — an empty body must not leave Message blank because downstream UIs
// render it directly.
func TestErrorNormalizer_EmptyBodyFallback(t *testing.T) {
	pe := errorNormalizer{}.Normalize(http.StatusForbidden, http.Header{}, nil)
	if pe.Message == "" {
		t.Error("Message must fall back to http.StatusText, not blank")
	}
	if pe.Code != provcore.CodeAuthFailed {
		t.Errorf("403 must map to auth_failed, got %q", pe.Code)
	}
}

// TestErrorNormalizer_StatusMatrix covers every explicit status arm:
// 400/422 → invalid_request, 401/403/402 → auth_failed, 408/504 → timeout,
// 429 → rate_limited, 5xx default → upstream_error.
func TestErrorNormalizer_StatusMatrix(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{http.StatusBadRequest, provcore.CodeInvalidRequest},
		{http.StatusUnprocessableEntity, provcore.CodeInvalidRequest},
		{http.StatusUnauthorized, provcore.CodeAuthFailed},
		{http.StatusForbidden, provcore.CodeAuthFailed},
		{http.StatusPaymentRequired, provcore.CodeAuthFailed},
		{http.StatusRequestTimeout, provcore.CodeTimeout},
		{http.StatusGatewayTimeout, provcore.CodeTimeout},
		{http.StatusTooManyRequests, provcore.CodeRateLimited},
		{http.StatusBadGateway, provcore.CodeUpstreamError},
		{http.StatusServiceUnavailable, provcore.CodeUpstreamError},
	}
	for _, tc := range cases {
		pe := errorNormalizer{}.Normalize(tc.status, http.Header{}, []byte(`{"detail":"x"}`))
		if pe.Code != tc.want {
			t.Errorf("status=%d Code=%q want %q", tc.status, pe.Code, tc.want)
		}
	}
}

// TestErrorNormalizer_RateLimit_NoHeader covers the 429 path without a
// retry-after header — Code must still be rate_limited but RetryAfter
// stays nil so the caller picks its own backoff.
func TestErrorNormalizer_RateLimit_NoHeader(t *testing.T) {
	pe := errorNormalizer{}.Normalize(http.StatusTooManyRequests, http.Header{}, []byte(`{"detail":"slow down"}`))
	if pe.Code != provcore.CodeRateLimited {
		t.Errorf("Code=%q want rate_limited", pe.Code)
	}
	if pe.RetryAfter != nil {
		t.Errorf("RetryAfter=%v want nil (no header)", pe.RetryAfter)
	}
}

// TestErrorNormalizer_PreservesRawBody pins the Raw passthrough so the
// audit pipeline can persist the original error bytes for forensics.
func TestErrorNormalizer_PreservesRawBody(t *testing.T) {
	raw := []byte(`{"detail":"x","extra":"y"}`)
	pe := errorNormalizer{}.Normalize(http.StatusBadRequest, http.Header{}, raw)
	if string(pe.Raw) != string(raw) {
		t.Errorf("Raw=%q want round-trip", pe.Raw)
	}
	if pe.Status != http.StatusBadRequest {
		t.Errorf("Status=%d want 400", pe.Status)
	}
}

// TestParseRetryAfter covers all branches: empty, integer seconds, negative
// integer, HTTP-date in the future, HTTP-date in the past (clamped to 0),
// and garbage input → nil.
func TestParseRetryAfter(t *testing.T) {
	if got := parseRetryAfter(""); got != nil {
		t.Errorf("empty must return nil, got %v", got)
	}
	if got := parseRetryAfter("7"); got == nil || *got != 7*time.Second {
		t.Errorf("integer secs=7 → %v, want 7s", got)
	}
	if got := parseRetryAfter("-1"); got != nil {
		t.Errorf("negative integer must return nil, got %v", got)
	}
	future := time.Now().Add(10 * time.Second).UTC().Format(http.TimeFormat)
	got := parseRetryAfter(future)
	if got == nil || *got <= 0 || *got > 15*time.Second {
		t.Errorf("future HTTP-date returned %v, want ~10s", got)
	}
	past := time.Now().Add(-30 * time.Second).UTC().Format(http.TimeFormat)
	got = parseRetryAfter(past)
	if got == nil || *got != 0 {
		t.Errorf("past HTTP-date must clamp to 0, got %v", got)
	}
	if got := parseRetryAfter("not-a-time"); got != nil {
		t.Errorf("garbage must return nil, got %v", got)
	}
}

// Spec / Stream / Transport constructor nil-logger defaults

// TestNewSpec_NilLoggerDefaults pins the slog.Default() fallback so the
// adapter is safe to construct without explicit logger wiring.
func TestNewSpec_NilLoggerDefaults(t *testing.T) {
	s := NewSpec(nil)
	if !s.Valid() {
		t.Fatal("spec invalid after nil-logger construction")
	}
	if s.Format != provcore.FormatReplicate {
		t.Errorf("Format=%q", s.Format)
	}
}

// TestNewStreamDecoder_NilLoggerDefaults pins the slog.Default() fallback
// on the standalone constructor (used by spec_adapter wiring).
func TestNewStreamDecoder_NilLoggerDefaults(t *testing.T) {
	d := NewStreamDecoder(nil)
	if d == nil || d.log == nil {
		t.Fatalf("StreamDecoder.log must default to slog.Default(), got %+v", d)
	}
}

// TestNewTransport_NilLoggerDefaults pins the slog.Default() fallback
// on the transport constructor.
func TestNewTransport_NilLoggerDefaults(t *testing.T) {
	tr := NewTransport(nil)
	if tr == nil || tr.log == nil || tr.client == nil || tr.probe == nil {
		t.Fatalf("Transport must be fully populated, got %+v", tr)
	}
}

// StreamDecoder.Open / streamSession edge cases

// TestStreamDecoder_Open_NilBody pins the nil-body guard — the gateway
// must never wrap a nil reader (which would panic on first Read).
func TestStreamDecoder_Open_NilBody(t *testing.T) {
	d := NewStreamDecoder(slog.Default())
	if _, err := d.Open(nil, typology.WireShapeOpenAIChat); err == nil {
		t.Fatal("expected error on nil body")
	}
}

// TestStreamSession_Next_ErrorEventTerminatesStream covers the
// `event: error` branch: it must surface a typed ProviderError (NOT fold
// the error text into assistant Delta + report a billed HTTP-200 success),
// and any subsequent Next call must return io.EOF.
func TestStreamSession_Next_ErrorEventTerminatesStream(t *testing.T) {
	raw := "event: error\ndata: model crashed\n\n"
	d := NewStreamDecoder(slog.Default())
	sess, err := d.Open(io.NopCloser(strings.NewReader(raw)), typology.WireShapeOpenAIChat)
	if err != nil {
		t.Fatalf("Open err=%v", err)
	}
	defer sess.Close() //nolint:errcheck

	c, err := sess.Next(context.Background())
	if err == nil {
		t.Fatalf("error event must return a typed error, got chunk=%+v nil err", c)
	}
	var pe *provcore.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("error event must return *provcore.ProviderError, got %T: %v", err, err)
	}
	if pe.Code != provcore.CodeUpstreamError {
		t.Errorf("Code=%q want %q", pe.Code, provcore.CodeUpstreamError)
	}
	if pe.Status != http.StatusBadGateway {
		t.Errorf("Status=%d want %d", pe.Status, http.StatusBadGateway)
	}
	if pe.Message != "model crashed" {
		t.Errorf("Message=%q want 'model crashed'", pe.Message)
	}
	if c.Delta != "" || c.Done {
		t.Errorf("error event must not emit assistant Delta or a Done success, got %+v", c)
	}
	// Subsequent Next returns io.EOF (stream considered finished).
	if _, err := sess.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Errorf("Next after error: err=%v want io.EOF", err)
	}
}

// TestStreamSession_Next_ErrorEventEmptyData_DefaultMessage covers the
// empty error-payload branch: the typed error still carries a fallback
// message.
func TestStreamSession_Next_ErrorEventEmptyData_DefaultMessage(t *testing.T) {
	raw := "event: error\ndata: \n\n"
	d := NewStreamDecoder(slog.Default())
	sess, err := d.Open(io.NopCloser(strings.NewReader(raw)), typology.WireShapeOpenAIChat)
	if err != nil {
		t.Fatalf("Open err=%v", err)
	}
	defer sess.Close() //nolint:errcheck

	_, err = sess.Next(context.Background())
	var pe *provcore.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("want *provcore.ProviderError, got %T: %v", err, err)
	}
	if pe.Message != "replicate stream error" {
		t.Errorf("Message=%q want fallback 'replicate stream error'", pe.Message)
	}
}

// TestStreamSession_Next_LogsEvent_DefaultArm covers the default switch
// arm — `logs` (and any unknown event type) must yield a no-Delta chunk
// with the raw frame preserved for audit but no canonical text.
func TestStreamSession_Next_LogsEvent_DefaultArm(t *testing.T) {
	raw := "event: logs\ndata: tokens/sec=42\n\nevent: done\ndata: {}\n\n"
	d := NewStreamDecoder(slog.Default())
	sess, err := d.Open(io.NopCloser(strings.NewReader(raw)), typology.WireShapeOpenAIChat)
	if err != nil {
		t.Fatalf("Open err=%v", err)
	}
	defer sess.Close() //nolint:errcheck

	c, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next err=%v", err)
	}
	if c.Delta != "" {
		t.Errorf("logs event must NOT carry Delta, got %q", c.Delta)
	}
	if c.NativeEvent != "logs" {
		t.Errorf("NativeEvent=%q want 'logs'", c.NativeEvent)
	}
	if !strings.Contains(string(c.RawBytes), "tokens/sec=42") {
		t.Errorf("RawBytes must carry the original payload, got %q", c.RawBytes)
	}
}

// TestStreamSession_NextAfterDone_ReturnsEOF asserts that once the
// `done` event has been delivered, subsequent Next calls return io.EOF
// — the executor uses io.EOF as the canonical stream-finished sentinel.
func TestStreamSession_NextAfterDone_ReturnsEOF(t *testing.T) {
	raw := "event: done\ndata: {}\n\n"
	d := NewStreamDecoder(slog.Default())
	sess, err := d.Open(io.NopCloser(strings.NewReader(raw)), typology.WireShapeOpenAIChat)
	if err != nil {
		t.Fatalf("Open err=%v", err)
	}
	defer sess.Close() //nolint:errcheck

	if _, err := sess.Next(context.Background()); err != nil {
		t.Fatalf("first Next: %v", err)
	}
	if _, err := sess.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Errorf("second Next err=%v want io.EOF", err)
	}
}

// TestStreamSession_Next_CtxCanceled asserts that a cancelled context
// short-circuits Next before draining the scanner — important so the
// gateway can abort a slow Replicate stream when the client disconnects.
func TestStreamSession_Next_CtxCanceled(t *testing.T) {
	raw := "event: output\ndata: hi\n\n"
	d := NewStreamDecoder(slog.Default())
	sess, err := d.Open(io.NopCloser(strings.NewReader(raw)), typology.WireShapeOpenAIChat)
	if err != nil {
		t.Fatalf("Open err=%v", err)
	}
	defer sess.Close() //nolint:errcheck

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sess.Next(ctx); err == nil {
		t.Fatal("expected context error after cancel")
	}
}

// TestStreamSession_Next_ScannerEOF asserts that an empty stream
// surfaces the scanner's io.EOF up to the caller without spurious
// chunks.
func TestStreamSession_Next_ScannerEOF(t *testing.T) {
	d := NewStreamDecoder(slog.Default())
	sess, err := d.Open(io.NopCloser(strings.NewReader("")), typology.WireShapeOpenAIChat)
	if err != nil {
		t.Fatalf("Open err=%v", err)
	}
	defer sess.Close() //nolint:errcheck

	if _, err := sess.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Errorf("empty stream err=%v want io.EOF", err)
	}
}

// TestStreamSession_Close_Then_Next_EOF verifies Close marks done so a
// later Next returns EOF — the executor sometimes calls Close on error
// paths and then drains.
func TestStreamSession_Close_Then_Next_EOF(t *testing.T) {
	raw := "event: output\ndata: hi\n\n"
	d := NewStreamDecoder(slog.Default())
	sess, err := d.Open(io.NopCloser(strings.NewReader(raw)), typology.WireShapeOpenAIChat)
	if err != nil {
		t.Fatalf("Open err=%v", err)
	}
	if err := sess.Close(); err != nil {
		t.Errorf("Close err=%v", err)
	}
	if _, err := sess.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Errorf("Next after Close: err=%v want io.EOF", err)
	}
}

// Transport.BuildURL / Do / Probe

// TestTransport_BuildURL_AllEndpoints covers every supported endpoint
// plus the trailing-slash normalization on BaseURL and the
// unsupported-endpoint guard.
func TestTransport_BuildURL_AllEndpoints(t *testing.T) {
	tr := NewTransport(slog.Default())
	cases := []struct {
		endpoint typology.WireShape
		baseURL  string
		want     string
	}{
		{typology.WireShapeOpenAIChat, "https://api.replicate.com", "https://api.replicate.com/v1/predictions"},
		{typology.WireShapeOpenAIChat, "https://api.replicate.com/", "https://api.replicate.com/v1/predictions"},
		{typology.WireShapeNone, "https://api.replicate.com", "https://api.replicate.com/v1/models"},
	}
	for _, tc := range cases {
		got, err := tr.BuildURL(provcore.CallTarget{BaseURL: tc.baseURL}, tc.endpoint, false)
		if err != nil {
			t.Errorf("endpoint=%s err=%v", tc.endpoint, err)
			continue
		}
		if got != tc.want {
			t.Errorf("endpoint=%s got=%q want=%q", tc.endpoint, got, tc.want)
		}
	}
	// Unsupported endpoint must error.
	if _, err := tr.BuildURL(provcore.CallTarget{BaseURL: "https://x"}, typology.WireShapeOpenAIEmbeddings, false); err == nil {
		t.Error("unsupported endpoint must error")
	}
}

// TestTransport_BuildURL_EmptyBaseURL pins the empty-BaseURL guard so a
// half-configured target never produces "/v1/predictions" with no host.
func TestTransport_BuildURL_EmptyBaseURL(t *testing.T) {
	tr := NewTransport(slog.Default())
	if _, err := tr.BuildURL(provcore.CallTarget{}, typology.WireShapeOpenAIChat, false); err == nil {
		t.Error("empty BaseURL must error")
	}
}

// TestTransport_Do_DelegatesToClient drives a real HTTP round-trip
// through httptest to cover the Do path. We assert the request hits the
// server and the body round-trips.
func TestTransport_Do_DelegatesToClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method=%q want POST", r.Method)
		}
		if r.URL.Path != "/v1/predictions" {
			t.Errorf("path=%q want /v1/predictions", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"pred_test","status":"starting"}`))
	}))
	defer srv.Close()

	tr := NewTransport(slog.Default())
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/predictions", strings.NewReader(`{}`))
	resp, err := tr.Do(context.Background(), req, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("Do err=%v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status=%d want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"pred_test"`) {
		t.Errorf("body=%q", body)
	}
}

// TestTransport_Probe_EmptyBaseURL pins the empty-base-URL guard so a
// half-configured target produces a not-OK probe without a network
// dial.
func TestTransport_Probe_EmptyBaseURL(t *testing.T) {
	tr := NewTransport(slog.Default())
	r, err := tr.Probe(context.Background(), provcore.CallTarget{})
	if err != nil {
		t.Fatalf("Probe err=%v", err)
	}
	if r == nil || r.OK {
		t.Errorf("empty base URL must produce not-OK probe, got %+v", r)
	}
}

// TestTransport_Probe_Success covers the 2xx happy path — Replicate
// serves /v1/models to authenticated tokens. We assert the Token auth
// header rides the request.
func TestTransport_Probe_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path=%q want /v1/models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Token r8_probe" {
			t.Errorf("Authorization=%q want 'Token r8_probe'", got)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer srv.Close()

	tr := NewTransport(slog.Default())
	r, err := tr.Probe(context.Background(), provcore.CallTarget{BaseURL: srv.URL, APIKey: "r8_probe"})
	if err != nil {
		t.Fatalf("Probe err=%v", err)
	}
	if !r.OK {
		t.Errorf("expected OK probe, got %+v", r)
	}
	if r.LatencyMs < 0 {
		t.Errorf("LatencyMs=%d", r.LatencyMs)
	}
	if r.Detail != "ok" {
		t.Errorf("Detail=%q want 'ok'", r.Detail)
	}
}

// TestTransport_Probe_NoAPIKey covers the auth-header-omitted branch —
// Probe without an API key still issues the request (Replicate's
// /v1/models is publicly listable) but no Authorization header is sent.
func TestTransport_Probe_NoAPIKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization=%q want empty (no key)", got)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer srv.Close()

	tr := NewTransport(slog.Default())
	r, err := tr.Probe(context.Background(), provcore.CallTarget{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("Probe err=%v", err)
	}
	if !r.OK {
		t.Errorf("expected OK probe, got %+v", r)
	}
}

// TestTransport_Probe_HTTPFailure covers the non-2xx branch — 5xx from
// Replicate /v1/models marks the probe not-OK but does NOT surface a Go
// error (a returned err would crash the orchestrator's polling loop).
func TestTransport_Probe_HTTPFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	tr := NewTransport(slog.Default())
	r, err := tr.Probe(context.Background(), provcore.CallTarget{BaseURL: srv.URL, APIKey: "k"})
	if err != nil {
		t.Fatalf("Probe err=%v", err)
	}
	if r.OK {
		t.Errorf("5xx must mark OK=false: %+v", r)
	}
	if !strings.Contains(r.Detail, "500") {
		t.Errorf("Detail must mention status, got %q", r.Detail)
	}
}

// TestTransport_Probe_TransportError covers the dial-failure branch —
// pointing at a closed server records r.Err and OK=false.
func TestTransport_Probe_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := srv.URL
	srv.Close()

	tr := NewTransport(slog.Default())
	r, err := tr.Probe(context.Background(), provcore.CallTarget{BaseURL: addr, APIKey: "k"})
	if err != nil {
		t.Fatalf("Probe err=%v", err)
	}
	if r.OK {
		t.Error("closed server must produce not-OK probe")
	}
	if r.Err == nil {
		t.Error("Err must be populated on transport failure")
	}
}

// TestTransport_Probe_BadURL covers the http.NewRequestWithContext error
// branch — a base URL containing an illegal control char fails at
// request construction, before any network call.
func TestTransport_Probe_BadURL(t *testing.T) {
	tr := NewTransport(slog.Default())
	r, err := tr.Probe(context.Background(), provcore.CallTarget{BaseURL: "http://\x7f", APIKey: "k"})
	if err != nil {
		t.Fatalf("Probe err=%v", err)
	}
	if r.OK {
		t.Error("malformed URL must produce not-OK probe")
	}
	if r.Err == nil {
		t.Error("Err must be populated on request-construction failure")
	}
}

package cohere

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

// The tests in this file fill the coverage gaps for spec_cohere to push the
// package above the 95% statement-coverage gate (binding
// [[unit_test_coverage_95]]). They assert OBSERVABLE behavior — Cohere wire
// shape ↔ canonical conversion, SSE stream chunk projection, error
// normalization, and transport URL / auth / probe / Do — and avoid
// err==nil padding.

// codec.EncodeRequest edge branches

// TestCodec_EncodeRequest_UnsupportedEndpoint pins the dispatch guard —
// only chat-completions and embeddings are valid; anything else (e.g.
// models, responses) must surface a typed error so the dispatcher never
// routes the wrong shape to Cohere.
func TestCodec_EncodeRequest_UnsupportedEndpoint(t *testing.T) {
	_, err := codec{}.EncodeRequest(typology.WireShapeNone, []byte(`{}`), provcore.CallTarget{})
	if err == nil || !strings.Contains(err.Error(), "unsupported endpoint") {
		t.Fatalf("expected unsupported-endpoint error, got %v", err)
	}
}

// TestCodec_EncodeRequest_EmptyBodyIsNil asserts that an empty canonical
// body is passed through as (nil, nil, nil) so the caller can skip the
// HTTP body entirely instead of sending `""`.
func TestCodec_EncodeRequest_EmptyBodyIsNil(t *testing.T) {
	encRes, err := codec{}.EncodeRequest(typology.WireShapeCohereChat, nil, provcore.CallTarget{})
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
// codec rejects malformed bodies before downstream parsers can corrupt
// state.
func TestCodec_EncodeRequest_InvalidJSON(t *testing.T) {
	_, err := codec{}.EncodeRequest(typology.WireShapeCohereChat, []byte(`{not json`), provcore.CallTarget{})
	if err == nil || !strings.Contains(err.Error(), "invalid canonical body") {
		t.Fatalf("expected invalid-body error, got %v", err)
	}
}

// TestCodec_EncodeRequest_EmbeddingsTranslated verifies the embeddings
// branch translates canonical → Cohere wire shape (texts array, model,
// truncate). A v2 model is used here so no input_type requirement fires.
func TestCodec_EncodeRequest_EmbeddingsTranslated(t *testing.T) {
	body := []byte(`{"model":"embed-english-light-v2.0","input":["hello"]}`)
	encRes, err := codec{}.EncodeRequest(typology.WireShapeCohereEmbed, body, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// Wire body must have texts array (not OpenAI "input" field).
	if !gjson.GetBytes(encRes.Body, "texts").IsArray() {
		t.Errorf("embeddings wire body must have texts array, got %s", encRes.Body)
	}
	if gjson.GetBytes(encRes.Body, "input").Exists() {
		t.Errorf("embeddings wire body must NOT have 'input' field (Cohere uses 'texts'): %s", encRes.Body)
	}
}

// codec.DecodeResponse edge branches

// TestCodec_DecodeResponse_EmbeddingsDecoded verifies that the embeddings
// endpoint response is translated from Cohere wire shape into canonical
// OpenAI embeddings format (not passed through verbatim).
func TestCodec_DecodeResponse_EmbeddingsDecoded(t *testing.T) {
	body := []byte(`{"embeddings":[[0.1,0.2]],"meta":{"billed_units":{"input_tokens":3}}}`)
	decRes, err := codec{}.DecodeResponse(typology.WireShapeCohereEmbed, body, "", provcore.DecodeContext{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// Canonical response must have data[] array, not Cohere's embeddings field.
	if !gjson.GetBytes(decRes.CanonicalBody, "data").IsArray() {
		t.Errorf("embeddings decode must produce canonical data[] array, got %s", decRes.CanonicalBody)
	}
	if gjson.GetBytes(decRes.CanonicalBody, "object").Str != "list" {
		t.Errorf("embeddings decode must set object='list': %s", decRes.CanonicalBody)
	}
}

// TestCodec_DecodeResponse_EmptyBody pins the early-return guard so an
// empty native body returns (empty, zero-usage, nil) instead of failing
// validation.
func TestCodec_DecodeResponse_EmptyBody(t *testing.T) {
	decRes, err := codec{}.DecodeResponse(typology.WireShapeCohereChat, nil, "", provcore.DecodeContext{})
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

// TestCodec_DecodeResponse_InvalidJSON pins the gjson.Valid guard so the
// codec rejects malformed Cohere bodies up-front.
func TestCodec_DecodeResponse_InvalidJSON(t *testing.T) {
	_, err := codec{}.DecodeResponse(typology.WireShapeCohereChat, []byte(`{not json`), "", provcore.DecodeContext{})
	if err == nil || !strings.Contains(err.Error(), "invalid response body") {
		t.Fatalf("expected invalid-response error, got %v", err)
	}
}

// TestCodec_DecodeResponse_MissingFinishReasonDefaultsToStop pins the
// "stop" default — Cohere occasionally omits finish_reason on
// short/trivial responses; canonical OpenAI requires a non-empty value.
func TestCodec_DecodeResponse_MissingFinishReasonDefaultsToStop(t *testing.T) {
	body := []byte(`{
		"id":"r1",
		"model":"command-r-plus",
		"message":{"role":"assistant","content":[{"type":"text","text":"ok"}]}
	}`)
	decRes, err := codec{}.DecodeResponse(typology.WireShapeCohereChat, body, "", provcore.DecodeContext{})
	out := decRes.CanonicalBody
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got := gjson.GetBytes(out, "choices.0.finish_reason").String(); got != "stop" {
		t.Errorf("missing finish_reason must default to stop, got %q", got)
	}
}

// TestCodec_DecodeResponse_NoUsageOmitsUsageBlock verifies the codec
// does not synthesize an empty usage object when the upstream did not
// emit one — downstream cost ledger relies on usage absence ⇒ unknown.
func TestCodec_DecodeResponse_NoUsageOmitsUsageBlock(t *testing.T) {
	body := []byte(`{
		"id":"r1","model":"command-r-plus",
		"message":{"role":"assistant","content":[{"type":"text","text":"ok"}]},
		"finish_reason":"COMPLETE"
	}`)
	decRes, err := codec{}.DecodeResponse(typology.WireShapeCohereChat, body, "", provcore.DecodeContext{})
	out := decRes.CanonicalBody
	usage := decRes.Usage
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if gjson.GetBytes(out, "usage").Exists() {
		t.Errorf("usage must be omitted when upstream lacks tokens, got %s", string(out))
	}
	if usage.PromptTokens != nil || usage.CompletionTokens != nil {
		t.Errorf("expected zero usage, got %+v", usage)
	}
}

// errors.Normalize / parseRetryAfter

// TestErrorNormalizer_ErrorObjectMessage covers the {"error":{"message":...,"type":...}}
// envelope branch — both message AND type must be lifted onto
// ProviderError.
func TestErrorNormalizer_ErrorObjectMessage(t *testing.T) {
	pe := errorNormalizer{}.Normalize(400, http.Header{}, []byte(`{"error":{"message":"bad model","type":"invalid_request"}}`))
	if pe.Message != "bad model" {
		t.Errorf("Message=%q want bad model", pe.Message)
	}
	if pe.Type != "invalid_request" {
		t.Errorf("Type=%q want invalid_request", pe.Type)
	}
	if pe.Code != provcore.CodeInvalidRequest {
		t.Errorf("Code=%q want %q", pe.Code, provcore.CodeInvalidRequest)
	}
}

// TestErrorNormalizer_DataField covers the legacy {"data":"...","status":...}
// envelope Cohere occasionally returns on 401.
func TestErrorNormalizer_DataField(t *testing.T) {
	pe := errorNormalizer{}.Normalize(401, http.Header{}, []byte(`{"data":"invalid token"}`))
	if pe.Message != "invalid token" {
		t.Errorf("Message=%q want invalid token", pe.Message)
	}
	if pe.Code != provcore.CodeAuthFailed {
		t.Errorf("Code=%q want %q", pe.Code, provcore.CodeAuthFailed)
	}
}

// TestErrorNormalizer_EmptyBodyFallback pins the http.StatusText fallback
// — an empty body must not leave Message blank because downstream UI
// renders it directly.
func TestErrorNormalizer_EmptyBodyFallback(t *testing.T) {
	pe := errorNormalizer{}.Normalize(http.StatusForbidden, http.Header{}, nil)
	if pe.Message == "" {
		t.Error("Message must fall back to http.StatusText, not blank")
	}
	if pe.Code != provcore.CodeAuthFailed {
		t.Errorf("403 must map to auth_failed, got %q", pe.Code)
	}
}

// TestErrorNormalizer_RateLimitedWithRetryAfter covers the 429 + retry-after
// branch. RetryAfter must be populated so the routing layer can back off.
func TestErrorNormalizer_RateLimitedWithRetryAfter(t *testing.T) {
	h := http.Header{}
	h.Set("retry-after", "3")
	pe := errorNormalizer{}.Normalize(http.StatusTooManyRequests, h, []byte(`{"message":"slow down"}`))
	if pe.Code != provcore.CodeRateLimited {
		t.Errorf("Code=%q want rate_limited", pe.Code)
	}
	if pe.RetryAfter == nil || *pe.RetryAfter != 3*time.Second {
		t.Errorf("RetryAfter=%v want 3s", pe.RetryAfter)
	}
}

// TestErrorNormalizer_RateLimited_NoRetryAfter covers the 429 path
// without a retry-after header — Code must still be rate_limited and
// RetryAfter remains nil (caller picks its own backoff).
func TestErrorNormalizer_RateLimited_NoRetryAfter(t *testing.T) {
	pe := errorNormalizer{}.Normalize(http.StatusTooManyRequests, http.Header{}, []byte(`{"message":"slow"}`))
	if pe.Code != provcore.CodeRateLimited {
		t.Errorf("Code=%q want rate_limited", pe.Code)
	}
	if pe.RetryAfter != nil {
		t.Errorf("RetryAfter=%v want nil (no header)", pe.RetryAfter)
	}
}

// TestErrorNormalizer_Timeout covers 408 + 504 → CodeTimeout.
func TestErrorNormalizer_Timeout(t *testing.T) {
	for _, status := range []int{http.StatusRequestTimeout, http.StatusGatewayTimeout} {
		pe := errorNormalizer{}.Normalize(status, http.Header{}, []byte(`{"message":"timeout"}`))
		if pe.Code != provcore.CodeTimeout {
			t.Errorf("status=%d Code=%q want timeout", status, pe.Code)
		}
	}
}

// TestErrorNormalizer_UpstreamFallback covers the default switch arm
// (e.g. 502, 503) — anything outside the explicit map must surface
// upstream_error.
func TestErrorNormalizer_UpstreamFallback(t *testing.T) {
	pe := errorNormalizer{}.Normalize(http.StatusBadGateway, http.Header{}, []byte(`{"message":"upstream blew up"}`))
	if pe.Code != provcore.CodeUpstreamError {
		t.Errorf("Code=%q want upstream_error", pe.Code)
	}
}

// TestParseRetryAfter covers all branches: empty, integer seconds,
// HTTP-date in the future, HTTP-date in the past (clamped to 0), and
// garbage input → nil.
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
	if s.Format != provcore.FormatCohere {
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

// TestNewTransport_NilLoggerDefaults pins the slog.Default() fallback on
// the transport constructor.
func TestNewTransport_NilLoggerDefaults(t *testing.T) {
	tr := NewTransport(nil)
	if tr == nil || tr.log == nil || tr.client == nil || tr.probe == nil {
		t.Fatalf("Transport must be fully populated, got %+v", tr)
	}
}

// StreamDecoder.Open / streamSession edge cases

// TestStreamDecoder_Open_NilBody pins the nil-body guard so the gateway
// never wraps a nil reader (which would panic on first Read).
func TestStreamDecoder_Open_NilBody(t *testing.T) {
	d := NewStreamDecoder(slog.Default())
	if _, err := d.Open(nil, typology.WireShapeCohereChat); err == nil {
		t.Fatal("expected error on nil body")
	}
}

// TestStreamSession_NextAfterDone_ReturnsEOF asserts that once message-end
// has been delivered, subsequent Next calls return io.EOF — the executor
// uses io.EOF as the canonical stream-finished sentinel.
func TestStreamSession_NextAfterDone_ReturnsEOF(t *testing.T) {
	raw := `data: {"type":"message-end","delta":{"finish_reason":"COMPLETE"}}` + "\n\n"
	d := NewStreamDecoder(slog.Default())
	sess, err := d.Open(io.NopCloser(strings.NewReader(raw)), typology.WireShapeCohereChat)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close() //nolint:errcheck
	if _, err := sess.Next(context.Background()); err != nil {
		t.Fatalf("first Next: %v", err)
	}
	// Second call must return io.EOF, not panic and not re-emit the chunk.
	if _, err := sess.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Errorf("second Next err=%v want io.EOF", err)
	}
}

// TestStreamSession_Next_CtxCanceled asserts that a cancelled context
// short-circuits Next before draining the scanner — important so the
// gateway can abort a slow Cohere stream when the client disconnects.
func TestStreamSession_Next_CtxCanceled(t *testing.T) {
	raw := `data: {"type":"content-delta","delta":{"message":{"content":{"text":"x"}}}}` + "\n\n"
	d := NewStreamDecoder(slog.Default())
	sess, err := d.Open(io.NopCloser(strings.NewReader(raw)), typology.WireShapeCohereChat)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close() //nolint:errcheck
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sess.Next(ctx); err == nil {
		t.Fatal("expected context error after cancel")
	}
}

// TestStreamSession_Next_ScannerEOF asserts that an empty stream surfaces
// the scanner's io.EOF up to the caller without spurious chunks.
func TestStreamSession_Next_ScannerEOF(t *testing.T) {
	d := NewStreamDecoder(slog.Default())
	sess, err := d.Open(io.NopCloser(strings.NewReader("")), typology.WireShapeCohereChat)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close() //nolint:errcheck
	if _, err := sess.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Errorf("empty stream err=%v want io.EOF", err)
	}
}

// TestStreamSession_Next_ToolCallDelta_ArgumentsOnly covers the
// arguments-only tool-call delta (no id / name) — Cohere sends these
// after the initial tool-call-start. The canonical projection must keep
// arguments but omit `function.name` / `id` fields to match OpenAI
// streaming semantics.
func TestStreamSession_Next_ToolCallDelta_ArgumentsOnly(t *testing.T) {
	raw := `data: {"type":"tool-call-delta","index":0,"delta":{"message":{"tool_calls":[{"function":{"arguments":"{\"q\":1}"}}]}}}` + "\n\n"
	d := NewStreamDecoder(slog.Default())
	sess, err := d.Open(io.NopCloser(strings.NewReader(raw)), typology.WireShapeCohereChat)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close() //nolint:errcheck
	c, err := sess.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(c.ToolCallDeltas) != 1 {
		t.Fatalf("ToolCallDeltas=%+v", c.ToolCallDeltas)
	}
	d0 := c.ToolCallDeltas[0]
	if d0.Arguments != `{"q":1}` {
		t.Errorf("Arguments=%q", d0.Arguments)
	}
	if d0.Name != "" || d0.ID != "" {
		t.Errorf("Name/ID must be empty on arguments-only delta, got Name=%q ID=%q", d0.Name, d0.ID)
	}
	// Canonical SSE frame must carry function.arguments but not function.name.
	if !strings.Contains(string(c.RawBytes), `"arguments":"{\"q\":1}"`) {
		t.Errorf("RawBytes missing canonical arguments: %s", c.RawBytes)
	}
	if strings.Contains(string(c.RawBytes), `"name":`) {
		t.Errorf("RawBytes must omit function.name on arguments-only delta: %s", c.RawBytes)
	}
}

// TestStreamSession_Next_DefaultEventNoDelta covers the catch-all branch
// (message-start, content-start, content-end, tool-call-end). These
// emit no canonical Delta but still produce an empty-delta RawBytes
// frame so the audit pipeline sees a chunk per upstream event.
func TestStreamSession_Next_DefaultEventNoDelta(t *testing.T) {
	raw := `data: {"type":"message-start","id":"m1","delta":{"message":{"role":"assistant"}}}` + "\n\n"
	d := NewStreamDecoder(slog.Default())
	sess, err := d.Open(io.NopCloser(strings.NewReader(raw)), typology.WireShapeCohereChat)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close() //nolint:errcheck
	c, err := sess.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if c.Delta != "" {
		t.Errorf("message-start must not yield Delta, got %q", c.Delta)
	}
	if c.NativeEvent != "message-start" {
		t.Errorf("NativeEvent=%q want message-start", c.NativeEvent)
	}
	if !strings.Contains(string(c.RawBytes), `"delta":{}`) {
		t.Errorf("RawBytes must carry empty-delta canonical frame, got %s", c.RawBytes)
	}
}

// TestStreamSession_Close_Idempotent verifies Close marks done so a
// later Next returns EOF — the executor sometimes calls Close on error
// paths and then drains.
func TestStreamSession_Close_Idempotent(t *testing.T) {
	raw := `data: {"type":"content-delta","delta":{"message":{"content":{"text":"x"}}}}` + "\n\n"
	d := NewStreamDecoder(slog.Default())
	sess, err := d.Open(io.NopCloser(strings.NewReader(raw)), typology.WireShapeCohereChat)
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Errorf("Close err=%v", err)
	}
	if _, err := sess.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Errorf("Next after Close: err=%v want io.EOF", err)
	}
}

// Transport.BuildURL / ApplyAuth / Do / Probe

// TestTransport_BuildURL_AllEndpoints covers every Endpoint arm:
// chat (/v2/chat), embeddings (/v2/embed), models (/v1/models), and the
// unsupported-endpoint guard. Also pins the trailing-slash trim.
func TestTransport_BuildURL_AllEndpoints(t *testing.T) {
	tr := NewTransport(slog.Default())
	cases := []struct {
		endpoint typology.WireShape
		baseURL  string
		want     string
	}{
		{typology.WireShapeCohereChat, "https://api.cohere.com", "https://api.cohere.com/v2/chat"},
		{typology.WireShapeCohereChat, "https://api.cohere.com/", "https://api.cohere.com/v2/chat"},
		{typology.WireShapeCohereEmbed, "https://api.cohere.com", "https://api.cohere.com/v2/embed"},
		{typology.WireShapeNone, "https://api.cohere.com", "https://api.cohere.com/v1/models"},
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
	if _, err := tr.BuildURL(provcore.CallTarget{BaseURL: "https://x"}, typology.WireShapeOpenAIResponses, false); err == nil {
		t.Error("unsupported endpoint must error")
	}
}

// TestTransport_BuildURL_EmptyBaseURL pins the base-URL guard so a
// half-configured target never produces "/v2/chat" with no host.
func TestTransport_BuildURL_EmptyBaseURL(t *testing.T) {
	tr := NewTransport(slog.Default())
	if _, err := tr.BuildURL(provcore.CallTarget{}, typology.WireShapeCohereChat, false); err == nil {
		t.Error("empty BaseURL must error")
	}
}

// TestTransport_ApplyAuth_MissingAPIKey pins the missing-key guard —
// callers must never fire a Cohere request without auth.
func TestTransport_ApplyAuth_MissingAPIKey(t *testing.T) {
	tr := NewTransport(slog.Default())
	req, _ := http.NewRequest(http.MethodPost, "https://api.cohere.com/v2/chat", nil)
	if err := tr.ApplyAuth(req, provcore.CallTarget{}); err == nil {
		t.Error("missing API key must error")
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
		if r.URL.Path != "/v2/chat" {
			t.Errorf("path=%q want /v2/chat", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"r1"}`))
	}))
	defer srv.Close()

	tr := NewTransport(slog.Default())
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v2/chat", strings.NewReader(`{}`))
	resp, err := tr.Do(context.Background(), req, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("Do err=%v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status=%d want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"r1"`) {
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

// TestTransport_Probe_Success covers the 2xx happy path — Cohere
// serves /v1/models to authenticated tokens and we assert the bearer
// token rides the request.
func TestTransport_Probe_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path=%q want /v1/models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer co_test" {
			t.Errorf("Authorization=%q want Bearer co_test", got)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()

	tr := NewTransport(slog.Default())
	r, err := tr.Probe(context.Background(), provcore.CallTarget{BaseURL: srv.URL, APIKey: "co_test"})
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
		t.Errorf("Detail=%q want ok", r.Detail)
	}
}

// TestTransport_Probe_HTTPFailure covers the non-2xx branch — 5xx from
// Cohere /v1/models marks the probe not-OK but does NOT surface a Go
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

// TestTransport_Probe_BadURL covers the http.NewRequestWithContext
// error branch — a base URL containing an illegal control char fails
// at request construction, before any network call.
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

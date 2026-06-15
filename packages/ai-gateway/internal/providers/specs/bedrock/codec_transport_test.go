package bedrock

// White-box tests for the bedrock adapter codec + transport. Internal package
// so we can exercise the unexported `errorNormalizer`, the codec error paths,
// and the Transport helper methods (BuildURL / ApplyAuth / Do / Probe) that
// the external spec_test.go reaches only through the SpecAdapter wrapper.
//
// Scope mirrors the binding rule from CLAUDE.md → "Unit test coverage
// ≥95%": tests must assert OBSERVABLE behavior — emitted wire bytes,
// SigV4 service scope, ProviderError code matrix, Probe HTTP outcomes —
// not just `err == nil`.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

// spec.go / codec.go / stream.go — constructor nil-logger fallbacks.

// TestNewSpec_NilLogger covers the `if log == nil` fallback in NewSpec
// + asserts the spec is fully wired (Format + every adapter component).
func TestNewSpec_NilLogger(t *testing.T) {
	spec := NewSpec(nil)
	if spec.Format != provcore.FormatBedrock {
		t.Errorf("Format=%v want FormatBedrock", spec.Format)
	}
	if spec.Transport == nil || spec.SchemaCodec == nil || spec.StreamDecoder == nil || spec.ErrorNormalizer == nil {
		t.Fatalf("NewSpec must wire every component: %+v", spec)
	}
}

// TestNewSpec_CustomLogger keeps a custom logger reference reachable
// through Transport (which exposes log via the package-private field) —
// verifies the non-nil branch.
func TestNewSpec_CustomLogger(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	spec := NewSpec(log)
	tr, ok := spec.Transport.(*Transport)
	if !ok || tr.log != log {
		t.Errorf("custom logger not preserved through NewSpec")
	}
}

// TestNewCodec_NilLogger covers the nil-logger fallback in NewCodec.
func TestNewCodec_NilLogger(t *testing.T) {
	c := NewCodec(nil)
	if c == nil {
		t.Fatal("NewCodec(nil) returned nil")
	}
	// Smoke: must still encode a minimal valid body.
	encRes, err := c.EncodeRequest(
		typology.WireShapeBedrockConverse,
		[]byte(`{"messages":[{"role":"user","content":"hi"}],"max_tokens":4}`),
		provcore.CallTarget{ProviderModelID: "anthropic.claude-3-haiku-20240307-v1:0"},
	)
	out := encRes.Body
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if gjson.GetBytes(out, "anthropic_version").String() != anthropicVersion {
		t.Errorf("anthropic_version missing in NewCodec(nil) output: %s", out)
	}
}

// TestNewTransport_NilLogger covers Transport's nil-logger fallback.
func TestNewTransport_NilLogger(t *testing.T) {
	tr := NewTransport(nil)
	if tr == nil || tr.log == nil || tr.client == nil || tr.probe == nil || tr.signer == nil {
		t.Fatalf("NewTransport(nil) wiring incomplete: %+v", tr)
	}
}

// TestNewBedrockStreamDecoder_NilLogger covers the nil-logger fallback.
func TestNewBedrockStreamDecoder_NilLogger(t *testing.T) {
	d := newBedrockStreamDecoder(nil)
	if d == nil || d.log == nil {
		t.Fatalf("newBedrockStreamDecoder(nil) wiring incomplete: %+v", d)
	}
	// Also reach the body-nil branch of Open — the existing
	// stream_test.go passes io.NopCloser(nil) but never asserts the nil
	// shortcut. Pass an explicit nil io.ReadCloser to cover the guard.
	_, err := d.Open(nil, typology.WireShapeBedrockConverse)
	if err == nil {
		t.Fatal("Open must reject streaming")
	}
	var pe *provcore.ProviderError
	if !errors.As(err, &pe) || pe.Code != provcore.CodeEndpointUnsupported {
		t.Fatalf("Open: want CodeEndpointUnsupported, got %#v", err)
	}
	if pe.Status != http.StatusBadRequest {
		t.Errorf("status=%d want 400", pe.Status)
	}
}

// codec.go — EncodeRequest / DecodeResponse error matrix.

// TestCodec_EncodeRequest_UnsupportedEndpoint asserts the early reject for
// endpoints that are neither chat_completions nor embeddings.
func TestCodec_EncodeRequest_UnsupportedEndpoint(t *testing.T) {
	c := NewCodec(slog.Default())
	_, err := c.EncodeRequest(typology.WireShapeNone, []byte(`{}`), provcore.CallTarget{})
	if err == nil || !strings.Contains(err.Error(), "unsupported endpoint") {
		t.Fatalf("expected unsupported-endpoint error, got %v", err)
	}
}

// TestCodec_EncodeRequest_EmbeddingsDispatch asserts that EndpointEmbeddings
// is dispatched to the titan/cohere embed codec (not rejected as unsupported).
// An empty ProviderModelID surfaces an "unsupported embedding model" error from
// the embed dispatcher — NOT a generic "unsupported endpoint" error.
func TestCodec_EncodeRequest_EmbeddingsDispatch(t *testing.T) {
	c := NewCodec(slog.Default())
	_, err := c.EncodeRequest(typology.WireShapeBedrockEmbeddings, []byte(`{"input":"hi"}`), provcore.CallTarget{})
	// EndpointEmbeddings is dispatched to embeddingEncodeRequest; empty
	// ProviderModelID yields "unsupported embedding model".
	if err == nil || !strings.Contains(err.Error(), "unsupported embedding model") {
		t.Fatalf("expected embed-model error, got %v", err)
	}
}

// TestCodec_DecodeResponse_Embeddings_CountMismatch_Rejected pins F-0220
// for Bedrock: a Cohere-on-Bedrock response with fewer vectors than the
// request `texts` must fail the decode (→ 502) instead of returning
// misaligned vectors. The request context carries the Bedrock wire body.
func TestCodec_DecodeResponse_Embeddings_CountMismatch_Rejected(t *testing.T) {
	c := NewCodec(slog.Default())
	reqBody := []byte(`{"texts":["a","b","c"]}`)
	native := []byte(`{"embeddings":[[0.1],[0.2]],"id":"x","response_type":"embeddings_floats"}`)
	_, err := c.DecodeResponse(typology.WireShapeBedrockEmbeddings, native, "application/json",
		provcore.DecodeContext{RequestBody: reqBody})
	if err == nil || !strings.Contains(err.Error(), "embedding count mismatch") {
		t.Fatalf("expected count-mismatch error, got %v", err)
	}
}

// TestCodec_DecodeResponse_Embeddings_CountMatch_Passes is the F-0220
// positive arm for the Titan single-input shape (inputText → 1 vector).
func TestCodec_DecodeResponse_Embeddings_CountMatch_Passes(t *testing.T) {
	c := NewCodec(slog.Default())
	reqBody := []byte(`{"inputText":"hello"}`)
	native := []byte(`{"embedding":[0.1,0.2],"inputTextTokenCount":3}`)
	res, err := c.DecodeResponse(typology.WireShapeBedrockEmbeddings, native, "application/json",
		provcore.DecodeContext{RequestBody: reqBody})
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if got := gjson.GetBytes(res.CanonicalBody, "data.#").Int(); got != 1 {
		t.Errorf("data count=%d want 1", got)
	}
}

// TestCodec_EncodeRequest_EmptyBody asserts the empty-body guard fires
// before anthropic codec ever runs.
func TestCodec_EncodeRequest_EmptyBody(t *testing.T) {
	c := NewCodec(slog.Default())
	_, err := c.EncodeRequest(typology.WireShapeBedrockConverse, nil, provcore.CallTarget{})
	if err == nil || !strings.Contains(err.Error(), "empty canonical body") {
		t.Fatalf("expected empty-body error, got %v", err)
	}
}

// TestCodec_EncodeRequest_AnthropicErrorPropagated covers the
// `if err != nil { return nil, nil, err }` branch — passing a body the
// anthropic codec rejects (no `model` and CallTarget.ProviderModelID
// empty) must surface that error without being shadowed.
func TestCodec_EncodeRequest_AnthropicErrorPropagated(t *testing.T) {
	c := NewCodec(slog.Default())
	_, err := c.EncodeRequest(
		typology.WireShapeBedrockConverse,
		// non-empty so we get past the empty-body guard; missing model
		// triggers the Anthropic codec's `missing model` error.
		[]byte(`{"messages":[{"role":"user","content":"hi"}]}`),
		provcore.CallTarget{},
	)
	if err == nil || !strings.Contains(err.Error(), "missing model") {
		t.Fatalf("expected anthropic missing-model error, got %v", err)
	}
}

// TestCodec_EncodeRequest_StripsModelSetsVersion is the positive case —
// `model` is removed from the body (Bedrock expects it in the URL) and
// the pinned `anthropic_version` is stamped.
func TestCodec_EncodeRequest_StripsModelSetsVersion(t *testing.T) {
	c := NewCodec(slog.Default())
	encRes, err := c.EncodeRequest(
		typology.WireShapeBedrockConverse,
		[]byte(`{"messages":[{"role":"user","content":"hi"}],"max_tokens":32}`),
		provcore.CallTarget{ProviderModelID: "anthropic.claude-3-sonnet-20240229-v1:0"},
	)
	out := encRes.Body
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(out, "model").Exists() {
		t.Errorf("model must be stripped from Bedrock body: %s", out)
	}
	if got := gjson.GetBytes(out, "anthropic_version").String(); got != anthropicVersion {
		t.Errorf("anthropic_version=%q want %q", got, anthropicVersion)
	}
}

// TestCodec_DecodeResponse_DelegatesToAnthropic asserts the response
// passthrough — Bedrock's InvokeModel response envelope is byte-for-byte
// Anthropic Messages shape, so decode parity is non-negotiable.
func TestCodec_DecodeResponse_DelegatesToAnthropic(t *testing.T) {
	c := NewCodec(slog.Default())
	native := []byte(`{
		"id":"msg_42","type":"message","role":"assistant",
		"content":[{"type":"text","text":"hi"}],
		"stop_reason":"end_turn",
		"usage":{"input_tokens":3,"output_tokens":1}
	}`)
	decRes, err := c.DecodeResponse(typology.WireShapeBedrockConverse, native, "", provcore.DecodeContext{})
	canon := decRes.CanonicalBody
	usage := decRes.Usage
	if err != nil {
		t.Fatal(err)
	}
	// Bedrock's response envelope is byte-for-byte Anthropic Messages
	// shape, so usage.input_tokens / usage.output_tokens must project
	// into the canonical Usage (PromptTokens / CompletionTokens).
	if usage.PromptTokens == nil || *usage.PromptTokens != 3 {
		t.Errorf("PromptTokens projection lost: %+v", usage.PromptTokens)
	}
	if usage.CompletionTokens == nil || *usage.CompletionTokens != 1 {
		t.Errorf("CompletionTokens projection lost: %+v", usage.CompletionTokens)
	}
	var parsed map[string]any
	if err := json.Unmarshal(canon, &parsed); err != nil {
		t.Fatalf("DecodeResponse returned non-JSON: %v", err)
	}
	choices, _ := parsed["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("no choices in canonical: %s", canon)
	}
}

// errors.go — errorNormalizer.Normalize matrix.

// TestErrorNormalizer_TypeMatrix covers the entire __type → canonical
// code switch — every documented Bedrock exception class must surface
// the right canonical code so the executor can branch on retry/auth.
func TestErrorNormalizer_TypeMatrix(t *testing.T) {
	cases := []struct {
		typ      string
		status   int
		wantCode string
	}{
		{"ThrottlingException", 429, provcore.CodeRateLimited},
		{"TooManyRequestsException", 429, provcore.CodeRateLimited},
		{"ValidationException", 400, provcore.CodeInvalidRequest},
		{"AccessDeniedException", 403, provcore.CodeAuthFailed},
		{"UnrecognizedClientException", 403, provcore.CodeAuthFailed},
		{"ModelNotReadyException", 503, provcore.CodeUpstreamError},
		{"ServiceUnavailableException", 503, provcore.CodeUpstreamError},
		{"InternalServerException", 500, provcore.CodeUpstreamError},
		{"ModelTimeoutException", 504, provcore.CodeTimeout},
	}
	var n errorNormalizer
	for _, tc := range cases {
		body := []byte(fmt.Sprintf(`{"__type":%q,"message":"boom"}`, tc.typ))
		pe := n.Normalize(tc.status, http.Header{}, body)
		if pe.Code != tc.wantCode {
			t.Errorf("%s → code=%q want %q", tc.typ, pe.Code, tc.wantCode)
		}
		if pe.Type != tc.typ {
			t.Errorf("%s → preserved type lost: got %q", tc.typ, pe.Type)
		}
		if pe.Message != "boom" {
			t.Errorf("%s → message lost: got %q", tc.typ, pe.Message)
		}
		if pe.Status != tc.status {
			t.Errorf("%s → status=%d want %d", tc.typ, pe.Status, tc.status)
		}
	}
}

// TestErrorNormalizer_StatusFallback hits the "type missing → infer
// from HTTP status" branch — covers lines 44-57 of errors.go.
func TestErrorNormalizer_StatusFallback(t *testing.T) {
	cases := []struct {
		status   int
		wantCode string
	}{
		{http.StatusBadRequest, provcore.CodeInvalidRequest},
		{http.StatusUnauthorized, provcore.CodeAuthFailed},
		{http.StatusForbidden, provcore.CodeAuthFailed},
		{http.StatusTooManyRequests, provcore.CodeRateLimited},
		{http.StatusRequestTimeout, provcore.CodeTimeout},
		{http.StatusGatewayTimeout, provcore.CodeTimeout},
		{http.StatusInternalServerError, provcore.CodeUpstreamError},
		{http.StatusBadGateway, provcore.CodeUpstreamError},
	}
	var n errorNormalizer
	for _, tc := range cases {
		pe := n.Normalize(tc.status, http.Header{}, []byte(`{}`))
		if pe.Code != tc.wantCode {
			t.Errorf("status %d → code=%q want %q", tc.status, pe.Code, tc.wantCode)
		}
	}
}

// TestErrorNormalizer_TypeFromLowercaseField exercises the
// `"type"` fallback when AWS payload uses the lower-case form (some
// Bedrock pre-flight 400s do).
func TestErrorNormalizer_TypeFromLowercaseField(t *testing.T) {
	var n errorNormalizer
	pe := n.Normalize(400, http.Header{}, []byte(`{"type":"ValidationException","message":"bad"}`))
	if pe.Type != "ValidationException" {
		t.Errorf("type=%q want ValidationException (lowercase fallback)", pe.Type)
	}
	if pe.Code != provcore.CodeInvalidRequest {
		t.Errorf("code=%q want %q", pe.Code, provcore.CodeInvalidRequest)
	}
}

// TestErrorNormalizer_MessageFromUppercaseField hits the "Message"
// fallback (Bedrock's runtime occasionally PascalCases it).
func TestErrorNormalizer_MessageFromUppercaseField(t *testing.T) {
	var n errorNormalizer
	pe := n.Normalize(400, http.Header{}, []byte(`{"__type":"ValidationException","Message":"upper"}`))
	if pe.Message != "upper" {
		t.Errorf("message=%q want upper (PascalCase fallback)", pe.Message)
	}
}

// TestErrorNormalizer_EmptyMessageDefaultsToStatusText pins the final
// fallback (line 29-31) where neither field is present.
func TestErrorNormalizer_EmptyMessageDefaultsToStatusText(t *testing.T) {
	var n errorNormalizer
	pe := n.Normalize(http.StatusTeapot, http.Header{}, []byte(`{}`))
	if pe.Message != http.StatusText(http.StatusTeapot) {
		t.Errorf("message=%q want %q", pe.Message, http.StatusText(http.StatusTeapot))
	}
}

// TestErrorNormalizer_RawPreserved asserts we never lose the original
// upstream bytes — required for the observability + audit trail.
func TestErrorNormalizer_RawPreserved(t *testing.T) {
	var n errorNormalizer
	body := []byte(`{"__type":"ValidationException","message":"boom"}`)
	pe := n.Normalize(400, http.Header{}, body)
	if !bytes.Equal(pe.Raw, body) {
		t.Errorf("Raw bytes mutated: %q want %q", pe.Raw, body)
	}
}

// transport.go — BuildURL error matrix.

// TestBuildURL_DefaultHostFromRegion covers the empty-BaseURL branch
// (line 69-75) — the host is synthesized from aws.region.
func TestBuildURL_DefaultHostFromRegion(t *testing.T) {
	tr := NewTransport(slog.Default())
	got, err := tr.BuildURL(
		provcore.CallTarget{
			ProviderModelID: "anthropic.claude-3-haiku-20240307-v1:0",
			Extras:          map[string]string{"aws.region": "eu-west-2"},
		},
		typology.WireShapeBedrockConverse, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "https://bedrock-runtime.eu-west-2.amazonaws.com/") {
		t.Errorf("default host: %s", got)
	}
}

// TestBuildURL_MissingBaseAndRegion covers the error branch when neither
// BaseURL nor aws.region is provided.
func TestBuildURL_MissingBaseAndRegion(t *testing.T) {
	tr := NewTransport(slog.Default())
	_, err := tr.BuildURL(
		provcore.CallTarget{ProviderModelID: "anthropic.claude-3-haiku-20240307-v1:0"},
		typology.WireShapeBedrockConverse, false,
	)
	if err == nil || !strings.Contains(err.Error(), "missing BaseURL and aws.region") {
		t.Fatalf("expected missing-region error, got %v", err)
	}
}

// TestBuildURL_UnsupportedEndpoint asserts that only chat_completions and
// embeddings are allowed — EndpointModels is not supported.
func TestBuildURL_UnsupportedEndpoint(t *testing.T) {
	tr := NewTransport(slog.Default())
	_, err := tr.BuildURL(
		provcore.CallTarget{
			ProviderModelID: "anthropic.claude-3-haiku-20240307-v1:0",
			Extras:          map[string]string{"aws.region": "us-east-1"},
		},
		typology.WireShapeNone, false,
	)
	if err == nil || !strings.Contains(err.Error(), "only chat_completions and embeddings are supported") {
		t.Errorf("endpoint models: want unsupported error, got %v", err)
	}
}

// TestBuildURL_Embeddings asserts the embeddings endpoint constructs a
// /model/<modelId>/invoke URL (no -with-response-stream suffix).
func TestBuildURL_Embeddings(t *testing.T) {
	tr := NewTransport(slog.Default())
	got, err := tr.BuildURL(
		provcore.CallTarget{
			ProviderModelID: "amazon.titan-embed-text-v2:0",
			Extras:          map[string]string{"aws.region": "us-east-1"},
		},
		typology.WireShapeBedrockEmbeddings, false,
	)
	if err != nil {
		t.Fatalf("BuildURL embeddings: %v", err)
	}
	if !strings.Contains(got, "amazon.titan-embed-text-v2:0/invoke") {
		t.Errorf("embed URL=%q should contain titan model and /invoke", got)
	}
	// Streaming flag should not change the embed URL (no response-stream for embeddings).
	gotStream, err := tr.BuildURL(
		provcore.CallTarget{
			ProviderModelID: "amazon.titan-embed-text-v2:0",
			Extras:          map[string]string{"aws.region": "us-east-1"},
		},
		typology.WireShapeBedrockEmbeddings, true,
	)
	if err != nil {
		t.Fatalf("BuildURL embeddings stream=true: %v", err)
	}
	if got != gotStream {
		t.Errorf("stream=true must not change embed URL: %q vs %q", got, gotStream)
	}
}

// TestBuildURL_Embeddings_MissingModel covers the missing ProviderModelID path.
func TestBuildURL_Embeddings_MissingModel(t *testing.T) {
	tr := NewTransport(slog.Default())
	_, err := tr.BuildURL(
		provcore.CallTarget{Extras: map[string]string{"aws.region": "us-east-1"}},
		typology.WireShapeBedrockEmbeddings, false,
	)
	if err == nil || !strings.Contains(err.Error(), "missing ProviderModelID") {
		t.Fatalf("expected missing-model error, got %v", err)
	}
}

// TestBuildURL_MissingProviderModelID covers the empty-model branch.
func TestBuildURL_MissingProviderModelID(t *testing.T) {
	tr := NewTransport(slog.Default())
	_, err := tr.BuildURL(
		provcore.CallTarget{Extras: map[string]string{"aws.region": "us-east-1"}},
		typology.WireShapeBedrockConverse, false,
	)
	if err == nil || !strings.Contains(err.Error(), "missing ProviderModelID") {
		t.Fatalf("expected missing-model error, got %v", err)
	}
}

// TestBuildURL_TrailingSlashStripped pins the normalization — a
// BaseURL with trailing slash must NOT yield `//model/...`.
func TestBuildURL_TrailingSlashStripped(t *testing.T) {
	tr := NewTransport(slog.Default())
	got, err := tr.BuildURL(
		provcore.CallTarget{
			BaseURL:         "https://custom.example.com/",
			ProviderModelID: "anthropic.claude-3-haiku-20240307-v1:0",
		},
		typology.WireShapeBedrockConverse, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "com//") {
		t.Errorf("trailing slash not stripped: %s", got)
	}
}

// transport.go — ApplyAuth.

// TestApplyAuth_MissingAccessKey covers the access-key error branch.
func TestApplyAuth_MissingAccessKey(t *testing.T) {
	tr := NewTransport(slog.Default())
	r := httptest.NewRequest(http.MethodPost, "https://x/invoke", nil)
	err := tr.ApplyAuth(r, provcore.CallTarget{
		Extras: map[string]string{"aws.region": "us-east-1", "aws.secretKey": "s"},
	})
	if err == nil || !strings.Contains(err.Error(), "aws.accessKey") {
		t.Fatalf("missing-accessKey: got %v", err)
	}
}

// TestApplyAuth_MissingSecretKey is the symmetric case.
func TestApplyAuth_MissingSecretKey(t *testing.T) {
	tr := NewTransport(slog.Default())
	r := httptest.NewRequest(http.MethodPost, "https://x/invoke", nil)
	err := tr.ApplyAuth(r, provcore.CallTarget{
		Extras: map[string]string{"aws.region": "us-east-1", "aws.accessKey": "a"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

// TestApplyAuth_MissingRegion covers the region check.
func TestApplyAuth_MissingRegion(t *testing.T) {
	tr := NewTransport(slog.Default())
	r := httptest.NewRequest(http.MethodPost, "https://x/invoke", nil)
	err := tr.ApplyAuth(r, provcore.CallTarget{
		Extras: map[string]string{"aws.accessKey": "a", "aws.secretKey": "s"},
	})
	if err == nil || !strings.Contains(err.Error(), "aws.region") {
		t.Fatalf("missing-region: got %v", err)
	}
}

// TestApplyAuth_StreamingAcceptHeader covers the path-based Accept
// switch — `/invoke-with-response-stream` advertises eventstream.
func TestApplyAuth_StreamingAcceptHeader(t *testing.T) {
	tr := NewTransport(slog.Default())
	r := httptest.NewRequest(http.MethodPost, "https://x/model/m/invoke-with-response-stream", nil)
	err := tr.ApplyAuth(r, provcore.CallTarget{
		Extras: map[string]string{"aws.region": "us-east-1", "aws.accessKey": "a", "aws.secretKey": "s"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Header.Get("Accept"); got != "application/vnd.amazon.eventstream" {
		t.Errorf("streaming Accept=%q want application/vnd.amazon.eventstream", got)
	}
}

// TestApplyAuth_NonStreamingAcceptHeader covers the JSON Accept branch.
func TestApplyAuth_NonStreamingAcceptHeader(t *testing.T) {
	tr := NewTransport(slog.Default())
	r := httptest.NewRequest(http.MethodPost, "https://x/model/m/invoke", nil)
	err := tr.ApplyAuth(r, provcore.CallTarget{
		Extras: map[string]string{"aws.region": "us-east-1", "aws.accessKey": "a", "aws.secretKey": "s"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Header.Get("Accept"); got != "application/json" {
		t.Errorf("non-stream Accept=%q want application/json", got)
	}
}

// TestApplyAuth_PreserveExistingAccept asserts a caller-provided Accept
// header is not overwritten (operator-set headers must win).
func TestApplyAuth_PreserveExistingAccept(t *testing.T) {
	tr := NewTransport(slog.Default())
	r := httptest.NewRequest(http.MethodPost, "https://x/model/m/invoke", nil)
	r.Header.Set("Accept", "application/x-custom")
	err := tr.ApplyAuth(r, provcore.CallTarget{
		Extras: map[string]string{"aws.region": "us-east-1", "aws.accessKey": "a", "aws.secretKey": "s"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Header.Get("Accept"); got != "application/x-custom" {
		t.Errorf("Accept overwritten: %q", got)
	}
}

// TestApplyAuth_DoesNotSmuggleCredsInHeaders locks the F-0232c contract:
// ApplyAuth validates the credentials and sets only protocol headers — it
// must NOT stamp the AWS credentials into any internal request header. The
// credentials now reach the SigV4 signer through the CallTarget that Do
// receives, never through the forwarded header map.
func TestApplyAuth_DoesNotSmuggleCredsInHeaders(t *testing.T) {
	tr := NewTransport(slog.Default())
	r := httptest.NewRequest(http.MethodPost, "https://x/model/m/invoke", nil)
	err := tr.ApplyAuth(r, provcore.CallTarget{
		Extras: map[string]string{
			"aws.region":       "us-east-1",
			"aws.accessKey":    "a",
			"aws.secretKey":    "s",
			"aws.sessionToken": "tok",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{
		"X-Nexus-Bedrock-AccessKey", "X-Nexus-Bedrock-SecretKey",
		"X-Nexus-Bedrock-Region", "X-Nexus-Bedrock-SessionToken",
	} {
		if got := r.Header.Get(h); got != "" {
			t.Errorf("ApplyAuth must not set credential header %q (got %q)", h, got)
		}
	}
	if got := r.Header.Get("content-type"); got != "application/json" {
		t.Errorf("content-type=%q want application/json", got)
	}
}

// transport.go — Do.

// bedrockCredTarget is the CallTarget Do now reads SigV4 credentials from
// (F-0232c). region+accessKey+secretKey are required; sessionToken optional.
func bedrockCredTarget() provcore.CallTarget {
	return provcore.CallTarget{Extras: map[string]string{
		"aws.region": "us-east-1", "aws.accessKey": "a", "aws.secretKey": "s",
	}}
}

// TestDo_MissingCredentials covers the early-fail branch when the
// CallTarget carries no SigV4 credentials — Do must not silently dispatch
// unsigned (F-0232c: credentials come from the target, not headers).
func TestDo_MissingCredentials(t *testing.T) {
	tr := NewTransport(slog.Default())
	r := httptest.NewRequest(http.MethodPost, "https://example.invalid/invoke", strings.NewReader(`{}`))
	resp, err := tr.Do(context.Background(), r, provcore.CallTarget{})
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "missing SigV4 credentials") {
		t.Fatalf("expected missing-credentials error, got %v", err)
	}
}

// TestDo_BodyReadError covers the io.ReadAll error branch when the
// caller wires a body that always errors. Without a working body the
// request must not reach the signer (and certainly must not dispatch
// unsigned to upstream).
func TestDo_BodyReadError(t *testing.T) {
	tr := NewTransport(slog.Default())
	r := httptest.NewRequest(http.MethodPost, "https://example.invalid/invoke", &erroringReader{})
	resp, err := tr.Do(context.Background(), r, bedrockCredTarget())
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "read body") {
		t.Fatalf("expected read-body error, got %v", err)
	}
}

// erroringReader implements io.Reader to force the body-read error path.
type erroringReader struct{}

func (erroringReader) Read(_ []byte) (int, error) { return 0, errors.New("boom") }
func (erroringReader) Close() error               { return nil }

// TestDo_NilBodyUsesEmptyPayloadHash covers the `r.Body == nil` branch —
// SigV4 must still compute and stamp x-amz-content-sha256 with the
// canonical empty SHA-256, then dispatch successfully.
func TestDo_NilBodyUsesEmptyPayloadHash(t *testing.T) {
	var capturedAmz string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAmz = r.Header.Get("x-amz-content-sha256")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	tr := NewTransport(slog.Default())
	r, err := http.NewRequest(http.MethodGet, srv.URL+"/model/m/invoke", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Body = nil
	resp, err := tr.Do(context.Background(), r, bedrockCredTarget())
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	wantHash := func() string {
		sum := sha256.Sum256(nil)
		return hex.EncodeToString(sum[:])
	}()
	if capturedAmz != wantHash {
		t.Errorf("x-amz-content-sha256=%q want empty-SHA %q", capturedAmz, wantHash)
	}
}

// TestDo_SignsFromTargetCreds locks the F-0232c contract end-to-end: Do
// reads the SigV4 credentials from the CallTarget (not from forwarded
// headers), produces a signed request, propagates the session token to
// X-Amz-Security-Token, and never reintroduces the old X-Nexus-Bedrock-*
// credential-smuggling headers on the wire.
func TestDo_SignsFromTargetCreds(t *testing.T) {
	var saw http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		saw = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	tr := NewTransport(slog.Default())
	r, err := http.NewRequest(http.MethodPost, srv.URL+"/model/m/invoke", strings.NewReader(`{"k":"v"}`))
	if err != nil {
		t.Fatal(err)
	}
	target := provcore.CallTarget{Extras: map[string]string{
		"aws.region": "us-east-1", "aws.accessKey": "a", "aws.secretKey": "s",
		"aws.sessionToken": "tok",
	}}
	resp, err := tr.Do(context.Background(), r, target)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if !strings.HasPrefix(saw.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
		t.Errorf("missing SigV4 Authorization on upstream: %q", saw.Get("Authorization"))
	}
	// Session token from the target reaches the signer.
	if saw.Get("X-Amz-Security-Token") != "tok" {
		t.Errorf("X-Amz-Security-Token=%q want tok", saw.Get("X-Amz-Security-Token"))
	}
	// The old credential-smuggling headers must never appear on the wire.
	for _, h := range []string{
		"X-Nexus-Bedrock-AccessKey", "X-Nexus-Bedrock-SecretKey",
		"X-Nexus-Bedrock-Region", "X-Nexus-Bedrock-SessionToken",
	} {
		if saw.Get(h) != "" {
			t.Errorf("credential-smuggling header %q present on upstream: %q", h, saw.Get(h))
		}
	}
}

// transport.go — Probe.

// TestProbe_MissingRegion is the early-return guard.
func TestProbe_MissingRegion(t *testing.T) {
	tr := NewTransport(slog.Default())
	r, err := tr.Probe(context.Background(), provcore.CallTarget{})
	if err != nil {
		t.Fatal(err)
	}
	if r == nil || r.OK || !strings.Contains(r.Detail, "missing aws.region") {
		t.Errorf("missing-region: %+v", r)
	}
}

// TestProbe_MissingCredentials covers the access/secret-key guard.
func TestProbe_MissingCredentials(t *testing.T) {
	tr := NewTransport(slog.Default())
	r, err := tr.Probe(context.Background(), provcore.CallTarget{
		Extras: map[string]string{"aws.region": "us-east-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r == nil || r.OK || !strings.Contains(r.Detail, "aws.accessKey") {
		t.Errorf("missing-cred: %+v", r)
	}
}

// TestProbe_Success exercises the happy path through httptest. The
// canonical "ProbeUsesControlPlaneService" assertion is critical:
// AWS rejects ListFoundationModels signed with `bedrock-runtime` scope
// with SignatureDoesNotMatch, so the SigV4 Credential clause MUST
// resolve to `bedrock` (control-plane) service.
//
// Probe builds the URL from `https://bedrock.<region>.amazonaws.com`,
// not from CallTarget.BaseURL, so we cannot point it at httptest
// directly via the public surface. Instead we swap the Transport's
// probe http.Client with one whose Transport intercepts every request
// and routes to a local httptest server, so we still cover the full
// Probe code path (signer, dispatch, response classification).
func TestProbe_Success(t *testing.T) {
	tr := NewTransport(slog.Default())
	var sawAuth string
	var sawHost string
	var sawPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		sawHost = r.Host
		sawPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"modelSummaries":[]}`)
	}))
	defer srv.Close()
	srvURL, _ := url.Parse(srv.URL)
	tr.probe = redirectClient(srvURL)

	r, err := tr.Probe(context.Background(), provcore.CallTarget{
		Extras: map[string]string{
			"aws.region":    "us-east-1",
			"aws.accessKey": "AKIA",
			"aws.secretKey": "secret",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !r.OK {
		t.Fatalf("expected OK probe, got %+v", r)
	}
	if r.LatencyMs < 0 {
		t.Errorf("LatencyMs=%d", r.LatencyMs)
	}
	if sawHost != "bedrock.us-east-1.amazonaws.com" {
		t.Errorf("Probe must call the control-plane host, saw Host=%q", sawHost)
	}
	if sawPath != "/foundation-models" {
		t.Errorf("Probe path=%q want /foundation-models", sawPath)
	}
	// Critical: SigV4 service scope = `bedrock`, NOT `bedrock-runtime`.
	credIdx := strings.Index(sawAuth, "Credential=")
	if credIdx < 0 {
		t.Fatalf("Authorization missing Credential clause: %q", sawAuth)
	}
	rest := sawAuth[credIdx+len("Credential="):]
	if comma := strings.Index(rest, ","); comma >= 0 {
		rest = rest[:comma]
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 5 {
		t.Fatalf("Credential clause malformed: %q", rest)
	}
	if parts[3] != "bedrock" {
		t.Errorf("SigV4 service=%q want bedrock (control-plane scope); AWS rejects /foundation-models signed with bedrock-runtime", parts[3])
	}
}

// TestProbe_NonOK exercises the non-2xx branch.
func TestProbe_NonOK(t *testing.T) {
	tr := NewTransport(slog.Default())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	srvURL, _ := url.Parse(srv.URL)
	tr.probe = redirectClient(srvURL)

	r, err := tr.Probe(context.Background(), provcore.CallTarget{
		Extras: map[string]string{
			"aws.region":    "us-east-1",
			"aws.accessKey": "AKIA",
			"aws.secretKey": "secret",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.OK {
		t.Errorf("403 must mark OK=false: %+v", r)
	}
	if !strings.Contains(r.Detail, "403") {
		t.Errorf("Detail must surface HTTP status: %q", r.Detail)
	}
}

// TestProbe_TransportError covers the network-failure branch — we
// short-circuit the probe client with one whose RoundTripper always
// errors.
func TestProbe_TransportError(t *testing.T) {
	tr := NewTransport(slog.Default())
	tr.probe = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial-timeout")
	})}
	r, err := tr.Probe(context.Background(), provcore.CallTarget{
		Extras: map[string]string{
			"aws.region":    "us-east-1",
			"aws.accessKey": "AKIA",
			"aws.secretKey": "secret",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.OK {
		t.Errorf("transport-error must mark OK=false: %+v", r)
	}
	if !strings.Contains(r.Detail, "dial-timeout") {
		t.Errorf("Detail must wrap the dial error: %q", r.Detail)
	}
	if r.Err == nil {
		t.Error("Err must be populated on transport error")
	}
}

// roundTripFunc lets a test inject a RoundTripper without a separate
// type — mirrors the helper used in spec_anthropic coverage tests.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// redirectClient returns an http.Client whose Transport rewrites every
// outbound request to point at the test server (preserving the original
// path). This lets us exercise Probe without changing its hardcoded
// `https://bedrock.<region>.amazonaws.com` URL while still capturing
// the SigV4 headers, path, and Host the AWS signer produced.
func redirectClient(target *url.URL) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		// Preserve original Host (carries the AWS host the signer
		// produced) — handlers read r.Host to verify it.
		origHost := r.URL.Host
		r.URL.Scheme = target.Scheme
		r.URL.Host = target.Host
		// Keep r.Host as the AWS host so the handler can assert against it.
		r.Host = origHost
		return http.DefaultTransport.RoundTrip(r)
	})}
}

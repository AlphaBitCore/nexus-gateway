package envelope

import (
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/tidwall/gjson"
)

// TestEncodeErrorEnvelope_OpenAIIngressAnthropicUpstream is the
// regression test for traffic d914275a-0dae-4d13-a811-69e4d432c441: an
// OpenAI client called /v1/chat/completions, the gateway routed to
// Anthropic and the upstream returned a 400. Before this fix the client
// received the raw Anthropic envelope ({"type":"error","error":{...}})
// which strict OpenAI SDKs can't deserialise and surface as an empty
// response. After this fix the body must be the OpenAI shape so the
// SDK reads error.message cleanly.
func TestEncodeErrorEnvelope_OpenAIIngressAnthropicUpstream(t *testing.T) {
	pe := &provcore.ProviderError{
		Status:  400,
		Code:    provcore.CodeInvalidRequest,
		Type:    "invalid_request_error",
		Message: "`temperature` is deprecated for this model.",
		Raw:     []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"` + "`temperature`" + ` is deprecated for this model."}}`),
	}

	body := encodeErrorEnvelopeForIngress(provcore.FormatOpenAI, provcore.FormatAnthropic, pe)

	// Must be in OpenAI shape, not Anthropic's {"type":"error", ...}.
	if got := gjson.GetBytes(body, "type").String(); got != "" {
		t.Errorf("OpenAI envelope must not have top-level type field; got %q (body=%s)", got, body)
	}
	if got := gjson.GetBytes(body, "error.message").String(); got != pe.Message {
		t.Errorf("error.message=%q want %q (body=%s)", got, pe.Message, body)
	}
	if got := gjson.GetBytes(body, "error.type").String(); got != "invalid_request_error" {
		t.Errorf("error.type=%q want invalid_request_error (body=%s)", got, body)
	}
	if got := gjson.GetBytes(body, "error.code").String(); got != provcore.CodeInvalidRequest {
		t.Errorf("error.code=%q want %q (body=%s)", got, provcore.CodeInvalidRequest, body)
	}
}

// TestEncodeErrorEnvelope_SameFormatPassthroughPreservesRaw asserts
// native Anthropic clients still receive verbatim upstream bytes so
// any provider-specific fields beyond Message (request_id headers,
// non-canonical error.type strings the SDK branches on, …) are not
// lost in translation. Same idea for OpenAI-shape upstreams.
func TestEncodeErrorEnvelope_SameFormatPassthroughPreservesRaw(t *testing.T) {
	rawAnthropic := []byte(`{"type":"error","error":{"type":"overloaded_error","message":"slow down"}}`)
	pe := &provcore.ProviderError{
		Status:  529,
		Code:    provcore.CodeUpstreamError,
		Type:    "overloaded_error",
		Message: "slow down",
		Raw:     rawAnthropic,
	}
	body := encodeErrorEnvelopeForIngress(provcore.FormatAnthropic, provcore.FormatAnthropic, pe)
	if string(body) != string(rawAnthropic) {
		t.Errorf("same-format passthrough should return raw bytes verbatim; got %s", body)
	}

	rawOpenAI := []byte(`{"error":{"message":"out of quota","type":"insufficient_quota","code":"insufficient_quota","param":null}}`)
	peOAI := &provcore.ProviderError{
		Status:  429,
		Code:    provcore.CodeRateLimited,
		Type:    "insufficient_quota",
		Message: "out of quota",
		Raw:     rawOpenAI,
	}
	body = encodeErrorEnvelopeForIngress(provcore.FormatOpenAI, provcore.FormatOpenAI, peOAI)
	if string(body) != string(rawOpenAI) {
		t.Errorf("OpenAI same-format passthrough should return raw bytes verbatim; got %s", body)
	}
}

// TestEncodeErrorEnvelope_OpenAICompatFamilyPassthrough asserts the
// IsOpenAIFamily members (DeepSeek, GLM, Azure-OpenAI, Mistral,
// xAI, Groq, Moonshot, ...) all participate in the passthrough
// shortcut so clients targeting an OpenAI-compat alias get raw bytes
// when the upstream is also in that family.
func TestEncodeErrorEnvelope_OpenAICompatFamilyPassthrough(t *testing.T) {
	raw := []byte(`{"error":{"message":"bad","type":"invalid_request_error","code":"invalid_request"}}`)
	pe := &provcore.ProviderError{
		Status:  400,
		Code:    provcore.CodeInvalidRequest,
		Type:    "invalid_request_error",
		Message: "bad",
		Raw:     raw,
	}
	body := encodeErrorEnvelopeForIngress(provcore.FormatOpenAI, provcore.FormatDeepSeek, pe)
	if string(body) != string(raw) {
		t.Errorf("openai-shape family passthrough should return raw bytes; got %s", body)
	}
}

// TestEncodeErrorEnvelope_AnthropicIngressOpenAIUpstream is the
// reverse cross-format direction: native Anthropic client hits the
// gateway, routes to an OpenAI-compat upstream, upstream 401s. Client
// must see the Anthropic shape its SDK expects.
func TestEncodeErrorEnvelope_AnthropicIngressOpenAIUpstream(t *testing.T) {
	pe := &provcore.ProviderError{
		Status:  401,
		Code:    provcore.CodeAuthFailed,
		Type:    "invalid_api_key",
		Message: "bad credentials",
		Raw:     []byte(`{"error":{"message":"bad credentials","type":"invalid_api_key","code":"invalid_api_key"}}`),
	}
	body := encodeErrorEnvelopeForIngress(provcore.FormatAnthropic, provcore.FormatOpenAI, pe)
	if got := gjson.GetBytes(body, "type").String(); got != "error" {
		t.Errorf("Anthropic envelope must have top-level type=error; got %q (body=%s)", got, body)
	}
	if got := gjson.GetBytes(body, "error.message").String(); got != pe.Message {
		t.Errorf("error.message=%q want %q", got, pe.Message)
	}
	if got := gjson.GetBytes(body, "error.type").String(); got != "authentication_error" {
		t.Errorf("error.type=%q want authentication_error", got)
	}
}

// TestEncodeErrorEnvelope_GeminiShape asserts the Gemini ingress emits
// the Google Cloud envelope shape (with the gRPC status name) so
// google-cloud-aiplatform clients can deserialise the body.
func TestEncodeErrorEnvelope_GeminiShape(t *testing.T) {
	pe := &provcore.ProviderError{
		Status:  429,
		Code:    provcore.CodeRateLimited,
		Type:    "rate_limit_error",
		Message: "slow down",
	}
	body := encodeErrorEnvelopeForIngress(provcore.FormatGemini, provcore.FormatAnthropic, pe)
	if got := gjson.GetBytes(body, "error.code").Int(); got != 429 {
		t.Errorf("error.code=%d want 429 (body=%s)", got, body)
	}
	if got := gjson.GetBytes(body, "error.message").String(); got != pe.Message {
		t.Errorf("error.message=%q want %q", got, pe.Message)
	}
	if got := gjson.GetBytes(body, "error.status").String(); got != "RESOURCE_EXHAUSTED" {
		t.Errorf("error.status=%q want RESOURCE_EXHAUSTED", got)
	}
}

// TestOpenAIErrorTypeMapping pins the providers.Code* → OpenAI
// `error.type` string mapping so callers that branch on type strings
// (e.g. langchain rate-limit retries) don't break on a future canonical
// code rename.
func TestOpenAIErrorTypeMapping(t *testing.T) {
	cases := []struct {
		code string
		want string
	}{
		{provcore.CodeInvalidRequest, "invalid_request_error"},
		{provcore.CodeAuthFailed, "authentication_error"},
		{provcore.CodeRateLimited, "rate_limit_error"},
		{provcore.CodeTimeout, "timeout_error"},
		{provcore.CodeUpstreamError, "api_error"},
		{provcore.CodeEndpointUnsupported, "invalid_request_error"},
		{provcore.CodeNotImplemented, "invalid_request_error"},
		{provcore.CodeNoCompatibleProvider, "api_error"},
		{"unknown_future_code", "api_error"},
	}
	for _, tc := range cases {
		pe := &provcore.ProviderError{Code: tc.code}
		got := openaiErrorType(pe)
		if got != tc.want {
			t.Errorf("openaiErrorType(%q)=%q want %q", tc.code, got, tc.want)
		}
	}
}

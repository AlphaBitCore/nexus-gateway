// Package envelope — error_envelope_gap_test.go covers branches not
// reached by the existing test files.
//
// Named failure modes:
//   - EncodeErrorEnvelopeForIngress exported wrapper
//   - SynthesizeSSEErrorFrame: all 4 ingress shapes + SSE prefixes
//   - anthropicErrorType: all ProviderError.Code variants
//   - geminiStatusForHTTPCode: all status codes including UNKNOWN
//   - encodeErrorEnvelopeForIngressForStream: Responses fallback
package envelope

import (
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/tidwall/gjson"
)

// EncodeErrorEnvelopeForIngress (exported wrapper)

func TestEncodeErrorEnvelopeForIngress_exportedWrapper_openAIShape(t *testing.T) {
	pe := &provcore.ProviderError{
		Status:  401,
		Code:    provcore.CodeAuthFailed,
		Message: "bad key",
	}
	body := EncodeErrorEnvelopeForIngress(provcore.FormatOpenAI, provcore.FormatGemini, pe)
	if gjson.GetBytes(body, "error.message").String() != "bad key" {
		t.Errorf("message: got %q", gjson.GetBytes(body, "error.message").String())
	}
	if gjson.GetBytes(body, "error.type").String() != "authentication_error" {
		t.Errorf("type: got %q", gjson.GetBytes(body, "error.type").String())
	}
}

func TestEncodeErrorEnvelopeForIngress_geminiIngress_geminiShape(t *testing.T) {
	pe := &provcore.ProviderError{
		Status:  400,
		Code:    provcore.CodeInvalidRequest,
		Message: "invalid argument",
	}
	body := EncodeErrorEnvelopeForIngress(provcore.FormatGemini, provcore.FormatOpenAI, pe)
	if gjson.GetBytes(body, "error.status").String() != "INVALID_ARGUMENT" {
		t.Errorf("gemini status: got %q", gjson.GetBytes(body, "error.status").String())
	}
}

func TestEncodeErrorEnvelopeForIngress_vertexIngress_geminiShape(t *testing.T) {
	pe := &provcore.ProviderError{
		Status:  403,
		Code:    provcore.CodeAuthFailed,
		Message: "forbidden",
	}
	body := EncodeErrorEnvelopeForIngress(provcore.FormatVertex, provcore.FormatOpenAI, pe)
	if gjson.GetBytes(body, "error.status").String() != "PERMISSION_DENIED" {
		t.Errorf("vertex gemini status: got %q", gjson.GetBytes(body, "error.status").String())
	}
}

func TestEncodeErrorEnvelopeForIngress_responsesIngress_responsesShape(t *testing.T) {
	pe := &provcore.ProviderError{
		Status:  429,
		Code:    provcore.CodeRateLimited,
		Message: "quota",
	}
	body := EncodeErrorEnvelopeForIngress(provcore.FormatOpenAIResponses, provcore.FormatOpenAI, pe)
	if gjson.GetBytes(body, "error.type").String() != "rate_limit_error" {
		t.Errorf("responses type: got %q", gjson.GetBytes(body, "error.type").String())
	}
}

func TestEncodeErrorEnvelopeForIngress_sameShapeSyntheticError_fallsThrough(t *testing.T) {
	// Raw is empty but ingress == upstream → falls through to per-format encoder
	pe := &provcore.ProviderError{
		Status:  503,
		Code:    provcore.CodeUpstreamError,
		Message: "upstream error",
		Raw:     nil, // empty raw
	}
	body := EncodeErrorEnvelopeForIngress(provcore.FormatAnthropic, provcore.FormatAnthropic, pe)
	// Should produce Anthropic shape
	if gjson.GetBytes(body, "type").String() != "error" {
		t.Errorf("anthropic envelope type: got %q", gjson.GetBytes(body, "type").String())
	}
}

func TestSynthesizeSSEErrorFrame_openAIIngress_dataPrefix(t *testing.T) {
	pe := &provcore.ProviderError{Code: provcore.CodeUpstreamError, Message: "fail"}
	frame := SynthesizeSSEErrorFrame(provcore.FormatOpenAI, pe)
	frameStr := string(frame)
	if !strings.HasPrefix(frameStr, "data: ") {
		t.Errorf("OpenAI SSE frame should start with 'data: ', got %q", frameStr)
	}
	if !strings.HasSuffix(frameStr, "\n\n") {
		t.Errorf("SSE frame should end with \\n\\n, got %q", frameStr)
	}
}

func TestSynthesizeSSEErrorFrame_anthropicIngress_eventErrorPrefix(t *testing.T) {
	pe := &provcore.ProviderError{Code: provcore.CodeRateLimited, Message: "rate"}
	frame := SynthesizeSSEErrorFrame(provcore.FormatAnthropic, pe)
	frameStr := string(frame)
	if !strings.HasPrefix(frameStr, "event: error\n") {
		t.Errorf("Anthropic SSE frame should start with 'event: error\\n', got %q", frameStr)
	}
}

func TestSynthesizeSSEErrorFrame_geminiIngress_dataPrefix(t *testing.T) {
	pe := &provcore.ProviderError{Status: 500, Code: provcore.CodeUpstreamError, Message: "internal"}
	frame := SynthesizeSSEErrorFrame(provcore.FormatGemini, pe)
	frameStr := string(frame)
	if !strings.HasPrefix(frameStr, "data: ") {
		t.Errorf("Gemini SSE frame should start with 'data: ', got %q", frameStr)
	}
	// Body should be Gemini shape
	dataJSON := strings.TrimPrefix(strings.TrimSuffix(strings.TrimSuffix(frameStr, "\n\n"), "\n"), "data: ")
	if gjson.Get(dataJSON, "error.status").String() != "INTERNAL" {
		t.Errorf("Gemini SSE body: got %q", dataJSON)
	}
}

func TestSynthesizeSSEErrorFrame_responsesAPIIngress_responseFailed(t *testing.T) {
	pe := &provcore.ProviderError{Code: provcore.CodeAuthFailed, Message: "unauthorized"}
	frame := SynthesizeSSEErrorFrame(provcore.FormatOpenAIResponses, pe)
	frameStr := string(frame)
	if !strings.HasPrefix(frameStr, "event: response.failed\n") {
		t.Errorf("Responses SSE frame: got %q", frameStr)
	}
	// Extract data line and parse
	lines := strings.Split(frameStr, "\n")
	var dataJSON string
	for _, line := range lines {
		if strings.HasPrefix(line, "data: ") {
			dataJSON = strings.TrimPrefix(line, "data: ")
			break
		}
	}
	if dataJSON == "" {
		t.Fatal("no data line in response.failed frame")
	}
	if gjson.Get(dataJSON, "type").String() != "response.failed" {
		t.Errorf("type: got %q", gjson.Get(dataJSON, "type").String())
	}
	if gjson.Get(dataJSON, "response.status").String() != "failed" {
		t.Errorf("response.status: got %q", gjson.Get(dataJSON, "response.status").String())
	}
}

// anthropicErrorType coverage

func TestAnthropicErrorEnvelope_allCodes(t *testing.T) {
	cases := []struct {
		code     string
		wantType string
	}{
		{provcore.CodeInvalidRequest, "invalid_request_error"},
		{provcore.CodeAuthFailed, "authentication_error"},
		{provcore.CodeRateLimited, "rate_limit_error"},
		{provcore.CodeUpstreamError, "api_error"},
		{provcore.CodeTimeout, "api_error"},
		{"unknown_code", "api_error"},
	}
	for _, tc := range cases {
		pe := &provcore.ProviderError{Code: tc.code, Message: "msg"}
		body := encodeAnthropicErrorEnvelope(pe)
		got := gjson.GetBytes(body, "error.type").String()
		if got != tc.wantType {
			t.Errorf("code %q: got type %q, want %q", tc.code, got, tc.wantType)
		}
	}
}

// openaiErrorType additional codes

func TestOpenAIErrorEnvelope_allCodes(t *testing.T) {
	cases := []struct {
		code     string
		wantType string
	}{
		{provcore.CodeEndpointUnsupported, "invalid_request_error"},
		{provcore.CodeNotImplemented, "invalid_request_error"},
		{provcore.CodeNoCompatibleProvider, "api_error"},
		{"other_code", "api_error"},
	}
	for _, tc := range cases {
		pe := &provcore.ProviderError{Code: tc.code, Message: "msg"}
		body := encodeOpenAIErrorEnvelope(pe)
		got := gjson.GetBytes(body, "error.type").String()
		if got != tc.wantType {
			t.Errorf("code %q: got type %q, want %q", tc.code, got, tc.wantType)
		}
	}
}

// responsesAPIErrorType: feature_requires_native_responses_target

func TestResponsesAPIErrorEnvelope_featureCode_unsupportedFeature(t *testing.T) {
	pe := &provcore.ProviderError{
		Code:    "feature_requires_native_responses_target",
		Message: "this feature requires Responses target",
	}
	body := encodeResponsesAPIErrorEnvelope(pe)
	if gjson.GetBytes(body, "error.type").String() != "unsupported_feature" {
		t.Errorf("type: got %q, want unsupported_feature", gjson.GetBytes(body, "error.type").String())
	}
}

// geminiStatusForHTTPCode: all branches

func TestGeminiStatusForHTTPCode_allCodes(t *testing.T) {
	cases := []struct {
		code   int
		status string
	}{
		{400, "INVALID_ARGUMENT"},
		{401, "UNAUTHENTICATED"},
		{403, "PERMISSION_DENIED"},
		{404, "NOT_FOUND"},
		{408, "DEADLINE_EXCEEDED"},
		{409, "ALREADY_EXISTS"},
		{429, "RESOURCE_EXHAUSTED"},
		{500, "INTERNAL"},
		{501, "UNIMPLEMENTED"},
		{503, "UNAVAILABLE"},
		{504, "DEADLINE_EXCEEDED"},
		{502, "UNKNOWN"}, // unknown fallback
	}
	for _, tc := range cases {
		pe := &provcore.ProviderError{Status: tc.code, Message: "err"}
		body := encodeGeminiErrorEnvelope(pe)
		got := gjson.GetBytes(body, "error.status").String()
		if got != tc.status {
			t.Errorf("HTTP %d: got %q, want %q", tc.code, got, tc.status)
		}
	}
}

// encodeErrorEnvelopeForIngressForStream: Responses fallback

func TestSSEErrorFrame_responsesViaStreamEncoder_responsesShape(t *testing.T) {
	// encodeErrorEnvelopeForIngressForStream with FormatOpenAIResponses
	// falls through to encodeResponsesAPIErrorEnvelope.
	pe := &provcore.ProviderError{Code: provcore.CodeInvalidRequest, Message: "bad input"}
	body := encodeErrorEnvelopeForIngressForStream(provcore.FormatOpenAIResponses, pe)
	if gjson.GetBytes(body, "error.type").String() != "invalid_request_error" {
		t.Errorf("type: got %q", gjson.GetBytes(body, "error.type").String())
	}
}

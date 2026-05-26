package envelope

import (
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/tidwall/gjson"
)

// TestEncodeErrorEnvelopeForIngress_Responses_RateLimit pins the
// Responses-API envelope for a 429 → rate_limit_error type, and pins
// the JSON shape exactly so a strict SDK consumer can rely on it.
func TestEncodeErrorEnvelopeForIngress_Responses_RateLimit(t *testing.T) {
	pe := &provcore.ProviderError{
		Status:  429,
		Code:    provcore.CodeRateLimited,
		Message: "rate limited; retry in 5s",
	}
	out := encodeErrorEnvelopeForIngress(provcore.FormatOpenAIResponses, provcore.FormatAnthropic, pe)
	if got := gjson.GetBytes(out, "error.message").String(); got != "rate limited; retry in 5s" {
		t.Errorf("error.message = %q", got)
	}
	if got := gjson.GetBytes(out, "error.type").String(); got != "rate_limit_error" {
		t.Errorf("error.type = %q, want rate_limit_error", got)
	}
	if got := gjson.GetBytes(out, "error.code").String(); got != provcore.CodeRateLimited {
		t.Errorf("error.code = %q", got)
	}
}

// TestEncodeErrorEnvelopeForIngress_Responses_Auth covers
// CodeAuthFailed → authentication_error.
func TestEncodeErrorEnvelopeForIngress_Responses_Auth(t *testing.T) {
	pe := &provcore.ProviderError{
		Status:  401,
		Code:    provcore.CodeAuthFailed,
		Message: "invalid api key",
	}
	out := encodeErrorEnvelopeForIngress(provcore.FormatOpenAIResponses, provcore.FormatOpenAI, pe)
	if got := gjson.GetBytes(out, "error.type").String(); got != "authentication_error" {
		t.Errorf("error.type = %q", got)
	}
}

// TestEncodeErrorEnvelopeForIngress_Responses_InvalidRequest covers
// CodeInvalidRequest → invalid_request_error.
func TestEncodeErrorEnvelopeForIngress_Responses_InvalidRequest(t *testing.T) {
	pe := &provcore.ProviderError{
		Status:  400,
		Code:    provcore.CodeInvalidRequest,
		Message: "missing model",
	}
	out := encodeErrorEnvelopeForIngress(provcore.FormatOpenAIResponses, provcore.FormatAnthropic, pe)
	if got := gjson.GetBytes(out, "error.type").String(); got != "invalid_request_error" {
		t.Errorf("error.type = %q", got)
	}
}

// TestSynthesizeSSEErrorFrame_Responses pins the response.failed SSE
// frame structure required by OpenAI's Responses SDK on stream errors.
//
// Required shape:
//
//	event: response.failed
//	data: {"type":"response.failed","sequence_number":0,"response":{...,"error":{...}}}
func TestSynthesizeSSEErrorFrame_Responses(t *testing.T) {
	pe := &provcore.ProviderError{
		Status:  500,
		Code:    provcore.CodeUpstreamError,
		Message: "upstream 5xx",
	}
	frame := synthesizeSSEErrorFrame(provcore.FormatOpenAIResponses, pe)
	s := string(frame)
	if !strings.HasPrefix(s, "event: response.failed\n") {
		t.Fatalf("frame should start with `event: response.failed\\n`; got:\n%s", s)
	}
	if !strings.HasSuffix(s, "\n\n") {
		t.Errorf("frame should end with double newline")
	}
	// Extract the data payload.
	parts := strings.SplitN(s, "data: ", 2)
	if len(parts) != 2 {
		t.Fatalf("frame should have a data line; got:\n%s", s)
	}
	dataLine := strings.TrimSuffix(parts[1], "\n\n")
	if !gjson.Valid(dataLine) {
		t.Fatalf("data payload is not valid JSON: %s", dataLine)
	}
	if got := gjson.Get(dataLine, "type").String(); got != "response.failed" {
		t.Errorf("payload.type = %q", got)
	}
	if got := gjson.Get(dataLine, "sequence_number").Int(); got != 0 {
		t.Errorf("payload.sequence_number = %d (want 0 for pre-stream / first-event failure)", got)
	}
	if got := gjson.Get(dataLine, "response.status").String(); got != "failed" {
		t.Errorf("payload.response.status = %q", got)
	}
	if got := gjson.Get(dataLine, "response.error.message").String(); got != "upstream 5xx" {
		t.Errorf("payload.response.error.message = %q", got)
	}
	if got := gjson.Get(dataLine, "response.error.type").String(); got != "api_error" {
		t.Errorf("payload.response.error.type = %q (want api_error for CodeUpstreamError)", got)
	}
}

// TestSynthesizeSSEErrorFrame_NotResponses_StillWorks pins that the
// existing OpenAI / Anthropic / Gemini paths are not regressed.
func TestSynthesizeSSEErrorFrame_NotResponses_StillWorks(t *testing.T) {
	pe := &provcore.ProviderError{
		Status:  429,
		Code:    provcore.CodeRateLimited,
		Message: "rate limited",
	}
	// OpenAI ingress: plain `data: {"error":{...}}\n\n`.
	out := synthesizeSSEErrorFrame(provcore.FormatOpenAI, pe)
	s := string(out)
	if !strings.HasPrefix(s, "data: ") || strings.Contains(s, "event: ") {
		t.Errorf("OpenAI frame shape unexpected: %s", s)
	}
	// Anthropic ingress: `event: error\ndata: ...`.
	outA := synthesizeSSEErrorFrame(provcore.FormatAnthropic, pe)
	if !strings.HasPrefix(string(outA), "event: error\n") {
		t.Errorf("Anthropic frame shape unexpected: %s", string(outA))
	}
}

// TestResponsesAPIErrorType_AllCases pins the providers.Code* →
// Responses error type mapping table.
func TestResponsesAPIErrorType_AllCases(t *testing.T) {
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
		{"feature_requires_native_responses_target", "unsupported_feature"},
		{"some-unknown-code", "api_error"},
	}
	for _, c := range cases {
		got := responsesAPIErrorType(&provcore.ProviderError{Code: c.code})
		if got != c.want {
			t.Errorf("code=%s: got %q, want %q", c.code, got, c.want)
		}
	}
}

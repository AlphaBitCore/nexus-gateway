package envelope

import (
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/tidwall/gjson"
)

// TestEncodeErrorEnvelopeForIngress_ExportedWrapper confirms the public
// function delegates to the private implementation and returns the correct
// ingress-shaped body (OpenAI client, Anthropic upstream).
func TestEncodeErrorEnvelopeForIngress_ExportedWrapper(t *testing.T) {
	pe := &provcore.ProviderError{
		Status:  400,
		Code:    provcore.CodeInvalidRequest,
		Message: "bad param",
	}
	// OpenAI ingress, Anthropic upstream → must be OpenAI shape.
	out := EncodeErrorEnvelopeForIngress(provcore.FormatOpenAI, provcore.FormatAnthropic, pe)
	if got := gjson.GetBytes(out, "error.message").String(); got != "bad param" {
		t.Errorf("error.message = %q, want bad param", got)
	}
	if got := gjson.GetBytes(out, "error.type").String(); got != "invalid_request_error" {
		t.Errorf("error.type = %q, want invalid_request_error", got)
	}
}

// TestSynthesizeSSEErrorFrame_ExportedWrapper confirms the public function
// delegates correctly and returns an SSE frame in the ingress format.
func TestSynthesizeSSEErrorFrame_ExportedWrapper(t *testing.T) {
	pe := &provcore.ProviderError{
		Status:  500,
		Code:    provcore.CodeUpstreamError,
		Message: "upstream error",
	}
	frame := SynthesizeSSEErrorFrame(provcore.FormatOpenAI, pe)
	s := string(frame)
	if !strings.HasPrefix(s, "data: ") {
		t.Errorf("OpenAI SSE frame should start with 'data: ', got: %s", s)
	}
	if !strings.HasSuffix(s, "\n\n") {
		t.Errorf("SSE frame should end with double newline")
	}
}

// TestAnthropicErrorType_AllCases pins the Code* → Anthropic error.type
// mapping so native Anthropic SDKs receive the string they branch on.
func TestAnthropicErrorType_AllCases(t *testing.T) {
	cases := []struct {
		code string
		want string
	}{
		{provcore.CodeInvalidRequest, "invalid_request_error"},
		{provcore.CodeAuthFailed, "authentication_error"},
		{provcore.CodeRateLimited, "rate_limit_error"},
		{provcore.CodeUpstreamError, "api_error"},
		{provcore.CodeTimeout, "api_error"},
		{"some-unknown-code", "api_error"},
	}
	for _, tc := range cases {
		got := anthropicErrorType(&provcore.ProviderError{Code: tc.code})
		if got != tc.want {
			t.Errorf("anthropicErrorType(%q) = %q, want %q", tc.code, got, tc.want)
		}
	}
}

// TestEncodeErrorEnvelopeForIngressForStream_AllIngress exercises every
// ingress branch: FormatVertex (same encoder as Gemini), and
// FormatOpenAIResponses (defensive fallback — Responses path is normally
// handled by synthesizeSSEErrorFrame directly).
func TestEncodeErrorEnvelopeForIngressForStream_AllIngress(t *testing.T) {
	pe := &provcore.ProviderError{
		Status:  500,
		Code:    provcore.CodeUpstreamError,
		Message: "stream broke",
	}

	// FormatVertex → Gemini shape: error.code = HTTP status, error.status = gRPC name.
	outVertex := encodeErrorEnvelopeForIngressForStream(provcore.FormatVertex, pe)
	if got := gjson.GetBytes(outVertex, "error.code").Int(); got != 500 {
		t.Errorf("Vertex: error.code = %d, want 500", got)
	}
	if got := gjson.GetBytes(outVertex, "error.status").String(); got != "INTERNAL" {
		t.Errorf("Vertex: error.status = %q, want INTERNAL", got)
	}

	// FormatOpenAIResponses → Responses-API shape as defensive fallback.
	outResp := encodeErrorEnvelopeForIngressForStream(provcore.FormatOpenAIResponses, pe)
	if got := gjson.GetBytes(outResp, "error.message").String(); got != "stream broke" {
		t.Errorf("Responses fallback: error.message = %q, want stream broke", got)
	}
	if got := gjson.GetBytes(outResp, "error.type").String(); got != "api_error" {
		t.Errorf("Responses fallback: error.type = %q, want api_error", got)
	}

	// FormatAnthropic stream frame → Anthropic shape.
	outAnth := encodeErrorEnvelopeForIngressForStream(provcore.FormatAnthropic, pe)
	if got := gjson.GetBytes(outAnth, "type").String(); got != "error" {
		t.Errorf("Anthropic stream: top-level type = %q, want error", got)
	}

	// Default/OpenAI path.
	outOAI := encodeErrorEnvelopeForIngressForStream(provcore.FormatOpenAI, pe)
	if got := gjson.GetBytes(outOAI, "error.message").String(); got != "stream broke" {
		t.Errorf("OpenAI stream: error.message = %q, want stream broke", got)
	}
}

// TestGeminiStatusForHTTPCode_AllCodes pins every gRPC status name our
// normaliser emits so Vertex AI / Gemini clients get the string they
// branch on, and unknown codes fall back to UNKNOWN.
func TestGeminiStatusForHTTPCode_AllCodes(t *testing.T) {
	cases := []struct {
		code int
		want string
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
		{599, "UNKNOWN"}, // unmapped code → UNKNOWN
	}
	for _, tc := range cases {
		got := geminiStatusForHTTPCode(tc.code)
		if got != tc.want {
			t.Errorf("geminiStatusForHTTPCode(%d) = %q, want %q", tc.code, got, tc.want)
		}
	}
}

// TestParsePeriodStart_MalformedKey covers the error branch in
// parsePeriodStart: a key that cannot be parsed as "YYYY-MM" must fall
// back to the first day of the current month without panicking.
func TestParsePeriodStart_MalformedKey(t *testing.T) {
	// The fallback returns time.Now() month start — we can't pin the exact
	// date, but we can assert the returned time is the first of its month
	// and is in UTC, which is the only observable behavior.
	got := parsePeriodStart("not-a-period-key")
	if got.Day() != 1 {
		t.Errorf("fallback parsePeriodStart: day = %d, want 1 (first of month)", got.Day())
	}
	if got.Location() != nil && got.Location().String() != "UTC" {
		t.Errorf("fallback parsePeriodStart: location = %v, want UTC", got.Location())
	}
	if got.Hour() != 0 || got.Minute() != 0 || got.Second() != 0 {
		t.Errorf("fallback parsePeriodStart: time should be midnight, got %v", got)
	}
}

// TestEncodeErrorEnvelope_SameFormatNoRaw_FallsThroughToEncoder verifies
// the passthrough branch with an empty Raw field: rather than returning
// empty bytes, the function must fall through to format-specific encoding
// and emit a parseable body.
func TestEncodeErrorEnvelope_SameFormatNoRaw_FallsThroughToEncoder(t *testing.T) {
	pe := &provcore.ProviderError{
		Status:  401,
		Code:    provcore.CodeAuthFailed,
		Message: "auth failed before dispatch",
		Raw:     nil, // no upstream bytes — synthetic failure
	}
	// Same format (Anthropic→Anthropic) but no Raw → must not return nil/empty.
	out := encodeErrorEnvelopeForIngress(provcore.FormatAnthropic, provcore.FormatAnthropic, pe)
	if len(out) == 0 {
		t.Fatal("expected non-empty body from no-raw same-format path")
	}
	if got := gjson.GetBytes(out, "error.message").String(); got != "auth failed before dispatch" {
		t.Errorf("error.message = %q, want auth failed before dispatch", got)
	}
}

// TestSynthesizeSSEErrorFrame_Gemini confirms Gemini ingress emits the
// correct plain `data: {...}\n\n` frame (no event: prefix) with the gRPC
// status field populated.
func TestSynthesizeSSEErrorFrame_Gemini(t *testing.T) {
	pe := &provcore.ProviderError{
		Status:  429,
		Code:    provcore.CodeRateLimited,
		Message: "quota exceeded",
	}
	frame := synthesizeSSEErrorFrame(provcore.FormatGemini, pe)
	s := string(frame)
	if strings.Contains(s, "event:") {
		t.Errorf("Gemini SSE frame must not have event: prefix; got: %s", s)
	}
	if !strings.HasPrefix(s, "data: ") {
		t.Errorf("Gemini SSE frame should start with 'data: '; got: %s", s)
	}
	parts := strings.SplitN(s, "data: ", 2)
	dataLine := strings.TrimSuffix(parts[1], "\n\n")
	if got := gjson.Get(dataLine, "error.status").String(); got != "RESOURCE_EXHAUSTED" {
		t.Errorf("error.status = %q, want RESOURCE_EXHAUSTED", got)
	}
}

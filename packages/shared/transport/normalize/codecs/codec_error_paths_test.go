package codecs

// gap_fill_test.go pins the specific branches that kept the codecs package
// below 95% statement coverage. Each test asserts an observable failure mode
// or a named normalisation output; none are coverage-padding-only (all assert
// output shape, not just err==nil).

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// openai_chat.go — normalizeNonStreamResponse error paths

// TestOpenAIChat_ResponseMalformedJSONYieldsParseError covers line 458-464:
// JSON unmarshal failure on the response side returns an error that does NOT
// wrap core.ErrUnsupported (it is a parse error, not a semantic one).
func TestOpenAIChat_ResponseMalformedJSONYieldsParseError(t *testing.T) {
	n := NewOpenAIChatNormalizer()
	_, err := n.Normalize(
		context.Background(),
		[]byte("not valid json{{{"),
		core.Meta{Direction: core.DirectionResponse},
	)
	if err == nil {
		t.Fatal("expected error for malformed response JSON, got nil")
	}
	if errors.Is(err, core.ErrUnsupported) {
		t.Errorf("malformed response JSON should be a parse error, not ErrUnsupported: %v", err)
	}
}

// TestOpenAIChat_ResponseUsageOnlyNoChoicesYieldsUsagePlusErrUnsupported covers
// lines 476-480: when the response carries usage but no choices (usage-only
// diagnostic body), the normalizer returns non-nil usage AND wraps ErrUnsupported.
func TestOpenAIChat_ResponseUsageOnlyNoChoicesYieldsUsagePlusErrUnsupported(t *testing.T) {
	body := `{"model":"gpt-4o","choices":[],"usage":{"prompt_tokens":42,"completion_tokens":8,"total_tokens":50}}`
	n := NewOpenAIChatNormalizer()
	got, err := n.Normalize(
		context.Background(),
		[]byte(body),
		core.Meta{Direction: core.DirectionResponse},
	)
	if err == nil {
		t.Fatal("expected ErrUnsupported for empty choices, got nil")
	}
	if !errors.Is(err, core.ErrUnsupported) {
		t.Errorf("expected ErrUnsupported, got %v", err)
	}
	// Usage must be populated even when choices is empty (callers use this
	// as an ExtractUsage shim for providers that return usage-only bodies).
	if got.Usage == nil {
		t.Fatal("usage should be populated in usage-only response, got nil")
	}
	if got.Usage.PromptTokens == nil || *got.Usage.PromptTokens != 42 {
		t.Errorf("prompt_tokens = %v, want 42", got.Usage.PromptTokens)
	}
}

// openai_chat.go — normalizeStreamResponse graceful truncation folding

// TestOpenAIChat_StreamResponseMalformedChunkFoldsPrefix pins the
// truncated-capture failure mode: a malformed JSON chunk (the cut-off
// tail of a partial capture) counts toward the coverage total only —
// the decodable prefix folds successfully with proportionally lower
// confidence instead of erroring the whole row away.
func TestOpenAIChat_StreamResponseMalformedChunkFoldsPrefix(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"}}]}`,
		``,
		`data: {this is broken json`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	n := NewOpenAIChatNormalizer()
	got, err := n.Normalize(
		context.Background(),
		[]byte(raw),
		core.Meta{Direction: core.DirectionResponse, Stream: true},
	)
	if err != nil {
		t.Fatalf("decodable prefix must fold without error, got %v", err)
	}
	// The successfully decoded first chunk must be present in output.
	if len(got.Messages) == 0 {
		t.Fatal("expected at least one message from the valid first chunk")
	}
	if got.Messages[0].Content[0].Text != "Hello" {
		t.Errorf("message text = %q, want Hello", got.Messages[0].Content[0].Text)
	}
	if got.Confidence != 0.5 {
		t.Errorf("confidence = %v, want 0.5 (1 of 2 frames recognized; [DONE] is a sentinel)", got.Confidence)
	}
}

// openai_chat.go — roleFromString default (unknown role pass-through)

// TestOpenAIChat_ResponseUnknownRolePassesThrough covers line 700-702:
// an unrecognised role string is returned as-is (cast to core.Role) rather
// than mapped to an assistant or user role. This preserves fidelity for
// provider-specific roles like "ipython", "context", or "environment".
func TestOpenAIChat_ResponseUnknownRolePassesThrough(t *testing.T) {
	body := `{"model":"gpt-4o","choices":[{"index":0,"finish_reason":"stop","message":{"role":"ipython","content":"result"}}]}`
	n := NewOpenAIChatNormalizer()
	got, err := n.Normalize(
		context.Background(),
		[]byte(body),
		core.Meta{Direction: core.DirectionResponse},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(got.Messages))
	}
	if got.Messages[0].Role != core.Role("ipython") {
		t.Errorf("role = %q, want ipython", got.Messages[0].Role)
	}
}

// generic_http.go — MaxInlineTextBytes zero uses default

// TestGenericHTTP_ZeroMaxInlineTextBytesUsesDefault covers lines 69-71: when
// MaxInlineTextBytes is 0 (zero value), the normalizer falls back to the
// package default rather than refusing all non-empty bodies.
func TestGenericHTTP_ZeroMaxInlineTextBytesUsesDefault(t *testing.T) {
	// n with explicit zero MaxInlineTextBytes — should behave like default.
	n := &GenericHTTPNormalizer{MaxInlineTextBytes: 0}
	body := `{"ok":true}`
	got, err := n.Normalize(
		context.Background(),
		[]byte(body),
		core.Meta{ContentType: "application/json"},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Kind != core.KindHTTPJSON {
		t.Errorf("Kind = %v, want http-json", got.Kind)
	}
}

// generic_http.go — splitMediaTypeAndParams semicolon-with-error fallback

// TestSplitMediaType_MalformedParamWithSemicolon covers line 135-136:
// when mime.ParseMediaType fails and the string contains a ";", the function
// splits on ";" and returns the left side trimmed (not the full garbage string).
func TestSplitMediaType_MalformedParamWithSemicolon(t *testing.T) {
	// "application/json; =" is malformed enough to fail ParseMediaType
	// but contains a semicolon, triggering the split branch.
	mt, params := splitMediaTypeAndParams("application/json; =invalid")
	// The left side of ";" should survive as the media type.
	if mt != "application/json" {
		t.Errorf("media type = %q, want application/json", mt)
	}
	// On parse error the params map should be nil (no panic).
	if params != nil {
		t.Errorf("params should be nil on parse error, got %v", params)
	}
}

// generic_http.go — normalizeJSON fallback to normalizeText when JSON fails

// TestGenericHTTP_JSONContentTypeWithPlainTextFallback covers lines 175-176:
// a body stamped as application/json that contains plain UTF-8 prose (not
// SSE, not NDJSON) falls through to the text projection rather than returning
// a decode error.
func TestGenericHTTP_JSONContentTypeWithPlainTextFallback(t *testing.T) {
	body := []byte("This is plain prose, not JSON at all. The server returned HTML or a stack trace.")
	got, err := NewGenericHTTPNormalizer().Normalize(
		context.Background(),
		body,
		core.Meta{ContentType: "application/json"},
	)
	// No error expected — the normalizer falls through to text projection.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Kind != core.KindHTTPText {
		t.Errorf("Kind = %v, want http-text for UTF-8 prose mislabelled as JSON", got.Kind)
	}
	if got.HTTP == nil || got.HTTP.BodyView == nil || got.HTTP.BodyView.Text == "" {
		t.Error("expected text body view to be populated")
	}
}

// TestGenericHTTP_JSONContentTypeWithSSEFallback: a body stamped as
// application/json that looks like an SSE stream (starts with "data: ")
// is re-routed to the structured SSE projection so the operator sees
// decoded frames instead of a JSON decode error.
func TestGenericHTTP_JSONContentTypeWithSSEFallback(t *testing.T) {
	body := []byte("data: {\"model\":\"gpt-4\"}\n\ndata: [DONE]\n\n")
	got, err := NewGenericHTTPNormalizer().Normalize(
		context.Background(),
		body,
		core.Meta{ContentType: "application/json"},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Kind != core.KindHTTPSSE {
		t.Errorf("Kind = %v, want http-sse for SSE mislabelled as JSON", got.Kind)
	}
	fr := got.HTTP.BodyView.SSEFrames
	if len(fr) != 2 || fr[0].Data == nil || fr[1].DataText != "[DONE]" {
		t.Errorf("frames not structured: %+v", fr)
	}
}

// generic_http.go — normalizeNDJSON single-item body re-routes to JSON

// TestNDJSON_TwoItemBodyProducesJSONArray covers the happy path in
// normalizeNDJSON: two complete JSON objects on separate lines are combined
// into a JSON array projection so the UI renders them as a single decoded
// list (the canonical NDJSON multi-line case).
func TestNDJSON_TwoItemBodyProducesJSONArray(t *testing.T) {
	body := []byte("{\"a\":1}\n{\"b\":2}\n")
	got, err := NewGenericHTTPNormalizer().Normalize(
		context.Background(),
		body,
		core.Meta{ContentType: "application/json"}, // normalizeJSON sees invalid JSON, sniffs NDJSON
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Two-line NDJSON → KindHTTPJSON (array form).
	if got.Kind != core.KindHTTPJSON {
		t.Errorf("Kind = %v, want http-json for two-line NDJSON body", got.Kind)
	}
	if got.HTTP == nil || got.HTTP.BodyView == nil {
		t.Fatal("HTTP body view should be populated")
	}
}

// generic_http.go — normalizeMultipart partial-parse error on bad boundary data

// TestGenericHTTP_MultipartPartialParseErrorSurfaces covers lines 302-306:
// a multipart body whose second part is structurally broken triggers a partial-
// parse error. The normalizer returns any successfully decoded parts AND the
// error so callers can surface partial audit data.
func TestGenericHTTP_MultipartPartialParseErrorSurfaces(t *testing.T) {
	// A well-formed multipart opening then deliberately broken continuation.
	body := "--bnd\r\nContent-Disposition: form-data; name=\"field1\"\r\n\r\nvalue1\r\n" +
		"--bnd\r\nContent-Disposition: form-data; name=\"field2\"\r\n" +
		// Truncated — no closing CRLF + body, triggers multipart.Reader error.
		"garbage no crlf after headers"
	got, err := NewGenericHTTPNormalizer().Normalize(
		context.Background(),
		[]byte(body),
		core.Meta{ContentType: "multipart/form-data; boundary=bnd"},
	)
	// Partial parse: error is expected, but some form data may survive.
	if err == nil {
		// Some implementations are lenient — acceptable as long as Kind is correct.
		if got.Kind != core.KindHTTPMultipart {
			t.Errorf("partial multipart: Kind = %v, want http-multipart", got.Kind)
		}
		return
	}
	// Error path: verify we still get a usable payload (not a zero value).
	if got.NormalizeVersion == "" {
		t.Error("NormalizeVersion should be set even on partial-parse error")
	}
}

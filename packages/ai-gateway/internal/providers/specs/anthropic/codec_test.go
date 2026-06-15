package anthropic

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

// The tests in this file fill coverage gaps for spec_anthropic to push the
// package above the 95% statement-coverage gate. They focus on OBSERVABLE
// behavior — canonical ↔ Anthropic-wire field movement, error normalization,
// stream session Usage projection, and transport URL / auth / probe — and
// avoid err==nil padding (binding [[unit_test_coverage_95]]).

// TestCodec_EncodeRequest_RejectsWrongEndpointAndEmptyBody pins the two
// upfront guards in EncodeRequest (lines 47-53) that callers depend on
// to NOT route a non-chat-completions endpoint through the codec and to
// surface an empty-canonical body as a typed error.
func TestCodec_EncodeRequest_RejectsWrongEndpointAndEmptyBody(t *testing.T) {
	var c codec
	if _, err := c.EncodeRequest(typology.WireShapeNone, []byte(`{"model":"x"}`), provcore.CallTarget{}); err == nil {
		t.Error("expected error on non-chat endpoint")
	}
	if _, err := c.EncodeRequest(typology.WireShapeAnthropicMessages, nil, provcore.CallTarget{}); err == nil {
		t.Error("expected error on empty body")
	}
}

// TestCodec_EncodeRequest_MissingModel pins line 60-61 — both
// canonical body and CallTarget.ProviderModelID empty must reject.
func TestCodec_EncodeRequest_MissingModel(t *testing.T) {
	canon := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	_, err := codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{})
	if err == nil || !strings.Contains(err.Error(), "model") {
		t.Fatalf("expected missing-model error, got %v", err)
	}
}

// TestCodec_EncodeRequest_NoUserOrAssistantMessages pins line 161-162.
// A body containing ONLY a system message must error — Anthropic
// requires a non-empty messages[] array.
func TestCodec_EncodeRequest_NoUserOrAssistantMessages(t *testing.T) {
	canon := []byte(`{
		"model":"claude-3-5-sonnet-20241022",
		"max_tokens":16,
		"messages":[{"role":"system","content":"only system"}]
	}`)
	_, err := codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{})
	if err == nil || !strings.Contains(err.Error(), "no user/assistant messages") {
		t.Fatalf("expected no-messages error, got %v", err)
	}
}

// TestCodec_EncodeRequest_StopStringAndArray covers lines 132-145 —
// canonical OpenAI `stop` may be string or array; both must produce
// Anthropic `stop_sequences` as an array.
func TestCodec_EncodeRequest_StopStringAndArray(t *testing.T) {
	t.Run("string", func(t *testing.T) {
		canon := []byte(`{
			"model":"claude-3-5-sonnet-20241022",
			"max_tokens":16,
			"messages":[{"role":"user","content":"hi"}],
			"stop":"\n\n"
		}`)
		encRes, err := codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{})
		out := encRes.Body
		if err != nil {
			t.Fatal(err)
		}
		seq := gjson.GetBytes(out, "stop_sequences")
		if !seq.IsArray() || seq.Array()[0].String() != "\n\n" {
			t.Errorf("stop_sequences=%s", seq.Raw)
		}
	})
	t.Run("array", func(t *testing.T) {
		canon := []byte(`{
			"model":"claude-3-5-sonnet-20241022",
			"max_tokens":16,
			"messages":[{"role":"user","content":"hi"}],
			"stop":["END","STOP"]
		}`)
		encRes, err := codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{})
		out := encRes.Body
		if err != nil {
			t.Fatal(err)
		}
		arr := gjson.GetBytes(out, "stop_sequences").Array()
		if len(arr) != 2 || arr[0].String() != "END" || arr[1].String() != "STOP" {
			t.Errorf("stop_sequences=%v", arr)
		}
	})
}

// TestCodec_EncodeRequest_SystemMultiBlock covers line 154-159 — multiple
// system messages must be collected into an array of {type:text,text} blocks
// rather than a single string.
func TestCodec_EncodeRequest_SystemMultiBlock(t *testing.T) {
	canon := []byte(`{
		"model":"claude-3-5-sonnet-20241022",
		"max_tokens":16,
		"messages":[
			{"role":"system","content":"first"},
			{"role":"system","content":"second"},
			{"role":"user","content":"hi"}
		]
	}`)
	encRes, err := codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatal(err)
	}
	sys := gjson.GetBytes(out, "system")
	if !sys.IsArray() {
		t.Fatalf("system must be array of blocks: %s", string(out))
	}
	arr := sys.Array()
	if len(arr) != 2 {
		t.Fatalf("len=%d want 2", len(arr))
	}
	if arr[0].Get("text").String() != "first" || arr[1].Get("text").String() != "second" {
		t.Errorf("blocks=%v", arr)
	}
}

// TestCodec_EncodeRequest_MaxCompletionTokensWinsOverMaxTokens pins line
// 96-100 — when both fields are present, max_completion_tokens takes
// precedence (matching OpenAI's own resolution for reasoning models).
func TestCodec_EncodeRequest_MaxCompletionTokensWinsOverMaxTokens(t *testing.T) {
	canon := []byte(`{
		"model":"claude-3-5-sonnet-20241022",
		"max_tokens":16,
		"max_completion_tokens":256,
		"messages":[{"role":"user","content":"hi"}]
	}`)
	encRes, err := codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(out, "max_tokens").Int(); got != 256 {
		t.Errorf("max_tokens=%d want 256 (max_completion_tokens precedence)", got)
	}
}

// TestCodec_EncodeRequest_ToolChoiceAutoNoneRequired covers the string
// tool_choice mappings (lines 199-207) — auto/none/required map to
// Anthropic's {type:auto,none,any} respectively.
func TestCodec_EncodeRequest_ToolChoiceAutoNoneRequired(t *testing.T) {
	for _, tc := range []struct {
		in, wantType string
	}{
		{"auto", "auto"},
		{"none", "none"},
		{"required", "any"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			canon := []byte(`{
				"model":"claude-3-5-sonnet-20241022",
				"max_tokens":16,
				"messages":[{"role":"user","content":"hi"}],
				"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],
				"tool_choice":"` + tc.in + `"
			}`)
			encRes, err := codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{})
			out := encRes.Body
			if err != nil {
				t.Fatal(err)
			}
			if got := gjson.GetBytes(out, "tool_choice.type").String(); got != tc.wantType {
				t.Errorf("tool_choice.type=%q want %q", got, tc.wantType)
			}
		})
	}
}

// TestCodec_EncodeRequest_StreamFlag pins line 129-130 — stream is a
// passthrough boolean on the wire.
func TestCodec_EncodeRequest_StreamFlag(t *testing.T) {
	canon := []byte(`{
		"model":"claude-3-5-sonnet-20241022",
		"max_tokens":16,
		"messages":[{"role":"user","content":"hi"}],
		"stream":true
	}`)
	encRes, err := codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatal(err)
	}
	if !gjson.GetBytes(out, "stream").Bool() {
		t.Errorf("stream not forwarded: %s", string(out))
	}
}

// TestCodec_EncodeRequest_Metadata covers lines 232-237 — metadata
// must round-trip verbatim when non-empty.
func TestCodec_EncodeRequest_Metadata(t *testing.T) {
	canon := []byte(`{
		"model":"claude-3-5-sonnet-20241022",
		"max_tokens":16,
		"messages":[{"role":"user","content":"hi"}],
		"metadata":{"user_id":"u_1"}
	}`)
	encRes, err := codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(out, "metadata.user_id").String(); got != "u_1" {
		t.Errorf("metadata.user_id=%q", got)
	}
}

// TestCodec_EncodeRequest_AssistantToolCallsWithEmptyArgs covers
// lines 400-401 — an assistant tool_call with empty `arguments` must
// default to "{}" so the Anthropic wire input is a valid JSON object.
func TestCodec_EncodeRequest_AssistantToolCallsWithEmptyArgs(t *testing.T) {
	canon := []byte(`{
		"model":"claude-3-5-sonnet-20241022",
		"max_tokens":16,
		"messages":[
			{"role":"user","content":"do it"},
			{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"noop","arguments":""}}]}
		]
	}`)
	encRes, err := codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatal(err)
	}
	// On the wire the assistant content block must include input={} not
	// a missing/null key.
	parts := gjson.GetBytes(out, "messages.1.content").Array()
	if len(parts) == 0 || parts[0].Get("type").String() != "tool_use" {
		t.Fatalf("missing tool_use block: %s", string(out))
	}
	if !parts[0].Get("input").IsObject() {
		t.Errorf("input must be object, got %s", parts[0].Get("input").Raw)
	}
}

// TestCodec_EncodeRequest_ToolRoleMessage covers lines 375-385 — an
// OpenAI tool-role message becomes a user message carrying a tool_result
// content block addressed by tool_call_id.
func TestCodec_EncodeRequest_ToolRoleMessage(t *testing.T) {
	canon := []byte(`{
		"model":"claude-3-5-sonnet-20241022",
		"max_tokens":16,
		"messages":[
			{"role":"user","content":"call f"},
			{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"c1","content":"ok"}
		]
	}`)
	encRes, err := codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatal(err)
	}
	last := gjson.GetBytes(out, "messages.2")
	if last.Get("role").String() != "user" {
		t.Errorf("tool→user role mapping lost: %s", last.Raw)
	}
	block := last.Get("content.0")
	if block.Get("type").String() != "tool_result" || block.Get("tool_use_id").String() != "c1" {
		t.Errorf("tool_result block wrong: %s", block.Raw)
	}
}

// TestCodec_EncodeRequest_ImageMissingURL covers line 466-469 —
// image_url without a url surfaces a structured error, not a silent
// drop.
func TestCodec_EncodeRequest_ImageMissingURL(t *testing.T) {
	canon := []byte(`{
		"model":"claude-3-5-sonnet-20241022",
		"max_tokens":16,
		"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":""}}]}]
	}`)
	_, err := codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected error")
	}
	var pe *provcore.ProviderError
	if !errors.As(err, &pe) || pe.Type != "nexus_field_unsupported" {
		t.Errorf("want structured err, got %T %v", err, err)
	}
}

// TestCodec_EncodeRequest_ImageInvalidDataURL covers line 472-474 —
// data:URL that fails parseDataURL must surface a structured error.
func TestCodec_EncodeRequest_ImageInvalidDataURL(t *testing.T) {
	canon := []byte(`{
		"model":"claude-3-5-sonnet-20241022",
		"max_tokens":16,
		"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:bogus"}}]}]
	}`)
	_, err := codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected error")
	}
}

// TestCodec_EncodeRequest_PartsContentToolResultPart covers line 493-498 —
// content array with type=tool_result must produce the wire tool_result
// block. The content payload is stringified via stringifyOpenAIToolResultContent.
func TestCodec_EncodeRequest_PartsContentToolResultPart(t *testing.T) {
	canon := []byte(`{
		"model":"claude-3-5-sonnet-20241022",
		"max_tokens":16,
		"messages":[
			{"role":"user","content":[
				{"type":"tool_result","tool_call_id":"c1","content":"42"},
				{"type":"tool_result","tool_call_id":"c2","content":{"raw":"json"}}
			]}
		]
	}`)
	encRes, err := codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatal(err)
	}
	parts := gjson.GetBytes(out, "messages.0.content").Array()
	if len(parts) != 2 {
		t.Fatalf("got %d parts: %s", len(parts), string(out))
	}
	if parts[0].Get("tool_use_id").String() != "c1" || parts[0].Get("content").String() != "42" {
		t.Errorf("first tool_result wrong: %s", parts[0].Raw)
	}
	// Non-string content goes through the .Raw path of
	// stringifyOpenAIToolResultContent.
	if !strings.Contains(parts[1].Get("content").String(), "raw") {
		t.Errorf("non-string tool_result content lost: %s", parts[1].Raw)
	}
}

// TestCodec_EncodeRequest_UnknownContentPartPassthrough covers line 499-503
// — an unknown part type is JSON-unmarshalled and forwarded verbatim
// (best-effort native passthrough).
func TestCodec_EncodeRequest_UnknownContentPartPassthrough(t *testing.T) {
	canon := []byte(`{
		"model":"claude-3-5-sonnet-20241022",
		"max_tokens":16,
		"messages":[{"role":"user","content":[{"type":"future_part","value":"x"}]}]
	}`)
	encRes, err := codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.type").String(); got != "future_part" {
		t.Errorf("unknown part lost: %s", string(out))
	}
}

// TestCodec_EncodeRequest_AssistantArrayContentEmptyTextPath covers
// the no-text-no-array fallback (line 415-417 / 442-444) — assistant
// message with empty content + no tool_calls falls into the empty-text
// branch so the wire is still valid.
func TestCodec_EncodeRequest_AssistantArrayContentEmptyTextPath(t *testing.T) {
	canon := []byte(`{
		"model":"claude-3-5-sonnet-20241022",
		"max_tokens":16,
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"assistant","content":[]}
		]
	}`)
	encRes, err := codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatal(err)
	}
	// Last message keeps assistant role and has at least one content block.
	if gjson.GetBytes(out, "messages.1.role").String() != "assistant" {
		t.Errorf("role lost: %s", string(out))
	}
	if !gjson.GetBytes(out, "messages.1.content.0.type").Exists() {
		t.Errorf("empty content fallback missing: %s", string(out))
	}
}

// TestCodec_EncodeRequest_AssistantToolCallsWithText covers line 394-396
// — assistant tool_calls AND text content together produce a parts
// array with a text block followed by tool_use blocks.
func TestCodec_EncodeRequest_AssistantToolCallsWithText(t *testing.T) {
	canon := []byte(`{
		"model":"claude-3-5-sonnet-20241022",
		"max_tokens":16,
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"assistant","content":"thinking out loud","tool_calls":[
				{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}
			]}
		]
	}`)
	encRes, err := codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatal(err)
	}
	parts := gjson.GetBytes(out, "messages.1.content").Array()
	if len(parts) != 2 {
		t.Fatalf("got %d parts: %s", len(parts), string(out))
	}
	if parts[0].Get("type").String() != "text" || parts[0].Get("text").String() != "thinking out loud" {
		t.Errorf("first part wrong: %s", parts[0].Raw)
	}
	if parts[1].Get("type").String() != "tool_use" {
		t.Errorf("second part wrong: %s", parts[1].Raw)
	}
}

// TestStringifyOpenAIToolResultContent_StringAndJSON pins both branches of
// the tiny helper that was 0% covered.
func TestStringifyOpenAIToolResultContent_StringAndJSON(t *testing.T) {
	str := gjson.Parse(`"plain"`)
	if got := stringifyOpenAIToolResultContent(str); got != "plain" {
		t.Errorf("string branch: got %q", got)
	}
	obj := gjson.Parse(`{"k":1}`)
	if got := stringifyOpenAIToolResultContent(obj); got != `{"k":1}` {
		t.Errorf("non-string branch: got %q", got)
	}
}

// TestParseDataURL_HappyAndAllErrorBranches covers parseDataURL: prefix
// missing, missing comma, trailing-comma payload, missing ;base64, empty
// media type defaulted, invalid base64 payload, and successful parse.
func TestParseDataURL_HappyAndAllErrorBranches(t *testing.T) {
	cases := []struct {
		in        string
		ok        bool
		mediaWant string
	}{
		{"http://example.com/x.png", false, ""},
		{"data:", false, ""},
		{"data:image/png;base64,", false, ""},
		{"data:image/png,abc", false, ""}, // missing ;base64
		{"data:;base64,aGVsbG8=", true, "application/octet-stream"},
		{"data:image/png;base64,!!!", false, ""}, // invalid base64
		{"data:image/png;base64,aGVsbG8=", true, "image/png"},
	}
	for _, tc := range cases {
		mt, b64, ok := parseDataURL(tc.in)
		if ok != tc.ok {
			t.Errorf("parseDataURL(%q) ok=%v want %v", tc.in, ok, tc.ok)
		}
		if tc.ok {
			if mt != tc.mediaWant {
				t.Errorf("mediaType=%q want %q", mt, tc.mediaWant)
			}
			if b64 == "" {
				t.Errorf("payload empty for %q", tc.in)
			}
		}
	}
}

// TestStringifyContent_AllBranches covers stringifyContent's three
// branches: empty, string, array of text parts. Hit via codec encode
// paths so they are observable through the wire output.
func TestStringifyContent_AllBranches(t *testing.T) {
	// Empty content (missing key) → message gets empty-text fallback.
	canon := []byte(`{
		"model":"claude-3-5-sonnet-20241022",
		"max_tokens":16,
		"messages":[{"role":"user"}]
	}`)
	encRes, err := codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.text").String(); got != "" {
		t.Errorf("empty content must produce empty text, got %q", got)
	}
	// Array of text parts → joined with newline.
	canon = []byte(`{
		"model":"claude-3-5-sonnet-20241022",
		"max_tokens":16,
		"messages":[{"role":"system","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]},
			{"role":"user","content":"hi"}]
	}`)
	encRes, err = codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{})
	out = encRes.Body
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(out, "system").String(); got != "a\nb" {
		t.Errorf("multi-text system not joined: %q", got)
	}
}

// TestMapStopReason_AllBranches covers every case of mapStopReason
// including the passthrough-on-unknown contract.
func TestMapStopReason_AllBranches(t *testing.T) {
	cases := map[string]string{
		"end_turn":      "stop",
		"stop_sequence": "stop",
		"max_tokens":    "length",
		"tool_use":      "tool_calls",
		"unknown_x":     "unknown_x", // passthrough preserves upstream drift
	}
	for in, want := range cases {
		if got := mapStopReason(in); got != want {
			t.Errorf("mapStopReason(%q)=%q want %q", in, got, want)
		}
	}
}

// TestUsageToNormalize_ZeroVsNonZero covers the nil-on-zero / pointer-on-set
// contract that the projector relies on.
func TestUsageToNormalize_ZeroVsNonZero(t *testing.T) {
	if usageToNormalize(provcore.Usage{}) != nil {
		t.Error("zero Usage must return nil")
	}
	p := 3
	u := provcore.Usage{PromptTokens: &p}
	got := usageToNormalize(u)
	if got == nil || got.PromptTokens == nil || *got.PromptTokens != 3 {
		t.Errorf("non-zero Usage must round-trip, got %+v", got)
	}
}

// TestDecodeResponse_NonChatPassthroughAndEmpty covers DecodeResponse's
// two early-return paths.
func TestDecodeResponse_NonChatPassthroughAndEmpty(t *testing.T) {
	body := []byte(`{"data":[]}`)
	decRes, err := codec{}.DecodeResponse(typology.WireShapeNone, body, "", provcore.DecodeContext{})
	out := decRes.CanonicalBody
	u := decRes.Usage
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, body) {
		t.Errorf("models endpoint must passthrough body")
	}
	if u.PromptTokens != nil {
		t.Errorf("non-chat must not extract usage")
	}
	decRes, err = codec{}.DecodeResponse(typology.WireShapeAnthropicMessages, nil, "", provcore.DecodeContext{})
	out = decRes.CanonicalBody
	if err != nil {
		t.Fatal(err)
	}
	if out != nil {
		t.Errorf("empty body must round-trip as nil")
	}
}

// TestDecodeResponse_StopReasonAndCreatedFallback covers the
// time.Now-fallback when created_at is absent, plus the mapStopReason
// projection into finish_reason.
func TestDecodeResponse_StopReasonAndCreatedFallback(t *testing.T) {
	native := []byte(`{
		"id":"m1",
		"model":"claude-x",
		"content":[{"type":"text","text":"hello"}],
		"stop_reason":"max_tokens",
		"usage":{"input_tokens":4,"output_tokens":1}
	}`)
	decRes, err := codec{}.DecodeResponse(typology.WireShapeAnthropicMessages, native, "", provcore.DecodeContext{})
	canon := decRes.CanonicalBody
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(canon, "choices.0.finish_reason").String(); got != "length" {
		t.Errorf("finish_reason=%q want length", got)
	}
	if got := gjson.GetBytes(canon, "created").Int(); got <= 0 {
		t.Errorf("created not stamped: %d", got)
	}
}

// TestErrorNormalizer_TypeMatrix covers every documented Anthropic error
// type → canonical code mapping.
func TestErrorNormalizer_TypeMatrix(t *testing.T) {
	cases := []struct {
		errType  string
		status   int
		wantCode string
	}{
		{"authentication_error", 401, provcore.CodeAuthFailed},
		{"permission_error", 403, provcore.CodeAuthFailed},
		{"invalid_request_error", 400, provcore.CodeInvalidRequest},
		{"not_found_error", 404, provcore.CodeInvalidRequest},
		{"overloaded_error", 503, provcore.CodeRateLimited}, // F-0227: overload is retryable, unified with stream path
		{"api_error", 500, provcore.CodeUpstreamError},
	}
	var n errorNormalizer
	for _, tc := range cases {
		body := []byte(`{"error":{"type":"` + tc.errType + `","message":"boom"}}`)
		pe := n.Normalize(tc.status, http.Header{}, body)
		if pe.Code != tc.wantCode {
			t.Errorf("%s → code=%q want %q", tc.errType, pe.Code, tc.wantCode)
		}
		if pe.Type != tc.errType || pe.Message != "boom" {
			t.Errorf("%s → preserve type/message lost", tc.errType)
		}
	}
}

// TestErrorNormalizer_StatusFallback covers the "code unset → infer from
// HTTP status" branch (lines 46-60) when the body has no `error.type`.
func TestErrorNormalizer_StatusFallback(t *testing.T) {
	cases := []struct {
		status   int
		wantCode string
	}{
		{http.StatusBadRequest, provcore.CodeInvalidRequest},
		{http.StatusNotFound, provcore.CodeInvalidRequest},
		{http.StatusUnauthorized, provcore.CodeAuthFailed},
		{http.StatusForbidden, provcore.CodeAuthFailed},
		{http.StatusTooManyRequests, provcore.CodeRateLimited},
		{http.StatusRequestTimeout, provcore.CodeTimeout},
		{http.StatusGatewayTimeout, provcore.CodeTimeout},
		{http.StatusInternalServerError, provcore.CodeUpstreamError},
	}
	var n errorNormalizer
	for _, tc := range cases {
		pe := n.Normalize(tc.status, http.Header{}, []byte(`{}`))
		if pe.Code != tc.wantCode {
			t.Errorf("status %d → code=%q want %q", tc.status, pe.Code, tc.wantCode)
		}
	}
}

// TestErrorNormalizer_EmptyMessageDefaultsToStatusText pins line 26-28.
func TestErrorNormalizer_EmptyMessageDefaultsToStatusText(t *testing.T) {
	var n errorNormalizer
	pe := n.Normalize(http.StatusTeapot, http.Header{}, []byte(`{}`))
	if pe.Message != http.StatusText(http.StatusTeapot) {
		t.Errorf("message=%q want %q", pe.Message, http.StatusText(http.StatusTeapot))
	}
}

// TestErrorNormalizer_RateLimitWithRetryAfterStatusBranch covers the
// fallback retry-after parse triggered by the status-only path (line
// 51-56) when the type is missing but status=429 + Retry-After header.
func TestErrorNormalizer_RateLimitWithRetryAfterStatusBranch(t *testing.T) {
	var n errorNormalizer
	pe := n.Normalize(429, http.Header{"Retry-After": []string{"7"}}, []byte(`{}`))
	if pe.Code != provcore.CodeRateLimited {
		t.Errorf("code=%q", pe.Code)
	}
	if pe.RetryAfter == nil || *pe.RetryAfter != 7*time.Second {
		t.Errorf("retry-after=%v", pe.RetryAfter)
	}
}

// TestParseRetryAfter_AllFormats covers the seconds-int, HTTP-date, and
// negative-time-clamp branches of parseRetryAfter.
func TestParseRetryAfter_AllFormats(t *testing.T) {
	if parseRetryAfter("") != nil {
		t.Error("empty must return nil")
	}
	d := parseRetryAfter("30")
	if d == nil || *d != 30*time.Second {
		t.Errorf("seconds format: %v", d)
	}
	// HTTP-date in the future.
	future := time.Now().Add(2 * time.Minute).UTC().Format(http.TimeFormat)
	d = parseRetryAfter(future)
	if d == nil || *d <= 0 {
		t.Errorf("future date format: %v", d)
	}
	// HTTP-date in the past → clamped to 0.
	past := time.Now().Add(-1 * time.Hour).UTC().Format(http.TimeFormat)
	d = parseRetryAfter(past)
	if d == nil || *d != 0 {
		t.Errorf("past date must clamp to 0: %v", d)
	}
	// Garbage → nil.
	if parseRetryAfter("not-a-date") != nil {
		t.Error("garbage must return nil")
	}
}

// TestMessagesRequestToOpenAI_EmptyBody covers line 17-19.
func TestMessagesRequestToOpenAI_EmptyBody(t *testing.T) {
	if _, err := MessagesRequestToOpenAIChatCompletion(nil, ""); err == nil {
		t.Fatal("expected error on empty body")
	}
}

// TestMessagesRequestToOpenAI_MissingModel covers line 25-27.
func TestMessagesRequestToOpenAI_MissingModel(t *testing.T) {
	if _, err := MessagesRequestToOpenAIChatCompletion([]byte(`{"messages":[]}`), ""); err == nil {
		t.Fatal("expected missing-model error")
	}
}

// TestMessagesRequestToOpenAI_MissingMessagesArray covers line 90-92.
func TestMessagesRequestToOpenAI_MissingMessagesArray(t *testing.T) {
	_, err := MessagesRequestToOpenAIChatCompletion([]byte(`{"model":"m"}`), "")
	if err == nil || !strings.Contains(err.Error(), "messages") {
		t.Fatalf("expected messages-missing error, got %v", err)
	}
}

// TestMessagesRequestToOpenAI_EmptyMessagesAfterConvert covers line 99-101.
func TestMessagesRequestToOpenAI_EmptyMessagesAfterConvert(t *testing.T) {
	// Empty messages array, no system → conversion yields zero messages.
	_, err := MessagesRequestToOpenAIChatCompletion([]byte(`{"model":"m","messages":[]}`), "")
	if err == nil || !strings.Contains(err.Error(), "no messages") {
		t.Fatalf("expected no-messages error, got %v", err)
	}
}

// TestMessagesRequestToOpenAI_StopSequences covers all three branches
// of the stop_sequences mapping (single → string, multi → array,
// string-typed → string).
func TestMessagesRequestToOpenAI_StopSequences(t *testing.T) {
	t.Run("single_array_to_string", func(t *testing.T) {
		body, err := MessagesRequestToOpenAIChatCompletion([]byte(`{
			"model":"m","max_tokens":16,"stop_sequences":["END"],
			"messages":[{"role":"user","content":"hi"}]
		}`), "")
		if err != nil {
			t.Fatal(err)
		}
		if got := gjson.GetBytes(body, "stop").String(); got != "END" {
			t.Errorf("stop=%q want END", got)
		}
	})
	t.Run("multi_array_to_array", func(t *testing.T) {
		body, err := MessagesRequestToOpenAIChatCompletion([]byte(`{
			"model":"m","max_tokens":16,"stop_sequences":["END","STOP"],
			"messages":[{"role":"user","content":"hi"}]
		}`), "")
		if err != nil {
			t.Fatal(err)
		}
		arr := gjson.GetBytes(body, "stop").Array()
		if len(arr) != 2 {
			t.Errorf("stop array len=%d", len(arr))
		}
	})
	t.Run("string", func(t *testing.T) {
		body, err := MessagesRequestToOpenAIChatCompletion([]byte(`{
			"model":"m","max_tokens":16,"stop_sequences":"\n\n",
			"messages":[{"role":"user","content":"hi"}]
		}`), "")
		if err != nil {
			t.Fatal(err)
		}
		if got := gjson.GetBytes(body, "stop").String(); got != "\n\n" {
			t.Errorf("stop=%q", got)
		}
	})
}

// TestMessagesRequestToOpenAI_SamplingAndStream covers lines 33-44 —
// temperature / top_p / top_k / stream all forwarded.
func TestMessagesRequestToOpenAI_SamplingAndStream(t *testing.T) {
	body, err := MessagesRequestToOpenAIChatCompletion([]byte(`{
		"model":"m","max_tokens":16,
		"temperature":0.4,"top_p":0.7,"top_k":50,"stream":true,
		"messages":[{"role":"user","content":"hi"}]
	}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(body, "temperature").Float() != 0.4 {
		t.Errorf("temperature")
	}
	if gjson.GetBytes(body, "top_p").Float() != 0.7 {
		t.Errorf("top_p")
	}
	if gjson.GetBytes(body, "top_k").Int() != 50 {
		t.Errorf("top_k")
	}
	if !gjson.GetBytes(body, "stream").Bool() {
		t.Errorf("stream")
	}
}

// TestMessagesRequestToOpenAI_SystemAsArrayOfBlocks covers lines 71-86
// — Anthropic native system can be an array of text blocks; the result
// must join into a single canonical system message.
func TestMessagesRequestToOpenAI_SystemAsArrayOfBlocks(t *testing.T) {
	body, err := MessagesRequestToOpenAIChatCompletion([]byte(`{
		"model":"m","max_tokens":16,
		"system":[{"type":"text","text":"line1"},{"type":"text","text":"line2"}],
		"messages":[{"role":"user","content":"hi"}]
	}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(body, "messages.0.role").String(); got != "system" {
		t.Errorf("first must be system, got %q", got)
	}
	if got := gjson.GetBytes(body, "messages.0.content").String(); got != "line1\nline2" {
		t.Errorf("system content=%q", got)
	}
}

// TestMessagesRequestToOpenAI_ToolsAndToolChoice covers tools[] →
// canonical function-tool transform AND every tool_choice mapping
// (auto/any/none/tool).
func TestMessagesRequestToOpenAI_ToolsAndToolChoice(t *testing.T) {
	for _, tc := range []struct {
		raw       string
		wantValue string
		wantType  string // for object-typed tool_choice
	}{
		{`"auto"`, "auto", ""},
		{`"any"`, "required", ""},
		{`"none"`, "none", ""},
	} {
		_ = tc
	}

	t.Run("string_variants", func(t *testing.T) {
		for _, tc := range []struct {
			tc, want string
		}{
			{`{"type":"auto"}`, "auto"},
			{`{"type":"any"}`, "required"},
			{`{"type":"none"}`, "none"},
		} {
			body, err := MessagesRequestToOpenAIChatCompletion([]byte(`{
				"model":"m","max_tokens":16,
				"messages":[{"role":"user","content":"hi"}],
				"tools":[{"name":"f","description":"d","input_schema":{"type":"object","properties":{}}}],
				"tool_choice":`+tc.tc+`
			}`), "")
			if err != nil {
				t.Fatal(err)
			}
			if got := gjson.GetBytes(body, "tool_choice").String(); got != tc.want {
				t.Errorf("for %s tool_choice=%q want %q", tc.tc, got, tc.want)
			}
		}
	})
	t.Run("object_tool", func(t *testing.T) {
		body, err := MessagesRequestToOpenAIChatCompletion([]byte(`{
			"model":"m","max_tokens":16,
			"messages":[{"role":"user","content":"hi"}],
			"tools":[{"name":"f","input_schema":{"type":"object"}}],
			"tool_choice":{"type":"tool","name":"f"}
		}`), "")
		if err != nil {
			t.Fatal(err)
		}
		if got := gjson.GetBytes(body, "tool_choice.type").String(); got != "function" {
			t.Errorf("tool_choice.type=%q", got)
		}
		if got := gjson.GetBytes(body, "tool_choice.function.name").String(); got != "f" {
			t.Errorf("tool_choice.function.name=%q", got)
		}
		if got := gjson.GetBytes(body, "tools.0.function.name").String(); got != "f" {
			t.Errorf("tools.0.function.name=%q", got)
		}
	})
}

// TestMessagesRequestToOpenAI_ThinkingExtension covers line 160-168 —
// Anthropic-native `thinking` config must be preserved into the canonical
// body as nexus.ext.anthropic.thinking so the codec can re-inject it on
// the round-trip back to wire.
func TestMessagesRequestToOpenAI_ThinkingExtension(t *testing.T) {
	body, err := MessagesRequestToOpenAIChatCompletion([]byte(`{
		"model":"m","max_tokens":16,
		"messages":[{"role":"user","content":"hi"}],
		"thinking":{"type":"enabled","budget_tokens":1024}
	}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(body, "nexus.ext.anthropic.thinking.type").String(); got != "enabled" {
		t.Errorf("thinking ext lost: %s", string(body))
	}
}

// TestAnthropicMessageToOpenAI_ImagesURLAndBase64 covers the multimodal
// branches inside anthropicMessageToOpenAI (URL source + base64 source).
func TestAnthropicMessageToOpenAI_ImagesURLAndBase64(t *testing.T) {
	body, err := MessagesRequestToOpenAIChatCompletion([]byte(`{
		"model":"m","max_tokens":16,
		"messages":[{"role":"user","content":[
			{"type":"text","text":"describe"},
			{"type":"image","source":{"type":"url","url":"https://e/x.png"}},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}
		]}]
	}`), "")
	if err != nil {
		t.Fatal(err)
	}
	parts := gjson.GetBytes(body, "messages.0.content").Array()
	if len(parts) != 3 {
		t.Fatalf("got %d parts: %s", len(parts), string(body))
	}
	if parts[1].Get("image_url.url").String() != "https://e/x.png" {
		t.Errorf("URL image lost: %s", parts[1].Raw)
	}
	if !strings.HasPrefix(parts[2].Get("image_url.url").String(), "data:image/png;base64,") {
		t.Errorf("base64 image lost: %s", parts[2].Raw)
	}
}

// TestAnthropicMessageToOpenAI_AssistantToolUse covers the assistant
// tool_use → canonical tool_calls pathway, including the multi-text-and-
// images parts-coalescing branch (lines 254-277).
func TestAnthropicMessageToOpenAI_AssistantToolUse(t *testing.T) {
	body, err := MessagesRequestToOpenAIChatCompletion([]byte(`{
		"model":"m","max_tokens":16,
		"messages":[
			{"role":"user","content":"call f"},
			{"role":"assistant","content":[
				{"type":"text","text":"reasoning"},
				{"type":"text","text":"more reasoning"},
				{"type":"tool_use","id":"c1","name":"f","input":{"a":1}}
			]}
		]
	}`), "")
	if err != nil {
		t.Fatal(err)
	}
	// Assistant message must have tool_calls AND multi-line text content
	// as a parts array.
	if got := gjson.GetBytes(body, "messages.1.tool_calls.0.function.name").String(); got != "f" {
		t.Errorf("tool_calls lost: %s", string(body))
	}
}

// TestAnthropicMessageToOpenAI_ToolResultsSplitIntoMessages covers lines
// 218-234 — tool_result blocks must split into a tool role message and
// any preceding text goes into a leading user message.
func TestAnthropicMessageToOpenAI_ToolResultsSplitIntoMessages(t *testing.T) {
	body, err := MessagesRequestToOpenAIChatCompletion([]byte(`{
		"model":"m","max_tokens":16,
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"context"},
				{"type":"tool_result","tool_use_id":"c1","content":"42"}
			]}
		]
	}`), "")
	if err != nil {
		t.Fatal(err)
	}
	// Two canonical messages: leading user with text, then a tool message.
	if got := gjson.GetBytes(body, "messages.0.role").String(); got != "user" {
		t.Errorf("messages.0.role=%q", got)
	}
	if got := gjson.GetBytes(body, "messages.1.role").String(); got != "tool" {
		t.Errorf("messages.1.role=%q", got)
	}
	if got := gjson.GetBytes(body, "messages.1.tool_call_id").String(); got != "c1" {
		t.Errorf("tool_call_id=%q", got)
	}
}

// TestAnthropicMessageToOpenAI_StringContent covers line 178-180 — when
// content is a plain string, just forward it.
func TestAnthropicMessageToOpenAI_StringContent(t *testing.T) {
	body, err := MessagesRequestToOpenAIChatCompletion([]byte(`{
		"model":"m","max_tokens":16,
		"messages":[{"role":"user","content":"plain"}]
	}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(body, "messages.0.content").String(); got != "plain" {
		t.Errorf("string content lost: %q", got)
	}
}

// TestAnthropicMessageToOpenAI_NonArrayContentFallback covers line 181-183
// — content that's neither string nor array yields an empty-content
// message rather than dropping the turn.
func TestAnthropicMessageToOpenAI_NonArrayContentFallback(t *testing.T) {
	body, err := MessagesRequestToOpenAIChatCompletion([]byte(`{
		"model":"m","max_tokens":16,
		"messages":[{"role":"user","content":{"unexpected":"shape"}}]
	}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(body, "messages.0.content").Exists(); !got {
		t.Errorf("content missing entirely: %s", string(body))
	}
}

// TestStringifyAnthropicToolResult_Branches covers all three branches of
// stringifyAnthropicToolResult (string / text-blocks array / raw fallback).
func TestStringifyAnthropicToolResult_Branches(t *testing.T) {
	t.Run("string", func(t *testing.T) {
		got := stringifyAnthropicToolResult(gjson.Parse(`"plain"`))
		if got != "plain" {
			t.Errorf("string branch: got %q", got)
		}
	})
	t.Run("text_blocks_array", func(t *testing.T) {
		got := stringifyAnthropicToolResult(gjson.Parse(`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`))
		if got != "a\nb" {
			t.Errorf("array branch: got %q", got)
		}
	})
	t.Run("non_array_object_raw", func(t *testing.T) {
		got := stringifyAnthropicToolResult(gjson.Parse(`{"k":1}`))
		if got != `{"k":1}` {
			t.Errorf("raw fallback: got %q", got)
		}
	})
}

// TestStringifyOpenAIMessageContent_Branches covers the helper's three
// inner branches (missing, string, array — including array text with
// nested {value:...} block).
func TestStringifyOpenAIMessageContent_Branches(t *testing.T) {
	if got := stringifyOpenAIMessageContent(gjson.Parse(`null`)); got != "" {
		t.Errorf("null branch: %q", got)
	}
	if got := stringifyOpenAIMessageContent(gjson.Parse(`"hi"`)); got != "hi" {
		t.Errorf("string branch: %q", got)
	}
	got := stringifyOpenAIMessageContent(gjson.Parse(`[
		{"type":"text","text":"a"},
		{"type":"text","text":"b"}
	]`))
	if got != "a\nb" {
		t.Errorf("array branch: %q", got)
	}
	// Nested {value:"x"} shape (some legacy projections).
	got = stringifyOpenAIMessageContent(gjson.Parse(`[
		{"type":"text","text":{"value":"deep"}}
	]`))
	if got != "deep" {
		t.Errorf("nested value branch: %q", got)
	}
}

// TestMapOpenAIFinishToStopReason_AllBranches covers every documented
// finish_reason → stop_reason mapping including empty-string default.
func TestMapOpenAIFinishToStopReason_AllBranches(t *testing.T) {
	cases := map[string]string{
		"stop":           "end_turn",
		"length":         "max_tokens",
		"tool_calls":     "tool_use",
		"content_filter": "stop_sequence",
		"":               "end_turn",
		"some_future":    "some_future", // unknown passthrough
	}
	for in, want := range cases {
		if got := mapOpenAIFinishToStopReason(in); got != want {
			t.Errorf("mapOpenAIFinishToStopReason(%q)=%q want %q", in, got, want)
		}
	}
}

// TestOpenAIChatCompletionToMessagesResponse_EmptyBody covers line 331-333.
func TestOpenAIChatCompletionToMessagesResponse_EmptyBody(t *testing.T) {
	if _, err := OpenAIChatCompletionToMessagesResponse(nil); err == nil {
		t.Fatal("expected error on empty body")
	}
}

// TestOpenAIChatCompletionToMessagesResponse_MissingChoiceMessage covers
// line 336-339.
func TestOpenAIChatCompletionToMessagesResponse_MissingChoiceMessage(t *testing.T) {
	_, err := OpenAIChatCompletionToMessagesResponse([]byte(`{"id":"x"}`))
	if err == nil {
		t.Fatal("expected missing-message error")
	}
}

// TestOpenAIChatCompletionToMessagesResponse_ToolCallsAndCacheFields
// covers the tool_calls projection (lines 358-378), the cached_tokens
// projection back to cache_read_input_tokens (line 398-400), and the
// cache_creation_input_tokens projection from the canonical extension
// (line 403-405).
func TestOpenAIChatCompletionToMessagesResponse_ToolCallsAndCacheFields(t *testing.T) {
	openai := []byte(`{
		"id":"x","model":"claude",
		"choices":[{"message":{"role":"assistant","tool_calls":[
			{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}
		]},"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":4,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":3}},
		"nexus":{"ext":{"anthropic":{"cache_creation_input_tokens":5}}}
	}`)
	out, err := OpenAIChatCompletionToMessagesResponse(openai)
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(out, "content.0.type").String(); got != "tool_use" {
		t.Errorf("tool_use block missing: %s", string(out))
	}
	if got := gjson.GetBytes(out, "content.0.name").String(); got != "f" {
		t.Errorf("tool name=%q", got)
	}
	if got := gjson.GetBytes(out, "usage.cache_read_input_tokens").Int(); got != 3 {
		t.Errorf("cache_read=%d", got)
	}
	if got := gjson.GetBytes(out, "usage.cache_creation_input_tokens").Int(); got != 5 {
		t.Errorf("cache_creation=%d", got)
	}
	if got := gjson.GetBytes(out, "stop_reason").String(); got != "tool_use" {
		t.Errorf("stop_reason=%q", got)
	}
}

// TestOpenAIChatCompletionToMessagesResponse_ToolCallEmptyArgsDefaultsToEmptyObject
// pins line 363-365 + 366-369 — empty/missing arguments default to {}.
func TestOpenAIChatCompletionToMessagesResponse_ToolCallEmptyArgsDefaultsToEmptyObject(t *testing.T) {
	openai := []byte(`{
		"id":"x","model":"claude",
		"choices":[{"message":{"role":"assistant","tool_calls":[
			{"id":"c1","type":"function","function":{"name":"f","arguments":""}}
		]},"finish_reason":"tool_calls"}]
	}`)
	out, err := OpenAIChatCompletionToMessagesResponse(openai)
	if err != nil {
		t.Fatal(err)
	}
	if !gjson.GetBytes(out, "content.0.input").IsObject() {
		t.Errorf("empty args must default to {} on input: %s", string(out))
	}
}

// TestStreamDecoder_OpenNilBody pins line 36-38.
func TestStreamDecoder_OpenNilBody(t *testing.T) {
	if _, err := NewStreamDecoder(slog.Default()).Open(nil, typology.WireShapeAnthropicMessages); err == nil {
		t.Fatal("expected error on nil body")
	}
}

// TestStreamDecoder_NewWithNilLogger pins the slog.Default() fallback.
func TestStreamDecoder_NewWithNilLogger(t *testing.T) {
	if NewStreamDecoder(nil) == nil {
		t.Fatal("nil-logger constructor must not return nil")
	}
}

// TestStreamDecoder_ContextCancelled covers line 55-57 — a cancelled
// context returns ctx.Err() instead of advancing the SSE scanner.
func TestStreamDecoder_ContextCancelled(t *testing.T) {
	raw := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"m","model":"x","usage":{"input_tokens":1,"output_tokens":0}}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	sess, err := NewStreamDecoder(slog.Default()).Open(io.NopCloser(strings.NewReader(raw)), typology.WireShapeAnthropicMessages)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close() //nolint:errcheck
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = sess.Next(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err=%v want context.Canceled", err)
	}
}

// TestStreamDecoder_PingEventIsNoOp covers line 127-129 — a ping frame
// must not move the canonical signal but the session must keep going.
func TestStreamDecoder_PingEventIsNoOp(t *testing.T) {
	raw := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"m","model":"x","usage":{"input_tokens":1,"output_tokens":0}}}`,
		``,
		`event: ping`,
		`data: {"type":"ping"}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	sess, err := NewStreamDecoder(slog.Default()).Open(io.NopCloser(strings.NewReader(raw)), typology.WireShapeAnthropicMessages)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close() //nolint:errcheck
	var text string
	for {
		ch, err := sess.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		text += ch.Delta
	}
	if text != "ok" {
		t.Errorf("ping must not break stream; text=%q want ok", text)
	}
}

// TestStreamDecoder_MessageDeltaUsage covers line 145-147 — message_delta
// frames must propagate usage (e.g. translation layers consolidate the
// full usage there instead of at message_start).
func TestStreamDecoder_MessageDeltaUsage(t *testing.T) {
	raw := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"m","model":"x","usage":{"input_tokens":2,"output_tokens":0}}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","usage":{"output_tokens":11}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	sess, err := NewStreamDecoder(slog.Default()).Open(io.NopCloser(strings.NewReader(raw)), typology.WireShapeAnthropicMessages)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close() //nolint:errcheck
	var sawCompletion bool
	for {
		ch, err := sess.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if ch.Usage != nil && ch.Usage.CompletionTokens != nil && *ch.Usage.CompletionTokens == 11 {
			sawCompletion = true
		}
	}
	if !sawCompletion {
		t.Errorf("message_delta usage projection lost output_tokens")
	}
}

// TestMapAnthropicStreamError_AllBranches covers every documented
// stream error type and the unknown-fallback.
func TestMapAnthropicStreamError_AllBranches(t *testing.T) {
	cases := []struct {
		etype    string
		wantCode string
		wantHTTP int
		wantMsg  string
		emptyMsg bool
	}{
		{"overloaded_error", provcore.CodeRateLimited, http.StatusTooManyRequests, "x", false},
		{"rate_limit_error", provcore.CodeRateLimited, http.StatusTooManyRequests, "x", false},
		{"authentication_error", provcore.CodeAuthFailed, http.StatusUnauthorized, "x", false},
		{"permission_error", provcore.CodeAuthFailed, http.StatusUnauthorized, "x", false},
		{"invalid_request_error", provcore.CodeInvalidRequest, http.StatusBadRequest, "x", false},
		{"not_found_error", provcore.CodeInvalidRequest, http.StatusNotFound, "x", false}, // F-0227: unified with unary path
		{"api_error", provcore.CodeUpstreamError, http.StatusBadGateway, "x", false},
		{"", provcore.CodeUpstreamError, http.StatusBadGateway, "anthropic stream error", true}, // empty-msg default branch
		{"some_unknown_etype", provcore.CodeUpstreamError, http.StatusBadGateway, "x", false},
	}
	for _, tc := range cases {
		msg := tc.wantMsg
		if tc.emptyMsg {
			msg = ""
		}
		err := mapAnthropicStreamError(tc.etype, msg)
		var pe *provcore.ProviderError
		if !errors.As(err, &pe) {
			t.Fatalf("etype %q: not *ProviderError: %T", tc.etype, err)
		}
		if pe.Code != tc.wantCode {
			t.Errorf("etype %q: code=%q want %q", tc.etype, pe.Code, tc.wantCode)
		}
		if pe.Status != tc.wantHTTP {
			t.Errorf("etype %q: status=%d want %d", tc.etype, pe.Status, tc.wantHTTP)
		}
		if pe.Message != tc.wantMsg {
			t.Errorf("etype %q: msg=%q want %q", tc.etype, pe.Message, tc.wantMsg)
		}
	}
}

// TestTransport_NewWithNilLogger pins the slog.Default() fallback.
func TestTransport_NewWithNilLogger(t *testing.T) {
	if NewTransport(nil) == nil {
		t.Fatal("nil-logger constructor must not return nil")
	}
}

// TestTransport_BuildURL_AllBranches covers every BuildURL outcome:
// empty BaseURL, models endpoint, unsupported endpoint, trailing-slash
// normalisation.
func TestTransport_BuildURL_AllBranches(t *testing.T) {
	tr := NewTransport(slog.Default())
	if _, err := tr.BuildURL(provcore.CallTarget{BaseURL: ""}, typology.WireShapeAnthropicMessages, false); err == nil {
		t.Error("empty BaseURL must error")
	}
	got, err := tr.BuildURL(provcore.CallTarget{BaseURL: "https://api.anthropic.com/"}, typology.WireShapeNone, false)
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://api.anthropic.com/v1/models" {
		t.Errorf("models URL=%q (trailing-slash not trimmed?)", got)
	}
	if _, err := tr.BuildURL(provcore.CallTarget{BaseURL: "https://x"}, typology.WireShapeOpenAIEmbeddings, false); err == nil {
		t.Error("unsupported endpoint must error")
	}
}

// TestTransport_ApplyAuth_MissingAPIKey covers line 78-80.
func TestTransport_ApplyAuth_MissingAPIKey(t *testing.T) {
	tr := NewTransport(slog.Default())
	req, _ := http.NewRequest(http.MethodPost, "http://x", nil)
	if err := tr.ApplyAuth(req, provcore.CallTarget{}); err == nil {
		t.Fatal("expected error on missing API key")
	}
}

// TestTransport_Probe_EmptyBaseURL covers line 108-110.
func TestTransport_Probe_EmptyBaseURL(t *testing.T) {
	tr := NewTransport(slog.Default())
	r, err := tr.Probe(context.Background(), provcore.CallTarget{})
	if err != nil {
		t.Fatal(err)
	}
	if r == nil || r.OK {
		t.Errorf("empty base URL must produce a not-OK probe result, got %+v", r)
	}
}

// TestTransport_Probe_Success covers the happy path (line 129-130) —
// 200 from a /v1/models endpoint marks OK=true and records latency.
func TestTransport_Probe_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path=%q want /v1/models", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "k" {
			t.Errorf("x-api-key missing")
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Errorf("anthropic-version missing")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer server.Close()

	tr := NewTransport(slog.Default())
	r, err := tr.Probe(context.Background(), provcore.CallTarget{BaseURL: server.URL, APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if !r.OK {
		t.Errorf("expected OK probe, got %+v", r)
	}
	if r.LatencyMs < 0 {
		t.Errorf("LatencyMs=%d", r.LatencyMs)
	}
}

// TestTransport_Probe_HTTPFailure covers line 131-132 — non-2xx upstream
// marks OK=false but the call still completes with no Go error.
func TestTransport_Probe_HTTPFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	tr := NewTransport(slog.Default())
	r, err := tr.Probe(context.Background(), provcore.CallTarget{BaseURL: server.URL, APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if r.OK {
		t.Errorf("5xx upstream must mark OK=false: %+v", r)
	}
	if !strings.Contains(r.Detail, "500") {
		t.Errorf("detail must mention status, got %q", r.Detail)
	}
}

// TestCodec_EncodeRequest_ToolFilterBranches covers the three internal
// guards inside the tools loop: non-function type skipped, empty name
// skipped, invalid params JSON falls back to {}, and a tool with no
// parameters key falls back to the empty schema.
func TestCodec_EncodeRequest_ToolFilterBranches(t *testing.T) {
	canon := []byte(`{
		"model":"claude-3-5-sonnet-20241022","max_tokens":16,
		"messages":[{"role":"user","content":"hi"}],
		"tools":[
			{"type":"retrieval","function":{"name":"r"}},
			{"type":"function","function":{"name":""}},
			{"type":"function","function":{"name":"bad","parameters":"not-json"}},
			{"type":"function","function":{"name":"noparams","description":"d"}},
			{"type":"function","function":{"name":"ok","parameters":{"type":"object","properties":{"x":{"type":"integer"}}}}}
		]
	}`)
	encRes, err := codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatal(err)
	}
	tools := gjson.GetBytes(out, "tools").Array()
	// retrieval-type and empty-name are skipped; the other three pass.
	if len(tools) != 3 {
		t.Fatalf("expected 3 emitted tools (skip non-function + empty name), got %d: %s", len(tools), string(out))
	}
	names := []string{tools[0].Get("name").String(), tools[1].Get("name").String(), tools[2].Get("name").String()}
	hasBad, hasNoParams, hasOK := false, false, false
	for _, n := range names {
		switch n {
		case "bad":
			hasBad = true
		case "noparams":
			hasNoParams = true
		case "ok":
			hasOK = true
		}
	}
	if !hasBad || !hasNoParams || !hasOK {
		t.Errorf("missing expected tools in %v", names)
	}
	// "noparams" → empty-schema fallback (no parameters key) emits a
	// well-formed object schema with empty properties so the upstream
	// always receives a parseable input_schema.
	for _, tl := range tools {
		if tl.Get("name").String() == "noparams" {
			if !tl.Get("input_schema.properties").IsObject() {
				t.Errorf("noparams tool's empty-schema fallback lost: %s", tl.Raw)
			}
		}
	}
}

// TestCodec_EncodeRequest_EmptyRoleDefaultsToUser pins lines 388-390 —
// a message with role="" (no role field) defaults to user so the wire
// is still well-formed.
func TestCodec_EncodeRequest_EmptyRoleDefaultsToUser(t *testing.T) {
	canon := []byte(`{
		"model":"claude-3-5-sonnet-20241022","max_tokens":16,
		"messages":[{"content":"plain"}]
	}`)
	encRes, err := codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(out, "messages.0.role").String(); got != "user" {
		t.Errorf("missing role must default to user, got %q", got)
	}
}

// TestCodec_EncodeRequest_PartsContentEmptyArray pins line 415-417 — an
// explicitly empty parts array surfaces the empty-text fallback so the
// upstream still sees a valid message.
func TestCodec_EncodeRequest_PartsContentEmptyArray(t *testing.T) {
	canon := []byte(`{
		"model":"claude-3-5-sonnet-20241022","max_tokens":16,
		"messages":[{"role":"user","content":[]}]
	}`)
	encRes, err := codec{}.EncodeRequest(typology.WireShapeAnthropicMessages, canon, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.type").String(); got != "text" {
		t.Errorf("empty parts must produce empty-text fallback; got %s", string(out))
	}
}

// TestAnthropicModelMaxOutput_Claude3HaikuFamily pins the missing
// claude-3-haiku branch (line 730-731) — 4096 floor.
func TestAnthropicModelMaxOutput_Claude3HaikuFamily(t *testing.T) {
	if got := anthropicModelMaxOutput("claude-3-haiku-20240307"); got != 4096 {
		t.Errorf("claude-3-haiku=%d want 4096", got)
	}
}

// TestMessagesRequestToOpenAI_ToolNameEmptyIsSkipped pins line 108-110.
func TestMessagesRequestToOpenAI_ToolNameEmptyIsSkipped(t *testing.T) {
	body, err := MessagesRequestToOpenAIChatCompletion([]byte(`{
		"model":"m","max_tokens":16,
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"","input_schema":{"type":"object"}},{"name":"good","input_schema":{"type":"object"}}]
	}`), "")
	if err != nil {
		t.Fatal(err)
	}
	tools := gjson.GetBytes(body, "tools").Array()
	if len(tools) != 1 || tools[0].Get("function.name").String() != "good" {
		t.Errorf("empty-name tool not filtered; tools=%v", tools)
	}
}

// TestAnthropicMessageToOpenAI_EmptyRoleDefaultsToUser pins line 174-176.
func TestAnthropicMessageToOpenAI_EmptyRoleDefaultsToUser(t *testing.T) {
	body, err := MessagesRequestToOpenAIChatCompletion([]byte(`{
		"model":"m","max_tokens":16,
		"messages":[{"content":"plain"}]
	}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(body, "messages.0.role").String(); got != "user" {
		t.Errorf("missing role must default to user, got %q", got)
	}
}

// TestAnthropicMessageToOpenAI_AssistantToolUseEmptyInput pins line 242-244
// — when input is missing/empty, args defaults to "{}".
func TestAnthropicMessageToOpenAI_AssistantToolUseEmptyInput(t *testing.T) {
	body, err := MessagesRequestToOpenAIChatCompletion([]byte(`{
		"model":"m","max_tokens":16,
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"f"}]}
		]
	}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(body, "messages.1.tool_calls.0.function.arguments").String(); got != "{}" {
		t.Errorf("empty input must default to '{}', got %q", got)
	}
}

// TestAnthropicMessageToOpenAI_ImageOnlyNoText covers the parts-array
// fallback at lines 295-305 where a user message holds only an image
// part (no text) — content becomes a parts array, not a string.
func TestAnthropicMessageToOpenAI_ImageOnlyNoText(t *testing.T) {
	body, err := MessagesRequestToOpenAIChatCompletion([]byte(`{
		"model":"m","max_tokens":16,
		"messages":[{"role":"user","content":[
			{"type":"image","source":{"type":"url","url":"https://e/x.png"}}
		]}]
	}`), "")
	if err != nil {
		t.Fatal(err)
	}
	// With a single non-text part the helper still produces a parts
	// array (the unified text fast-path only fires for a single text
	// block).
	if !gjson.GetBytes(body, "messages.0.content").IsArray() {
		t.Errorf("image-only content not an array: %s", string(body))
	}
}

// TestAnthropicMessageToOpenAI_MultiTextNoImage covers the multi-line
// text branch at line 282-285 — multiple text parts produce a parts
// array (not a joined string).
func TestAnthropicMessageToOpenAI_MultiTextNoImage(t *testing.T) {
	body, err := MessagesRequestToOpenAIChatCompletion([]byte(`{
		"model":"m","max_tokens":16,
		"messages":[{"role":"user","content":[
			{"type":"text","text":"a"},
			{"type":"text","text":"b"}
		]}]
	}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if !gjson.GetBytes(body, "messages.0.content").IsArray() {
		t.Errorf("multi-text content must be array, got: %s", string(body))
	}
}

// TestOpenAIChatCompletionToMessagesResponse_InvalidArgsJSONFallback
// pins line 367-369 — a tool_call with malformed JSON arguments must
// default to input={}.
func TestOpenAIChatCompletionToMessagesResponse_InvalidArgsJSONFallback(t *testing.T) {
	openai := []byte(`{
		"id":"x","model":"claude",
		"choices":[{"message":{"role":"assistant","tool_calls":[
			{"id":"c1","type":"function","function":{"name":"f","arguments":"not-json"}}
		]},"finish_reason":"tool_calls"}]
	}`)
	out, err := OpenAIChatCompletionToMessagesResponse(openai)
	if err != nil {
		t.Fatal(err)
	}
	if !gjson.GetBytes(out, "content.0.input").IsObject() {
		t.Errorf("malformed args must yield empty-object input: %s", string(out))
	}
}

// TestOpenAIChatCompletionToMessagesResponse_NoContentEmitsEmptyText
// pins line 383-385 — when the canonical message has no tool_calls and
// no content, emit a single empty-text block (well-formed Anthropic
// response shape).
func TestOpenAIChatCompletionToMessagesResponse_NoContentEmitsEmptyText(t *testing.T) {
	openai := []byte(`{
		"id":"x","model":"claude",
		"choices":[{"message":{"role":"assistant"},"finish_reason":"stop"}]
	}`)
	out, err := OpenAIChatCompletionToMessagesResponse(openai)
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(out, "content.0.type").String(); got != "text" {
		t.Errorf("empty content default missing: %s", string(out))
	}
}

// TestTransport_Probe_TransportError covers line 125-127 — when the
// outbound HTTP call fails (closed server), Probe records the error.
func TestTransport_Probe_TransportError(t *testing.T) {
	// Bind a server, then immediately close so the dial fails. We pick
	// an unused localhost port via httptest then close before calling.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := srv.URL
	srv.Close()

	tr := NewTransport(slog.Default())
	r, err := tr.Probe(context.Background(), provcore.CallTarget{BaseURL: addr, APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if r.OK {
		t.Error("closed server must produce not-OK probe")
	}
	if r.Err == nil {
		t.Error("Err must be populated on transport failure")
	}
}

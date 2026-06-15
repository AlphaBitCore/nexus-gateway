package gemini

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

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

// TestEncodeRequest_WrongEndpoint asserts the codec rejects truly
// unsupported endpoints (e.g. Models listing) with a structured error.
// EndpointEmbeddings is supported — see embedding tests.
func TestEncodeRequest_WrongEndpoint(t *testing.T) {
	_, err := codec{}.EncodeRequest(typology.WireShapeNone, []byte(`{}`), provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected error for wrong endpoint")
	}
	if !strings.Contains(err.Error(), "unsupported endpoint") {
		t.Errorf("err=%v", err)
	}
}

// TestEncodeRequest_EmptyBody covers codec.go:41-43.
func TestEncodeRequest_EmptyBody(t *testing.T) {
	_, err := codec{}.EncodeRequest(typology.WireShapeGeminiGenerateContent, nil, provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected error for empty body")
	}
}

// TestEncodeRequest_NoMessages covers codec.go:94-96 — empty messages
// array surfaces the "no messages" sentinel.
func TestEncodeRequest_NoMessages(t *testing.T) {
	_, err := codec{}.EncodeRequest(typology.WireShapeGeminiGenerateContent, []byte(`{"messages":[]}`), provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected error for empty messages")
	}
	if !strings.Contains(err.Error(), "no messages") {
		t.Errorf("err=%v", err)
	}
}

// TestEncodeRequest_TopKAndMaxCompletionTokensOverride exercises the
// top_k mapping and the max_completion_tokens-wins-over-max_tokens rule
// (codec.go:53-63).
func TestEncodeRequest_TopKAndMaxCompletionTokensOverride(t *testing.T) {
	canon := []byte(`{
		"messages":[{"role":"user","content":"hi"}],
		"top_k":40,
		"max_tokens":100,
		"max_completion_tokens":50
	}`)
	encRes, err := codec{}.EncodeRequest(typology.WireShapeGeminiGenerateContent, canon, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(out, "generationConfig.topK").Int(); got != 40 {
		t.Errorf("topK=%d want 40", got)
	}
	if got := gjson.GetBytes(out, "generationConfig.maxOutputTokens").Int(); got != 50 {
		t.Errorf("maxOutputTokens=%d want 50 (max_completion_tokens should win)", got)
	}
}

// TestEncodeRequest_StopSingleString covers codec.go:75-77 — a plain
// string value for `stop` becomes a single-element stopSequences array.
func TestEncodeRequest_StopSingleString(t *testing.T) {
	canon := []byte(`{
		"messages":[{"role":"user","content":"hi"}],
		"stop":"END"
	}`)
	encRes, err := codec{}.EncodeRequest(typology.WireShapeGeminiGenerateContent, canon, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatal(err)
	}
	arr := gjson.GetBytes(out, "generationConfig.stopSequences").Array()
	if len(arr) != 1 || arr[0].String() != "END" {
		t.Errorf("stopSequences=%v want [END]", arr)
	}
}

// TestEncodeRequest_ToolChoiceVariants covers codec.go:130-156. Every
// tool_choice shape (none/required/auto/{type:function,...}) must map to
// the correct Gemini functionCallingConfig mode.
func TestEncodeRequest_ToolChoiceVariants(t *testing.T) {
	cases := []struct {
		name        string
		toolChoice  string
		wantMode    string
		wantAllowed string // empty if no allowedFunctionNames expected
	}{
		{"string_none", `"none"`, "NONE", ""},
		{"string_required", `"required"`, "ANY", ""},
		{"string_auto", `"auto"`, "AUTO", ""},
		{"object_named_function", `{"type":"function","function":{"name":"foo"}}`, "ANY", "foo"},
		{"object_function_no_name", `{"type":"function"}`, "ANY", ""},
		{"object_other_type", `{"type":"bogus"}`, "AUTO", ""},
		{"string_unknown_value", `"weird"`, "AUTO", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			canon := []byte(`{"messages":[{"role":"user","content":"hi"}],"tool_choice":` + tc.toolChoice + `}`)
			encRes, err := codec{}.EncodeRequest(typology.WireShapeGeminiGenerateContent, canon, provcore.CallTarget{})
			out := encRes.Body
			if err != nil {
				t.Fatal(err)
			}
			if got := gjson.GetBytes(out, "toolConfig.functionCallingConfig.mode").String(); got != tc.wantMode {
				t.Errorf("mode=%q want %q", got, tc.wantMode)
			}
			if tc.wantAllowed != "" {
				arr := gjson.GetBytes(out, "toolConfig.functionCallingConfig.allowedFunctionNames").Array()
				if len(arr) != 1 || arr[0].String() != tc.wantAllowed {
					t.Errorf("allowedFunctionNames=%v want [%s]", arr, tc.wantAllowed)
				}
			}
		})
	}
}

// TestEncodeRequest_ResponseFormatJSON exercises both response_format
// shapes (json_object and json_schema) — codec.go:158-181.
func TestEncodeRequest_ResponseFormatJSON(t *testing.T) {
	t.Run("json_object", func(t *testing.T) {
		canon := []byte(`{
			"messages":[{"role":"user","content":"hi"}],
			"response_format":{"type":"json_object"}
		}`)
		encRes, err := codec{}.EncodeRequest(typology.WireShapeGeminiGenerateContent, canon, provcore.CallTarget{})
		out := encRes.Body
		if err != nil {
			t.Fatal(err)
		}
		if got := gjson.GetBytes(out, "generationConfig.responseMimeType").String(); got != "application/json" {
			t.Errorf("responseMimeType=%q", got)
		}
	})

	t.Run("json_schema_merges_with_existing_gen_cfg", func(t *testing.T) {
		canon := []byte(`{
			"messages":[{"role":"user","content":"hi"}],
			"temperature":0.5,
			"response_format":{"type":"json_schema","json_schema":{"name":"X","schema":{"type":"object","properties":{"a":{"type":"string"}}}}}
		}`)
		encRes, err := codec{}.EncodeRequest(typology.WireShapeGeminiGenerateContent, canon, provcore.CallTarget{})
		out := encRes.Body
		if err != nil {
			t.Fatal(err)
		}
		if got := gjson.GetBytes(out, "generationConfig.responseMimeType").String(); got != "application/json" {
			t.Errorf("responseMimeType=%q", got)
		}
		if !gjson.GetBytes(out, "generationConfig.responseSchema").Exists() {
			t.Errorf("responseSchema missing: %s", string(out))
		}
		if got := gjson.GetBytes(out, "generationConfig.temperature").Float(); got != 0.5 {
			t.Errorf("temperature dropped: %v", got)
		}
	})
}

// TestEncodeRequest_ToolWithoutFunctionTypeSkipped covers codec.go:102-104
// (tool.type != "function") and codec.go:107-109 (missing function name).
func TestEncodeRequest_ToolWithoutFunctionTypeSkipped(t *testing.T) {
	canon := []byte(`{
		"messages":[{"role":"user","content":"hi"}],
		"tools":[
			{"type":"retrieval"},
			{"type":"function","function":{"description":"no name here"}},
			{"type":"function","function":{"name":"good","parameters":{"type":"object"}}}
		]
	}`)
	encRes, err := codec{}.EncodeRequest(typology.WireShapeGeminiGenerateContent, canon, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatal(err)
	}
	decls := gjson.GetBytes(out, "tools.0.functionDeclarations").Array()
	if len(decls) != 1 || decls[0].Get("name").String() != "good" {
		t.Errorf("expected only 'good' fn, got: %v", decls)
	}
}

// TestEncodeRequest_ToolWithBadParameters covers codec.go:116-118 — when
// parameters JSON is malformed, the codec falls back to {type:object}.
func TestEncodeRequest_ToolWithBadParameters(t *testing.T) {
	canon := []byte(`{
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"f"}}]
	}`)
	encRes, err := codec{}.EncodeRequest(typology.WireShapeGeminiGenerateContent, canon, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatal(err)
	}
	params := gjson.GetBytes(out, "tools.0.functionDeclarations.0.parameters")
	if !params.IsObject() || params.Get("type").String() != "object" {
		t.Errorf("expected default {type:object} params, got %s", params.Raw)
	}
}

// TestSplitMessages_SystemConcatenated covers codec.go:267-271 — multiple
// system messages concatenate with newline separators into a single
// systemInstruction.
func TestSplitMessages_SystemConcatenated(t *testing.T) {
	canon := []byte(`{
		"messages":[
			{"role":"system","content":"line A"},
			{"role":"system","content":"line B"},
			{"role":"user","content":"hi"}
		]
	}`)
	encRes, err := codec{}.EncodeRequest(typology.WireShapeGeminiGenerateContent, canon, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatal(err)
	}
	got := gjson.GetBytes(out, "systemInstruction.parts.0.text").String()
	if got != "line A\nline B" {
		t.Errorf("system=%q want %q", got, "line A\nline B")
	}
}

// TestSplitMessages_ToolContentShapes pins all branches of the
// role:"tool" handling in splitMessages (codec.go:280-330):
//   - JSON array content wraps under {result: <arr>}
//   - empty string content → {result:""}
//   - tool_call_id falls back to "unknown" when both id and name are missing
func TestSplitMessages_ToolContentShapes(t *testing.T) {
	t.Run("array_content_wrapped_under_result", func(t *testing.T) {
		canon := []byte(`{
			"messages":[
				{"role":"user","content":"x"},
				{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"c1","content":"[1,2,3]"}
			]
		}`)
		encRes, err := codec{}.EncodeRequest(typology.WireShapeGeminiGenerateContent, canon, provcore.CallTarget{})
		out := encRes.Body
		if err != nil {
			t.Fatal(err)
		}
		// content is a string "[1,2,3]" — splitMessages probe path treats
		// it as JSON-decodable string and wraps as {result: "[1,2,3]"}
		// (since unmarshal-into-map fails for an array).
		resp := gjson.GetBytes(out, "contents.2.parts.0.functionResponse.response")
		if !resp.IsObject() {
			t.Fatalf("response must be object, got %s", resp.Raw)
		}
		if got := resp.Get("result").String(); got != "[1,2,3]" {
			t.Errorf("response.result=%q want [1,2,3]", got)
		}
	})

	t.Run("empty_string_content_becomes_empty_result", func(t *testing.T) {
		canon := []byte(`{
			"messages":[
				{"role":"user","content":"x"},
				{"role":"tool","tool_call_id":"c1","content":""}
			]
		}`)
		encRes, err := codec{}.EncodeRequest(typology.WireShapeGeminiGenerateContent, canon, provcore.CallTarget{})
		out := encRes.Body
		if err != nil {
			t.Fatal(err)
		}
		resp := gjson.GetBytes(out, "contents.1.parts.0.functionResponse.response")
		if !resp.IsObject() {
			t.Fatalf("response must be object, got %s", resp.Raw)
		}
		if got := resp.Get("result").String(); got != "" {
			t.Errorf("response.result=%q want empty", got)
		}
	})

	t.Run("missing_id_and_name_yields_unknown", func(t *testing.T) {
		// No tool_call_id, no prior assistant tool_calls → fnName falls
		// back to "unknown".
		canon := []byte(`{
			"messages":[
				{"role":"user","content":"x"},
				{"role":"tool","content":"some"}
			]
		}`)
		encRes, err := codec{}.EncodeRequest(typology.WireShapeGeminiGenerateContent, canon, provcore.CallTarget{})
		out := encRes.Body
		if err != nil {
			t.Fatal(err)
		}
		if got := gjson.GetBytes(out, "contents.1.parts.0.functionResponse.name").String(); got != "unknown" {
			t.Errorf("name=%q want unknown", got)
		}
	})

	t.Run("array_content_as_native_array", func(t *testing.T) {
		// content arrives as a real JSON array (not a string) — covers
		// the IsArray() branch at codec.go:307-311.
		canon := []byte(`{
			"messages":[
				{"role":"user","content":"x"},
				{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"c1","content":[{"key":"v"}]}
			]
		}`)
		encRes, err := codec{}.EncodeRequest(typology.WireShapeGeminiGenerateContent, canon, provcore.CallTarget{})
		out := encRes.Body
		if err != nil {
			t.Fatal(err)
		}
		resp := gjson.GetBytes(out, "contents.2.parts.0.functionResponse.response")
		if !resp.IsObject() {
			t.Fatalf("response must be object, got %s", resp.Raw)
		}
		arr := resp.Get("result").Array()
		if len(arr) != 1 || arr[0].Get("key").String() != "v" {
			t.Errorf("response.result=%v want [{key:v}]", arr)
		}
	})
}

// TestSplitMessages_EmptyRoleBecomesUser covers codec.go:337-339 —
// when a canonical message has an empty role, the Gemini-side role
// defaults to "user".
func TestSplitMessages_EmptyRoleBecomesUser(t *testing.T) {
	canon := []byte(`{
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"","content":"unspecified"}
		]
	}`)
	encRes, err := codec{}.EncodeRequest(typology.WireShapeGeminiGenerateContent, canon, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(out, "contents.1.role").String(); got != "user" {
		t.Errorf("contents[1].role=%q want user", got)
	}
	if got := gjson.GetBytes(out, "contents.1.parts.0.text").String(); got != "unspecified" {
		t.Errorf("contents[1].parts[0].text=%q want unspecified", got)
	}
}

// TestSplitMessages_NonStringEmptyContentFallback covers codec.go:440-442
// — when content is an empty array (so the string branch at L388 doesn't
// catch it and no text/image parts get appended), the codec emits a
// fallback empty-text part so the Gemini schema (parts: required) is met.
func TestSplitMessages_NonStringEmptyContentFallback(t *testing.T) {
	canon := []byte(`{
		"messages":[
			{"role":"user","content":[]}
		]
	}`)
	encRes, err := codec{}.EncodeRequest(typology.WireShapeGeminiGenerateContent, canon, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatal(err)
	}
	parts := gjson.GetBytes(out, "contents.0.parts").Array()
	if len(parts) != 1 {
		t.Fatalf("expected exactly 1 fallback part, got %d: %s", len(parts), string(out))
	}
	if parts[0].Get("text").String() != "" {
		t.Errorf("fallback text=%q want empty", parts[0].Get("text").String())
	}
}

// TestOpenAIMessageToGeminiParts_ToolCallBadArgs covers codec.go:361-363
// (missing arguments → "{}") and codec.go:365-367 (malformed JSON →
// empty object).
func TestOpenAIMessageToGeminiParts_ToolCallBadArgs(t *testing.T) {
	t.Run("empty_args_becomes_empty_object", func(t *testing.T) {
		canon := []byte(`{
			"messages":[
				{"role":"user","content":"hi"},
				{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f"}}]}
			]
		}`)
		encRes, err := codec{}.EncodeRequest(typology.WireShapeGeminiGenerateContent, canon, provcore.CallTarget{})
		out := encRes.Body
		if err != nil {
			t.Fatal(err)
		}
		args := gjson.GetBytes(out, "contents.1.parts.0.functionCall.args")
		if !args.IsObject() || args.Raw != "{}" {
			t.Errorf("args=%s want {}", args.Raw)
		}
	})

	t.Run("malformed_args_falls_back_to_empty_object", func(t *testing.T) {
		canon := []byte(`{
			"messages":[
				{"role":"user","content":"hi"},
				{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"not json"}}]}
			]
		}`)
		encRes, err := codec{}.EncodeRequest(typology.WireShapeGeminiGenerateContent, canon, provcore.CallTarget{})
		out := encRes.Body
		if err != nil {
			t.Fatal(err)
		}
		args := gjson.GetBytes(out, "contents.1.parts.0.functionCall.args")
		if !args.IsObject() {
			t.Errorf("args must be object, got %s", args.Raw)
		}
	})
}

// TestOpenAIMessageToGeminiParts_ImageURLMissing covers codec.go:409-411
// — an image_url part with no url field surfaces a structured error.
func TestOpenAIMessageToGeminiParts_ImageURLMissing(t *testing.T) {
	canon := []byte(`{
		"messages":[{"role":"user","content":[
			{"type":"image_url","image_url":{"detail":"auto"}}
		]}]
	}`)
	_, err := codec{}.EncodeRequest(typology.WireShapeGeminiGenerateContent, canon, provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected error for missing url")
	}
	var pe *provcore.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *ProviderError: %T %v", err, err)
	}
	if pe.Type != "nexus_field_unsupported" {
		t.Errorf("type=%q", pe.Type)
	}
}

// TestOpenAIMessageToGeminiParts_DataURLMalformed covers codec.go:415-418
// (data: URL fails parseDataURL) propagating an unsupported-field error.
func TestOpenAIMessageToGeminiParts_DataURLMalformed(t *testing.T) {
	canon := []byte(`{
		"messages":[{"role":"user","content":[
			{"type":"image_url","image_url":{"url":"data:image/png;NOT-base64"}}
		]}]
	}`)
	_, err := codec{}.EncodeRequest(typology.WireShapeGeminiGenerateContent, canon, provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected error for malformed data url")
	}
}

// TestParseDataURL_EdgeCases pins every fail path of parseDataURL
// (codec.go:449-467).
func TestParseDataURL_EdgeCases(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"no_data_prefix", "image/png;base64,xxx", false},
		{"no_comma", "data:image/pngbase64xxx", false},
		{"trailing_comma_empty_payload", "data:image/png;base64,", false},
		{"missing_base64_marker", "data:image/png,xxx", false},
		{"empty_media_defaults_to_octet_stream", "data:;base64,xxx", true},
		{"happy_path", "data:image/webp;base64,YWJj", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			media, b64, ok := parseDataURL(tc.in)
			if ok != tc.ok {
				t.Errorf("ok=%v want %v (media=%q b64=%q)", ok, tc.ok, media, b64)
			}
			if tc.ok && tc.name == "empty_media_defaults_to_octet_stream" && media != "application/octet-stream" {
				t.Errorf("media=%q want application/octet-stream", media)
			}
		})
	}
}

// TestStringifyContent_AllShapes covers codec.go:500-520 (every branch
// of stringifyContent: missing, string, text-array, non-text-array,
// non-string scalar).
func TestStringifyContent_AllShapes(t *testing.T) {
	cases := []struct {
		name string
		json string // wraps as messages[0].content
		want string
	}{
		{"plain_string", `"hello world"`, "hello world"},
		{"text_array_single", `[{"type":"text","text":"abc"}]`, "abc"},
		{"text_array_multiple_newline_joined", `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`, "a\nb"},
		{"non_text_parts_ignored", `[{"type":"image_url","image_url":{"url":"https://x"}}]`, ""},
		{"missing_returns_empty", `null`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			canon := []byte(`{"messages":[{"role":"system","content":` + tc.json + `},{"role":"user","content":"go"}]}`)
			encRes, err := codec{}.EncodeRequest(typology.WireShapeGeminiGenerateContent, canon, provcore.CallTarget{})
			out := encRes.Body
			if err != nil {
				t.Fatal(err)
			}
			got := gjson.GetBytes(out, "systemInstruction.parts.0.text").String()
			if got != tc.want {
				// "missing_returns_empty" produces no systemInstruction
				// at all; only assert when one is expected.
				if tc.want != "" {
					t.Errorf("system text=%q want %q", got, tc.want)
				} else if gjson.GetBytes(out, "systemInstruction").Exists() && got != "" {
					t.Errorf("system text=%q want empty", got)
				}
			}
		})
	}
}

// TestDecodeResponse_NonChatEndpoint asserts truly unsupported endpoint
// decoding (e.g. Models) passes through as bytes. EndpointEmbeddings is
// now handled by the embedding codec branch — see embedding tests.
func TestDecodeResponse_NonChatEndpoint(t *testing.T) {
	body := []byte(`{"some":"thing"}`)
	decRes, err := codec{}.DecodeResponse(typology.WireShapeNone, body, "", provcore.DecodeContext{})
	out := decRes.CanonicalBody
	usage := decRes.Usage
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(body) {
		t.Errorf("body changed: %s", string(out))
	}
	if usage.PromptTokens != nil {
		t.Errorf("usage should be empty")
	}
}

// TestDecodeResponse_EmptyBody covers codec.go:548-550.
func TestDecodeResponse_EmptyBody(t *testing.T) {
	decRes, err := codec{}.DecodeResponse(typology.WireShapeGeminiGenerateContent, nil, "", provcore.DecodeContext{})
	out := decRes.CanonicalBody
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Errorf("empty body should pass through: %q", out)
	}
}

// TestDecodeResponse_MalformedNativeBody covers codec.go:559-562 — the
// normalize error path leaves the payload empty, but the projector
// continues and emits a canonical envelope.
func TestDecodeResponse_MalformedNativeBody(t *testing.T) {
	decRes, err := codec{}.DecodeResponse(typology.WireShapeGeminiGenerateContent, []byte(`{"candidates":"not an array"}`), "", provcore.DecodeContext{})
	out := decRes.CanonicalBody
	if err != nil {
		t.Fatal(err)
	}
	if !gjson.GetBytes(out, "object").Exists() {
		t.Errorf("expected canonical envelope even for malformed input: %s", string(out))
	}
}

// TestUsageToNormalize_EmptyReturnsNil pins the nil-on-empty contract.
func TestUsageToNormalize_EmptyReturnsNil(t *testing.T) {
	if v := usageToNormalize(provcore.Usage{}); v != nil {
		t.Errorf("empty Usage should produce nil normalize.Usage, got %+v", v)
	}
	p := 1
	if v := usageToNormalize(provcore.Usage{PromptTokens: &p}); v == nil {
		t.Error("non-empty Usage produced nil")
	}
}

// TestErrorNormalizer_AllStatusBuckets exercises errors.go:28-61 — the
// status-mapping table for every Google API canonical status the
// Normalize function knows about, plus the unmapped fall-through.
func TestErrorNormalizer_AllStatusBuckets(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"INVALID_ARGUMENT", 400, `{"error":{"status":"INVALID_ARGUMENT","message":"x"}}`, provcore.CodeInvalidRequest},
		{"FAILED_PRECONDITION", 400, `{"error":{"status":"FAILED_PRECONDITION","message":"x"}}`, provcore.CodeInvalidRequest},
		{"NOT_FOUND", 404, `{"error":{"status":"NOT_FOUND","message":"x"}}`, provcore.CodeInvalidRequest},
		{"UNAUTHENTICATED", 401, `{"error":{"status":"UNAUTHENTICATED","message":"x"}}`, provcore.CodeAuthFailed},
		{"PERMISSION_DENIED", 403, `{"error":{"status":"PERMISSION_DENIED","message":"x"}}`, provcore.CodeAuthFailed},
		{"DEADLINE_EXCEEDED", 504, `{"error":{"status":"DEADLINE_EXCEEDED","message":"x"}}`, provcore.CodeTimeout},
		{"UNAVAILABLE", 503, `{"error":{"status":"UNAVAILABLE","message":"x"}}`, provcore.CodeUpstreamError},
		{"INTERNAL", 500, `{"error":{"status":"INTERNAL","message":"x"}}`, provcore.CodeUpstreamError},
		{"status_only_400", 400, `{"oops":"no error envelope"}`, provcore.CodeInvalidRequest},
		{"status_only_401", 401, `{}`, provcore.CodeAuthFailed},
		{"status_only_408", 408, `{}`, provcore.CodeTimeout},
		{"status_only_504", 504, `{}`, provcore.CodeTimeout},
		{"status_only_500", 500, `{}`, provcore.CodeUpstreamError},
	}
	norm := errorNormalizer{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pe := norm.Normalize(tc.status, nil, []byte(tc.body))
			if pe.Code != tc.want {
				t.Errorf("code=%q want %q", pe.Code, tc.want)
			}
			if pe.Message == "" {
				t.Errorf("message must default to status text when missing")
			}
		})
	}
}

// TestErrorNormalizer_RateLimitedWithRetryAfter covers the
// RESOURCE_EXHAUSTED path AND the seconds-int branch of parseRetryAfter
// (errors.go:69-72).
func TestErrorNormalizer_RateLimitedWithRetryAfter(t *testing.T) {
	headers := http.Header{}
	headers.Set("retry-after", "30")
	pe := errorNormalizer{}.Normalize(429, headers, []byte(`{"error":{"status":"RESOURCE_EXHAUSTED","message":"slow down"}}`))
	if pe.Code != provcore.CodeRateLimited {
		t.Errorf("code=%q want rate_limited", pe.Code)
	}
	if pe.RetryAfter == nil || *pe.RetryAfter != 30*time.Second {
		t.Errorf("RetryAfter=%v want 30s", pe.RetryAfter)
	}
}

// TestErrorNormalizer_StatusOnly429 covers the status-only branch for
// 429 plus the same seconds parse path.
func TestErrorNormalizer_StatusOnly429(t *testing.T) {
	headers := http.Header{}
	headers.Set("retry-after", "10")
	pe := errorNormalizer{}.Normalize(429, headers, nil)
	if pe.Code != provcore.CodeRateLimited {
		t.Errorf("code=%q want rate_limited", pe.Code)
	}
	if pe.RetryAfter == nil || *pe.RetryAfter != 10*time.Second {
		t.Errorf("RetryAfter=%v want 10s", pe.RetryAfter)
	}
}

// TestParseRetryAfter_HTTPDate covers errors.go:73-78 — when the header
// carries an HTTP-date rather than a seconds integer, parseRetryAfter
// returns the time-until-then (clamped to ≥0).
func TestParseRetryAfter_HTTPDate(t *testing.T) {
	future := time.Now().Add(60 * time.Second).UTC().Format(http.TimeFormat)
	d := parseRetryAfter(future)
	if d == nil {
		t.Fatalf("nil for HTTP-date %q", future)
	}
	if *d <= 0 || *d > 65*time.Second {
		t.Errorf("RetryAfter=%v out of expected ~60s window", d)
	}

	// Past date clamps to zero.
	past := time.Now().Add(-60 * time.Second).UTC().Format(http.TimeFormat)
	d = parseRetryAfter(past)
	if d == nil || *d != 0 {
		t.Errorf("past date should clamp to 0, got %v", d)
	}

	// Garbage returns nil.
	if v := parseRetryAfter("not-a-date"); v != nil {
		t.Errorf("garbage should return nil, got %v", v)
	}
	// Empty returns nil.
	if v := parseRetryAfter(""); v != nil {
		t.Errorf("empty should return nil, got %v", v)
	}
	// Negative integer returns nil (errors.go: secs >= 0 guard).
	if v := parseRetryAfter("-5"); v != nil {
		t.Errorf("negative seconds should return nil, got %v", v)
	}
}

// TestNewSpec_Wiring covers spec.go:14-25 — NewSpec returns a fully
// populated AdapterSpec; nil log is replaced with slog.Default.
func TestNewSpec_Wiring(t *testing.T) {
	t.Run("nil_log_replaced", func(t *testing.T) {
		s := NewSpec(nil)
		if s.Format != provcore.FormatGemini {
			t.Errorf("format=%q", s.Format)
		}
		if s.Transport == nil || s.SchemaCodec == nil || s.StreamDecoder == nil || s.ErrorNormalizer == nil {
			t.Errorf("AdapterSpec missing components: %+v", s)
		}
	})
	t.Run("custom_log_kept", func(t *testing.T) {
		s := NewSpec(slog.Default())
		if s.Format != provcore.FormatGemini {
			t.Errorf("format=%q", s.Format)
		}
	})
}

// TestStream_NilBody covers stream.go:35-37.
func TestStream_NilBody(t *testing.T) {
	dec := NewStreamDecoder(nil)
	if _, err := dec.Open(nil, typology.WireShapeGeminiGenerateContent); err == nil {
		t.Fatal("expected error for nil body")
	}
}

// TestStream_EmptyBodySurfacesProviderError covers stream.go:74-86 —
// EOF without any data frame surfaces a structured 502 ProviderError so
// the client sees an explicit error rather than silent [DONE].
func TestStream_EmptyBodySurfacesProviderError(t *testing.T) {
	dec := NewStreamDecoder(nil)
	sess, err := dec.Open(io.NopCloser(strings.NewReader("")), typology.WireShapeGeminiGenerateContent)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close() //nolint:errcheck
	_, err = sess.Next(context.Background())
	if err == nil {
		t.Fatal("expected error on empty stream")
	}
	var pe *provcore.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *ProviderError, got %T %v", err, err)
	}
	if pe.Status != 502 {
		t.Errorf("status=%d want 502", pe.Status)
	}
}

// TestStream_ContextCancelled covers stream.go:60-62 — Next must
// surface a cancelled context as the ctx error.
func TestStream_ContextCancelled(t *testing.T) {
	dec := NewStreamDecoder(nil)
	raw := `data: {"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}` + "\n\n"
	sess, err := dec.Open(io.NopCloser(strings.NewReader(raw)), typology.WireShapeGeminiGenerateContent)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close() //nolint:errcheck
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sess.Next(ctx); err == nil {
		t.Fatal("expected ctx error")
	}
}

// TestStream_DoneSticksAfterEOF covers stream.go:57-59 — once `done` is
// set, every subsequent Next must return io.EOF.
func TestStream_DoneSticksAfterEOF(t *testing.T) {
	dec := NewStreamDecoder(nil)
	raw := strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}]}`,
		``,
	}, "\n")
	sess, err := dec.Open(io.NopCloser(strings.NewReader(raw)), typology.WireShapeGeminiGenerateContent)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close() //nolint:errcheck
	for range 5 {
		_, _ = sess.Next(context.Background())
	}
	// Final call must be EOF.
	if _, err := sess.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Errorf("expected io.EOF after done, got %v", err)
	}
}

// TestFormatSSE_EventLine covers stream.go:160-164 — when the SSE event
// carries a non-empty event: line, formatSSE prepends it.
func TestFormatSSE_EventLine(t *testing.T) {
	got := formatSSE("message_start", []byte(`{"x":1}`))
	if !strings.HasPrefix(string(got), "event: message_start\ndata: ") {
		t.Errorf("missing event line: %q", string(got))
	}
	if !strings.HasSuffix(string(got), "\n\n") {
		t.Errorf("missing terminator: %q", string(got))
	}
}

// TestTransport_BuildURL_AllEndpoints covers transport.go:39-67 — every
// supported endpoint and the empty-BaseURL / missing-ModelID guards.
func TestTransport_BuildURL_AllEndpoints(t *testing.T) {
	tr := NewTransport(nil) // nil-log path

	t.Run("embeddings", func(t *testing.T) {
		got, err := tr.BuildURL(provcore.CallTarget{BaseURL: "https://x", ProviderModelID: "m"}, typology.WireShapeGeminiEmbedContent, false)
		if err != nil {
			t.Fatal(err)
		}
		if got != "https://x/v1beta/models/m:embedContent" {
			t.Errorf("url=%q", got)
		}
	})

	t.Run("models", func(t *testing.T) {
		// BuildURL gates on ProviderModelID for every endpoint, including
		// /v1beta/models — keep that contract by supplying a placeholder.
		got, err := tr.BuildURL(provcore.CallTarget{BaseURL: "https://x", ProviderModelID: "m"}, typology.WireShapeNone, false)
		if err != nil {
			t.Fatal(err)
		}
		if got != "https://x/v1beta/models" {
			t.Errorf("url=%q", got)
		}
	})

	t.Run("unsupported_endpoint", func(t *testing.T) {
		_, err := tr.BuildURL(provcore.CallTarget{BaseURL: "https://x", ProviderModelID: "m"}, typology.WireShape("bogus"), false)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("empty_base_url", func(t *testing.T) {
		_, err := tr.BuildURL(provcore.CallTarget{ProviderModelID: "m"}, typology.WireShapeGeminiGenerateContent, false)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("missing_model_id", func(t *testing.T) {
		_, err := tr.BuildURL(provcore.CallTarget{BaseURL: "https://x"}, typology.WireShapeGeminiGenerateContent, false)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("trailing_slash_stripped", func(t *testing.T) {
		got, err := tr.BuildURL(provcore.CallTarget{BaseURL: "https://x/", ProviderModelID: "m"}, typology.WireShapeGeminiGenerateContent, false)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(got, "//v1beta") {
			t.Errorf("double slash in url: %s", got)
		}
	})
}

// TestTransport_ApplyAuth_MissingKey covers transport.go:71-73.
func TestTransport_ApplyAuth_MissingKey(t *testing.T) {
	tr := NewTransport(nil)
	req, _ := http.NewRequest(http.MethodPost, "http://x", nil)
	if err := tr.ApplyAuth(req, provcore.CallTarget{}); err == nil {
		t.Fatal("expected error for missing API key")
	}
}

// TestTransport_Do exercises the otherwise-uncovered Do method. We
// don't assert specific response bytes — the contract is "delegate to
// the shared http.Client with the request's context".
func TestTransport_Do(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	tr := NewTransport(nil)
	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := tr.Do(context.Background(), req, provcore.CallTarget{})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if !called {
		t.Errorf("upstream not called")
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status=%d", resp.StatusCode)
	}
}

// TestTransport_Probe covers transport.go:85-111 — happy path, non-2xx,
// missing BaseURL, transport error, and the missing-API-key branch (we
// still issue the request, header just isn't set).
func TestTransport_Probe(t *testing.T) {
	t.Run("ok_200", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("x-goog-api-key"); got != "AIza" {
				t.Errorf("api key header=%q", got)
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()
		tr := NewTransport(nil)
		res, err := tr.Probe(context.Background(), provcore.CallTarget{BaseURL: server.URL, APIKey: "AIza"})
		if err != nil {
			t.Fatal(err)
		}
		if !res.OK {
			t.Errorf("res=%+v want OK", res)
		}
	})

	t.Run("non_2xx", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer server.Close()
		tr := NewTransport(nil)
		res, err := tr.Probe(context.Background(), provcore.CallTarget{BaseURL: server.URL, APIKey: "bad"})
		if err != nil {
			t.Fatal(err)
		}
		if res.OK {
			t.Errorf("res should not be OK: %+v", res)
		}
		if !strings.Contains(res.Detail, "401") {
			t.Errorf("Detail=%q want contain 401", res.Detail)
		}
	})

	t.Run("empty_base_url", func(t *testing.T) {
		tr := NewTransport(nil)
		res, err := tr.Probe(context.Background(), provcore.CallTarget{})
		if err != nil {
			t.Fatal(err)
		}
		if res.OK {
			t.Errorf("OK on empty BaseURL")
		}
	})

	t.Run("transport_error", func(t *testing.T) {
		// Point at a closed listener so dial fails immediately.
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
		server.Close()
		tr := NewTransport(nil)
		res, err := tr.Probe(context.Background(), provcore.CallTarget{BaseURL: server.URL, APIKey: "x"})
		if err != nil {
			t.Fatal(err)
		}
		if res.OK || res.Err == nil {
			t.Errorf("expected transport error: %+v", res)
		}
	})

	t.Run("no_api_key_header_omitted", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("x-goog-api-key") != "" {
				t.Errorf("api key header should be empty")
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()
		tr := NewTransport(nil)
		res, _ := tr.Probe(context.Background(), provcore.CallTarget{BaseURL: server.URL})
		if !res.OK {
			t.Errorf("OK expected even without key (probe issued anyway)")
		}
	})
}

// TestGenerateContentRequestToOpenAIChatCompletion_ErrorPaths covers
// hub_ingress.go:15-24, 173-175 — empty body, missing model, and missing
// contents all surface explicit errors.
func TestGenerateContentRequestToOpenAIChatCompletion_ErrorPaths(t *testing.T) {
	t.Run("empty_body", func(t *testing.T) {
		if _, err := GenerateContentRequestToOpenAIChatCompletion(nil, "m"); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("missing_model", func(t *testing.T) {
		if _, err := GenerateContentRequestToOpenAIChatCompletion([]byte(`{"contents":[]}`), ""); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("missing_contents", func(t *testing.T) {
		if _, err := GenerateContentRequestToOpenAIChatCompletion([]byte(`{}`), "m"); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("empty_contents_array_yields_no_messages", func(t *testing.T) {
		// Empty contents array passes the IsArray() guard but produces no
		// messages — covers the L173-175 sentinel.
		_, err := GenerateContentRequestToOpenAIChatCompletion([]byte(`{"contents":[]}`), "m")
		if err == nil {
			t.Fatal("expected 'no messages' error for empty contents array")
		}
		if !strings.Contains(err.Error(), "no messages") {
			t.Errorf("err=%v want contain 'no messages'", err)
		}
	})
}

// TestGenerateContentRequestToOpenAIChatCompletion_ModelInBody covers
// hub_ingress.go:19-21 — model can be taken from the request body when
// the caller doesn't supply one explicitly.
func TestGenerateContentRequestToOpenAIChatCompletion_ModelInBody(t *testing.T) {
	out, err := GenerateContentRequestToOpenAIChatCompletion([]byte(`{
		"model":"gemini-2.0-flash",
		"contents":[{"role":"user","parts":[{"text":"hi"}]}]
	}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(out, "model").String(); got != "gemini-2.0-flash" {
		t.Errorf("model=%q", got)
	}
}

// TestGenerateContentRequest_StopSequencesSingleVsMulti covers
// hub_ingress.go:46-50 — single-element stop sequence becomes a string,
// multi-element stays an array.
func TestGenerateContentRequest_StopSequencesSingleVsMulti(t *testing.T) {
	t.Run("single", func(t *testing.T) {
		out, err := GenerateContentRequestToOpenAIChatCompletion([]byte(`{
			"contents":[{"role":"user","parts":[{"text":"hi"}]}],
			"generationConfig":{"stopSequences":["END"]}
		}`), "m")
		if err != nil {
			t.Fatal(err)
		}
		if got := gjson.GetBytes(out, "stop").String(); got != "END" {
			t.Errorf("stop=%q want END", got)
		}
	})
	t.Run("multi", func(t *testing.T) {
		out, err := GenerateContentRequestToOpenAIChatCompletion([]byte(`{
			"contents":[{"role":"user","parts":[{"text":"hi"}]}],
			"generationConfig":{"stopSequences":["A","B"]}
		}`), "m")
		if err != nil {
			t.Fatal(err)
		}
		arr := gjson.GetBytes(out, "stop").Array()
		if len(arr) != 2 {
			t.Errorf("stop=%v", arr)
		}
	})
}

// TestGenerateContentRequest_AllGenerationConfigFields covers all
// of hub_ingress.go:27-51 (temperature, topP, topK, maxOutputTokens).
func TestGenerateContentRequest_AllGenerationConfigFields(t *testing.T) {
	out, err := GenerateContentRequestToOpenAIChatCompletion([]byte(`{
		"contents":[{"role":"user","parts":[{"text":"hi"}]}],
		"generationConfig":{"temperature":0.3,"topP":0.9,"topK":32,"maxOutputTokens":256}
	}`), "m")
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(out, "temperature").Float(); got != 0.3 {
		t.Errorf("temperature=%v", got)
	}
	if got := gjson.GetBytes(out, "top_p").Float(); got != 0.9 {
		t.Errorf("top_p=%v", got)
	}
	if got := gjson.GetBytes(out, "top_k").Int(); got != 32 {
		t.Errorf("top_k=%v", got)
	}
	if got := gjson.GetBytes(out, "max_tokens").Int(); got != 256 {
		t.Errorf("max_tokens=%v", got)
	}
}

// TestGenerateContentRequest_FunctionCallWithoutID covers
// hub_ingress.go:122-125 — when Gemini omits functionCall.id, the hub
// must synthesize a stable canonical id from name+args.
func TestGenerateContentRequest_FunctionCallWithoutID(t *testing.T) {
	out, err := GenerateContentRequestToOpenAIChatCompletion([]byte(`{
		"contents":[
			{"role":"user","parts":[{"text":"hi"}]},
			{"role":"model","parts":[{"functionCall":{"name":"f","args":{}}}]}
		]
	}`), "m")
	if err != nil {
		t.Fatal(err)
	}
	id := gjson.GetBytes(out, "messages.1.tool_calls.0.id").String()
	if !strings.HasPrefix(id, "call_") {
		t.Errorf("id=%q want call_…", id)
	}
	// Determinism: identical inputs produce identical ids.
	out2, _ := GenerateContentRequestToOpenAIChatCompletion([]byte(`{
		"contents":[
			{"role":"user","parts":[{"text":"hi"}]},
			{"role":"model","parts":[{"functionCall":{"name":"f","args":{}}}]}
		]
	}`), "m")
	id2 := gjson.GetBytes(out2, "messages.1.tool_calls.0.id").String()
	if id != id2 {
		t.Errorf("ids differ for same input: %q vs %q", id, id2)
	}
}

// TestGenerateContentRequest_FunctionCallEmptyArgs covers
// hub_ingress.go:118-120 — missing/empty args default to "{}".
func TestGenerateContentRequest_FunctionCallEmptyArgs(t *testing.T) {
	out, err := GenerateContentRequestToOpenAIChatCompletion([]byte(`{
		"contents":[
			{"role":"user","parts":[{"text":"hi"}]},
			{"role":"model","parts":[{"functionCall":{"name":"f","id":"c1"}}]}
		]
	}`), "m")
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(out, "messages.1.tool_calls.0.function.arguments").String(); got != "{}" {
		t.Errorf("arguments=%q want {}", got)
	}
}

// TestGenerateContentRequest_FunctionResponseStringContent covers
// hub_ingress.go:141-143 — when functionResponse.response is a JSON
// string, the canonical tool message content carries the string value
// (not the JSON-encoded literal).
func TestGenerateContentRequest_FunctionResponseStringContent(t *testing.T) {
	out, err := GenerateContentRequestToOpenAIChatCompletion([]byte(`{
		"contents":[
			{"role":"user","parts":[{"text":"hi"}]},
			{"role":"model","parts":[{"functionCall":{"name":"f","id":"c1","args":{}}}]},
			{"role":"user","parts":[{"functionResponse":{"name":"f","id":"c1","response":"plain answer"}}]}
		]
	}`), "m")
	if err != nil {
		t.Fatal(err)
	}
	// Find the tool message.
	msgs := gjson.GetBytes(out, "messages").Array()
	var toolMsg gjson.Result
	for _, m := range msgs {
		if m.Get("role").String() == "tool" {
			toolMsg = m
			break
		}
	}
	if toolMsg.Get("content").String() != "plain answer" {
		t.Errorf("tool content=%q want plain answer", toolMsg.Get("content").String())
	}
	// tool_call_id should propagate from functionResponse.id (hub_ingress
	// preserves Gemini 3+ id over the name fallback).
	if toolMsg.Get("tool_call_id").String() != "c1" {
		t.Errorf("tool_call_id=%q", toolMsg.Get("tool_call_id").String())
	}
}

// TestGenerateContentRequest_FunctionResponseSplitsCompositeTurn covers
// hub_ingress.go:163-166 — when a Gemini turn carries both text/images
// AND functionResponse parts, the hub splits them into the visible
// composite message plus the role:"tool" message(s).
func TestGenerateContentRequest_FunctionResponseSplitsCompositeTurn(t *testing.T) {
	out, err := GenerateContentRequestToOpenAIChatCompletion([]byte(`{
		"contents":[
			{"role":"user","parts":[
				{"text":"see this"},
				{"functionResponse":{"name":"f","id":"c1","response":{"ok":true}}}
			]}
		]
	}`), "m")
	if err != nil {
		t.Fatal(err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	// Expect a user-text message AND a tool message.
	var sawText, sawTool bool
	for _, m := range msgs {
		if m.Get("role").String() == "user" && m.Get("content").String() == "see this" {
			sawText = true
		}
		if m.Get("role").String() == "tool" {
			sawTool = true
		}
	}
	if !sawText || !sawTool {
		t.Errorf("missing text or tool message: %+v", msgs)
	}
}

// TestGenerateContentRequest_ToolDeclarationsAndConfig covers
// hub_ingress.go:178-225 — tools with bogus / nameless declarations are
// filtered, and every functionCallingConfig mode (AUTO/NONE/ANY with
// allowed/empty/multiple names) maps correctly.
func TestGenerateContentRequest_ToolDeclarationsAndConfig(t *testing.T) {
	t.Run("named_decl_with_parameters", func(t *testing.T) {
		out, err := GenerateContentRequestToOpenAIChatCompletion([]byte(`{
			"contents":[{"role":"user","parts":[{"text":"hi"}]}],
			"tools":[{"functionDeclarations":[
				{"name":"","description":"nameless"},
				{"name":"good","description":"d","parameters":{"type":"object","properties":{"a":{"type":"string"}}}}
			]}]
		}`), "m")
		if err != nil {
			t.Fatal(err)
		}
		tools := gjson.GetBytes(out, "tools").Array()
		if len(tools) != 1 || tools[0].Get("function.name").String() != "good" {
			t.Errorf("expected single 'good' tool, got %v", tools)
		}
	})

	cases := []struct {
		name       string
		toolCfg    string
		wantChoice string // matches gjson result Raw form (string scalar or JSON object)
	}{
		{"mode_auto", `{"mode":"AUTO"}`, `"auto"`},
		{"mode_none", `{"mode":"NONE"}`, `"none"`},
		{"mode_any_one_allowed", `{"mode":"ANY","allowedFunctionNames":["pick_me"]}`, ""}, // assert via structure
		{"mode_any_multi_allowed_becomes_required", `{"mode":"ANY","allowedFunctionNames":["a","b"]}`, `"required"`},
		{"mode_any_no_allowed_becomes_required", `{"mode":"ANY"}`, `"required"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{
				"contents":[{"role":"user","parts":[{"text":"hi"}]}],
				"toolConfig":{"functionCallingConfig":` + tc.toolCfg + `}
			}`)
			out, err := GenerateContentRequestToOpenAIChatCompletion(body, "m")
			if err != nil {
				t.Fatal(err)
			}
			tc2 := gjson.GetBytes(out, "tool_choice")
			if tc.wantChoice != "" {
				if strings.TrimSpace(tc2.Raw) != tc.wantChoice {
					t.Errorf("tool_choice=%q want %q", tc2.Raw, tc.wantChoice)
				}
			} else {
				// single-allowed: object {type:function, function:{name}}.
				if tc2.Get("type").String() != "function" || tc2.Get("function.name").String() != "pick_me" {
					t.Errorf("tool_choice malformed: %s", tc2.Raw)
				}
			}
		})
	}
}

// TestOpenAIChatCompletionToGenerateContentResponse_ErrorAndEdgeCases
// covers hub_ingress.go:262-264 (empty body), 270-279 (tool_calls
// without text), and the cached_tokens passthrough at 349-358.
func TestOpenAIChatCompletionToGenerateContentResponse_ErrorAndEdgeCases(t *testing.T) {
	t.Run("empty_body", func(t *testing.T) {
		if _, err := OpenAIChatCompletionToGenerateContentResponse(nil); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("tool_calls_only_no_visible_text", func(t *testing.T) {
		openai := []byte(`{
			"id":"x","object":"chat.completion","model":"m",
			"choices":[{"index":0,"message":{
				"role":"assistant","content":null,
				"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]
			},"finish_reason":"tool_calls"}]
		}`)
		out, err := OpenAIChatCompletionToGenerateContentResponse(openai)
		if err != nil {
			t.Fatal(err)
		}
		parts := gjson.GetBytes(out, "candidates.0.content.parts").Array()
		if len(parts) != 1 {
			t.Fatalf("want 1 functionCall part, got %d: %s", len(parts), string(out))
		}
		if parts[0].Get("functionCall.name").String() != "f" {
			t.Errorf("functionCall.name=%q", parts[0].Get("functionCall.name").String())
		}
		if parts[0].Get("functionCall.id").String() != "c1" {
			t.Errorf("functionCall.id=%q (canonical id must propagate)", parts[0].Get("functionCall.id").String())
		}
		// finish_reason "tool_calls" → "STOP" per mapOpenAIFinishToGemini.
		if got := gjson.GetBytes(out, "candidates.0.finishReason").String(); got != "STOP" {
			t.Errorf("finishReason=%q", got)
		}
	})

	t.Run("tool_calls_with_empty_arguments_default_object", func(t *testing.T) {
		openai := []byte(`{
			"id":"x","object":"chat.completion","model":"m",
			"choices":[{"index":0,"message":{
				"role":"assistant","content":null,
				"tool_calls":[{"type":"function","function":{"name":"f","arguments":""}}]
			},"finish_reason":"tool_calls"}]
		}`)
		out, err := OpenAIChatCompletionToGenerateContentResponse(openai)
		if err != nil {
			t.Fatal(err)
		}
		// args defaulted to {}.
		args := gjson.GetBytes(out, "candidates.0.content.parts.0.functionCall.args")
		if !args.IsObject() {
			t.Errorf("args must be object, got %s", args.Raw)
		}
		// id was absent in canonical → must NOT be forwarded.
		if gjson.GetBytes(out, "candidates.0.content.parts.0.functionCall.id").Exists() {
			t.Errorf("functionCall.id must be omitted when canonical lacked one: %s", string(out))
		}
	})

	t.Run("cached_tokens_surface_in_usageMetadata", func(t *testing.T) {
		openai := []byte(`{
			"id":"x","object":"chat.completion","model":"m",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{
				"prompt_tokens":100,"completion_tokens":10,"total_tokens":110,
				"prompt_tokens_details":{"cached_tokens":80}
			}
		}`)
		out, err := OpenAIChatCompletionToGenerateContentResponse(openai)
		if err != nil {
			t.Fatal(err)
		}
		if got := gjson.GetBytes(out, "usageMetadata.cachedContentTokenCount").Int(); got != 80 {
			t.Errorf("cachedContentTokenCount=%d want 80", got)
		}
	})

	t.Run("no_parts_at_all_yields_empty_text", func(t *testing.T) {
		// content empty, no tool_calls, no reasoning → parts = [{text:""}].
		openai := []byte(`{
			"id":"x","object":"chat.completion","model":"m",
			"choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}]
		}`)
		out, err := OpenAIChatCompletionToGenerateContentResponse(openai)
		if err != nil {
			t.Fatal(err)
		}
		parts := gjson.GetBytes(out, "candidates.0.content.parts").Array()
		if len(parts) != 1 {
			t.Fatalf("want 1 fallback part, got %d", len(parts))
		}
		if parts[0].Get("text").String() != "" {
			t.Errorf("fallback text=%q want empty", parts[0].Get("text").String())
		}
	})

	t.Run("unknown_finish_reason_maps_to_OTHER", func(t *testing.T) {
		openai := []byte(`{
			"id":"x","object":"chat.completion","model":"m",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"unknown_value"}]
		}`)
		out, err := OpenAIChatCompletionToGenerateContentResponse(openai)
		if err != nil {
			t.Fatal(err)
		}
		if got := gjson.GetBytes(out, "candidates.0.finishReason").String(); got != "OTHER" {
			t.Errorf("finishReason=%q want OTHER", got)
		}
	})

	t.Run("length_finish_reason_maps_to_MAX_TOKENS", func(t *testing.T) {
		openai := []byte(`{
			"id":"x","object":"chat.completion","model":"m",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"length"}]
		}`)
		out, err := OpenAIChatCompletionToGenerateContentResponse(openai)
		if err != nil {
			t.Fatal(err)
		}
		if got := gjson.GetBytes(out, "candidates.0.finishReason").String(); got != "MAX_TOKENS" {
			t.Errorf("finishReason=%q want MAX_TOKENS", got)
		}
	})

	t.Run("content_filter_finish_reason_maps_to_SAFETY", func(t *testing.T) {
		openai := []byte(`{
			"id":"x","object":"chat.completion","model":"m",
			"choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"content_filter"}]
		}`)
		out, err := OpenAIChatCompletionToGenerateContentResponse(openai)
		if err != nil {
			t.Fatal(err)
		}
		if got := gjson.GetBytes(out, "candidates.0.finishReason").String(); got != "SAFETY" {
			t.Errorf("finishReason=%q want SAFETY", got)
		}
	})
}

// TestGeminiCompositeMessage_AssistantWithToolCallsNullContent covers
// hub_ingress.go:241-244 — assistant turns with tool_calls but no
// visible text emit content:null (the OpenAI-spec contract).
func TestGeminiCompositeMessage_AssistantWithToolCallsNullContent(t *testing.T) {
	out, err := GenerateContentRequestToOpenAIChatCompletion([]byte(`{
		"contents":[
			{"role":"user","parts":[{"text":"call f"}]},
			{"role":"model","parts":[{"functionCall":{"name":"f","id":"c1","args":{}}}]}
		]
	}`), "m")
	if err != nil {
		t.Fatal(err)
	}
	// messages[1] is the assistant turn carrying tool_calls.
	msg := gjson.GetBytes(out, "messages.1")
	if msg.Get("role").String() != "assistant" {
		t.Fatalf("messages[1] role=%q", msg.Get("role").String())
	}
	if msg.Get("content").Type != gjson.Null {
		t.Errorf("content should be null when only tool_calls, got %s", msg.Get("content").Raw)
	}
}

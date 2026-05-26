package gemini

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

// TestMapFinishReason_AllEnums covers SDD T-GEM-FINISH: every Gemini-side
// FinishReason value documented today must collapse to one of the five
// canonical OpenAI finish_reason strings; unknown values pass through.
func TestMapFinishReason_AllEnums(t *testing.T) {
	cases := []struct {
		gemini    string
		canonical string
	}{
		{"STOP", "stop"},
		{"MAX_TOKENS", "length"},
		{"SAFETY", "content_filter"},
		{"RECITATION", "content_filter"},
		{"LANGUAGE", "content_filter"},
		{"PROHIBITED_CONTENT", "content_filter"},
		{"SPII", "content_filter"},
		{"BLOCKLIST", "content_filter"},
		{"IMAGE_SAFETY", "content_filter"},
		{"MODEL_ARMOR", "content_filter"},
		{"MALFORMED_FUNCTION_CALL", "tool_calls"},
		{"UNEXPECTED_TOOL_CALL", "tool_calls"},
		{"OTHER", "stop"},
		{"", "stop"},
		{"FUTURE_VENDOR_VALUE", "FUTURE_VENDOR_VALUE"},
	}
	for _, tc := range cases {
		got := mapFinishReason(tc.gemini)
		if got != tc.canonical {
			t.Errorf("mapFinishReason(%q)=%q want %q", tc.gemini, got, tc.canonical)
		}
	}
}

// TestRoundTrip_functionCall_GeminiNative covers SDD T-GEM-TOOLS: a four-turn
// Gemini function-calling conversation (user → model functionCall → user
// functionResponse → model text) must round-trip through canonical OpenAI
// without losing tool definitions, function-call args, or function-response
// content.
func TestRoundTrip_functionCall_GeminiNative(t *testing.T) {
	native, err := os.ReadFile(filepath.Join("..", "..", "..", "execution", "canonicalbridge", "testdata", "gemini_chat_functioncall.native.json"))
	if err != nil {
		t.Fatal(err)
	}
	canon, err := GenerateContentRequestToOpenAIChatCompletion(native, "gemini-1.5-flash")
	if err != nil {
		t.Fatalf("hub_ingress: %v", err)
	}

	if got := gjson.GetBytes(canon, "messages.0.role").String(); got != "system" {
		t.Errorf("system instruction lost: messages.0.role=%q", got)
	}
	if got := gjson.GetBytes(canon, "messages.2.tool_calls.0.function.name").String(); got != "get_weather" {
		t.Errorf("functionCall lost: tool_calls[0].name=%q", got)
	}
	if args := gjson.GetBytes(canon, "messages.2.tool_calls.0.function.arguments").String(); !strings.Contains(args, "San Francisco") {
		t.Errorf("functionCall args lost: %q", args)
	}
	if got := gjson.GetBytes(canon, "messages.3.role").String(); got != "tool" {
		t.Errorf("functionResponse must canonicalise to role=tool, got %q", got)
	}
	if content := gjson.GetBytes(canon, "messages.3.content").String(); !strings.Contains(content, "62") {
		t.Errorf("functionResponse content lost: %q", content)
	}

	encRes, err := codec{}.EncodeRequest(typology.WireShapeGeminiGenerateContent, canon, provcore.CallTarget{})
	wire := encRes.Body
	if err != nil {
		t.Fatalf("encode back: %v", err)
	}
	if got := gjson.GetBytes(wire, "systemInstruction.parts.0.text").String(); !strings.Contains(got, "weather assistant") {
		t.Errorf("system instruction lost on encode: %q", got)
	}
	if got := gjson.GetBytes(wire, "tools.0.functionDeclarations.0.name").String(); got != "get_weather" {
		t.Errorf("tools lost on encode: %q", got)
	}
	// Locate the model turn that carries the functionCall part — re-encoding
	// emits roles user/model/user/model. The second `model` turn is the
	// final text answer; the functionCall is on the first `model` turn.
	contents := gjson.GetBytes(wire, "contents").Array()
	if len(contents) < 4 {
		t.Fatalf("expected 4 contents turns, got %d: %s", len(contents), string(wire))
	}
	if got := contents[1].Get("parts.0.functionCall.name").String(); got != "get_weather" {
		t.Errorf("functionCall lost on encode: %q (turn=%s)", got, contents[1].Raw)
	}
	if got := contents[2].Get("parts.0.functionResponse.name").String(); got != "get_weather" {
		t.Errorf("functionResponse lost on encode: %q (turn=%s)", got, contents[2].Raw)
	}
}

// TestStreamDecoder_FunctionCallBufferedAcrossFrames covers SDD T-GEM-TOOLS
// streaming acceptance: a function-call argument that arrives across three
// content frames must surface as ToolCallDeltas with progressive args; the
// final usage trailer is delivered with Done=true (matching T-GEM-STREAM).
func TestStreamDecoder_FunctionCallBufferedAcrossFrames(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "functioncall_stream.sse"))
	if err != nil {
		t.Fatal(err)
	}
	dec := NewStreamDecoder(nil)
	sess, err := dec.Open(io.NopCloser(strings.NewReader(string(raw))), typology.WireShapeGeminiGenerateContent)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close() //nolint:errcheck

	var deltaArgs []string
	var doneCount int
	var finalUsage *provcore.Usage
	for {
		ch, err := sess.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		for _, d := range ch.ToolCallDeltas {
			if d.Name == "get_weather" {
				deltaArgs = append(deltaArgs, d.Arguments)
			}
		}
		if ch.Done {
			doneCount++
		}
		if ch.Usage != nil {
			finalUsage = ch.Usage
		}
	}
	if len(deltaArgs) < 3 {
		t.Errorf("expected at least 3 args fragments, got %d: %#v", len(deltaArgs), deltaArgs)
	}
	last := deltaArgs[len(deltaArgs)-1]
	if !strings.Contains(last, "San Francisco") {
		t.Errorf("final args fragment missing location: %q", last)
	}
	if doneCount != 1 {
		t.Errorf("expected exactly one Done frame, got %d", doneCount)
	}
	if finalUsage == nil || finalUsage.PromptTokens == nil || *finalUsage.PromptTokens != 12 {
		t.Errorf("final usage missing or wrong: %+v", finalUsage)
	}
}

// TestDecodeResponse_ContextCacheUsage covers SDD T-USAGE-EXT for Gemini:
// cachedContentTokenCount and thoughtsTokenCount land on both the
// canonical body (prompt_tokens_details / completion_tokens_details) AND
// the typed Usage envelope (CacheReadTokens / ReasoningTokens) so analytics
// see the cache + reasoning split without re-parsing the body.
func TestDecodeResponse_ContextCacheUsage(t *testing.T) {
	native, err := os.ReadFile(filepath.Join("testdata", "context_cache_response.json"))
	if err != nil {
		t.Fatal(err)
	}
	decRes, err := codec{}.DecodeResponse(typology.WireShapeGeminiGenerateContent, native, "")
	canon := decRes.CanonicalBody
	usage := decRes.Usage
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(canon, "usage.prompt_tokens_details.cached_tokens").Int(); got != 768 {
		t.Errorf("cached_tokens=%d want 768", got)
	}
	if got := gjson.GetBytes(canon, "usage.completion_tokens_details.reasoning_tokens").Int(); got != 4 {
		t.Errorf("reasoning_tokens=%d want 4", got)
	}
	if usage.CacheReadTokens == nil || *usage.CacheReadTokens != 768 {
		t.Errorf("typed Usage.CacheReadTokens=%v want 768", usage.CacheReadTokens)
	}
	if usage.ReasoningTokens == nil || *usage.ReasoningTokens != 4 {
		t.Errorf("typed Usage.ReasoningTokens=%v want 4", usage.ReasoningTokens)
	}
}

// TestDecodeResponse_ThoughtPartsBecomeReasoningContent covers the case:
// when Gemini 2.5+ emits candidate parts tagged thought=true (as it does
// when generationConfig.thinkingConfig.includeThoughts is enabled), the
// codec must route them to choices[0].message.reasoning_content rather
// than concatenating them into the visible content string. The
// downstream openai_chat normalizer picks reasoning_content up as a
// canonical ContentReasoning block, so audit and OpenAI-spec clients
// see thinking text separately from the visible answer.
func TestDecodeResponse_ThoughtPartsBecomeReasoningContent(t *testing.T) {
	cases := []struct {
		name             string
		native           string
		wantContent      string
		wantReasoning    string
		wantReasoningKey bool // whether reasoning_content key must be present
	}{
		{
			name: "interleaved_thought_and_text",
			native: `{
				"candidates":[{"content":{"parts":[
					{"text":"Let me think about this. ","thought":true},
					{"text":"The answer is 42."},
					{"text":"Actually, ","thought":true},
					{"text":" everything is fine."}
				]},"finishReason":"STOP","index":0}],
				"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"thoughtsTokenCount":8,"totalTokenCount":23}
			}`,
			wantContent:      "The answer is 42. everything is fine.",
			wantReasoning:    "Let me think about this. Actually, ",
			wantReasoningKey: true,
		},
		{
			name: "thought_only_no_visible_text",
			native: `{
				"candidates":[{"content":{"parts":[
					{"text":"Only thinking, no visible output.","thought":true}
				]},"finishReason":"MAX_TOKENS","index":0}],
				"usageMetadata":{"promptTokenCount":5,"thoughtsTokenCount":7,"totalTokenCount":12}
			}`,
			wantContent:      "",
			wantReasoning:    "Only thinking, no visible output.",
			wantReasoningKey: true,
		},
		{
			name: "no_thought_today_behavior_unchanged",
			native: `{
				"candidates":[{"content":{"parts":[
					{"text":"Plain answer."}
				]},"finishReason":"STOP","index":0}],
				"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}
			}`,
			wantContent:      "Plain answer.",
			wantReasoning:    "",
			wantReasoningKey: false,
		},
		{
			name: "thought_false_treated_as_visible_text",
			native: `{
				"candidates":[{"content":{"parts":[
					{"text":"Visible.","thought":false}
				]},"finishReason":"STOP","index":0}],
				"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1,"totalTokenCount":4}
			}`,
			wantContent:      "Visible.",
			wantReasoning:    "",
			wantReasoningKey: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decRes, err := codec{}.DecodeResponse(typology.WireShapeGeminiGenerateContent, []byte(tc.native), "")
			canon := decRes.CanonicalBody
			if err != nil {
				t.Fatalf("DecodeResponse: %v", err)
			}
			gotContent := gjson.GetBytes(canon, "choices.0.message.content").String()
			if gotContent != tc.wantContent {
				t.Errorf("content = %q, want %q", gotContent, tc.wantContent)
			}
			reasoningResult := gjson.GetBytes(canon, "choices.0.message.reasoning_content")
			if tc.wantReasoningKey {
				if !reasoningResult.Exists() {
					t.Errorf("reasoning_content key missing; want %q", tc.wantReasoning)
				} else if got := reasoningResult.String(); got != tc.wantReasoning {
					t.Errorf("reasoning_content = %q, want %q", got, tc.wantReasoning)
				}
			} else if reasoningResult.Exists() {
				t.Errorf("reasoning_content should not be present, got %q", reasoningResult.String())
			}
		})
	}
}

// TestEncodeRequest_FunctionCallIDOnlyWhenPresent pins the audit fix:
// functionCall.id must NOT be synthesized on encode for Gemini 1.5/2.x
// (which reject the field as unknown). Only when canonical actually
// supplied tool_calls[].id do we forward it.
func TestEncodeRequest_FunctionCallIDOnlyWhenPresent(t *testing.T) {
	t.Run("canonical_id_present_forwarded", func(t *testing.T) {
		canon := []byte(`{
			"messages":[
				{"role":"user","content":"hi"},
				{"role":"assistant","content":null,"tool_calls":[
					{"id":"call_xyz","type":"function","function":{"name":"f","arguments":"{}"}}
				]}
			]
		}`)
		encRes, err := codec{}.EncodeRequest(typology.WireShapeGeminiGenerateContent, canon, provcore.CallTarget{})
		out := encRes.Body
		if err != nil {
			t.Fatal(err)
		}
		// contents[1] is the model turn carrying the functionCall part.
		if got := gjson.GetBytes(out, "contents.1.parts.0.functionCall.id").String(); got != "call_xyz" {
			t.Errorf("functionCall.id=%q want call_xyz", got)
		}
	})

	t.Run("canonical_id_absent_omitted", func(t *testing.T) {
		canon := []byte(`{
			"messages":[
				{"role":"user","content":"hi"},
				{"role":"assistant","content":null,"tool_calls":[
					{"type":"function","function":{"name":"f","arguments":"{}"}}
				]}
			]
		}`)
		encRes, err := codec{}.EncodeRequest(typology.WireShapeGeminiGenerateContent, canon, provcore.CallTarget{})
		out := encRes.Body
		if err != nil {
			t.Fatal(err)
		}
		if gjson.GetBytes(out, "contents.1.parts.0.functionCall.id").Exists() {
			t.Errorf("functionCall.id must NOT be synthesized for older Gemini models: %s", string(out))
		}
		// name + args still present
		if got := gjson.GetBytes(out, "contents.1.parts.0.functionCall.name").String(); got != "f" {
			t.Errorf("functionCall.name=%q", got)
		}
	})
}

// TestEncodeRequest_FunctionResponseIDAndShape pins three audit fixes:
// (1) canonical role:"tool" tool_call_id propagates to functionResponse.id
//
//	so Gemini 3 multi-tool turns can match call↔response;
//
// (2) JSON-object content forwards as the documented Gemini shape (object,
//
//	not bare value);
//
// (3) Non-object content (plain string, scalar) is wrapped as
//
//	{"result": <value>} so the schema is always object-typed.
func TestEncodeRequest_FunctionResponseIDAndShape(t *testing.T) {
	t.Run("json_object_content_forwarded_as_object", func(t *testing.T) {
		canon := []byte(`{
			"messages":[
				{"role":"user","content":"hi"},
				{"role":"assistant","content":null,"tool_calls":[
					{"id":"call_xyz","type":"function","function":{"name":"get_weather","arguments":"{}"}}
				]},
				{"role":"tool","tool_call_id":"call_xyz","content":"{\"temp\":62}"}
			]
		}`)
		encRes, err := codec{}.EncodeRequest(typology.WireShapeGeminiGenerateContent, canon, provcore.CallTarget{})
		out := encRes.Body
		if err != nil {
			t.Fatal(err)
		}
		if got := gjson.GetBytes(out, "contents.2.parts.0.functionResponse.id").String(); got != "call_xyz" {
			t.Errorf("functionResponse.id=%q want call_xyz: %s", got, string(out))
		}
		if got := gjson.GetBytes(out, "contents.2.parts.0.functionResponse.name").String(); got != "get_weather" {
			t.Errorf("functionResponse.name=%q want get_weather", got)
		}
		resp := gjson.GetBytes(out, "contents.2.parts.0.functionResponse.response")
		if !resp.IsObject() {
			t.Fatalf("response must be object, got: %s", resp.Raw)
		}
		if got := resp.Get("temp").Int(); got != 62 {
			t.Errorf("response.temp=%d want 62 (round-trip should preserve original object)", got)
		}
		if resp.Get("result").Exists() {
			t.Errorf("must not wrap a JSON-object content with result: %s", resp.Raw)
		}
	})

	t.Run("plain_string_content_wrapped_as_result", func(t *testing.T) {
		canon := []byte(`{
			"messages":[
				{"role":"user","content":"hi"},
				{"role":"assistant","content":null,"tool_calls":[
					{"id":"call_xyz","type":"function","function":{"name":"echo","arguments":"{}"}}
				]},
				{"role":"tool","tool_call_id":"call_xyz","content":"plain text result"}
			]
		}`)
		encRes, err := codec{}.EncodeRequest(typology.WireShapeGeminiGenerateContent, canon, provcore.CallTarget{})
		out := encRes.Body
		if err != nil {
			t.Fatal(err)
		}
		resp := gjson.GetBytes(out, "contents.2.parts.0.functionResponse.response")
		if !resp.IsObject() {
			t.Fatalf("plain-string content must be wrapped as object: %s", resp.Raw)
		}
		if got := resp.Get("result").String(); got != "plain text result" {
			t.Errorf("response.result=%q want %q", got, "plain text result")
		}
	})

	t.Run("missing_tool_call_id_omits_id_field", func(t *testing.T) {
		canon := []byte(`{
			"messages":[
				{"role":"user","content":"hi"},
				{"role":"tool","tool_call_id":"","content":"x"}
			]
		}`)
		encRes, err := codec{}.EncodeRequest(typology.WireShapeGeminiGenerateContent, canon, provcore.CallTarget{})
		out := encRes.Body
		if err != nil {
			t.Fatal(err)
		}
		if gjson.GetBytes(out, "contents.1.parts.0.functionResponse.id").Exists() {
			t.Errorf("functionResponse.id must be omitted when canonical lacks tool_call_id: %s", string(out))
		}
	})
}

// TestRoundTrip_imageURL_GeminiNative covers SDD T-GEM-MULTIMODAL: a Gemini
// native request carrying inlineData and fileData parts must canonicalise to
// OpenAI image_url shapes, and re-encoding canonical OpenAI image_url back
// to Gemini must produce the symmetric inlineData / fileData parts.
func TestRoundTrip_imageURL_GeminiNative(t *testing.T) {
	native := []byte(`{
		"contents":[{"role":"user","parts":[
			{"text":"describe"},
			{"inlineData":{"mimeType":"image/png","data":"aGVsbG8="}},
			{"fileData":{"mimeType":"image/jpeg","fileUri":"https://example.com/cat.jpg"}}
		]}],
		"generationConfig":{"maxOutputTokens":32}
	}`)
	canon, err := GenerateContentRequestToOpenAIChatCompletion(native, "gemini-1.5-pro")
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(canon, "messages.0.content.1.image_url.url").String(); got != "data:image/png;base64,aGVsbG8=" {
		t.Errorf("inlineData lost on canonical: %q", got)
	}
	if got := gjson.GetBytes(canon, "messages.0.content.2.image_url.url").String(); got != "https://example.com/cat.jpg" {
		t.Errorf("fileData URL lost on canonical: %q", got)
	}

	encRes, err := codec{}.EncodeRequest(typology.WireShapeGeminiGenerateContent, canon, provcore.CallTarget{})
	wire := encRes.Body
	if err != nil {
		t.Fatal(err)
	}
	parts := gjson.GetBytes(wire, "contents.0.parts").Array()
	if len(parts) != 3 {
		t.Fatalf("expected 3 parts, got %d: %s", len(parts), string(wire))
	}
	// Order is preserved: text → inlineData → fileData.
	if got := parts[1].Get("inlineData.mimeType").String(); got != "image/png" {
		t.Errorf("inlineData.mimeType=%q want image/png", got)
	}
	if got := parts[1].Get("inlineData.data").String(); got != "aGVsbG8=" {
		t.Errorf("inlineData.data=%q want aGVsbG8=", got)
	}
	if got := parts[2].Get("fileData.fileUri").String(); got != "https://example.com/cat.jpg" {
		t.Errorf("fileData.fileUri=%q", got)
	}
	if got := parts[2].Get("fileData.mimeType").String(); got != "image/jpeg" {
		t.Errorf("fileData.mimeType=%q (extension-derived)", got)
	}
}

// TestGuessMimeFromURL pins the extension table used to resolve fileData
// mimeType for non-data: image_url URLs.
func TestGuessMimeFromURL(t *testing.T) {
	cases := []struct {
		url, want string
	}{
		{"https://example.com/a.png", "image/png"},
		{"https://example.com/a.JPG", "image/jpeg"},
		{"https://example.com/a.jpeg?token=x", "image/jpeg"},
		{"https://example.com/a.webp#frag", "image/webp"},
		{"https://example.com/a.gif", "image/gif"},
		{"https://example.com/a.heic", "image/heic"},
		{"https://example.com/a.heif", "image/heif"},
		{"https://example.com/unknown", "image/jpeg"},
	}
	for _, tc := range cases {
		if got := guessMimeFromURL(tc.url); got != tc.want {
			t.Errorf("guessMimeFromURL(%q)=%q want %q", tc.url, got, tc.want)
		}
	}
}

// TestCodec_EncodeRequest_imageURLDetailHigh asserts SDD §2.5 hard rule for
// Gemini: when an OpenAI canonical request points image_url.detail="high"
// at a Gemini target, the codec returns a structured ProviderError instead
// of dropping the field silently.
func TestCodec_EncodeRequest_imageURLDetailHigh(t *testing.T) {
	canon := []byte(`{
		"model":"gemini-1.5-pro",
		"messages":[{"role":"user","content":[
			{"type":"image_url","image_url":{"url":"https://example.com/x.png","detail":"high"}}
		]}]
	}`)
	_, err := codec{}.EncodeRequest(typology.WireShapeGeminiGenerateContent, canon, provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected error for detail=high")
	}
	var pe *provcore.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *provcore.ProviderError: %T %v", err, err)
	}
	if pe.Type != "nexus_field_unsupported" {
		t.Errorf("type=%q", pe.Type)
	}
}

// TestCodec_EncodeRequest_thinkingConfigPassthrough: an
// OpenAI-spec request carrying nexus.ext.gemini.thinking_config has that
// object merged into outgoing generationConfig.thinkingConfig, preserving
// sibling keys (temperature, responseMimeType) that the codec sets
// independently. Malformed extensions are dropped silently.
func TestCodec_EncodeRequest_thinkingConfigPassthrough(t *testing.T) {
	cases := []struct {
		name                    string
		canon                   string
		wantThinkingExists      bool
		wantIncludeThoughts     bool
		wantPreserveTemperature bool
		wantTemperatureValue    float64
	}{
		{
			name: "valid_object_merged_into_generationConfig",
			canon: `{
				"model": "gemini-2.5-pro",
				"messages": [{"role": "user", "content": "Reason carefully."}],
				"nexus": {"ext": {"gemini": {"thinking_config": {"include_thoughts": true}}}}
			}`,
			wantThinkingExists:  true,
			wantIncludeThoughts: true,
		},
		{
			name: "preserves_existing_temperature",
			canon: `{
				"model": "gemini-2.5-pro",
				"temperature": 0.7,
				"messages": [{"role": "user", "content": "Reason carefully."}],
				"nexus": {"ext": {"gemini": {"thinking_config": {"include_thoughts": true, "thinking_budget": 4096}}}}
			}`,
			wantThinkingExists:      true,
			wantIncludeThoughts:     true,
			wantPreserveTemperature: true,
			wantTemperatureValue:    0.7,
		},
		{
			name: "no_extension_no_thinkingConfig",
			canon: `{
				"model": "gemini-2.5-pro",
				"messages": [{"role": "user", "content": "Plain ask."}]
			}`,
			wantThinkingExists: false,
		},
		{
			name: "malformed_string_dropped",
			canon: `{
				"model": "gemini-2.5-pro",
				"messages": [{"role": "user", "content": "Plain ask."}],
				"nexus": {"ext": {"gemini": {"thinking_config": "not-an-object"}}}
			}`,
			wantThinkingExists: false,
		},
		{
			name: "empty_object_dropped",
			canon: `{
				"model": "gemini-2.5-pro",
				"messages": [{"role": "user", "content": "Plain ask."}],
				"nexus": {"ext": {"gemini": {"thinking_config": {}}}}
			}`,
			wantThinkingExists: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encRes, err := codec{}.EncodeRequest(typology.WireShapeGeminiGenerateContent, []byte(tc.canon), provcore.CallTarget{})
			out := encRes.Body
			if err != nil {
				t.Fatalf("EncodeRequest: %v", err)
			}
			thinking := gjson.GetBytes(out, "generationConfig.thinkingConfig")
			if tc.wantThinkingExists {
				if !thinking.Exists() {
					t.Fatalf("generationConfig.thinkingConfig missing; body=%s", string(out))
				}
				if gjson.GetBytes(out, "generationConfig.thinkingConfig.include_thoughts").Bool() != tc.wantIncludeThoughts {
					t.Errorf("include_thoughts mismatch; body=%s", string(out))
				}
			} else if thinking.Exists() {
				t.Errorf("generationConfig.thinkingConfig should not be present, got %s", thinking.Raw)
			}
			if tc.wantPreserveTemperature {
				got := gjson.GetBytes(out, "generationConfig.temperature").Float()
				if got != tc.wantTemperatureValue {
					t.Errorf("generationConfig.temperature = %v, want %v; body=%s", got, tc.wantTemperatureValue, string(out))
				}
			}
		})
	}
}

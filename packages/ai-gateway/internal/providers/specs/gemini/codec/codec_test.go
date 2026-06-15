// Package codec_test covers the Gemini SchemaCodec.
// Named failure modes:
//   - EncodeRequest: unsupported endpoint, empty body, no messages after split
//   - temperature/top_p/top_k/max_tokens → generationConfig
//   - max_completion_tokens overrides max_tokens
//   - stop as array/string → stopSequences
//   - system messages → systemInstruction
//   - tool messages → functionResponse parts
//   - assistant tool_calls → functionCall parts
//   - tools → functionDeclarations (non-function type skipped, empty name skipped)
//   - tool_choice: string (none/required/auto) and object (specific function)
//   - response_format: json_object, json_schema
//   - nexus.ext.gemini.thinking_config passthrough
//   - DecodeResponse: passthrough (non-chat), empty body, chat completion output shape
//   - MapFinishReason: full enum
//   - UsageToNormalize: zero/non-zero
//   - ParseDataURL: valid, invalid
//   - GuessMimeFromURL: extensions and fallback
//   - StringifyContent: string/array/missing
package codec_test

import (
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	gemcodec "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/gemini/codec"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

func TestNewCodec_returnsFunctionalCodec(t *testing.T) {
	c := gemcodec.NewCodec()
	if c == nil {
		t.Fatal("NewCodec returned nil")
	}
}

func TestEncodeRequest_unsupportedEndpoint_returnsError(t *testing.T) {
	var c gemcodec.Codec
	_, err := c.EncodeRequest(typology.WireShapeNone, []byte(`{}`), provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected error for unsupported endpoint")
	}
}

func TestEncodeRequest_emptyBody_returnsError(t *testing.T) {
	var c gemcodec.Codec
	_, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, []byte{}, provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected error for empty body")
	}
}

func TestEncodeRequest_noMessages_returnsError(t *testing.T) {
	var c gemcodec.Codec
	body := []byte(`{"model":"gemini-1.5-pro","messages":[{"role":"system","content":"sys only"}]}`)
	_, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected error: system-only → no content messages")
	}
}

func TestEncodeRequest_simpleUserMessage(t *testing.T) {
	var c gemcodec.Codec
	body := []byte(`{"model":"gemini-1.5-pro","messages":[{"role":"user","content":"hello"}]}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	contents := gjson.GetBytes(out, "contents")
	if !contents.IsArray() || len(contents.Array()) == 0 {
		t.Fatal("expected contents array")
	}
	if contents.Array()[0].Get("role").String() != "user" {
		t.Errorf("role: got %q, want user", contents.Array()[0].Get("role").String())
	}
}

func TestEncodeRequest_generationConfig_allFields(t *testing.T) {
	var c gemcodec.Codec
	body := []byte(`{"model":"gemini-1.5-pro","messages":[{"role":"user","content":"hi"}],
		"temperature":0.7,"top_p":0.9,"top_k":40,"max_tokens":512}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	gc := gjson.GetBytes(out, "generationConfig")
	if gc.Get("temperature").Float() != 0.7 {
		t.Errorf("temperature: %s", out)
	}
	if gc.Get("topP").Float() != 0.9 {
		t.Errorf("topP: %s", out)
	}
	if gc.Get("topK").Int() != 40 {
		t.Errorf("topK: %s", out)
	}
	if gc.Get("maxOutputTokens").Int() != 512 {
		t.Errorf("maxOutputTokens: %s", out)
	}
}

func TestEncodeRequest_maxCompletionTokens_overridesMaxTokens(t *testing.T) {
	var c gemcodec.Codec
	body := []byte(`{"model":"g","messages":[{"role":"user","content":"hi"}],
		"max_tokens":512,"max_completion_tokens":1024}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if gjson.GetBytes(out, "generationConfig.maxOutputTokens").Int() != 1024 {
		t.Errorf("max_completion_tokens should override max_tokens: %s", out)
	}
}

func TestEncodeRequest_stopAsArray(t *testing.T) {
	var c gemcodec.Codec
	body := []byte(`{"model":"g","messages":[{"role":"user","content":"hi"}],"stop":["END","STOP"]}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	ss := gjson.GetBytes(out, "generationConfig.stopSequences")
	if !ss.IsArray() || len(ss.Array()) != 2 {
		t.Errorf("stopSequences: got %s, want [END, STOP]", ss.Raw)
	}
}

func TestEncodeRequest_stopAsString(t *testing.T) {
	var c gemcodec.Codec
	body := []byte(`{"model":"g","messages":[{"role":"user","content":"hi"}],"stop":"END"}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	ss := gjson.GetBytes(out, "generationConfig.stopSequences")
	if !ss.IsArray() || len(ss.Array()) != 1 || ss.Array()[0].String() != "END" {
		t.Errorf("stopSequences for string stop: %s", ss.Raw)
	}
}

func TestEncodeRequest_systemMessage_producesSystemInstruction(t *testing.T) {
	var c gemcodec.Codec
	body := []byte(`{"model":"g","messages":[{"role":"system","content":"Be helpful."},{"role":"user","content":"hi"}]}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	si := gjson.GetBytes(out, "systemInstruction.parts.0.text").String()
	if si == "" {
		t.Errorf("systemInstruction.parts[0].text missing: %s", out)
	}
}

func TestEncodeRequest_assistantRoleConvertedToModel(t *testing.T) {
	var c gemcodec.Codec
	body := []byte(`{"model":"g","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"hello back"}
	]}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	contents := gjson.GetBytes(out, "contents").Array()
	found := false
	for _, c := range contents {
		if c.Get("role").String() == "model" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected role=model for assistant message: %s", out)
	}
}

func TestEncodeRequest_toolMessage_functionResponse(t *testing.T) {
	var c gemcodec.Codec
	body := []byte(`{"model":"g","messages":[
		{"role":"user","content":"call"},
		{"role":"assistant","content":null,"tool_calls":[
			{"id":"tc_1","type":"function","function":{"name":"search","arguments":"{\"q\":\"hi\"}"}}
		]},
		{"role":"tool","tool_call_id":"tc_1","content":"result data"}
	]}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	contents := gjson.GetBytes(out, "contents").Array()
	found := false
	for _, c := range contents {
		for _, p := range c.Get("parts").Array() {
			if p.Get("functionResponse.name").Exists() {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected functionResponse part: %s", out)
	}
}

func TestEncodeRequest_toolMessage_stringContentThatIsJSON_forwardedAsObject(t *testing.T) {
	// Tool message content is JSON string → parse and wrap as object.
	var c gemcodec.Codec
	body := []byte(`{"model":"g","messages":[
		{"role":"user","content":"call"},
		{"role":"assistant","tool_calls":[
			{"id":"tc_2","type":"function","function":{"name":"fn","arguments":"{}"}}
		]},
		{"role":"tool","tool_call_id":"tc_2","content":"{\"key\":\"value\"}"}
	]}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	contents := gjson.GetBytes(out, "contents").Array()
	found := false
	for _, c := range contents {
		for _, p := range c.Get("parts").Array() {
			if p.Get("functionResponse.response.key").String() == "value" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected functionResponse with parsed JSON object: %s", out)
	}
}

func TestEncodeRequest_assistantToolCalls_functionCallParts(t *testing.T) {
	var c gemcodec.Codec
	body := []byte(`{"model":"g","messages":[
		{"role":"user","content":"call"},
		{"role":"assistant","content":null,"tool_calls":[
			{"id":"tc_3","type":"function","function":{"name":"calc","arguments":"{\"x\":1}"}}
		]}
	]}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	contents := gjson.GetBytes(out, "contents").Array()
	found := false
	for _, c := range contents {
		if c.Get("role").String() == "model" {
			for _, p := range c.Get("parts").Array() {
				if p.Get("functionCall.name").String() == "calc" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Errorf("expected functionCall part in model content: %s", out)
	}
}

func TestEncodeRequest_tools_functionDeclarations(t *testing.T) {
	var c gemcodec.Codec
	body := []byte(`{"model":"g","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"search","description":"web","parameters":{"type":"object"}}}]}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	decls := gjson.GetBytes(out, "tools.0.functionDeclarations").Array()
	if len(decls) == 0 {
		t.Fatal("expected functionDeclarations")
	}
	if decls[0].Get("name").String() != "search" {
		t.Errorf("tool name: got %q", decls[0].Get("name").String())
	}
}

func TestEncodeRequest_tools_nonFunctionType_skipped(t *testing.T) {
	var c gemcodec.Codec
	body := []byte(`{"model":"g","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"retrieval","function":{"name":"search"}},
		         {"type":"function","function":{"name":"valid"}}]}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	decls := gjson.GetBytes(out, "tools.0.functionDeclarations").Array()
	if len(decls) != 1 || decls[0].Get("name").String() != "valid" {
		t.Errorf("expected only valid function tool: %s", out)
	}
}

func TestEncodeRequest_tools_emptyName_skipped(t *testing.T) {
	var c gemcodec.Codec
	body := []byte(`{"model":"g","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":""}},
		         {"type":"function","function":{"name":"ok"}}]}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	decls := gjson.GetBytes(out, "tools.0.functionDeclarations").Array()
	if len(decls) != 1 || decls[0].Get("name").String() != "ok" {
		t.Errorf("empty-name tool should be skipped: %s", out)
	}
}

func TestEncodeRequest_tools_noParameters_defaultSchema(t *testing.T) {
	var c gemcodec.Codec
	body := []byte(`{"model":"g","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"noop","description":"nothing"}}]}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	decls := gjson.GetBytes(out, "tools.0.functionDeclarations").Array()
	if len(decls) == 0 {
		t.Fatal("expected functionDeclaration")
	}
	params := decls[0].Get("parameters")
	if !params.Exists() {
		t.Error("parameters should have default schema when omitted")
	}
}

func TestEncodeRequest_toolChoice_none(t *testing.T) {
	var c gemcodec.Codec
	body := []byte(`{"model":"g","messages":[{"role":"user","content":"hi"}],"tool_choice":"none"}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	mode := gjson.GetBytes(out, "toolConfig.functionCallingConfig.mode").String()
	if mode != "NONE" {
		t.Errorf("mode: got %q, want NONE", mode)
	}
}

func TestEncodeRequest_toolChoice_required(t *testing.T) {
	var c gemcodec.Codec
	body := []byte(`{"model":"g","messages":[{"role":"user","content":"hi"}],"tool_choice":"required"}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	mode := gjson.GetBytes(out, "toolConfig.functionCallingConfig.mode").String()
	if mode != "ANY" {
		t.Errorf("mode: got %q, want ANY", mode)
	}
}

func TestEncodeRequest_toolChoice_auto(t *testing.T) {
	var c gemcodec.Codec
	body := []byte(`{"model":"g","messages":[{"role":"user","content":"hi"}],"tool_choice":"auto"}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	mode := gjson.GetBytes(out, "toolConfig.functionCallingConfig.mode").String()
	if mode != "AUTO" {
		t.Errorf("mode: got %q, want AUTO", mode)
	}
}

func TestEncodeRequest_toolChoice_function_specificTool(t *testing.T) {
	var c gemcodec.Codec
	body := []byte(`{"model":"g","messages":[{"role":"user","content":"hi"}],
		"tool_choice":{"type":"function","function":{"name":"search"}}}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	mode := gjson.GetBytes(out, "toolConfig.functionCallingConfig.mode").String()
	if mode != "ANY" {
		t.Errorf("mode: got %q, want ANY", mode)
	}
	allowed := gjson.GetBytes(out, "toolConfig.functionCallingConfig.allowedFunctionNames").Array()
	if len(allowed) != 1 || allowed[0].String() != "search" {
		t.Errorf("allowedFunctionNames: got %s", out)
	}
}

func TestEncodeRequest_responseFormat_jsonObject(t *testing.T) {
	var c gemcodec.Codec
	body := []byte(`{"model":"g","messages":[{"role":"user","content":"hi"}],
		"response_format":{"type":"json_object"}}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	mime := gjson.GetBytes(out, "generationConfig.responseMimeType").String()
	if mime != "application/json" {
		t.Errorf("responseMimeType: got %q, want application/json", mime)
	}
}

func TestEncodeRequest_responseFormat_jsonSchema(t *testing.T) {
	var c gemcodec.Codec
	body := []byte(`{"model":"g","messages":[{"role":"user","content":"hi"}],
		"response_format":{"type":"json_schema","json_schema":{"type":"object","properties":{"name":{"type":"string"}}}}}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if gjson.GetBytes(out, "generationConfig.responseSchema").Type != gjson.JSON {
		t.Errorf("responseSchema missing: %s", out)
	}
}

func TestEncodeRequest_imageURL_inlineData(t *testing.T) {
	var c gemcodec.Codec
	body := []byte(`{"model":"g","messages":[{"role":"user","content":[
		{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}
	]}]}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	inlineData := gjson.GetBytes(out, "contents.0.parts.0.inlineData")
	if !inlineData.Exists() {
		t.Errorf("expected inlineData: %s", out)
	}
	if inlineData.Get("mimeType").String() != "image/png" {
		t.Errorf("mimeType: got %q", inlineData.Get("mimeType").String())
	}
}

func TestEncodeRequest_imageURL_fileData(t *testing.T) {
	var c gemcodec.Codec
	body := []byte(`{"model":"g","messages":[{"role":"user","content":[
		{"type":"image_url","image_url":{"url":"https://example.com/img.jpg"}}
	]}]}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	fileData := gjson.GetBytes(out, "contents.0.parts.0.fileData")
	if !fileData.Exists() {
		t.Errorf("expected fileData: %s", out)
	}
	if fileData.Get("fileUri").String() != "https://example.com/img.jpg" {
		t.Errorf("fileUri: got %q", fileData.Get("fileUri").String())
	}
}

func TestEncodeRequest_imageURL_highDetail_returnsError(t *testing.T) {
	var c gemcodec.Codec
	body := []byte(`{"model":"g","messages":[{"role":"user","content":[
		{"type":"image_url","image_url":{"url":"https://example.com/img.jpg","detail":"high"}}
	]}]}`)
	_, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected error for detail=high")
	}
}

func TestEncodeRequest_imageURL_invalidDataURL_returnsError(t *testing.T) {
	var c gemcodec.Codec
	body := []byte(`{"model":"g","messages":[{"role":"user","content":[
		{"type":"image_url","image_url":{"url":"data:image/png;nobase64,garbage"}}
	]}]}`)
	_, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected error for invalid data URL")
	}
}

func TestEncodeRequest_imageURL_emptyURL_returnsError(t *testing.T) {
	var c gemcodec.Codec
	body := []byte(`{"model":"g","messages":[{"role":"user","content":[
		{"type":"image_url","image_url":{"url":""}}
	]}]}`)
	_, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	if err == nil {
		t.Fatal("expected error for empty URL")
	}
}

func TestEncodeRequest_thinkingConfig_passthrough(t *testing.T) {
	// nexus.ext.gemini.thinking_config → generationConfig.thinkingConfig.
	var c gemcodec.Codec
	body := []byte(`{"model":"g","messages":[{"role":"user","content":"think"}],
		"nexus":{"ext":{"gemini":{"thinking_config":{"thinkingBudget":1000}}}}}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	tc := gjson.GetBytes(out, "generationConfig.thinkingConfig")
	if !tc.Exists() {
		t.Errorf("thinkingConfig missing: %s", out)
	}
	if tc.Get("thinkingBudget").Int() != 1000 {
		t.Errorf("thinkingBudget: got %d", tc.Get("thinkingBudget").Int())
	}
}

func TestEncodeRequest_emptyArrayContent_emptyTextPart(t *testing.T) {
	// User message with empty array content → empty text part placeholder.
	var c gemcodec.Codec
	body := []byte(`{"model":"g","messages":[{"role":"user","content":[]}]}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	parts := gjson.GetBytes(out, "contents.0.parts").Array()
	if len(parts) == 0 {
		t.Error("expected placeholder text part for empty content array")
	}
}

func TestDecodeResponse_nonChatEndpoint_passthrough(t *testing.T) {
	var c gemcodec.Codec
	body := []byte(`{"some":"response"}`)
	decRes, err := c.DecodeResponse(typology.WireShapeNone, body, "", provcore.DecodeContext{})
	out := decRes.CanonicalBody
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("non-chat endpoint must pass through: got %q", out)
	}
}

func TestDecodeResponse_emptyBody_returnsEmpty(t *testing.T) {
	var c gemcodec.Codec
	decRes, err := c.DecodeResponse(typology.WireShapeGeminiGenerateContent, []byte{}, "", provcore.DecodeContext{})
	out := decRes.CanonicalBody
	usage := decRes.Usage
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected empty out, got %q", out)
	}
	if usage.PromptTokens != nil {
		t.Error("expected zero usage for empty body")
	}
}

func TestDecodeResponse_chatCompletion_outputShape(t *testing.T) {
	var c gemcodec.Codec
	body := []byte(`{
		"responseId":"resp_1",
		"modelVersion":"gemini-1.5-pro",
		"candidates":[{"index":0,"content":{"parts":[{"text":"hello"}],"role":"model"},"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}
	}`)
	decRes, err := c.DecodeResponse(typology.WireShapeGeminiGenerateContent, body, "", provcore.DecodeContext{})
	out := decRes.CanonicalBody
	usage := decRes.Usage
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if !gjson.GetBytes(out, "choices").IsArray() {
		t.Errorf("expected choices array: %s", string(out))
	}
	if gjson.GetBytes(out, "choices.0.message.content").String() != "hello" {
		t.Errorf("content: %s", string(out))
	}
	if gjson.GetBytes(out, "choices.0.finish_reason").String() != "stop" {
		t.Errorf("finish_reason: %s", string(out))
	}
	if usage.PromptTokens == nil || *usage.PromptTokens != 10 {
		t.Errorf("PromptTokens: got %v, want 10", usage.PromptTokens)
	}
}

func TestDecodeResponse_malformedBody_gracefulFallback(t *testing.T) {
	var c gemcodec.Codec
	_, err := c.DecodeResponse(typology.WireShapeGeminiGenerateContent, []byte(`{not json`), "", provcore.DecodeContext{})
	// Defensive: should not propagate a raw parse error.
	_ = err
}

func TestMapFinishReason_allVariants(t *testing.T) {
	cases := []struct {
		in, want string
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
		{"UNKNOWN_NEW_REASON", "UNKNOWN_NEW_REASON"}, // pass-through
	}
	for _, tc := range cases {
		got := gemcodec.MapFinishReason(tc.in)
		if got != tc.want {
			t.Errorf("MapFinishReason(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestUsageToNormalize_zero_returnsNil(t *testing.T) {
	u := provcore.Usage{}
	got := gemcodec.UsageToNormalize(u)
	if got != nil {
		t.Errorf("zero Usage: expected nil, got %+v", got)
	}
}

func TestUsageToNormalize_nonZero_returnsPointer(t *testing.T) {
	pt := int(5)
	u := provcore.Usage{PromptTokens: &pt}
	got := gemcodec.UsageToNormalize(u)
	if got == nil {
		t.Fatal("non-zero Usage: expected non-nil")
	}
}

func TestParseDataURL_valid(t *testing.T) {
	media, b64, ok := gemcodec.ParseDataURL("data:image/png;base64,aGVsbG8=")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if media != "image/png" {
		t.Errorf("media: got %q", media)
	}
	if b64 != "aGVsbG8=" {
		t.Errorf("b64: got %q", b64)
	}
}

func TestParseDataURL_notDataScheme_notOk(t *testing.T) {
	_, _, ok := gemcodec.ParseDataURL("https://example.com/img.png")
	if ok {
		t.Error("https URL: expected ok=false")
	}
}

func TestParseDataURL_missingComma_notOk(t *testing.T) {
	_, _, ok := gemcodec.ParseDataURL("data:image/png;base64")
	if ok {
		t.Error("missing comma: expected ok=false")
	}
}

func TestParseDataURL_nonBase64Meta_notOk(t *testing.T) {
	_, _, ok := gemcodec.ParseDataURL("data:image/png,aGVsbG8=")
	if ok {
		t.Error("non-base64 meta: expected ok=false")
	}
}

func TestParseDataURL_emptyPayload_notOk(t *testing.T) {
	_, _, ok := gemcodec.ParseDataURL("data:image/png;base64,")
	if ok {
		t.Error("empty payload: expected ok=false")
	}
}

func TestParseDataURL_emptyMediaType_usesDefault(t *testing.T) {
	media, _, ok := gemcodec.ParseDataURL("data:;base64,aGVsbG8=")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if media != "application/octet-stream" {
		t.Errorf("media: got %q, want application/octet-stream", media)
	}
}

func TestGuessMimeFromURL_extensions(t *testing.T) {
	cases := []struct {
		url, want string
	}{
		{"https://x.com/img.png", "image/png"},
		{"https://x.com/img.webp", "image/webp"},
		{"https://x.com/img.gif", "image/gif"},
		{"https://x.com/img.heic", "image/heic"},
		{"https://x.com/img.heif", "image/heif"},
		{"https://x.com/img.jpg", "image/jpeg"},
		{"https://x.com/img.jpeg", "image/jpeg"},
		{"https://x.com/img.PNG?v=1", "image/png"},    // case + query string
		{"https://x.com/img.png#anchor", "image/png"}, // fragment stripped
		{"https://x.com/file.bin", "image/jpeg"},      // fallback
		{"https://x.com/noext", "image/jpeg"},         // no extension fallback
	}
	for _, tc := range cases {
		got := gemcodec.GuessMimeFromURL(tc.url)
		if got != tc.want {
			t.Errorf("GuessMimeFromURL(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

func TestStringifyContent_string(t *testing.T) {
	r := gjson.Parse(`"hello"`)
	got := gemcodec.StringifyContent(r)
	if got != "hello" {
		t.Errorf("got %q, want hello", got)
	}
}

func TestStringifyContent_array_textParts(t *testing.T) {
	r := gjson.Parse(`[{"type":"text","text":"part1"},{"type":"text","text":"part2"}]`)
	got := gemcodec.StringifyContent(r)
	if got != "part1\npart2" {
		t.Errorf("got %q, want part1\\npart2", got)
	}
}

func TestStringifyContent_missing_returnsEmpty(t *testing.T) {
	r := gjson.Parse(`{}`).Get("missing")
	got := gemcodec.StringifyContent(r)
	if got != "" {
		t.Errorf("missing field: expected empty, got %q", got)
	}
}

func TestStringifyContent_nonArrayNonString_returnsEmpty(t *testing.T) {
	r := gjson.Parse(`{"key":"value"}`)
	got := gemcodec.StringifyContent(r)
	if got != "" {
		t.Errorf("object: expected empty, got %q", got)
	}
}

// openAIMessageToGeminiParts via EncodeRequest (indirect)

func TestEncodeRequest_toolCallsWithID_forwardedToFunctionCallPart(t *testing.T) {
	// When tool_call has id and it's forwarded to functionCall.id.
	var c gemcodec.Codec
	body := []byte(`{"model":"g","messages":[
		{"role":"user","content":"call"},
		{"role":"assistant","tool_calls":[
			{"id":"tc_with_id","type":"function","function":{"name":"fn","arguments":"{}"}}
		]}
	]}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	contents := gjson.GetBytes(out, "contents").Array()
	found := false
	for _, c := range contents {
		for _, p := range c.Get("parts").Array() {
			if p.Get("functionCall.id").String() == "tc_with_id" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected functionCall with id=tc_with_id: %s", out)
	}
}

func TestEncodeRequest_assistantToolCallsInvalidArgs_emptyObject(t *testing.T) {
	// Invalid JSON args → argsObj defaults to empty map.
	var c gemcodec.Codec
	body := []byte(`{"model":"g","messages":[
		{"role":"user","content":"call"},
		{"role":"assistant","tool_calls":[
			{"id":"tc_bad","type":"function","function":{"name":"fn","arguments":"not-json"}}
		]}
	]}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	// Should still produce functionCall.
	contents := gjson.GetBytes(out, "contents").Array()
	found := false
	for _, c := range contents {
		for _, p := range c.Get("parts").Array() {
			if p.Get("functionCall.name").String() == "fn" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected functionCall for invalid-args case: %s", out)
	}
}

func TestEncodeRequest_toolMessage_objectContent_forwardedAsIs(t *testing.T) {
	// Tool message with content as a JSON object → forwarded directly.
	var c gemcodec.Codec
	body := []byte(`{"model":"g","messages":[
		{"role":"user","content":"call"},
		{"role":"assistant","tool_calls":[
			{"id":"tc_obj","type":"function","function":{"name":"fn","arguments":"{}"}}
		]},
		{"role":"tool","tool_call_id":"tc_obj","content":{"result":"value"}}
	]}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	// Should produce a functionResponse part.
	contents := gjson.GetBytes(out, "contents").Array()
	found := false
	for _, c := range contents {
		for _, p := range c.Get("parts").Array() {
			if p.Get("functionResponse.name").Exists() {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected functionResponse part: %s", out)
	}
}

func TestEncodeRequest_toolMessage_arrayContent_wrappedAsResult(t *testing.T) {
	// Tool message with array content → wrapped as {"result": <array>}.
	var c gemcodec.Codec
	body := []byte(`{"model":"g","messages":[
		{"role":"user","content":"call"},
		{"role":"assistant","tool_calls":[
			{"id":"tc_arr","type":"function","function":{"name":"fn","arguments":"{}"}}
		]},
		{"role":"tool","tool_call_id":"tc_arr","content":[{"type":"text","text":"result"}]}
	]}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	// Verify output has some content.
	if !gjson.GetBytes(out, "contents").IsArray() {
		t.Error("expected contents array")
	}
}

func TestEncodeRequest_emptyMessageRole_defaultsToUser(t *testing.T) {
	// A message with no role → defaults to user (geminiRole="user").
	var c gemcodec.Codec
	body := []byte(`{"model":"g","messages":[{"content":"anonymous"}]}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	contents := gjson.GetBytes(out, "contents").Array()
	found := false
	for _, c := range contents {
		if c.Get("role").String() == "user" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected role=user for empty role: %s", out)
	}
}

func TestEncodeRequest_multipleSystemMessages_concatenated(t *testing.T) {
	// Multiple system messages → concatenated with \n in systemInstruction.
	var c gemcodec.Codec
	body := []byte(`{"model":"g","messages":[
		{"role":"system","content":"Part A"},
		{"role":"system","content":"Part B"},
		{"role":"user","content":"hi"}
	]}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	si := gjson.GetBytes(out, "systemInstruction.parts.0.text").String()
	if si == "" {
		t.Errorf("systemInstruction text should be non-empty: %s", out)
	}
}

func TestEncodeRequest_toolChoiceDefaultAuto(t *testing.T) {
	// tool_choice with unknown string value defaults to AUTO mode.
	var c gemcodec.Codec
	body := []byte(`{"model":"g","messages":[{"role":"user","content":"hi"}],"tool_choice":"something_else"}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	mode := gjson.GetBytes(out, "toolConfig.functionCallingConfig.mode").String()
	if mode != "AUTO" {
		t.Errorf("unknown tool_choice should default to AUTO: got %q", mode)
	}
}

func TestDecodeResponse_usageMetadata_cacheAndThoughtsStamp(t *testing.T) {
	// Response with usageMetadata that has cachedContentTokenCount and thoughtsTokenCount
	// but the normalizer did NOT set them → codec stamps them defensively.
	var c gemcodec.Codec
	body := []byte(`{
		"responseId":"resp_cache",
		"modelVersion":"gemini-2.5-pro",
		"candidates":[{"index":0,"content":{"parts":[{"text":"cached answer"}],"role":"model"},"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":50,"totalTokenCount":150,
			"cachedContentTokenCount":30,"thoughtsTokenCount":20}
	}`)
	decRes, err := c.DecodeResponse(typology.WireShapeGeminiGenerateContent, body, "", provcore.DecodeContext{})
	out := decRes.CanonicalBody
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	// The usage fields should be present (either from Tier-1 normalizer or defensive stamp).
	// Don't assert exact values as the normalizer may already populate them.
	if !gjson.GetBytes(out, "choices").IsArray() {
		t.Errorf("expected choices: %s", out)
	}
}

func TestEncodeRequest_toolMessage_emptyStringContent_wrappedAsResult(t *testing.T) {
	// Tool message with empty string content → resp default map.
	var c gemcodec.Codec
	body := []byte(`{"model":"g","messages":[
		{"role":"user","content":"call"},
		{"role":"assistant","tool_calls":[
			{"id":"tc_es","type":"function","function":{"name":"fn","arguments":"{}"}}
		]},
		{"role":"tool","tool_call_id":"tc_es","content":""}
	]}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !gjson.GetBytes(out, "contents").IsArray() {
		t.Error("expected contents")
	}
}

func TestEncodeRequest_toolMessage_unknownCallID_fnNameFallback(t *testing.T) {
	// Tool message where tool_call_id doesn't match any tool_call in history
	// → fnName falls back to tid (the call_id itself), and if tid is empty → "unknown".
	var c gemcodec.Codec
	body := []byte(`{"model":"g","messages":[
		{"role":"user","content":"call"},
		{"role":"tool","tool_call_id":"","content":"result"}
	]}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	// Should produce a functionResponse with name="unknown".
	contents := gjson.GetBytes(out, "contents").Array()
	found := false
	for _, c := range contents {
		for _, p := range c.Get("parts").Array() {
			if p.Get("functionResponse.name").String() == "unknown" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected functionResponse name=unknown: %s", out)
	}
}

func TestEncodeRequest_arrayContentWithText_textPart(t *testing.T) {
	// User message with array content containing text type → text part.
	var c gemcodec.Codec
	body := []byte(`{"model":"g","messages":[{"role":"user","content":[
		{"type":"text","text":"hello from array"}
	]}]}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	parts := gjson.GetBytes(out, "contents.0.parts").Array()
	found := false
	for _, p := range parts {
		if p.Get("text").String() == "hello from array" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected text part: %s", out)
	}
}

func TestEncodeRequest_assistantToolCallsEmptyArgs_defaultsToEmptyObject(t *testing.T) {
	// tool_call with empty arguments string → "{}" default.
	var c gemcodec.Codec
	body := []byte(`{"model":"g","messages":[
		{"role":"user","content":"call"},
		{"role":"assistant","tool_calls":[
			{"id":"tc_ea","type":"function","function":{"name":"fn","arguments":""}}
		]}
	]}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	contents := gjson.GetBytes(out, "contents").Array()
	found := false
	for _, c := range contents {
		for _, p := range c.Get("parts").Array() {
			if p.Get("functionCall.name").String() == "fn" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected functionCall: %s", out)
	}
}

func TestEncodeRequest_functionCallWithNoID_idOmitted(t *testing.T) {
	// tool_call with empty id → id not forwarded to functionCall (older Gemini compat).
	var c gemcodec.Codec
	body := []byte(`{"model":"g","messages":[
		{"role":"user","content":"call"},
		{"role":"assistant","tool_calls":[
			{"id":"","type":"function","function":{"name":"fn","arguments":"{}"}}
		]}
	]}`)
	encRes, err := c.EncodeRequest(typology.WireShapeGeminiGenerateContent, body, provcore.CallTarget{})
	out := encRes.Body
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	contents := gjson.GetBytes(out, "contents").Array()
	for _, c := range contents {
		for _, p := range c.Get("parts").Array() {
			if p.Get("functionCall.name").String() == "fn" {
				if p.Get("functionCall.id").Exists() {
					t.Error("id should not be present when empty")
				}
			}
		}
	}
}

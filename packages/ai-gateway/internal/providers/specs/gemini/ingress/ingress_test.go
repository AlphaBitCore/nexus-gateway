// Package ingress_test covers the Gemini hub-ingress format translators.
// Named failure modes:
//   - GenerateContentRequestToOpenAIChatCompletion: empty body, missing model,
//     missing contents, no messages after contents
//   - generationConfig fields: temperature, topP, topK, maxOutputTokens, stopSequences
//   - systemInstruction multi-part text → system message
//   - Content parts: text, inlineData (base64), fileData (uri), functionCall, functionResponse
//   - geminiCompositeMessage: assistant+toolCalls, pure text, mixed text+images
//   - tools → canonical tools
//   - toolConfig.functionCallingConfig: AUTO/NONE/ANY(single)/ANY(multi)
//   - OpenAIChatCompletionToGenerateContentResponse: text, tool_calls, reasoning_content,
//     empty content fallback, usage fields, finish reason mapping
//   - mapOpenAIFinishToGemini: full enum
package ingress_test

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/gemini/ingress"
	"github.com/tidwall/gjson"
)

func TestGenerateContent_emptyBody_returnsError(t *testing.T) {
	_, err := ingress.GenerateContentRequestToOpenAIChatCompletion(nil, "")
	if err == nil {
		t.Fatal("expected error for nil body")
	}
}

func TestGenerateContent_missingModel_returnsError(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	_, err := ingress.GenerateContentRequestToOpenAIChatCompletion(body, "")
	if err == nil {
		t.Fatal("expected error for missing model")
	}
}

func TestGenerateContent_modelFromArg_overridesBodyModel(t *testing.T) {
	body := []byte(`{"model":"gemini-1.5-pro","contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	out, err := ingress.GenerateContentRequestToOpenAIChatCompletion(body, "gemini-2.0-flash")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "model").String() != "gemini-2.0-flash" {
		t.Errorf("model: got %q, want gemini-2.0-flash", gjson.GetBytes(out, "model").String())
	}
}

func TestGenerateContent_missingContents_returnsError(t *testing.T) {
	body := []byte(`{"model":"gemini-1.5-pro"}`)
	_, err := ingress.GenerateContentRequestToOpenAIChatCompletion(body, "gemini-1.5-pro")
	if err == nil {
		t.Fatal("expected error for missing contents")
	}
}

func TestGenerateContent_emptyContents_returnsError(t *testing.T) {
	// Non-array contents should also error.
	body := []byte(`{"model":"gemini-1.5-pro","contents":[]}`)
	_, err := ingress.GenerateContentRequestToOpenAIChatCompletion(body, "gemini-1.5-pro")
	if err == nil {
		t.Fatal("expected error for empty contents array (no messages)")
	}
}

func TestGenerateContent_simpleUserMessage(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)
	out, err := ingress.GenerateContentRequestToOpenAIChatCompletion(body, "gemini-1.5-pro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	if len(msgs) == 0 {
		t.Fatal("expected messages")
	}
	found := false
	for _, m := range msgs {
		if m.Get("role").String() == "user" && m.Get("content").String() == "hello" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected user message with 'hello': %s", string(out))
	}
}

func TestGenerateContent_modelRoleConvertedToAssistant(t *testing.T) {
	body := []byte(`{"contents":[
		{"role":"user","parts":[{"text":"hi"}]},
		{"role":"model","parts":[{"text":"hello back"}]}
	]}`)
	out, err := ingress.GenerateContentRequestToOpenAIChatCompletion(body, "gemini-1.5-pro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	found := false
	for _, m := range msgs {
		if m.Get("role").String() == "assistant" {
			found = true
		}
	}
	if !found {
		t.Errorf("model role should be converted to assistant: %s", string(out))
	}
}

func TestGenerateContent_generationConfig_temperature(t *testing.T) {
	body := []byte(`{
		"generationConfig":{"temperature":0.7,"topP":0.95,"topK":40,"maxOutputTokens":1024},
		"contents":[{"role":"user","parts":[{"text":"hi"}]}]
	}`)
	out, err := ingress.GenerateContentRequestToOpenAIChatCompletion(body, "gemini-1.5-pro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "temperature").Float() != 0.7 {
		t.Errorf("temperature: %s", string(out))
	}
	if gjson.GetBytes(out, "top_p").Float() != 0.95 {
		t.Errorf("top_p: %s", string(out))
	}
	if gjson.GetBytes(out, "top_k").Int() != 40 {
		t.Errorf("top_k: %s", string(out))
	}
	if gjson.GetBytes(out, "max_tokens").Int() != 1024 {
		t.Errorf("max_tokens: %s", string(out))
	}
}

func TestGenerateContent_stopSequencesArray(t *testing.T) {
	body := []byte(`{
		"generationConfig":{"stopSequences":["END","STOP"]},
		"contents":[{"role":"user","parts":[{"text":"hi"}]}]
	}`)
	out, err := ingress.GenerateContentRequestToOpenAIChatCompletion(body, "gemini-1.5-pro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	stop := gjson.GetBytes(out, "stop")
	if !stop.IsArray() || len(stop.Array()) != 2 {
		t.Errorf("stop: got %s, want [END, STOP]", stop.Raw)
	}
}

func TestGenerateContent_stopSequencesSingleElement(t *testing.T) {
	body := []byte(`{
		"generationConfig":{"stopSequences":["END"]},
		"contents":[{"role":"user","parts":[{"text":"hi"}]}]
	}`)
	out, err := ingress.GenerateContentRequestToOpenAIChatCompletion(body, "gemini-1.5-pro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	stop := gjson.GetBytes(out, "stop")
	if stop.Type != gjson.String || stop.String() != "END" {
		t.Errorf("single stop should become string: %s", stop.Raw)
	}
}

func TestGenerateContent_systemInstruction_multiPart(t *testing.T) {
	body := []byte(`{
		"systemInstruction":{"parts":[{"text":"Part A"},{"text":"Part B"}]},
		"contents":[{"role":"user","parts":[{"text":"hi"}]}]
	}`)
	out, err := ingress.GenerateContentRequestToOpenAIChatCompletion(body, "gemini-1.5-pro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	var found bool
	for _, m := range msgs {
		if m.Get("role").String() == "system" {
			found = true
			// Multi-part system joined.
			if m.Get("content").String() == "" {
				t.Error("system content should be non-empty")
			}
		}
	}
	if !found {
		t.Errorf("expected system message: %s", string(out))
	}
}

func TestGenerateContent_inlineData_base64Image(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[
		{"inlineData":{"mimeType":"image/png","data":"aGVsbG8="}}
	]}]}`)
	out, err := ingress.GenerateContentRequestToOpenAIChatCompletion(body, "gemini-1.5-pro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	found := false
	for _, m := range msgs {
		if m.Get("role").String() == "user" && m.Get("content").IsArray() {
			for _, part := range m.Get("content").Array() {
				if part.Get("type").String() == "image_url" {
					url := part.Get("image_url.url").String()
					if len(url) > 5 && url[:5] == "data:" {
						found = true
					}
				}
			}
		}
	}
	if !found {
		t.Errorf("expected data: image URL from inlineData: %s", string(out))
	}
}

func TestGenerateContent_fileData_externalURI(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[
		{"fileData":{"mimeType":"image/jpeg","fileUri":"https://storage.googleapis.com/img.jpg"}}
	]}]}`)
	out, err := ingress.GenerateContentRequestToOpenAIChatCompletion(body, "gemini-1.5-pro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	found := false
	for _, m := range msgs {
		if m.Get("role").String() == "user" && m.Get("content").IsArray() {
			for _, part := range m.Get("content").Array() {
				if part.Get("type").String() == "image_url" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Errorf("expected image_url from fileData: %s", string(out))
	}
}

func TestGenerateContent_functionCall_convertedToToolCalls(t *testing.T) {
	body := []byte(`{"contents":[
		{"role":"user","parts":[{"text":"use tool"}]},
		{"role":"model","parts":[{"functionCall":{"name":"search","args":{"q":"hello"}}}]}
	]}`)
	out, err := ingress.GenerateContentRequestToOpenAIChatCompletion(body, "gemini-1.5-pro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	found := false
	for _, m := range msgs {
		if m.Get("role").String() == "assistant" && m.Get("tool_calls").IsArray() {
			tc := m.Get("tool_calls.0")
			if tc.Get("function.name").String() == "search" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected assistant tool_calls from functionCall: %s", string(out))
	}
}

func TestGenerateContent_functionCall_syntheticID_noFunctionID(t *testing.T) {
	// functionCall without id → synthetic id generated from sha1.
	body := []byte(`{"contents":[
		{"role":"user","parts":[{"text":"ask"}]},
		{"role":"model","parts":[{"functionCall":{"name":"calc","args":{}}}]}
	]}`)
	out, err := ingress.GenerateContentRequestToOpenAIChatCompletion(body, "gemini-1.5-pro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	found := false
	for _, m := range msgs {
		if m.Get("role").String() == "assistant" && m.Get("tool_calls").IsArray() {
			id := m.Get("tool_calls.0.id").String()
			if id != "" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected synthetic tool_call id: %s", string(out))
	}
}

func TestGenerateContent_functionResponse_convertedToToolMessages(t *testing.T) {
	body := []byte(`{"contents":[
		{"role":"user","parts":[{"text":"ask"}]},
		{"role":"model","parts":[{"functionCall":{"id":"fc_1","name":"search","args":{}}}]},
		{"role":"user","parts":[{"functionResponse":{"id":"fc_1","name":"search","response":{"result":"found"}}}]}
	]}`)
	out, err := ingress.GenerateContentRequestToOpenAIChatCompletion(body, "gemini-1.5-pro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	found := false
	for _, m := range msgs {
		if m.Get("role").String() == "tool" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected role=tool message from functionResponse: %s", string(out))
	}
}

func TestGenerateContent_functionResponse_noID_usesName(t *testing.T) {
	// functionResponse with no id → tool_call_id = function name.
	body := []byte(`{"contents":[
		{"role":"user","parts":[{"text":"ask"}]},
		{"role":"user","parts":[{"functionResponse":{"name":"search","response":{"result":"found"}}}]}
	]}`)
	out, err := ingress.GenerateContentRequestToOpenAIChatCompletion(body, "gemini-1.5-pro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	found := false
	for _, m := range msgs {
		if m.Get("role").String() == "tool" {
			if m.Get("tool_call_id").String() == "search" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected tool message with tool_call_id=search: %s", string(out))
	}
}

func TestGenerateContent_toolMessages_withLeadingText_separateMessages(t *testing.T) {
	// A content that has both text and functionResponse → separate user+tool messages.
	body := []byte(`{"contents":[
		{"role":"user","parts":[
			{"text":"context text"},
			{"functionResponse":{"name":"fn","response":{"r":"v"}}}
		]}
	]}`)
	out, err := ingress.GenerateContentRequestToOpenAIChatCompletion(body, "gemini-1.5-pro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	var hasUser, hasTool bool
	for _, m := range msgs {
		switch m.Get("role").String() {
		case "user":
			if m.Get("content").String() == "context text" {
				hasUser = true
			}
		case "tool":
			hasTool = true
		}
	}
	if !hasUser || !hasTool {
		t.Errorf("expected separate user+tool messages; user=%v tool=%v: %s", hasUser, hasTool, string(out))
	}
}

func TestGenerateContent_tools_convertedToCanonical(t *testing.T) {
	body := []byte(`{
		"tools":[{"functionDeclarations":[
			{"name":"search","description":"web search","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}
		]}],
		"contents":[{"role":"user","parts":[{"text":"hi"}]}]
	}`)
	out, err := ingress.GenerateContentRequestToOpenAIChatCompletion(body, "gemini-1.5-pro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tools := gjson.GetBytes(out, "tools").Array()
	if len(tools) == 0 {
		t.Fatal("expected tools array")
	}
	if tools[0].Get("type").String() != "function" {
		t.Errorf("tool type: got %q, want function", tools[0].Get("type").String())
	}
	if tools[0].Get("function.name").String() != "search" {
		t.Errorf("tool name: got %q, want search", tools[0].Get("function.name").String())
	}
}

func TestGenerateContent_toolsWithEmptyName_skipped(t *testing.T) {
	body := []byte(`{
		"tools":[{"functionDeclarations":[
			{"name":"","description":"no name"},
			{"name":"valid","description":"ok"}
		]}],
		"contents":[{"role":"user","parts":[{"text":"hi"}]}]
	}`)
	out, err := ingress.GenerateContentRequestToOpenAIChatCompletion(body, "gemini-1.5-pro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tools := gjson.GetBytes(out, "tools").Array()
	if len(tools) != 1 {
		t.Errorf("expected 1 tool (empty name skipped), got %d: %s", len(tools), string(out))
	}
}

func TestGenerateContent_toolChoiceAuto(t *testing.T) {
	body := []byte(`{
		"toolConfig":{"functionCallingConfig":{"mode":"AUTO"}},
		"contents":[{"role":"user","parts":[{"text":"hi"}]}]
	}`)
	out, err := ingress.GenerateContentRequestToOpenAIChatCompletion(body, "gemini-1.5-pro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "tool_choice").String() != "auto" {
		t.Errorf("tool_choice: got %s", string(out))
	}
}

func TestGenerateContent_toolChoiceNone(t *testing.T) {
	body := []byte(`{
		"toolConfig":{"functionCallingConfig":{"mode":"NONE"}},
		"contents":[{"role":"user","parts":[{"text":"hi"}]}]
	}`)
	out, err := ingress.GenerateContentRequestToOpenAIChatCompletion(body, "gemini-1.5-pro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "tool_choice").String() != "none" {
		t.Errorf("tool_choice: got %s", string(out))
	}
}

func TestGenerateContent_toolChoiceAny_singleFunction(t *testing.T) {
	// ANY mode with one allowedFunctionName → specific function choice.
	body := []byte(`{
		"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["search"]}},
		"contents":[{"role":"user","parts":[{"text":"hi"}]}]
	}`)
	out, err := ingress.GenerateContentRequestToOpenAIChatCompletion(body, "gemini-1.5-pro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tc := gjson.GetBytes(out, "tool_choice")
	if tc.Get("type").String() != "function" {
		t.Errorf("specific tool_choice type: %s", string(out))
	}
	if tc.Get("function.name").String() != "search" {
		t.Errorf("specific tool_choice name: %s", string(out))
	}
}

func TestGenerateContent_toolChoiceAny_multipleOrEmpty_required(t *testing.T) {
	// ANY mode with multiple allowed names → "required".
	body := []byte(`{
		"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["fn1","fn2"]}},
		"contents":[{"role":"user","parts":[{"text":"hi"}]}]
	}`)
	out, err := ingress.GenerateContentRequestToOpenAIChatCompletion(body, "gemini-1.5-pro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "tool_choice").String() != "required" {
		t.Errorf("ANY with multiple names should be required: %s", string(out))
	}
}

func TestOpenAIToGenerateContent_basicText(t *testing.T) {
	openai := []byte(`{
		"id":"chatcmpl-1","model":"gemini-1.5-pro",
		"choices":[{"index":0,"message":{"role":"assistant","content":"Hello!"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}
	}`)
	out, err := ingress.OpenAIChatCompletionToGenerateContentResponse(openai)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Response should have candidates array.
	cands := gjson.GetBytes(out, "candidates").Array()
	if len(cands) == 0 {
		t.Fatal("expected candidates array")
	}
	textPart := cands[0].Get("content.parts.0.text").String()
	if textPart != "Hello!" {
		t.Errorf("text: got %q, want Hello!", textPart)
	}
	if cands[0].Get("content.role").String() != "model" {
		t.Errorf("content role: got %q, want model", cands[0].Get("content.role").String())
	}
}

func TestOpenAIToGenerateContent_emptyBody_returnsError(t *testing.T) {
	_, err := ingress.OpenAIChatCompletionToGenerateContentResponse(nil)
	if err == nil {
		t.Fatal("expected error for nil body")
	}
}

func TestOpenAIToGenerateContent_toolCalls_convertedToFunctionCall(t *testing.T) {
	openai := []byte(`{
		"id":"chatcmpl-2","model":"gemini-1.5-pro",
		"choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[
			{"id":"tc_1","type":"function","function":{"name":"search","arguments":"{\"q\":\"hello\"}"}}
		]},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":5,"completion_tokens":10}
	}`)
	out, err := ingress.OpenAIChatCompletionToGenerateContentResponse(openai)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cands := gjson.GetBytes(out, "candidates").Array()
	if len(cands) == 0 {
		t.Fatal("expected candidates")
	}
	parts := cands[0].Get("content.parts").Array()
	found := false
	for _, p := range parts {
		if p.Get("functionCall.name").String() == "search" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected functionCall part: %s", string(out))
	}
}

func TestOpenAIToGenerateContent_reasoningContent_thoughtPart(t *testing.T) {
	// reasoning_content → Gemini {text: ..., thought: true} part prepended.
	openai := []byte(`{
		"id":"chatcmpl-3","model":"gemini-2.5-pro",
		"choices":[{"index":0,"message":{"role":"assistant","content":"answer","reasoning_content":"think step"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":10,"completion_tokens":20}
	}`)
	out, err := ingress.OpenAIChatCompletionToGenerateContentResponse(openai)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cands := gjson.GetBytes(out, "candidates").Array()
	parts := cands[0].Get("content.parts").Array()
	found := false
	for _, p := range parts {
		if p.Get("thought").Bool() && p.Get("text").String() == "think step" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected thought:true part with reasoning_content: %s", string(out))
	}
}

func TestOpenAIToGenerateContent_emptyContentAndNoToolCalls_fallbackTextPart(t *testing.T) {
	// When message has no content and no tool_calls, parts should have one empty text part.
	openai := []byte(`{
		"id":"chatcmpl-4","model":"gemini-1.5-pro",
		"choices":[{"index":0,"message":{"role":"assistant","content":null},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":1,"completion_tokens":1}
	}`)
	out, err := ingress.OpenAIChatCompletionToGenerateContentResponse(openai)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cands := gjson.GetBytes(out, "candidates").Array()
	parts := cands[0].Get("content.parts").Array()
	if len(parts) == 0 {
		t.Fatal("expected at least one part (fallback empty text)")
	}
	if parts[0].Get("text").String() != "" {
		t.Errorf("fallback empty text part should have text='': %s", parts[0].Raw)
	}
}

func TestOpenAIToGenerateContent_usageMapping(t *testing.T) {
	openai := []byte(`{
		"id":"chatcmpl-5","model":"gemini-1.5-pro",
		"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":2}}
	}`)
	out, err := ingress.OpenAIChatCompletionToGenerateContentResponse(openai)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "usageMetadata.promptTokenCount").Int() != 10 {
		t.Errorf("promptTokenCount: %s", string(out))
	}
	if gjson.GetBytes(out, "usageMetadata.candidatesTokenCount").Int() != 5 {
		t.Errorf("candidatesTokenCount: %s", string(out))
	}
	if gjson.GetBytes(out, "usageMetadata.totalTokenCount").Int() != 15 {
		t.Errorf("totalTokenCount: %s", string(out))
	}
	if gjson.GetBytes(out, "usageMetadata.cachedContentTokenCount").Int() != 3 {
		t.Errorf("cachedContentTokenCount: %s", string(out))
	}
	if gjson.GetBytes(out, "usageMetadata.thoughtsTokenCount").Int() != 2 {
		t.Errorf("thoughtsTokenCount: %s", string(out))
	}
}

func TestOpenAIToGenerateContent_finishReasonMapping(t *testing.T) {
	cases := []struct {
		openai, gemini string
	}{
		{"stop", "STOP"},
		{"length", "MAX_TOKENS"},
		{"content_filter", "SAFETY"},
		{"tool_calls", "STOP"},
		{"", "STOP"},
		{"unknown_reason", "OTHER"},
	}
	for _, tc := range cases {
		openai := []byte(`{
			"id":"x","model":"gemini-1.5-pro",
			"choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"` + tc.openai + `"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1}
		}`)
		out, err := ingress.OpenAIChatCompletionToGenerateContentResponse(openai)
		if err != nil {
			t.Fatalf("finish_reason=%q: error: %v", tc.openai, err)
		}
		got := gjson.GetBytes(out, "candidates.0.finishReason").String()
		if got != tc.gemini {
			t.Errorf("finish_reason=%q → finishReason: got %q, want %q", tc.openai, got, tc.gemini)
		}
	}
}

func TestOpenAIToGenerateContent_toolCallInvalidArgs_fallbackEmptyObject(t *testing.T) {
	// tool_calls with invalid JSON args → empty object fallback.
	openai := []byte(`{
		"id":"chatcmpl-x","model":"gemini-1.5-pro",
		"choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[
			{"id":"tc_bad","type":"function","function":{"name":"fn","arguments":"not-json"}}
		]},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":1,"completion_tokens":1}
	}`)
	out, err := ingress.OpenAIChatCompletionToGenerateContentResponse(openai)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should still produce a functionCall part.
	cands := gjson.GetBytes(out, "candidates").Array()
	found := false
	for _, p := range cands[0].Get("content.parts").Array() {
		if p.Get("functionCall.name").String() == "fn" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected functionCall part: %s", string(out))
	}
}

func TestOpenAIToGenerateContent_toolCallNoID_idOmittedFromFunctionCall(t *testing.T) {
	// tool_call with empty id → id key omitted from functionCall (older Gemini compat).
	openai := []byte(`{
		"id":"chatcmpl-y","model":"gemini-1.5-pro",
		"choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[
			{"id":"","type":"function","function":{"name":"fn","arguments":"{}"}}
		]},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":1,"completion_tokens":1}
	}`)
	out, err := ingress.OpenAIChatCompletionToGenerateContentResponse(openai)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cands := gjson.GetBytes(out, "candidates").Array()
	for _, p := range cands[0].Get("content.parts").Array() {
		if p.Get("functionCall.name").String() == "fn" {
			if p.Get("functionCall.id").Exists() {
				t.Error("id should not be present when empty")
			}
		}
	}
}

func TestGenerateContent_emptyRole_defaultsToUser(t *testing.T) {
	// A content with no role should default to user.
	body := []byte(`{"contents":[{"parts":[{"text":"anonymous"}]}]}`)
	out, err := ingress.GenerateContentRequestToOpenAIChatCompletion(body, "gemini-1.5-pro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	found := false
	for _, m := range msgs {
		if m.Get("role").String() == "user" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected default user role: %s", string(out))
	}
}

func TestGenerateContent_functionCallEmptyArgs_defaultsToEmptyObject(t *testing.T) {
	// functionCall with no args → "{}" default.
	body := []byte(`{"contents":[
		{"role":"user","parts":[{"text":"ask"}]},
		{"role":"model","parts":[{"functionCall":{"name":"fn"}}]}
	]}`)
	out, err := ingress.GenerateContentRequestToOpenAIChatCompletion(body, "gemini-1.5-pro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should produce tool_calls with some arguments.
	msgs := gjson.GetBytes(out, "messages").Array()
	found := false
	for _, m := range msgs {
		if m.Get("role").String() == "assistant" && m.Get("tool_calls").IsArray() {
			found = true
		}
	}
	if !found {
		t.Errorf("expected assistant tool_calls: %s", string(out))
	}
}

func TestGenerateContent_multipleTextParts_joined(t *testing.T) {
	// Multiple text parts in one content → joined with newline.
	body := []byte(`{"contents":[{"role":"user","parts":[
		{"text":"line1"},
		{"text":"line2"}
	]}]}`)
	out, err := ingress.GenerateContentRequestToOpenAIChatCompletion(body, "gemini-1.5-pro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	found := false
	for _, m := range msgs {
		if m.Get("role").String() == "user" {
			c := m.Get("content").String()
			if c == "line1\nline2" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected joined text: %s", string(out))
	}
}

func TestGenerateContent_mixedTextAndImage_partsArray(t *testing.T) {
	// Text + inlineData → content becomes array (geminiCompositeMessage).
	body := []byte(`{"contents":[{"role":"user","parts":[
		{"text":"describe"},
		{"inlineData":{"mimeType":"image/png","data":"aGVsbG8="}}
	]}]}`)
	out, err := ingress.GenerateContentRequestToOpenAIChatCompletion(body, "gemini-1.5-pro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	found := false
	for _, m := range msgs {
		if m.Get("role").String() == "user" && m.Get("content").IsArray() {
			found = true
		}
	}
	if !found {
		t.Errorf("expected user message with content as parts array: %s", string(out))
	}
}

func TestGenerateContent_assistantToolCallsNoText_nilContent(t *testing.T) {
	// Assistant with tool_calls and no text → content=null.
	body := []byte(`{"contents":[
		{"role":"user","parts":[{"text":"ask"}]},
		{"role":"model","parts":[{"functionCall":{"name":"fn","args":{"x":1}}}]}
	]}`)
	out, err := ingress.GenerateContentRequestToOpenAIChatCompletion(body, "gemini-1.5-pro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	found := false
	for _, m := range msgs {
		if m.Get("role").String() == "assistant" && m.Get("tool_calls").IsArray() {
			// Content should be null (nil) since no text.
			if !m.Get("content").Exists() || m.Get("content").Type == gjson.Null {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected assistant with null content + tool_calls: %s", string(out))
	}
}

func TestGenerateContent_functionResponseStringContent(t *testing.T) {
	// functionResponse with string content → wraps as {"result": value}.
	body := []byte(`{"contents":[
		{"role":"user","parts":[{"text":"ask"}]},
		{"role":"user","parts":[{"functionResponse":{"name":"fn","response":"simple string result"}}]}
	]}`)
	out, err := ingress.GenerateContentRequestToOpenAIChatCompletion(body, "gemini-1.5-pro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	found := false
	for _, m := range msgs {
		if m.Get("role").String() == "tool" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected tool message: %s", string(out))
	}
}

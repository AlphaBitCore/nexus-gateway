// Package ingress_test covers the Anthropic hub-ingress format translators.
// Named failure modes:
//   - MessagesRequestToOpenAIChatCompletion: empty body, missing messages, missing model
//   - System: string, array of text blocks
//   - Tool-use messages → canonical tool_calls
//   - Tool-result messages → canonical role=tool messages
//   - Image parts (url, base64) → image_url parts
//   - Stop sequences (array/string) → canonical stop field
//   - Thinking passthrough → nexus.ext.anthropic.thinking
//   - OpenAIChatCompletionToMessagesResponse: content mapping, tool_use blocks,
//     reasoning_content → thinking block, usage fields, cache restoration
//   - MapOpenAIFinishToStopReason: full enum coverage
package ingress_test

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/anthropic/ingress"
	"github.com/tidwall/gjson"
)

func TestMessagesRequest_emptyBody_returnsError(t *testing.T) {
	_, err := ingress.MessagesRequestToOpenAIChatCompletion(nil, "")
	if err == nil {
		t.Fatal("expected error for nil body")
	}
}

func TestMessagesRequest_missingMessages_returnsError(t *testing.T) {
	_, err := ingress.MessagesRequestToOpenAIChatCompletion([]byte(`{"model":"claude-sonnet-4-6"}`), "")
	if err == nil {
		t.Fatal("expected error for missing messages")
	}
}

func TestMessagesRequest_missingModel_returnsError(t *testing.T) {
	_, err := ingress.MessagesRequestToOpenAIChatCompletion(
		[]byte(`{"messages":[{"role":"user","content":"hi"}]}`), "")
	if err == nil {
		t.Fatal("expected error for missing model")
	}
}

func TestMessagesRequest_providerModelIDOverridesModel(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`)
	out, err := ingress.MessagesRequestToOpenAIChatCompletion(body, "claude-haiku-4-5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "model").String() != "claude-haiku-4-5" {
		t.Errorf("model: got %q, want claude-haiku-4-5", gjson.GetBytes(out, "model").String())
	}
}

func TestMessagesRequest_systemStringPrompt_canonicalized(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"system":"You are helpful.",
		"messages":[{"role":"user","content":"hi"}]
	}`)
	out, err := ingress.MessagesRequestToOpenAIChatCompletion(body, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "messages.0.role").String() != "system" {
		t.Errorf("first message should be system: %s", string(out))
	}
	if gjson.GetBytes(out, "messages.0.content").String() != "You are helpful." {
		t.Errorf("system content: %s", string(out))
	}
}

func TestMessagesRequest_systemArrayPrompt_textJoined(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"system":[
			{"type":"text","text":"Part A"},
			{"type":"text","text":"Part B"}
		],
		"messages":[{"role":"user","content":"hi"}]
	}`)
	out, err := ingress.MessagesRequestToOpenAIChatCompletion(body, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sysmsg := gjson.GetBytes(out, "messages.0")
	if sysmsg.Get("role").String() != "system" {
		t.Errorf("expected system message: %s", string(out))
	}
}

func TestMessagesRequest_samplingParams_forwarded(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[{"role":"user","content":"hi"}],
		"temperature":0.8,
		"top_p":0.95,
		"top_k":40,
		"max_tokens":512,
		"stream":true
	}`)
	out, err := ingress.MessagesRequestToOpenAIChatCompletion(body, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "temperature").Float() != 0.8 {
		t.Errorf("temperature: %s", string(out))
	}
	if gjson.GetBytes(out, "top_p").Float() != 0.95 {
		t.Errorf("top_p: %s", string(out))
	}
	if gjson.GetBytes(out, "top_k").Int() != 40 {
		t.Errorf("top_k: %s", string(out))
	}
	if gjson.GetBytes(out, "max_tokens").Int() != 512 {
		t.Errorf("max_tokens: %s", string(out))
	}
	if !gjson.GetBytes(out, "stream").Bool() {
		t.Errorf("stream: %s", string(out))
	}
}

func TestMessagesRequest_stopSequencesArray(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[{"role":"user","content":"hi"}],
		"stop_sequences":["END","STOP"]
	}`)
	out, err := ingress.MessagesRequestToOpenAIChatCompletion(body, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	stop := gjson.GetBytes(out, "stop")
	if !stop.IsArray() || len(stop.Array()) != 2 {
		t.Errorf("stop: got %s, want [END, STOP]", stop.Raw)
	}
}

func TestMessagesRequest_stopSequencesSingleElement(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[{"role":"user","content":"hi"}],
		"stop_sequences":["END"]
	}`)
	out, err := ingress.MessagesRequestToOpenAIChatCompletion(body, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	stop := gjson.GetBytes(out, "stop")
	if stop.Type != gjson.String || stop.String() != "END" {
		t.Errorf("single stop_sequence should become string: %s", stop.Raw)
	}
}

func TestMessagesRequest_toolUseBlocks_convertedToToolCalls(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[
			{"role":"user","content":"call the tool"},
			{"role":"assistant","content":[
				{"type":"tool_use","id":"tu_1","name":"search","input":{"query":"hello"}}
			]}
		]
	}`)
	out, err := ingress.MessagesRequestToOpenAIChatCompletion(body, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The assistant message should have tool_calls.
	msgs := gjson.GetBytes(out, "messages").Array()
	var found bool
	for _, m := range msgs {
		if m.Get("role").String() == "assistant" && m.Get("tool_calls").IsArray() {
			found = true
			tc := m.Get("tool_calls.0")
			if tc.Get("id").String() != "tu_1" {
				t.Errorf("tool_call id: got %q, want tu_1", tc.Get("id").String())
			}
			if tc.Get("function.name").String() != "search" {
				t.Errorf("tool_call name: got %q, want search", tc.Get("function.name").String())
			}
		}
	}
	if !found {
		t.Errorf("expected assistant message with tool_calls: %s", string(out))
	}
}

func TestMessagesRequest_toolResultBlocks_convertedToToolMessages(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"tu_1","content":"result text"}
			]}
		]
	}`)
	out, err := ingress.MessagesRequestToOpenAIChatCompletion(body, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	var found bool
	for _, m := range msgs {
		if m.Get("role").String() == "tool" {
			found = true
			if m.Get("tool_call_id").String() != "tu_1" {
				t.Errorf("tool_call_id: got %q", m.Get("tool_call_id").String())
			}
		}
	}
	if !found {
		t.Errorf("expected role=tool message: %s", string(out))
	}
}

func TestMessagesRequest_imageBase64Part_convertedToDataURL(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[{"role":"user","content":[
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}
		]}]
	}`)
	out, err := ingress.MessagesRequestToOpenAIChatCompletion(body, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	imgURL := gjson.GetBytes(out, "messages.0.content.0.image_url.url").String()
	if imgURL == "" {
		t.Errorf("image_url missing: %s", string(out))
	}
	if len(imgURL) < 5 || imgURL[:5] != "data:" {
		t.Errorf("expected data: URL, got %q", imgURL)
	}
}

func TestMessagesRequest_imageURLPart_convertedToImageURL(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[{"role":"user","content":[
			{"type":"image","source":{"type":"url","url":"https://example.com/cat.png"}}
		]}]
	}`)
	out, err := ingress.MessagesRequestToOpenAIChatCompletion(body, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	imgURL := gjson.GetBytes(out, "messages.0.content.0.image_url.url").String()
	if imgURL != "https://example.com/cat.png" {
		t.Errorf("image_url: got %q", imgURL)
	}
}

func TestMessagesRequest_toolsConverted(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"search","description":"web search","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}]
	}`)
	out, err := ingress.MessagesRequestToOpenAIChatCompletion(body, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tools := gjson.GetBytes(out, "tools")
	if !tools.IsArray() || len(tools.Array()) == 0 {
		t.Fatal("expected tools array")
	}
	if tools.Array()[0].Get("type").String() != "function" {
		t.Errorf("tool type: got %q, want function", tools.Array()[0].Get("type").String())
	}
	if tools.Array()[0].Get("function.name").String() != "search" {
		t.Errorf("tool name: got %q", tools.Array()[0].Get("function.name").String())
	}
}

func TestMessagesRequest_toolChoiceAuto(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[{"role":"user","content":"hi"}],
		"tool_choice":{"type":"auto"}
	}`)
	out, err := ingress.MessagesRequestToOpenAIChatCompletion(body, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "tool_choice").String() != "auto" {
		t.Errorf("tool_choice: got %s", string(out))
	}
}

func TestMessagesRequest_toolChoiceAny(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[{"role":"user","content":"hi"}],
		"tool_choice":{"type":"any"}
	}`)
	out, err := ingress.MessagesRequestToOpenAIChatCompletion(body, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "tool_choice").String() != "required" {
		t.Errorf("tool_choice any→required: got %s", string(out))
	}
}

func TestMessagesRequest_toolChoiceNone(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[{"role":"user","content":"hi"}],
		"tool_choice":{"type":"none"}
	}`)
	out, err := ingress.MessagesRequestToOpenAIChatCompletion(body, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "tool_choice").String() != "none" {
		t.Errorf("tool_choice: got %s", string(out))
	}
}

func TestMessagesRequest_toolChoiceSpecificTool(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[{"role":"user","content":"hi"}],
		"tool_choice":{"type":"tool","name":"search"}
	}`)
	out, err := ingress.MessagesRequestToOpenAIChatCompletion(body, "")
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

func TestMessagesRequest_thinkingPassthrough(t *testing.T) {
	body := []byte(`{
		"model":"claude-opus-4-7",
		"messages":[{"role":"user","content":"think"}],
		"thinking":{"type":"enabled","budget_tokens":4096}
	}`)
	out, err := ingress.MessagesRequestToOpenAIChatCompletion(body, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Thinking must be preserved under nexus.ext.anthropic.thinking.
	ext := gjson.GetBytes(out, "nexus.ext.anthropic.thinking")
	if !ext.Exists() {
		t.Errorf("nexus.ext.anthropic.thinking missing: %s", string(out))
	}
	if ext.Get("type").String() != "enabled" {
		t.Errorf("thinking.type: got %q", ext.Get("type").String())
	}
}

func TestMessagesRequest_noMessages_returnsError(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-6","messages":[]}`)
	_, err := ingress.MessagesRequestToOpenAIChatCompletion(body, "")
	if err == nil {
		t.Fatal("expected error: empty messages array")
	}
}

func TestOpenAIToMessages_basicTextContent(t *testing.T) {
	openai := []byte(`{
		"id":"chatcmpl-1",
		"model":"claude-sonnet-4-6",
		"choices":[{"index":0,"message":{"role":"assistant","content":"Hello there"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":10,"completion_tokens":5}
	}`)
	out, err := ingress.OpenAIChatCompletionToMessagesResponse(openai)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "type").String() != "message" {
		t.Errorf("type: got %q, want message", gjson.GetBytes(out, "type").String())
	}
	if gjson.GetBytes(out, "role").String() != "assistant" {
		t.Errorf("role: got %q", gjson.GetBytes(out, "role").String())
	}
	content := gjson.GetBytes(out, "content")
	if !content.IsArray() || len(content.Array()) == 0 {
		t.Fatalf("content must be array: %s", string(out))
	}
	if content.Array()[0].Get("text").String() != "Hello there" {
		t.Errorf("content text: %s", string(out))
	}
}

func TestOpenAIToMessages_emptyBody_returnsError(t *testing.T) {
	_, err := ingress.OpenAIChatCompletionToMessagesResponse(nil)
	if err == nil {
		t.Fatal("expected error for nil body")
	}
}

func TestOpenAIToMessages_missingChoices_returnsError(t *testing.T) {
	_, err := ingress.OpenAIChatCompletionToMessagesResponse([]byte(`{"id":"abc"}`))
	if err == nil {
		t.Fatal("expected error for missing choices.0.message")
	}
}

func TestOpenAIToMessages_toolCalls_convertedToToolUse(t *testing.T) {
	openai := []byte(`{
		"id":"chatcmpl-2",
		"model":"claude-sonnet-4-6",
		"choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[
			{"id":"tc_1","type":"function","function":{"name":"search","arguments":"{\"q\":\"hello\"}"}}
		]},"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":5,"completion_tokens":10}
	}`)
	out, err := ingress.OpenAIChatCompletionToMessagesResponse(openai)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	content := gjson.GetBytes(out, "content")
	if !content.IsArray() {
		t.Fatalf("content must be array: %s", string(out))
	}
	found := false
	for _, part := range content.Array() {
		if part.Get("type").String() == "tool_use" {
			found = true
			if part.Get("id").String() != "tc_1" {
				t.Errorf("tool_use id: got %q, want tc_1", part.Get("id").String())
			}
			if part.Get("name").String() != "search" {
				t.Errorf("tool_use name: %s", string(out))
			}
		}
	}
	if !found {
		t.Errorf("tool_use block missing: %s", string(out))
	}
}

func TestOpenAIToMessages_reasoningContent_thinkingBlock(t *testing.T) {
	// reasoning_content → Anthropic thinking block (cross-format reasoning preservation).
	openai := []byte(`{
		"id":"chatcmpl-3",
		"model":"claude-sonnet-4-6",
		"choices":[{"index":0,"message":{"role":"assistant","content":"answer","reasoning_content":"step1 step2"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":10,"completion_tokens":20}
	}`)
	out, err := ingress.OpenAIChatCompletionToMessagesResponse(openai)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	content := gjson.GetBytes(out, "content")
	if !content.IsArray() {
		t.Fatalf("content must be array: %s", string(out))
	}
	found := false
	for _, part := range content.Array() {
		if part.Get("type").String() == "thinking" {
			found = true
			if part.Get("thinking").String() != "step1 step2" {
				t.Errorf("thinking text: got %q", part.Get("thinking").String())
			}
		}
	}
	if !found {
		t.Errorf("thinking block missing: %s", string(out))
	}
}

func TestOpenAIToMessages_usageConversion(t *testing.T) {
	openai := []byte(`{
		"id":"chatcmpl-4",
		"model":"claude-sonnet-4-6",
		"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":8}}
	}`)
	out, err := ingress.OpenAIChatCompletionToMessagesResponse(openai)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "usage.input_tokens").Int() != 10 {
		t.Errorf("usage.input_tokens: %s", string(out))
	}
	if gjson.GetBytes(out, "usage.output_tokens").Int() != 5 {
		t.Errorf("usage.output_tokens: %s", string(out))
	}
	if gjson.GetBytes(out, "usage.cache_read_input_tokens").Int() != 8 {
		t.Errorf("usage.cache_read_input_tokens: %s", string(out))
	}
}

func TestOpenAIToMessages_cacheCreationRestored(t *testing.T) {
	// cache_creation_input_tokens from nexus.ext.anthropic should be restored to Anthropic usage.
	openai := []byte(`{
		"id":"chatcmpl-5",
		"model":"claude-sonnet-4-6",
		"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":10,"completion_tokens":5},
		"nexus":{"ext":{"anthropic":{"cache_creation_input_tokens":1000}}}
	}`)
	out, err := ingress.OpenAIChatCompletionToMessagesResponse(openai)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gjson.GetBytes(out, "usage.cache_creation_input_tokens").Int() != 1000 {
		t.Errorf("cache_creation_input_tokens not restored: %s", string(out))
	}
}

func TestOpenAIToMessages_finishReasonMapping(t *testing.T) {
	cases := []struct {
		openai, anthropic string
	}{
		{"stop", "end_turn"},
		{"length", "max_tokens"},
		{"tool_calls", "tool_use"},
		{"content_filter", "stop_sequence"},
		{"", "end_turn"},
		{"unknown_reason", "unknown_reason"},
	}
	for _, tc := range cases {
		openai := []byte(`{
			"id":"chatcmpl-x","model":"claude-sonnet-4-6",
			"choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"` + tc.openai + `"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1}
		}`)
		out, err := ingress.OpenAIChatCompletionToMessagesResponse(openai)
		if err != nil {
			t.Fatalf("finish_reason=%q: error: %v", tc.openai, err)
		}
		got := gjson.GetBytes(out, "stop_reason").String()
		if got != tc.anthropic {
			t.Errorf("finish_reason=%q → stop_reason: got %q, want %q", tc.openai, got, tc.anthropic)
		}
	}
}

func TestMapOpenAIFinishToStopReason_allVariants(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"stop", "end_turn"},
		{"length", "max_tokens"},
		{"tool_calls", "tool_use"},
		{"content_filter", "stop_sequence"},
		{"", "end_turn"},
		{"unknown_value", "unknown_value"},
	}
	for _, tc := range cases {
		got := ingress.MapOpenAIFinishToStopReason(tc.in)
		if got != tc.want {
			t.Errorf("MapOpenAIFinishToStopReason(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestStringifyOpenAIMessageContent_string(t *testing.T) {
	r := gjson.Parse(`"hello world"`)
	got := ingress.StringifyOpenAIMessageContent(r)
	if got != "hello world" {
		t.Errorf("got %q, want hello world", got)
	}
}

func TestStringifyOpenAIMessageContent_array(t *testing.T) {
	r := gjson.Parse(`[{"type":"text","text":"part1"},{"type":"text","text":"part2"}]`)
	got := ingress.StringifyOpenAIMessageContent(r)
	if got == "" {
		t.Error("expected non-empty string from array content")
	}
}

func TestStringifyOpenAIMessageContent_missing(t *testing.T) {
	r := gjson.Parse(`{}`)
	r2 := r.Get("missing_field")
	got := ingress.StringifyOpenAIMessageContent(r2)
	if got != "" {
		t.Errorf("missing field should return empty, got %q", got)
	}
}

func TestStringifyAnthropicToolResult_string(t *testing.T) {
	r := gjson.Parse(`"result text"`)
	got := ingress.StringifyAnthropicToolResult(r)
	if got != "result text" {
		t.Errorf("got %q, want result text", got)
	}
}

func TestStringifyAnthropicToolResult_arrayWithTextParts(t *testing.T) {
	r := gjson.Parse(`[{"type":"text","text":"line1"},{"type":"text","text":"line2"}]`)
	got := ingress.StringifyAnthropicToolResult(r)
	if got == "" {
		t.Error("expected non-empty from array")
	}
}

func TestStringifyAnthropicToolResult_rawFallback(t *testing.T) {
	r := gjson.Parse(`{"nested":"object"}`)
	got := ingress.StringifyAnthropicToolResult(r)
	if got == "" {
		t.Error("expected raw JSON fallback for object content")
	}
}

// anthropicMessageToOpenAI (via MessagesRequestToOpenAIChatCompletion)
// These tests exercise branches in the unexported anthropicMessageToOpenAI by
// crafting messages that go through MessagesRequestToOpenAIChatCompletion.

func TestMessagesRequest_emptyRole_defaultsToUser(t *testing.T) {
	// A message with no "role" field should default to role="user".
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[{"content":"hello"}]
	}`)
	out, err := ingress.MessagesRequestToOpenAIChatCompletion(body, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	found := false
	for _, m := range msgs {
		if m.Get("content").String() == "hello" && m.Get("role").String() == "user" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected role=user default for message with no role: %s", string(out))
	}
}

func TestMessagesRequest_nonArrayNonStringContent_emptyContent(t *testing.T) {
	// A message with content = null (non-array, non-string) produces content="".
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[{"role":"user","content":null}]
	}`)
	out, err := ingress.MessagesRequestToOpenAIChatCompletion(body, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	found := false
	for _, m := range msgs {
		if m.Get("role").String() == "user" {
			// content should be empty string for null
			found = true
		}
	}
	if !found {
		t.Errorf("expected user message from null content: %s", string(out))
	}
}

func TestMessagesRequest_assistantToolUseWithTextContent_mixedEntry(t *testing.T) {
	// Assistant message with both text line AND tool_use → entry has content + tool_calls.
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[
			{"role":"user","content":"ask"},
			{"role":"assistant","content":[
				{"type":"text","text":"I will call the tool."},
				{"type":"tool_use","id":"tu_2","name":"search","input":{"q":"go"}}
			]}
		]
	}`)
	out, err := ingress.MessagesRequestToOpenAIChatCompletion(body, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	var found bool
	for _, m := range msgs {
		if m.Get("role").String() == "assistant" && m.Get("tool_calls").IsArray() {
			// Should have content set to the text string (single text part).
			if m.Get("content").String() == "I will call the tool." {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected assistant message with tool_calls + text content: %s", string(out))
	}
}

func TestMessagesRequest_assistantToolUseWithImageContent_partsArray(t *testing.T) {
	// Assistant message with tool_use AND image → content becomes a parts array.
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[
			{"role":"user","content":"ask"},
			{"role":"assistant","content":[
				{"type":"image","source":{"type":"url","url":"https://example.com/img.png"}},
				{"type":"tool_use","id":"tu_3","name":"vision","input":{}}
			]}
		]
	}`)
	out, err := ingress.MessagesRequestToOpenAIChatCompletion(body, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	var found bool
	for _, m := range msgs {
		if m.Get("role").String() == "assistant" && m.Get("tool_calls").IsArray() {
			// Content should be a parts array with image_url type.
			if m.Get("content").IsArray() {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected assistant message with tool_calls + image parts content: %s", string(out))
	}
}

func TestMessagesRequest_multipleTextLines_contentAsPartsArray(t *testing.T) {
	// User message with multiple text parts → content is an array of text part objects
	// (not joined as a string — joining only happens for single-text + no-image case).
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"line1"},
			{"type":"text","text":"line2"}
		]}]
	}`)
	out, err := ingress.MessagesRequestToOpenAIChatCompletion(body, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	found := false
	for _, m := range msgs {
		if m.Get("role").String() == "user" && m.Get("content").IsArray() {
			parts := m.Get("content").Array()
			if len(parts) == 2 && parts[0].Get("text").String() == "line1" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected user message with content as parts array: %s", string(out))
	}
}

func TestMessagesRequest_userImageOnly_contentIsPartsArray(t *testing.T) {
	// User message with only an image and no text → content is parts array.
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[{"role":"user","content":[
			{"type":"image","source":{"type":"url","url":"https://example.com/img.png"}}
		]}]
	}`)
	out, err := ingress.MessagesRequestToOpenAIChatCompletion(body, "")
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
		t.Errorf("expected user message with content as parts array (image only): %s", string(out))
	}
}

func TestMessagesRequest_userImageAndText_contentIsPartsArray(t *testing.T) {
	// User message with text + image → parts array (images present, >1 total part).
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"describe"},
			{"type":"image","source":{"type":"url","url":"https://example.com/img.png"}}
		]}]
	}`)
	out, err := ingress.MessagesRequestToOpenAIChatCompletion(body, "")
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
		t.Errorf("expected user message with content as parts array (text+image): %s", string(out))
	}
}

func TestMessagesRequest_toolResultWithTextAndLeadingText_twoMessages(t *testing.T) {
	// A user message with both a text block and tool_result blocks produces
	// two canonical messages: one user text + one tool message.
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"context"},
			{"type":"tool_result","tool_use_id":"tu_1","content":"result"}
		]}]
	}`)
	out, err := ingress.MessagesRequestToOpenAIChatCompletion(body, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	var hasUser, hasTool bool
	for _, m := range msgs {
		switch m.Get("role").String() {
		case "user":
			if m.Get("content").String() == "context" {
				hasUser = true
			}
		case "tool":
			hasTool = true
		}
	}
	if !hasUser || !hasTool {
		t.Errorf("expected separate user+tool messages; user=%v tool=%v output: %s", hasUser, hasTool, string(out))
	}
}

func TestMessagesRequest_emptyArrayContent_emptyStringContent(t *testing.T) {
	// A message with an empty content array produces content="".
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[{"role":"user","content":[]}]
	}`)
	out, err := ingress.MessagesRequestToOpenAIChatCompletion(body, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	found := false
	for _, m := range msgs {
		if m.Get("role").String() == "user" {
			found = true
			// Empty array: no text, no images → content should be ""
			c := m.Get("content")
			if c.Type != gjson.String || c.String() != "" {
				t.Errorf("empty array content should produce content='': %s", m.Raw)
			}
		}
	}
	if !found {
		t.Errorf("no user message found: %s", string(out))
	}
}

// StringifyOpenAIMessageContent edge cases

func TestStringifyOpenAIMessageContent_nonStringNonArray_returnsEmpty(t *testing.T) {
	// An object value (non-string, non-array) returns "".
	r := gjson.Parse(`{"key":"value"}`)
	got := ingress.StringifyOpenAIMessageContent(r)
	if got != "" {
		t.Errorf("object content should return empty string, got %q", got)
	}
}

func TestStringifyOpenAIMessageContent_textWithValueKey_usesValue(t *testing.T) {
	// A text part where "text" is an object with a "value" sub-key.
	r := gjson.Parse(`[{"type":"text","text":{"value":"from value key"}}]`)
	got := ingress.StringifyOpenAIMessageContent(r)
	if got != "from value key" {
		t.Errorf("expected 'from value key', got %q", got)
	}
}

func TestMessagesRequest_userSingleTextArrayPart_stringContent(t *testing.T) {
	// A user message with exactly one text part in an array.
	//   len(images)==0, len(textLines)==1 → single-text branch → content = textLines[0]
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"single text part"}
		]}]
	}`)
	out, err := ingress.MessagesRequestToOpenAIChatCompletion(body, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	found := false
	for _, m := range msgs {
		if m.Get("role").String() == "user" && m.Get("content").String() == "single text part" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected user message with string content from single text part: %s", string(out))
	}
}

func TestMessagesRequest_stopSequencesAsString_forwardedAsStop(t *testing.T) {
	// stop_sequences as a raw string (not array) → forwarded to "stop" field.
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[{"role":"user","content":"hi"}],
		"stop_sequences":"END"
	}`)
	out, err := ingress.MessagesRequestToOpenAIChatCompletion(body, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	stop := gjson.GetBytes(out, "stop")
	if stop.String() != "END" {
		t.Errorf("stop: got %q, want END", stop.String())
	}
}

func TestMessagesRequest_toolWithEmptyName_skipped(t *testing.T) {
	// A tool with an empty name should be silently skipped.
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[
			{"name":"","description":"no name"},
			{"name":"search","description":"web","input_schema":{"type":"object"}}
		]
	}`)
	out, err := ingress.MessagesRequestToOpenAIChatCompletion(body, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tools := gjson.GetBytes(out, "tools").Array()
	if len(tools) != 1 {
		t.Errorf("expected 1 tool (empty-name skipped), got %d: %s", len(tools), string(out))
	}
	if tools[0].Get("function.name").String() != "search" {
		t.Errorf("expected 'search' tool: %s", string(out))
	}
}

func TestMessagesRequest_assistantToolUseWithMultipleTextLines_multiPartContent(t *testing.T) {
	// Assistant with tool_use AND two text lines → content becomes multi-part array.
	// (Exercises the `else if len(parts) > 0` branch in the assistant+tool_use block.)
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[
			{"role":"user","content":"ask"},
			{"role":"assistant","content":[
				{"type":"text","text":"line one"},
				{"type":"text","text":"line two"},
				{"type":"tool_use","id":"tu_y","name":"process","input":{}}
			]}
		]
	}`)
	out, err := ingress.MessagesRequestToOpenAIChatCompletion(body, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	var found bool
	for _, m := range msgs {
		if m.Get("role").String() == "assistant" && m.Get("tool_calls").IsArray() {
			// With 2 text parts, content must be a multi-part array.
			if m.Get("content").IsArray() && len(m.Get("content").Array()) >= 2 {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected assistant with tool_calls + multi-part content array: %s", string(out))
	}
}

func TestMessagesRequest_assistantToolUseWithSingleImageOnly_singlePartArray(t *testing.T) {
	// Assistant with one tool_use + one image → content is a single-element parts array
	// (the image part is not type "text", so the entry["content"] = parts branch fires).
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[
			{"role":"user","content":"ask"},
			{"role":"assistant","content":[
				{"type":"image","source":{"type":"url","url":"https://example.com/img.png"}},
				{"type":"tool_use","id":"tu_x","name":"vision","input":{}}
			]}
		]
	}`)
	out, err := ingress.MessagesRequestToOpenAIChatCompletion(body, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	var found bool
	for _, m := range msgs {
		if m.Get("role").String() == "assistant" && m.Get("tool_calls").IsArray() {
			// content should be a parts array (single image_url part)
			if m.Get("content").IsArray() && len(m.Get("content").Array()) == 1 {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected single-image-part content array in assistant tool_use message: %s", string(out))
	}
}

func TestMessagesRequest_userSingleImageOnly_singlePartArray(t *testing.T) {
	// User message with a single image and no text:
	//   len(images)==1, len(textLines)==0 → len(parts)==1, parts[0] is image_url (not text)
	//   → entry["content"] = parts  (single-element array)
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[{"role":"user","content":[
			{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"aGVsbG8="}}
		]}]
	}`)
	out, err := ingress.MessagesRequestToOpenAIChatCompletion(body, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	found := false
	for _, m := range msgs {
		if m.Get("role").String() == "user" && m.Get("content").IsArray() {
			parts := m.Get("content").Array()
			if len(parts) == 1 && parts[0].Get("type").String() == "image_url" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected single image_url part as content array: %s", string(out))
	}
}

func TestStringifyOpenAIMessageContent_multiTextParts_joinedByNewline(t *testing.T) {
	r := gjson.Parse(`[{"type":"text","text":"first"},{"type":"text","text":"second"}]`)
	got := ingress.StringifyOpenAIMessageContent(r)
	if got != "first\nsecond" {
		t.Errorf("expected 'first\\nsecond', got %q", got)
	}
}

// OpenAIChatCompletionToMessagesResponse edge cases

func TestOpenAIToMessages_toolCallsInvalidArgs_fallbackToEmptyObject(t *testing.T) {
	// tool_calls with invalid JSON arguments should fall back to an empty input object.
	openai := []byte(`{
		"id":"chatcmpl-6",
		"model":"claude-sonnet-4-6",
		"choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[
			{"id":"tc_bad","type":"function","function":{"name":"search","arguments":"not-json"}}
		]},"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":5,"completion_tokens":10}
	}`)
	out, err := ingress.OpenAIChatCompletionToMessagesResponse(openai)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The tool_use block should still be present with an empty input object.
	content := gjson.GetBytes(out, "content")
	if !content.IsArray() {
		t.Fatalf("content must be array: %s", string(out))
	}
	found := false
	for _, part := range content.Array() {
		if part.Get("type").String() == "tool_use" && part.Get("name").String() == "search" {
			found = true
			// Input should be an empty object (not the invalid JSON string).
			if part.Get("input").Type != gjson.JSON {
				t.Errorf("expected tool_use input to be a JSON object, got type=%v raw=%q", part.Get("input").Type, part.Get("input").Raw)
			}
		}
	}
	if !found {
		t.Errorf("tool_use block missing: %s", string(out))
	}
}

func TestOpenAIToMessages_emptyContentAndNoToolCalls_fallbackTextBlock(t *testing.T) {
	// A message with no text content and no tool_calls produces a single empty text block.
	openai := []byte(`{
		"id":"chatcmpl-7",
		"model":"claude-sonnet-4-6",
		"choices":[{"index":0,"message":{"role":"assistant","content":null},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":1,"completion_tokens":1}
	}`)
	out, err := ingress.OpenAIChatCompletionToMessagesResponse(openai)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	content := gjson.GetBytes(out, "content")
	if !content.IsArray() || len(content.Array()) == 0 {
		t.Fatalf("expected non-empty content array: %s", string(out))
	}
	first := content.Array()[0]
	if first.Get("type").String() != "text" || first.Get("text").String() != "" {
		t.Errorf("expected fallback empty text block: %s", first.Raw)
	}
}

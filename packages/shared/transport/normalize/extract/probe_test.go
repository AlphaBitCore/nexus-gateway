package extract

import (
	"strings"
	"testing"
)

// Request-side probe

func TestDetectChatShape_ChatGPTWebRequest(t *testing.T) {
	// Real-shape ChatGPT-web request body (subset of baa07c15 capture).
	body := []byte(`{
		"model": "gpt-5-5",
		"messages": [{
			"author": {"role": "user"},
			"content": {"parts": ["hello! have you read any good books lately?"], "content_type": "text"},
			"metadata": {"suggestion_type": "autocomplete"}
		}],
		"suggestion_type": "autocomplete",
		"chosen_suggestion": {"type": "autocomplete", "index": 0},
		"client_contextual_info": {"app_name": "chatgpt.com"},
		"parent_message_id": "client-created-root"
	}`)
	d := DetectChatShape(body)
	if d.SpecID != "chatgpt-web" {
		t.Fatalf("specID: %q want chatgpt-web", d.SpecID)
	}
	if d.Confidence < 0.9 {
		t.Errorf("confidence: %v want >= 0.9", d.Confidence)
	}
	if d.Model != "gpt-5-5" {
		t.Errorf("model: %q", d.Model)
	}
	if len(d.UserPrompts) != 1 || !strings.Contains(d.UserPrompts[0], "have you read") {
		t.Errorf("user prompts: %+v", d.UserPrompts)
	}
}

func TestDetectChatShape_OpenAIChatRequest(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4o-mini",
		"messages": [
			{"role": "system", "content": "be helpful"},
			{"role": "user", "content": "what is the capital of France?"}
		],
		"temperature": 0.7,
		"max_tokens": 100
	}`)
	d := DetectChatShape(body)
	if d.SpecID != "openai-chat" {
		t.Fatalf("specID: %q want openai-chat", d.SpecID)
	}
	if d.Confidence < 0.85 {
		t.Errorf("confidence: %v", d.Confidence)
	}
	if d.Model != "gpt-4o-mini" {
		t.Errorf("model: %q", d.Model)
	}
	if len(d.MessageRoles) != 2 || d.MessageRoles[0] != "system" || d.MessageRoles[1] != "user" {
		t.Errorf("roles: %v", d.MessageRoles)
	}
}

func TestDetectChatShape_AnthropicMessages(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-6",
		"max_tokens": 1024,
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "tell me about Go generics"}]}
		],
		"system": "be terse"
	}`)
	d := DetectChatShape(body)
	if d.SpecID != "anthropic-messages" {
		t.Fatalf("specID: %q want anthropic-messages", d.SpecID)
	}
	if d.Confidence < 0.85 {
		t.Errorf("confidence: %v", d.Confidence)
	}
	if d.System != "be terse" {
		t.Errorf("system: %q", d.System)
	}
	if !strings.Contains(d.MessageContents[0], "Go generics") {
		t.Errorf("content: %q", d.MessageContents[0])
	}
}

func TestDetectChatShape_Gemini(t *testing.T) {
	body := []byte(`{
		"model": "gemini-2.5-flash",
		"contents": [
			{"role": "user", "parts": [{"text": "explain quantum entanglement"}]}
		],
		"generationConfig": {"temperature": 0.5}
	}`)
	d := DetectChatShape(body)
	if d.SpecID != "gemini-generate" {
		t.Fatalf("specID: %q want gemini-generate", d.SpecID)
	}
	if !strings.Contains(d.MessageContents[0], "quantum") {
		t.Errorf("content: %q", d.MessageContents[0])
	}
}

func TestDetectChatShape_NonChatJSON_BelowThreshold(t *testing.T) {
	// Random JSON with a `messages` field but no role / content shape.
	body := []byte(`{"messages": [123, 456], "foo": "bar"}`)
	d := DetectChatShape(body)
	// Below 0.7 → confidence low. May still hint at a spec but not winning.
	if d.Confidence >= 0.7 {
		t.Errorf("confidence too high: %v (%q)", d.Confidence, d.SpecID)
	}
}

func TestDetectChatShape_NonChatJSON_NoMessages(t *testing.T) {
	body := []byte(`{"foo": "bar", "count": 42}`)
	d := DetectChatShape(body)
	if d.Confidence > 0.1 {
		t.Errorf("confidence: %v want ~0 (%q)", d.Confidence, d.SpecID)
	}
}

func TestDetectChatShape_AnthropicLegacyCompletion(t *testing.T) {
	body := []byte(`{
		"model": "claude-instant-1",
		"prompt": "\n\nHuman: hi\n\nAssistant:",
		"max_tokens_to_sample": 256
	}`)
	d := DetectChatShape(body)
	if d.SpecID != "anthropic-completions-legacy" {
		t.Fatalf("specID: %q", d.SpecID)
	}
}

// Response-side probe

func TestDetectResponseShape_OpenAINonStream(t *testing.T) {
	body := []byte(`{
		"id": "chatcmpl-abc",
		"object": "chat.completion",
		"created": 1234567890,
		"model": "gpt-4o-mini",
		"choices": [{
			"index": 0,
			"message": {"role": "assistant", "content": "Paris is the capital of France."},
			"finish_reason": "stop"
		}],
		"usage": {"prompt_tokens": 20, "completion_tokens": 8, "total_tokens": 28}
	}`)
	d := DetectResponseShape(body)
	if d.SpecID != "openai-chat-nonstream" {
		t.Fatalf("specID: %q", d.SpecID)
	}
	if !strings.Contains(d.AssistantText, "Paris") {
		t.Errorf("assistant text: %q", d.AssistantText)
	}
	if d.FinishReason != "stop" {
		t.Errorf("finish: %q", d.FinishReason)
	}
	if d.Confidence < 0.7 {
		t.Errorf("confidence: %v", d.Confidence)
	}
}

func TestDetectResponseShape_AnthropicNonStream(t *testing.T) {
	body := []byte(`{
		"id": "msg_abc",
		"model": "claude-haiku-4-5",
		"content": [{"type": "text", "text": "Sure! Here is a haiku..."}],
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 12, "output_tokens": 25}
	}`)
	d := DetectResponseShape(body)
	if d.SpecID != "anthropic-messages-nonstream" {
		t.Fatalf("specID: %q", d.SpecID)
	}
	if !strings.Contains(d.AssistantText, "haiku") {
		t.Errorf("text: %q", d.AssistantText)
	}
}

func TestDetectResponseShape_GeminiNonStream(t *testing.T) {
	body := []byte(`{
		"candidates": [{
			"content": {"role": "model", "parts": [{"text": "Quantum entanglement is..."}]},
			"finishReason": "STOP"
		}],
		"modelVersion": "gemini-2.5-flash",
		"usageMetadata": {"promptTokenCount": 8, "candidatesTokenCount": 30, "totalTokenCount": 38}
	}`)
	d := DetectResponseShape(body)
	if d.SpecID != "gemini-generate-nonstream" {
		t.Fatalf("specID: %q", d.SpecID)
	}
	if !strings.Contains(d.AssistantText, "Quantum") {
		t.Errorf("text: %q", d.AssistantText)
	}
}

func TestDetectResponseShape_ChatGPTWebSSE(t *testing.T) {
	// End-to-end: walks the SSE, accumulates JSON-patch ops, extracts
	// the final assistant text. This is the baa07c15 case from the
	// a deploy story.
	raw := []byte(strings.Join([]string{
		"event: delta_encoding",
		`data: "v1"`,
		"",
		`data: {"type":"resume_conversation_token","token":"abc"}`,
		"",
		"event: delta",
		`data: {"p":"","o":"add","v":{"message":{"id":"asst1","author":{"role":"assistant"},"content":{"content_type":"text","parts":[""]}},"conversation_id":"conv1"}}`,
		"",
		"event: delta",
		`data: {"p":"/message/content/parts/0","o":"append","v":"A few that stand"}`,
		"",
		"event: delta",
		`data: {"v":" out recently,"}`,
		"",
		"event: delta",
		`data: {"v":" depending on the kind of reading mood you're in."}`,
		"",
		"event: delta",
		`data: {"p":"","o":"patch","v":[{"p":"/message/content/parts/0","o":"append","v":" Project Hail Mary by Andy Weir."}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n"))

	d := DetectResponseShape(raw)
	if d.SpecID != "chatgpt-web" {
		t.Fatalf("specID: %q want chatgpt-web", d.SpecID)
	}
	if d.Confidence < 0.5 {
		t.Errorf("confidence: %v want >= 0.5", d.Confidence)
	}
	if !d.IsStream {
		t.Errorf("expected IsStream=true")
	}
	if !strings.Contains(d.AssistantText, "Andy Weir") {
		t.Fatalf("assistant text: %q want to contain final delta", d.AssistantText)
	}
	if !strings.Contains(d.AssistantText, "A few that stand out") {
		t.Errorf("assistant text missing first delta: %q", d.AssistantText)
	}
}

func TestDetectResponseShape_OpenAISSE(t *testing.T) {
	raw := []byte(strings.Join([]string{
		`data: {"id":"x","object":"chat.completion.chunk","choices":[{"delta":{"role":"assistant"}}]}`,
		"",
		`data: {"id":"x","choices":[{"delta":{"content":"Hello"}}]}`,
		"",
		`data: {"id":"x","choices":[{"delta":{"content":" world"}}]}`,
		"",
		`data: {"id":"x","choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n"))
	d := DetectResponseShape(raw)
	if d.SpecID != "openai-chat-sse" {
		t.Fatalf("specID: %q", d.SpecID)
	}
	if d.AssistantText != "Hello world" {
		t.Fatalf("text: %q", d.AssistantText)
	}
}

func TestDetectResponseShape_AnthropicSSE(t *testing.T) {
	raw := []byte(strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg1","model":"claude-haiku-4-5","content":[],"usage":{"input_tokens":12}}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hi "}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"there!"}}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		"",
	}, "\n"))
	d := DetectResponseShape(raw)
	if d.SpecID != "anthropic-messages-sse" {
		t.Fatalf("specID: %q", d.SpecID)
	}
	if d.AssistantText != "Hi there!" {
		t.Fatalf("text: %q", d.AssistantText)
	}
}

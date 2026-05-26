package gemini

import (
	"testing"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

func TestGenerateContentRequestToOpenAIChatCompletion_Basic(t *testing.T) {
	native := []byte(`{
	  "contents": [{"role": "user", "parts": [{"text": "hello"}]}],
	  "generationConfig": {"maxOutputTokens": 64, "temperature": 0.2}
	}`)
	out, err := GenerateContentRequestToOpenAIChatCompletion(native, "gemini-1.5-flash")
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(out, "model").String() != "gemini-1.5-flash" {
		t.Fatalf("model: %s", string(out))
	}
	if !gjson.GetBytes(out, "messages").IsArray() {
		t.Fatalf("expected messages: %s", string(out))
	}
}

func TestOpenAIChatCompletionToGenerateContentResponse_Basic(t *testing.T) {
	openai := []byte(`{
	  "id": "test",
	  "object": "chat.completion",
	  "created": 1700000000,
	  "model": "gemini-1.5-flash",
	  "choices": [{
	    "index": 0,
	    "message": {"role": "assistant", "content": "done"},
	    "finish_reason": "stop"
	  }],
	  "usage": {"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3}
	}`)
	native, err := OpenAIChatCompletionToGenerateContentResponse(openai)
	if err != nil {
		t.Fatal(err)
	}
	if !gjson.GetBytes(native, "candidates").Exists() {
		t.Fatalf("expected candidates: %s", string(native))
	}
	codec := NewCodec()
	decRes, err := codec.DecodeResponse(typology.WireShapeGeminiGenerateContent, native, "")
	back := decRes.CanonicalBody
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if gjson.GetBytes(back, "object").String() != "chat.completion" {
		t.Fatalf("round-trip decode should restore chat.completion: %s", string(back))
	}
}

// TestOpenAIChatCompletionToGenerateContentResponse_ReasoningContent
// verifies the cross-format reasoning preservation fix: when canonical
// carries `choices[0].message.reasoning_content` (set by OpenAI o-series
// / gpt-5 / DeepSeek / Moonshot upstreams), the back-projection emits a
// Gemini-native `{text:"...", thought:true}` part. Without this fix, a
// Gemini-SDK client cross-routing to an OpenAI reasoning model would
// silently lose the model's thinking text (the count was already
// preserved via thoughtsTokenCount, but the text itself was dropped).
//
// Part ordering: thought part comes BEFORE visible text part, matching
// Gemini 2.5+'s natural ordering when
// generationConfig.thinkingConfig.includeThoughts is enabled.
func TestOpenAIChatCompletionToGenerateContentResponse_ReasoningContent(t *testing.T) {
	openai := []byte(`{
	  "id": "test-r",
	  "object": "chat.completion",
	  "model": "gpt-5",
	  "choices": [{
	    "index": 0,
	    "message": {
	      "role": "assistant",
	      "content": "Answer: 42.",
	      "reasoning_content": "Let me think about this carefully."
	    },
	    "finish_reason": "stop"
	  }],
	  "usage": {
	    "prompt_tokens": 5, "completion_tokens": 8, "total_tokens": 13,
	    "completion_tokens_details": {"reasoning_tokens": 6}
	  }
	}`)
	native, err := OpenAIChatCompletionToGenerateContentResponse(openai)
	if err != nil {
		t.Fatal(err)
	}
	parts := gjson.GetBytes(native, "candidates.0.content.parts")
	if !parts.IsArray() {
		t.Fatalf("parts not an array: %s", string(native))
	}
	arr := parts.Array()
	if len(arr) != 2 {
		t.Fatalf("want 2 parts (thought + text), got %d: %s", len(arr), string(native))
	}
	if !arr[0].Get("thought").Bool() {
		t.Errorf("first part should have thought=true; got %s", arr[0].Raw)
	}
	if got := arr[0].Get("text").String(); got != "Let me think about this carefully." {
		t.Errorf("thought text=%q want canonical reasoning", got)
	}
	if arr[1].Get("thought").Bool() {
		t.Errorf("second part should NOT have thought=true; got %s", arr[1].Raw)
	}
	if got := arr[1].Get("text").String(); got != "Answer: 42." {
		t.Errorf("visible text=%q want canonical content", got)
	}
	// Thought token count surfaces as Gemini-native usageMetadata.thoughtsTokenCount.
	if got := gjson.GetBytes(native, "usageMetadata.thoughtsTokenCount").Int(); got != 6 {
		t.Errorf("thoughtsTokenCount=%d want 6", got)
	}
}

// TestOpenAIChatCompletionToGenerateContentResponse_NoReasoningContent
// regression-free path: no reasoning_content → no thought part.
func TestOpenAIChatCompletionToGenerateContentResponse_NoReasoningContent(t *testing.T) {
	openai := []byte(`{
	  "id": "test-p",
	  "object": "chat.completion",
	  "model": "gpt-4o",
	  "choices": [{
	    "index": 0,
	    "message": {"role": "assistant", "content": "Plain answer."},
	    "finish_reason": "stop"
	  }],
	  "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}
	}`)
	native, err := OpenAIChatCompletionToGenerateContentResponse(openai)
	if err != nil {
		t.Fatal(err)
	}
	parts := gjson.GetBytes(native, "candidates.0.content.parts").Array()
	if len(parts) != 1 {
		t.Fatalf("want 1 text part, got %d: %s", len(parts), string(native))
	}
	for _, p := range parts {
		if p.Get("thought").Bool() {
			t.Errorf("unexpected thought=true part when canonical has no reasoning_content: %s", string(native))
		}
	}
}

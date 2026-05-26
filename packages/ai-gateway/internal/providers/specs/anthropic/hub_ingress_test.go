package anthropic

import (
	"encoding/json"
	"testing"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

func TestMessagesRequestToOpenAIChatCompletion_Basic(t *testing.T) {
	native := []byte(`{
	  "model": "claude-3-5-sonnet-20241022",
	  "max_tokens": 256,
	  "messages": [{"role": "user", "content": [{"type": "text", "text": "hello"}]}]
	}`)
	out, err := MessagesRequestToOpenAIChatCompletion(native, "claude-3-5-sonnet-20241022")
	if err != nil {
		t.Fatal(err)
	}
	if !gjson.GetBytes(out, "messages").IsArray() {
		t.Fatalf("expected messages array, got %s", string(out))
	}
	if gjson.GetBytes(out, "model").String() != "claude-3-5-sonnet-20241022" {
		t.Fatalf("model: %s", string(out))
	}
}

func TestOpenAIChatCompletionToMessagesResponse_RoundTripish(t *testing.T) {
	openai := []byte(`{
	  "id": "msg_1",
	  "object": "chat.completion",
	  "created": 1700000000,
	  "model": "claude-3-5-sonnet-20241022",
	  "choices": [{
	    "index": 0,
	    "message": {"role": "assistant", "content": "hi there"},
	    "finish_reason": "stop"
	  }],
	  "usage": {"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3}
	}`)
	native, err := OpenAIChatCompletionToMessagesResponse(openai)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(native, "type").String() != "message" {
		t.Fatalf("type: %s", string(native))
	}
	codec := NewSpec(nil).SchemaCodec
	decRes, err := codec.DecodeResponse(typology.WireShapeAnthropicMessages, native, "")
	back := decRes.CanonicalBody
	if err != nil {
		t.Fatal(err)
	}
	var round map[string]any
	if err := json.Unmarshal(back, &round); err != nil {
		t.Fatal(err)
	}
	if round["object"] != "chat.completion" {
		t.Fatalf("expected chat.completion object after decode, got %#v", round["object"])
	}
}

// TestOpenAIChatCompletionToMessagesResponse_ReasoningContent verifies
// the cross-format reasoning preservation fix: when canonical carries
// `choices[0].message.reasoning_content` (set by OpenAI o-series / gpt-5
// / DeepSeek / Moonshot upstreams), the back-projection emits an
// Anthropic-native `{type:"thinking", thinking:"..."}` content block.
// Without this fix, an Anthropic-SDK client cross-routing to an OpenAI
// reasoning model would silently lose the model's thinking text.
//
// Block ordering: thinking comes BEFORE text, matching the natural
// upstream block order (Anthropic returns thinking blocks first when
// extended-thinking is enabled).
func TestOpenAIChatCompletionToMessagesResponse_ReasoningContent(t *testing.T) {
	openai := []byte(`{
	  "id": "msg_r",
	  "object": "chat.completion",
	  "model": "gpt-5",
	  "choices": [{
	    "index": 0,
	    "message": {
	      "role": "assistant",
	      "content": "The answer is 42.",
	      "reasoning_content": "Let me think… the meaning of life is famously 42."
	    },
	    "finish_reason": "stop"
	  }],
	  "usage": {"prompt_tokens": 5, "completion_tokens": 8, "total_tokens": 13}
	}`)
	native, err := OpenAIChatCompletionToMessagesResponse(openai)
	if err != nil {
		t.Fatal(err)
	}
	content := gjson.GetBytes(native, "content")
	if !content.IsArray() {
		t.Fatalf("content not an array: %s", string(native))
	}
	arr := content.Array()
	if len(arr) != 2 {
		t.Fatalf("want 2 content blocks (thinking + text), got %d: %s", len(arr), string(native))
	}
	if arr[0].Get("type").String() != "thinking" {
		t.Errorf("first block type=%q want thinking; %s", arr[0].Get("type").String(), string(native))
	}
	if got := arr[0].Get("thinking").String(); got != "Let me think… the meaning of life is famously 42." {
		t.Errorf("thinking text=%q want canonical reasoning; %s", got, string(native))
	}
	if arr[1].Get("type").String() != "text" {
		t.Errorf("second block type=%q want text", arr[1].Get("type").String())
	}
	if got := arr[1].Get("text").String(); got != "The answer is 42." {
		t.Errorf("text=%q want canonical content", got)
	}
}

// TestOpenAIChatCompletionToMessagesResponse_NoReasoningContent confirms
// the fix is a no-op when canonical has no reasoning_content (the
// regression-free path for non-reasoning models).
func TestOpenAIChatCompletionToMessagesResponse_NoReasoningContent(t *testing.T) {
	openai := []byte(`{
	  "id": "msg_p",
	  "object": "chat.completion",
	  "model": "gpt-4o",
	  "choices": [{
	    "index": 0,
	    "message": {"role": "assistant", "content": "Plain answer."},
	    "finish_reason": "stop"
	  }],
	  "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}
	}`)
	native, err := OpenAIChatCompletionToMessagesResponse(openai)
	if err != nil {
		t.Fatal(err)
	}
	content := gjson.GetBytes(native, "content")
	arr := content.Array()
	if len(arr) != 1 {
		t.Fatalf("want 1 text block (no thinking), got %d: %s", len(arr), string(native))
	}
	if arr[0].Get("type").String() != "text" {
		t.Errorf("type=%q want text", arr[0].Get("type").String())
	}
	for _, b := range arr {
		if b.Get("type").String() == "thinking" {
			t.Errorf("unexpected thinking block when canonical has no reasoning_content: %s", string(native))
		}
	}
}

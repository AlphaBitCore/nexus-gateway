package codecs

import (
	"context"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

func TestOpenAIResponses_RequestShape(t *testing.T) {
	body := []byte(`{
	  "model": "gpt-5",
	  "instructions": "You are a helpful assistant.",
	  "input": [
	    {"role": "user", "content": [{"type": "input_text", "text": "What is 17*23?"}]}
	  ],
	  "max_output_tokens": 200,
	  "temperature": 0.0
	}`)
	n := NewOpenAIResponsesNormalizer()
	p, err := n.Normalize(context.Background(), body, core.Meta{
		AdapterType: "openai",
		Direction:   core.DirectionRequest,
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if p.Kind != core.KindAIChat {
		t.Errorf("kind=%q want %q", p.Kind, core.KindAIChat)
	}
	if p.Protocol != "openai-responses" {
		t.Errorf("protocol=%q want openai-responses", p.Protocol)
	}
	if p.Model != "gpt-5" {
		t.Errorf("model=%q want gpt-5", p.Model)
	}
	if len(p.Messages) != 2 {
		t.Fatalf("want 2 messages (system from instructions + user from input), got %d", len(p.Messages))
	}
	if p.Messages[0].Role != core.RoleSystem {
		t.Errorf("messages[0].role=%q want system", p.Messages[0].Role)
	}
	if got := p.Messages[0].Content[0].Text; got != "You are a helpful assistant." {
		t.Errorf("system text=%q want canonical instructions", got)
	}
	if p.Messages[1].Role != core.RoleUser {
		t.Errorf("messages[1].role=%q want user", p.Messages[1].Role)
	}
	if got := p.Messages[1].Content[0].Text; got != "What is 17*23?" {
		t.Errorf("user text=%q", got)
	}
	if p.Params == nil || p.Params.MaxTokens == nil || *p.Params.MaxTokens != 200 {
		t.Errorf("max_output_tokens not propagated to Params.MaxTokens")
	}
	if p.DetectedSpec != "openai-responses" {
		t.Errorf("DetectedSpec=%q want openai-responses", p.DetectedSpec)
	}
}

func TestOpenAIResponses_ResponseWithReasoningAndMessage(t *testing.T) {
	// Real prod-shape payload (from the smoke run's traffic_event_normalized
	// row that originally fell back to Tier-3 generic-http — this test
	// guards against that regression).
	body := []byte(`{
	  "id": "resp_1778947540432927000",
	  "object": "response",
	  "model": "kimi-k2.5",
	  "status": "completed",
	  "output": [
	    {"id": "rs_1", "type": "reasoning", "summary": [
	      {"type": "summary_text", "text": "Let me think about this carefully."}
	    ]},
	    {"id": "msg_1", "type": "message", "role": "assistant", "status": "completed", "content": [
	      {"type": "output_text", "text": "The answer is 391."}
	    ]}
	  ],
	  "usage": {
	    "input_tokens": 82, "output_tokens": 453, "total_tokens": 535,
	    "input_tokens_details": {"cached_tokens": 12},
	    "output_tokens_details": {"reasoning_tokens": 420}
	  }
	}`)
	n := NewOpenAIResponsesNormalizer()
	p, err := n.Normalize(context.Background(), body, core.Meta{
		AdapterType: "openai",
		Direction:   core.DirectionResponse,
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(p.Messages) != 1 {
		t.Fatalf("want 1 assistant message, got %d", len(p.Messages))
	}
	m := p.Messages[0]
	if m.Role != core.RoleAssistant {
		t.Errorf("role=%q want assistant", m.Role)
	}
	if len(m.Content) != 2 {
		t.Fatalf("want 2 content blocks (reasoning + text), got %d", len(m.Content))
	}
	if m.Content[0].Type != core.ContentReasoning || m.Content[0].Text != "Let me think about this carefully." {
		t.Errorf("content[0] = %+v want core.ContentReasoning/canonical text", m.Content[0])
	}
	if m.Content[1].Type != core.ContentText || m.Content[1].Text != "The answer is 391." {
		t.Errorf("content[1] = %+v want core.ContentText/canonical answer", m.Content[1])
	}
	if p.Usage == nil {
		t.Fatal("usage missing")
	}
	if *p.Usage.PromptTokens != 82 || *p.Usage.CompletionTokens != 453 || *p.Usage.TotalTokens != 535 {
		t.Errorf("usage tokens = pt=%v ct=%v tt=%v want 82/453/535",
			derefIntPtr(p.Usage.PromptTokens), derefIntPtr(p.Usage.CompletionTokens), derefIntPtr(p.Usage.TotalTokens))
	}
	if p.Usage.CacheReadTokens == nil || *p.Usage.CacheReadTokens != 12 {
		t.Errorf("CacheReadTokens=%v want 12", derefIntPtr(p.Usage.CacheReadTokens))
	}
	if p.Usage.ReasoningTokens == nil || *p.Usage.ReasoningTokens != 420 {
		t.Errorf("ReasoningTokens=%v want 420", derefIntPtr(p.Usage.ReasoningTokens))
	}
	if p.FinishReason != "completed" {
		t.Errorf("FinishReason=%q want completed (Responses-API uses status)", p.FinishReason)
	}
}

func TestOpenAIResponses_ResponseEmptyOutput(t *testing.T) {
	// Defensive: a dry-run / refused / abstained response has output:[].
	// Normalizer must still emit ONE assistant message so consumers
	// filtering by core.Role don't drop the row entirely.
	body := []byte(`{
	  "id": "resp_x", "object": "response", "model": "gpt-5", "status": "completed",
	  "output": [],
	  "usage": {"input_tokens": 10, "output_tokens": 0, "total_tokens": 10}
	}`)
	p, err := NewOpenAIResponsesNormalizer().Normalize(context.Background(), body, core.Meta{
		AdapterType: "openai", Direction: core.DirectionResponse,
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(p.Messages) != 1 || p.Messages[0].Role != core.RoleAssistant {
		t.Fatalf("want 1 assistant message even on empty output; got %+v", p.Messages)
	}
}

func TestOpenAIResponses_ResponseToolCall(t *testing.T) {
	body := []byte(`{
	  "id": "resp_t", "object": "response", "model": "gpt-5", "status": "completed",
	  "output": [
	    {"id": "fc_1", "type": "function_call", "call_id": "call_abc", "name": "get_weather",
	     "arguments": "{\"city\":\"NYC\"}"}
	  ],
	  "usage": {"input_tokens": 20, "output_tokens": 10, "total_tokens": 30}
	}`)
	p, err := NewOpenAIResponsesNormalizer().Normalize(context.Background(), body, core.Meta{
		AdapterType: "openai", Direction: core.DirectionResponse,
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(p.Messages) != 1 || len(p.Messages[0].Content) != 1 {
		t.Fatalf("want 1 message + 1 content block; got %+v", p.Messages)
	}
	b := p.Messages[0].Content[0]
	if b.Type != core.ContentToolUse {
		t.Errorf("content[0].type=%v want core.ContentToolUse", b.Type)
	}
	if b.ToolUse == nil || b.ToolUse.Name != "get_weather" || b.ToolUse.CallID != "call_abc" {
		t.Errorf("ToolUse=%+v want name=get_weather callId=call_abc", b.ToolUse)
	}
	if got := b.ToolUse.Input["city"]; got != "NYC" {
		t.Errorf("ToolUse.Input.city=%v want NYC", got)
	}
}

func TestOpenAIResponses_Registration(t *testing.T) {
	// Verify the dispatch wiring: a body posted to /v1/responses with
	// adapter_type=openai must resolve to OpenAIResponsesNormalizer
	// (NOT OpenAIChatNormalizer, which would fail to extract the
	// output[] items and confidence-fall back to Tier-3).
	reg := core.NewRegistry()
	RegisterDefaultAIBuiltins(reg)
	body := []byte(`{"id":"r","object":"response","model":"gpt-5","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":5,"output_tokens":1,"total_tokens":6}}`)
	p, err := reg.Normalize(context.Background(), body, core.Meta{
		AdapterType:  "openai",
		EndpointPath: "/v1/responses",
		Direction:    core.DirectionResponse,
	})
	if err != nil {
		t.Fatalf("registry.Normalize: %v", err)
	}
	if p.Protocol != "openai-responses" {
		t.Errorf("protocol=%q want openai-responses (dispatch should pick Responses normalizer for /v1/responses)", p.Protocol)
	}
	if !strings.EqualFold(string(p.Kind), "ai-chat") {
		t.Errorf("kind=%q want ai-chat", p.Kind)
	}
}

// derefIntPtr already defined in anthropic_messages.go; reuse that.

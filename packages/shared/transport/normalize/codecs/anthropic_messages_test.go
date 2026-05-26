package codecs

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

func TestAnthropic_RequestSimple(t *testing.T) {
	body := `{"model":"claude-3-5-sonnet","system":"You are helpful.","messages":[{"role":"user","content":"Hi"}],"max_tokens":50}`
	n := NewAnthropicMessagesNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Kind != core.KindAIChat || got.Protocol != "anthropic-messages" {
		t.Errorf("kind/protocol wrong: %+v", got)
	}
	if got.Model != "claude-3-5-sonnet" {
		t.Errorf("model = %q", got.Model)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("messages = %d, want 2 (system + user)", len(got.Messages))
	}
	if got.Messages[0].Role != core.RoleSystem || got.Messages[0].Content[0].Text != "You are helpful." {
		t.Errorf("system message wrong: %+v", got.Messages[0])
	}
	if got.Messages[1].Role != core.RoleUser || got.Messages[1].Content[0].Text != "Hi" {
		t.Errorf("user message wrong: %+v", got.Messages[1])
	}
	if got.Params == nil || got.Params.MaxTokens == nil || *got.Params.MaxTokens != 50 {
		t.Errorf("max_tokens not captured: %+v", got.Params)
	}
}

func TestAnthropic_RequestWithSystemArray(t *testing.T) {
	body := `{"model":"claude-3-5-sonnet","system":[{"type":"text","text":"sys1"},{"type":"text","text":"sys2"}],"messages":[{"role":"user","content":"hi"}],"max_tokens":10}`
	n := NewAnthropicMessagesNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Messages[0].Role != core.RoleSystem || len(got.Messages[0].Content) != 2 {
		t.Errorf("system array not exploded: %+v", got.Messages[0])
	}
}

func TestAnthropic_RequestWithTools(t *testing.T) {
	body := `{"model":"claude","messages":[{"role":"user","content":"weather"}],"tools":[{"name":"get_weather","description":"get it","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],"max_tokens":10}`
	n := NewAnthropicMessagesNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Tools) != 1 || got.Tools[0].Name != "get_weather" {
		t.Fatalf("tools: %+v", got.Tools)
	}
	if got.Tools[0].ParametersJSONSchema["type"] != "object" {
		t.Errorf("schema not preserved: %+v", got.Tools[0].ParametersJSONSchema)
	}
}

func TestAnthropic_RequestWithToolResult(t *testing.T) {
	body := `{"model":"claude","messages":[{"role":"user","content":"start"},{"role":"assistant","content":[{"type":"tool_use","id":"u_1","name":"get_weather","input":{"city":"SF"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"u_1","content":"sunny"}]}],"max_tokens":10}`
	n := NewAnthropicMessagesNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 3 messages: user, assistant(tool_use), user(tool_result)
	if len(got.Messages) != 3 {
		t.Fatalf("messages = %d", len(got.Messages))
	}
	asst := got.Messages[1]
	if asst.Content[0].Type != core.ContentToolUse || asst.Content[0].ToolUse.Name != "get_weather" {
		t.Errorf("tool_use wrong: %+v", asst.Content[0])
	}
	if asst.Content[0].ToolUse.Input["city"] != "SF" {
		t.Errorf("tool input wrong: %+v", asst.Content[0].ToolUse.Input)
	}
	tr := got.Messages[2].Content[0]
	if tr.Type != core.ContentToolResult || tr.ToolResult.CallID != "u_1" || tr.ToolResult.Output != "sunny" {
		t.Errorf("tool_result wrong: %+v", tr)
	}
}

func TestAnthropic_RequestEmptyMessagesYieldsErrUnsupported(t *testing.T) {
	body := `{"model":"claude","messages":[]}`
	n := NewAnthropicMessagesNormalizer()
	_, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected core.ErrUnsupported, got %v", err)
	}
}

func TestAnthropic_NonStreamResponse(t *testing.T) {
	body := `{"model":"claude","content":[{"type":"text","text":"Hi!"}],"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":3,"cache_read_input_tokens":4}}`
	n := NewAnthropicMessagesNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Messages) != 1 || got.Messages[0].Content[0].Text != "Hi!" {
		t.Errorf("response message wrong: %+v", got.Messages)
	}
	if got.FinishReason != "end_turn" {
		t.Errorf("finish_reason = %q", got.FinishReason)
	}
	if got.Usage == nil || got.Usage.CacheReadTokens == nil || *got.Usage.CacheReadTokens != 4 {
		t.Errorf("cache read tokens not captured: %+v", got.Usage)
	}
	// Normalized convention: PromptTokens = uncached input_tokens
	// (7) + cache_read_input_tokens (4) + cache_creation_input_tokens (0) = 11.
	// TotalTokens = PromptTokens (11) + CompletionTokens (3) = 14.
	if got.Usage.PromptTokens == nil || *got.Usage.PromptTokens != 11 {
		t.Errorf("prompt tokens wrong: %+v", got.Usage)
	}
	if got.Usage.TotalTokens == nil || *got.Usage.TotalTokens != 14 {
		t.Errorf("total tokens wrong: %+v", got.Usage)
	}
}

func TestAnthropic_StreamResponse_TextDeltas(t *testing.T) {
	raw := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"model":"claude","usage":{"input_tokens":5,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" there"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	n := NewAnthropicMessagesNormalizer()
	got, err := n.Normalize(context.Background(), []byte(raw), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Messages) != 1 || got.Messages[0].Content[0].Text != "Hi there" {
		t.Errorf("assembled text = %+v", got.Messages)
	}
	if got.FinishReason != "end_turn" {
		t.Errorf("finish_reason = %q", got.FinishReason)
	}
	if got.Usage == nil || *got.Usage.PromptTokens != 5 || *got.Usage.CompletionTokens != 2 {
		t.Errorf("usage wrong: %+v", got.Usage)
	}
	if !got.Stream {
		t.Errorf("Stream not set")
	}
}

func TestAnthropic_StreamResponse_ThinkingPreserved(t *testing.T) {
	raw := strings.Join([]string{
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me consider"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"final answer"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		``,
	}, "\n")

	n := NewAnthropicMessagesNormalizer()
	got, err := n.Normalize(context.Background(), []byte(raw), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Messages[0].Content) != 2 {
		t.Fatalf("expected 2 content blocks (reasoning + text), got %+v", got.Messages[0].Content)
	}
	if got.Messages[0].Content[0].Type != core.ContentReasoning || got.Messages[0].Content[0].Text != "Let me consider" {
		t.Errorf("reasoning block wrong: %+v", got.Messages[0].Content[0])
	}
	if got.Messages[0].Content[1].Type != core.ContentText || got.Messages[0].Content[1].Text != "final answer" {
		t.Errorf("text block wrong: %+v", got.Messages[0].Content[1])
	}
}

func TestAnthropic_StreamResponse_ToolUse(t *testing.T) {
	raw := strings.Join([]string{
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"u_x","name":"weather"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"SF\"}"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		``,
	}, "\n")

	n := NewAnthropicMessagesNormalizer()
	got, err := n.Normalize(context.Background(), []byte(raw), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Messages[0].Content) != 1 || got.Messages[0].Content[0].Type != core.ContentToolUse {
		t.Fatalf("expected single tool_use block, got %+v", got.Messages[0].Content)
	}
	tu := got.Messages[0].Content[0].ToolUse
	if tu.Name != "weather" || tu.CallID != "u_x" || tu.Input["city"] != "SF" {
		t.Errorf("tool_use wrong: %+v", tu)
	}
}

func TestAnthropic_EmptyBody(t *testing.T) {
	n := NewAnthropicMessagesNormalizer()
	for _, dir := range []core.Direction{core.DirectionRequest, core.DirectionResponse} {
		_, err := n.Normalize(context.Background(), nil, core.Meta{Direction: dir})
		if !errors.Is(err, core.ErrUnsupported) {
			t.Errorf("direction=%v: expected core.ErrUnsupported, got %v", dir, err)
		}
	}
}

func TestAnthropic_ID(t *testing.T) {
	if id := NewAnthropicMessagesNormalizer().ID(); id != "anthropic-messages" {
		t.Errorf("ID = %q", id)
	}
}

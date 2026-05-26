package codecs

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

func TestOpenAIChat_RequestSimple(t *testing.T) {
	body := `{"model":"gpt-4o","messages":[{"role":"system","content":"You are helpful."},{"role":"user","content":"Hi"}],"temperature":0.7}`
	n := NewOpenAIChatNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Kind != core.KindAIChat {
		t.Errorf("kind = %v, want %v", got.Kind, core.KindAIChat)
	}
	if got.Model != "gpt-4o" {
		t.Errorf("model = %q, want gpt-4o", got.Model)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(got.Messages))
	}
	if got.Messages[0].Role != core.RoleSystem || got.Messages[0].Content[0].Text != "You are helpful." {
		t.Errorf("system message wrong: %+v", got.Messages[0])
	}
	if got.Messages[1].Role != core.RoleUser || got.Messages[1].Content[0].Text != "Hi" {
		t.Errorf("user message wrong: %+v", got.Messages[1])
	}
	if got.Params == nil || got.Params.Temperature == nil || *got.Params.Temperature != 0.7 {
		t.Errorf("temperature missing: %+v", got.Params)
	}
}

func TestOpenAIChat_RequestWithTools(t *testing.T) {
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"weather?"}],"tools":[{"type":"function","function":{"name":"get_weather","description":"Get the weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]}`
	n := NewOpenAIChatNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Tools) != 1 || got.Tools[0].Name != "get_weather" {
		t.Fatalf("tools = %+v", got.Tools)
	}
	if got.Tools[0].ParametersJSONSchema["type"] != "object" {
		t.Errorf("parameters schema not preserved: %+v", got.Tools[0].ParametersJSONSchema)
	}
}

func TestOpenAIChat_RequestWithMultimodal(t *testing.T) {
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"https://example.com/cat.png"}}]}]}`
	n := NewOpenAIChatNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	blocks := got.Messages[0].Content
	if len(blocks) != 2 {
		t.Fatalf("content blocks = %d, want 2", len(blocks))
	}
	if blocks[0].Type != core.ContentText || blocks[0].Text != "describe" {
		t.Errorf("first block: %+v", blocks[0])
	}
	if blocks[1].Type != core.ContentImageRef || blocks[1].ImageRef == nil || blocks[1].ImageRef.SpillKey == "" {
		t.Errorf("image block: %+v", blocks[1])
	}
}

func TestOpenAIChat_RequestEmptyMessagesYieldsErrUnsupported(t *testing.T) {
	body := `{"model":"gpt-4o","messages":[]}`
	n := NewOpenAIChatNormalizer()
	_, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected core.ErrUnsupported, got %v", err)
	}
}

func TestOpenAIChat_RequestMalformedYieldsError(t *testing.T) {
	n := NewOpenAIChatNormalizer()
	_, err := n.Normalize(context.Background(), []byte("not json"), core.Meta{Direction: core.DirectionRequest})
	if err == nil {
		t.Fatal("expected error on malformed JSON")
	}
	if errors.Is(err, core.ErrUnsupported) {
		t.Errorf("malformed JSON should be a parse error, not core.ErrUnsupported: %v", err)
	}
}

func TestOpenAIChat_NonStreamResponseSimple(t *testing.T) {
	body := `{"model":"gpt-4o","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Hello!"}}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`
	n := NewOpenAIChatNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Messages) != 1 || got.Messages[0].Content[0].Text != "Hello!" {
		t.Errorf("response message wrong: %+v", got.Messages)
	}
	if got.FinishReason != "stop" {
		t.Errorf("finish_reason = %q", got.FinishReason)
	}
	if got.Usage == nil || got.Usage.PromptTokens == nil || *got.Usage.PromptTokens != 10 {
		t.Errorf("usage prompt tokens wrong: %+v", got.Usage)
	}
}

func TestOpenAIChat_NonStreamResponseWithToolUse(t *testing.T) {
	body := `{"model":"gpt-4o","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]}}]}`
	n := NewOpenAIChatNormalizer()
	got, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	blocks := got.Messages[0].Content
	if len(blocks) != 1 || blocks[0].Type != core.ContentToolUse {
		t.Fatalf("expected one tool_use block, got %+v", blocks)
	}
	if blocks[0].ToolUse.Name != "get_weather" || blocks[0].ToolUse.Input["city"] != "SF" {
		t.Errorf("tool_use wrong: %+v", blocks[0].ToolUse)
	}
}

func TestOpenAIChat_StreamResponseSimple(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
		``,
		`data: {"model":"gpt-4o","choices":[{"index":0,"delta":{"content":"Hi"}}]}`,
		``,
		`data: {"model":"gpt-4o","choices":[{"index":0,"delta":{"content":" there"},"finish_reason":"stop"}]}`,
		``,
		`data: {"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	n := NewOpenAIChatNormalizer()
	got, err := n.Normalize(context.Background(), []byte(raw), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("messages = %d", len(got.Messages))
	}
	if got.Messages[0].Content[0].Text != "Hi there" {
		t.Errorf("assembled text = %q", got.Messages[0].Content[0].Text)
	}
	if got.FinishReason != "stop" {
		t.Errorf("finish_reason = %q", got.FinishReason)
	}
	if got.Usage == nil || *got.Usage.PromptTokens != 5 || *got.Usage.CompletionTokens != 2 {
		t.Errorf("usage = %+v", got.Usage)
	}
	if !got.Stream {
		t.Errorf("Stream flag not set")
	}
}

func TestOpenAIChat_StreamResponseEmptyYieldsErrUnsupported(t *testing.T) {
	// Empty stream — only the [DONE] marker.
	raw := "data: [DONE]\n\n"
	n := NewOpenAIChatNormalizer()
	_, err := n.Normalize(context.Background(), []byte(raw), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected core.ErrUnsupported on empty stream, got %v", err)
	}
}

func TestOpenAIChat_EmptyBodyYieldsErrUnsupported(t *testing.T) {
	n := NewOpenAIChatNormalizer()
	for _, dir := range []core.Direction{core.DirectionRequest, core.DirectionResponse} {
		_, err := n.Normalize(context.Background(), nil, core.Meta{Direction: dir})
		if !errors.Is(err, core.ErrUnsupported) {
			t.Errorf("direction=%v: expected core.ErrUnsupported on empty body, got %v", dir, err)
		}
	}
}

func TestOpenAIChat_UnknownDirectionYieldsErrUnsupported(t *testing.T) {
	n := NewOpenAIChatNormalizer()
	_, err := n.Normalize(context.Background(), []byte(`{}`), core.Meta{Direction: core.Direction("ufo")})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected core.ErrUnsupported, got %v", err)
	}
}

func TestOpenAIChat_ID(t *testing.T) {
	if id := NewOpenAIChatNormalizer().ID(); id != "openai-chat" {
		t.Errorf("ID = %q, want openai-chat", id)
	}
}

// TestOpenAIChatStream_MultiToolCall_NoIndexFallsBackToZero verifies that a
// single streaming tool-call delta with no explicit `index` field is correctly
// placed at aggregation-map slot 0.  This is the baseline single-tool-call
// path; the fix must not regress it.
func TestOpenAIChatStream_MultiToolCall_NoIndexFallsBackToZero(t *testing.T) {
	// No "index" field present in either delta — falls back to pos=0 for both.
	raw := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"id":"call_A","type":"function","function":{"name":"get_weather","arguments":"{\"city\":"}}]}}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"\"Paris\"}"}}]},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	got, err := NewOpenAIChatNormalizer().Normalize(
		context.Background(), []byte(raw),
		core.Meta{Direction: core.DirectionResponse, Stream: true},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("messages count = %d, want 1", len(got.Messages))
	}
	blocks := got.Messages[0].Content
	if len(blocks) != 1 || blocks[0].Type != core.ContentToolUse {
		t.Fatalf("expected exactly one ContentToolUse block, got %+v", blocks)
	}
	tu := blocks[0].ToolUse
	if tu.Name != "get_weather" {
		t.Errorf("tool name = %q, want get_weather", tu.Name)
	}
	if tu.CallID != "call_A" {
		t.Errorf("call id = %q, want call_A", tu.CallID)
	}
	if tu.Input["city"] != "Paris" {
		t.Errorf("tool input city = %v, want Paris", tu.Input["city"])
	}
}

// TestOpenAIChatStream_MultiToolCall_TwoInterleavedExplicitIndex verifies that
// two interleaved tool-call deltas with explicit index=0 and index=1 are
// preserved as two separate ContentToolUse blocks in the final aggregated
// response.  Before the fix indexOfToolCall always returned 0, causing all
// tool-call deltas to be merged into the single slot 0 — silently discarding
// every second tool call.
func TestOpenAIChatStream_MultiToolCall_TwoInterleavedExplicitIndex(t *testing.T) {
	// OpenAI sends parallel tool calls with interleaved deltas keyed by index.
	// Chunk 1: both tool calls start (name fragments).
	// Chunk 2: both tool calls receive argument fragments.
	// Chunk 3: finish.
	raw := strings.Join([]string{
		// First chunk — two tool calls announced in the same delta.
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_0","type":"function","function":{"name":"get_weather","arguments":""}},{"index":1,"id":"call_1","type":"function","function":{"name":"get_time","arguments":""}}]}}]}`,
		``,
		// Second chunk — arguments for tool call 0.
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"Paris\"}"}}]}}]}`,
		``,
		// Third chunk — arguments for tool call 1.
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"tz\":\"Europe/Paris\"}"}}]},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	got, err := NewOpenAIChatNormalizer().Normalize(
		context.Background(), []byte(raw),
		core.Meta{Direction: core.DirectionResponse, Stream: true},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("messages count = %d, want 1", len(got.Messages))
	}
	blocks := got.Messages[0].Content
	// Must have exactly two ContentToolUse blocks — one per tool call.
	if len(blocks) != 2 {
		t.Fatalf("content blocks = %d, want 2 (one per tool call); got %+v", len(blocks), blocks)
	}
	if blocks[0].Type != core.ContentToolUse || blocks[1].Type != core.ContentToolUse {
		t.Fatalf("expected both blocks to be ContentToolUse; got types %v, %v", blocks[0].Type, blocks[1].Type)
	}
	tu0 := blocks[0].ToolUse
	tu1 := blocks[1].ToolUse
	if tu0.Name != "get_weather" {
		t.Errorf("tool[0] name = %q, want get_weather", tu0.Name)
	}
	if tu0.CallID != "call_0" {
		t.Errorf("tool[0] call_id = %q, want call_0", tu0.CallID)
	}
	if tu0.Input["city"] != "Paris" {
		t.Errorf("tool[0] input city = %v, want Paris", tu0.Input["city"])
	}
	if tu1.Name != "get_time" {
		t.Errorf("tool[1] name = %q, want get_time", tu1.Name)
	}
	if tu1.CallID != "call_1" {
		t.Errorf("tool[1] call_id = %q, want call_1", tu1.CallID)
	}
	if tu1.Input["tz"] != "Europe/Paris" {
		t.Errorf("tool[1] input tz = %v, want Europe/Paris", tu1.Input["tz"])
	}
}

// TestOpenAIChatStream_MultiToolCall_NonZeroBaseIndex verifies that a stream
// containing only tool-call deltas at index=2 (with no index=0 or index=1
// deltas first) is NOT merged into slot 0.  Before the fix indexOfToolCall
// always returned 0 regardless of the wire index, so this tool call would be
// incorrectly placed at slot 0 and its distinct identity (e.g. a third
// parallel tool call continuing from a prior stream segment) would be lost.
func TestOpenAIChatStream_MultiToolCall_NonZeroBaseIndex(t *testing.T) {
	// Only index=2 deltas — simulates a stream segment whose first two tool
	// calls (index=0 and index=1) were already flushed or belong to a prior
	// context window. The normalizer must honour the wire index rather than
	// forcing the slot to 0.
	raw := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":2,"id":"call_2","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"nexus\"}"}}]}}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	got, err := NewOpenAIChatNormalizer().Normalize(
		context.Background(), []byte(raw),
		core.Meta{Direction: core.DirectionResponse, Stream: true},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("messages count = %d, want 1", len(got.Messages))
	}
	blocks := got.Messages[0].Content
	if len(blocks) != 1 || blocks[0].Type != core.ContentToolUse {
		t.Fatalf("expected exactly one ContentToolUse block at wire index 2; got %+v", blocks)
	}
	tu := blocks[0].ToolUse
	if tu.Name != "lookup" {
		t.Errorf("tool name = %q, want lookup", tu.Name)
	}
	if tu.CallID != "call_2" {
		t.Errorf("call_id = %q, want call_2", tu.CallID)
	}
	if tu.Input["q"] != "nexus" {
		t.Errorf("input q = %v, want nexus", tu.Input["q"])
	}
}

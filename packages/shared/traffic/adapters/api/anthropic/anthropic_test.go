package anthropic

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

func TestAdapter_ID(t *testing.T) {
	a := &Adapter{}
	if a.ID() != "anthropic" {
		t.Errorf("ID = %q", a.ID())
	}
}

func TestAdapter_Configure(t *testing.T) {
	a := &Adapter{}
	if err := a.Configure(nil); err != nil {
		t.Errorf("Configure(nil) = %v", err)
	}
	if err := a.Configure(map[string]any{"foo": "bar"}); err != nil {
		t.Errorf("Configure(map) = %v", err)
	}
}

func TestExtractRequest_Basic(t *testing.T) {
	body := []byte(`{
		"model": "claude-3-opus-20240229",
		"messages": [
			{"role": "user", "content": "Hello, Claude!"},
			{"role": "assistant", "content": "Hi there!"}
		]
	}`)

	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/messages")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nc.Segments) != 2 {
		t.Fatalf("expected 2 segments, got %d", len(nc.Segments))
	}
	if nc.Segments[0] != "Hello, Claude!" {
		t.Errorf("segment[0] = %q", nc.Segments[0])
	}
	if nc.Segments[1] != "Hi there!" {
		t.Errorf("segment[1] = %q", nc.Segments[1])
	}
	if nc.Metadata["model"] != "claude-3-opus-20240229" {
		t.Errorf("model = %q", nc.Metadata["model"])
	}
}

func TestExtractRequest_SystemString(t *testing.T) {
	body := []byte(`{
		"model": "claude-3-sonnet-20240229",
		"system": "You are a helpful assistant.",
		"messages": [
			{"role": "user", "content": "Hi"}
		]
	}`)

	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/messages")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nc.Segments) != 2 {
		t.Fatalf("expected 2 segments (system + user), got %d", len(nc.Segments))
	}
	if nc.Segments[0] != "You are a helpful assistant." {
		t.Errorf("segment[0] = %q", nc.Segments[0])
	}
	if nc.Segments[1] != "Hi" {
		t.Errorf("segment[1] = %q", nc.Segments[1])
	}
}

func TestExtractRequest_SystemArray(t *testing.T) {
	body := []byte(`{
		"model": "claude-3-opus-20240229",
		"system": [
			{"type": "text", "text": "System part 1"},
			{"type": "text", "text": "System part 2"}
		],
		"messages": [
			{"role": "user", "content": "Hello"}
		]
	}`)

	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/messages")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nc.Segments) != 3 {
		t.Fatalf("expected 3 segments (2 system + 1 user), got %d", len(nc.Segments))
	}
	if nc.Segments[0] != "System part 1" {
		t.Errorf("segment[0] = %q", nc.Segments[0])
	}
}

func TestExtractRequest_ArrayContent(t *testing.T) {
	body := []byte(`{
		"model": "claude-3-opus-20240229",
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "Describe this image"},
				{"type": "image", "source": {"type": "base64", "data": "..."}}
			]}
		]
	}`)

	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/messages")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nc.Segments) != 1 {
		t.Fatalf("expected 1 text segment, got %d", len(nc.Segments))
	}
	if nc.Segments[0] != "Describe this image" {
		t.Errorf("segment = %q", nc.Segments[0])
	}
}

func TestExtractRequest_MalformedJSON(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`not json`), "/v1/messages")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("expected ErrMalformed, got %v", err)
	}
}

func TestExtractRequest_MissingMessages(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`{"model":"claude-3-opus-20240229"}`), "/v1/messages")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("expected ErrUnknownSchema, got %v", err)
	}
}

func TestExtractResponse(t *testing.T) {
	body := []byte(`{
		"content": [
			{"type": "text", "text": "Hello! How can I help you today?"},
			{"type": "text", "text": "I am Claude."}
		]
	}`)

	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/v1/messages")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nc.Segments) != 2 {
		t.Fatalf("expected 2 segments, got %d", len(nc.Segments))
	}
	if nc.Segments[0] != "Hello! How can I help you today?" {
		t.Errorf("segment[0] = %q", nc.Segments[0])
	}
}

func TestExtractResponse_MalformedJSON(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{bad`), "/v1/messages")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("expected ErrMalformed, got %v", err)
	}
}

func TestExtractResponse_MissingContent(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"id":"msg_123"}`), "/v1/messages")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("expected ErrUnknownSchema, got %v", err)
	}
}

func TestExtractStreamChunk_TextDelta(t *testing.T) {
	chunk := []byte(`{
		"type": "content_block_delta",
		"index": 0,
		"delta": {"type": "text_delta", "text": "Hello"}
	}`)

	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v1/messages")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Hello" {
		t.Errorf("segments = %v", nc.Segments)
	}
}

func TestExtractStreamChunk_NonContentEvent(t *testing.T) {
	chunk := []byte(`{"type": "message_start", "message": {"id": "msg_123"}}`)

	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v1/messages")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("expected 0 segments for non-content event, got %d", len(nc.Segments))
	}
}

func TestExtractStreamChunk_MalformedJSON(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractStreamChunk(context.Background(), []byte(`not json`), "/v1/messages")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("expected ErrMalformed, got %v", err)
	}
}

// TestExtractStreamChunk_ThinkingDelta covers the audit gap: extended-
// thinking text on content_block_delta with type:"thinking_delta" must
// land on ReasoningSegments, NOT on Segments — keeping reasoning out of
// the assistant's user-visible aggregate while still letting compliance
// hooks scan it via ReasoningSegments.
func TestExtractStreamChunk_ThinkingDelta(t *testing.T) {
	a := &Adapter{}
	chunk := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me work this out…"}}`)
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v1/messages")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments leaked thinking text: %v", nc.Segments)
	}
	if len(nc.ReasoningSegments) != 1 || nc.ReasoningSegments[0] != "Let me work this out…" {
		t.Errorf("ReasoningSegments=%v", nc.ReasoningSegments)
	}
}

// TestExtractRequest_ToolResultText covers the audit gap: tool_result
// content blocks (returned to the model from a previous tool call) carry
// text that may contain customer PII; the extractor must surface it on
// Segments so compliance hooks see it. Both string-content and
// array-content (text sub-parts) shapes are supported.
func TestExtractRequest_ToolResultText(t *testing.T) {
	t.Run("string_content", func(t *testing.T) {
		body := []byte(`{
			"model":"claude-3-5-sonnet-20241022",
			"messages":[
				{"role":"user","content":[{"type":"text","text":"hi"}]},
				{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"f","input":{}}]},
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"customer SSN 123-45-6789"}]}
			]
		}`)
		a := &Adapter{}
		nc, err := a.ExtractRequest(context.Background(), body, "/v1/messages")
		if err != nil {
			t.Fatal(err)
		}
		// Segments expected: ["hi", "customer SSN 123-45-6789"].
		// (assistant tool_use carries no text — skipped.)
		if len(nc.Segments) != 2 || nc.Segments[1] != "customer SSN 123-45-6789" {
			t.Fatalf("Segments=%v want [hi, customer SSN ...]", nc.Segments)
		}
	})
	t.Run("array_content_with_text_subparts", func(t *testing.T) {
		body := []byte(`{
			"messages":[
				{"role":"user","content":[
					{"type":"tool_result","tool_use_id":"t","content":[
						{"type":"text","text":"line A"},
						{"type":"image","source":{"type":"base64","media_type":"image/png","data":"x"}},
						{"type":"text","text":"line B"}
					]}
				]}
			]
		}`)
		a := &Adapter{}
		nc, err := a.ExtractRequest(context.Background(), body, "/v1/messages")
		if err != nil {
			t.Fatal(err)
		}
		if len(nc.Segments) != 2 || nc.Segments[0] != "line A" || nc.Segments[1] != "line B" {
			t.Fatalf("Segments=%v want [line A, line B]", nc.Segments)
		}
	})
}

// TestExtractResponse_ThinkingBlock pins the non-stream side: a
// content[].type=thinking block reaches ReasoningSegments verbatim.
// content[].type=text continues to land on Segments.
func TestExtractResponse_ThinkingBlock(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{
		"id":"msg_1","type":"message","role":"assistant",
		"content":[
			{"type":"thinking","thinking":"Internal trace …"},
			{"type":"text","text":"The answer is 42."}
		],
		"stop_reason":"end_turn"
	}`)
	nc, err := a.ExtractResponse(context.Background(), body, "/v1/messages")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "The answer is 42." {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if len(nc.ReasoningSegments) != 1 || nc.ReasoningSegments[0] != "Internal trace …" {
		t.Errorf("ReasoningSegments=%v", nc.ReasoningSegments)
	}
}

// Tool-use coverage (compliance-critical: assistant tool_use blocks must
// reach hooks for MCP tool detection, PII in tool arguments, and audit
// accounting separate from text completions).

// TestExtractRequest_AssistantToolUseInHistory pins that an assistant
// turn with a tool_use block carried in conversation history surfaces
// the invocation on ToolCallSegments — without this, hooks scanning
// prior tool arguments for sensitive data lose the data.
func TestExtractRequest_AssistantToolUseInHistory(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-6",
		"messages": [
			{"role":"user","content":"what is the weather in NYC?"},
			{"role":"assistant","content":[
				{"type":"tool_use","id":"toolu_abc","name":"get_weather","input":{"city":"NYC"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_abc","content":"72F sunny"}
			]}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/messages")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 {
		t.Fatalf("ToolCallSegments len=%d want 1", len(nc.ToolCallSegments))
	}
	if !strings.Contains(nc.ToolCallSegments[0], `"get_weather"`) {
		t.Errorf("ToolCallSegments[0]=%q missing tool name", nc.ToolCallSegments[0])
	}
	if !strings.Contains(nc.ToolCallSegments[0], `"NYC"`) {
		t.Errorf("ToolCallSegments[0]=%q missing arguments", nc.ToolCallSegments[0])
	}
	// tool_result text still goes to Segments alongside.
	found := false
	for _, s := range nc.Segments {
		if s == "72F sunny" {
			found = true
		}
	}
	if !found {
		t.Errorf("tool_result text missing from Segments: %v", nc.Segments)
	}
}

// TestExtractRequest_ToolDefinitionsInMetadata pins that the top-level
// `tools` array (definitions, not invocations) lands in Metadata so
// audit can record what scope the model was given.
func TestExtractRequest_ToolDefinitionsInMetadata(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-6",
		"messages": [{"role":"user","content":"hi"}],
		"tools": [
			{"name":"get_weather","description":"Get weather","input_schema":{"type":"object"}}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/messages")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(nc.Metadata["tools"], `"get_weather"`) {
		t.Errorf("Metadata[tools]=%q missing definition", nc.Metadata["tools"])
	}
	if len(nc.ToolCallSegments) != 0 {
		t.Errorf("definitions must not leak to ToolCallSegments: %v", nc.ToolCallSegments)
	}
}

// TestExtractRequest_McpServersInMetadata pins Anthropic's native MCP
// integration: the `mcp_servers` array in the request body lists the
// MCP servers connected for this conversation. Audit records this as
// the MCP scope of the request.
func TestExtractRequest_McpServersInMetadata(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-6",
		"messages": [{"role":"user","content":"hi"}],
		"mcp_servers": [
			{"type":"url","url":"https://mcp.example.com/sse","name":"example-mcp"}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/messages")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(nc.Metadata["mcp_servers"], `"example-mcp"`) {
		t.Errorf("Metadata[mcp_servers]=%q missing entry", nc.Metadata["mcp_servers"])
	}
}

// TestExtractRequest_Extra covers the safety-net path: novel top-level
// fields land in Extra so a future Anthropic spec addition still
// reaches compliance hooks.
func TestExtractRequest_Extra(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-6",
		"messages": [{"role":"user","content":"hi"}],
		"x_future_audit_payload": {"sensitive": "must reach hooks"}
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/messages")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	x, ok := nc.Extra["x_future_audit_payload"]
	if !ok || !strings.Contains(x, "sensitive") {
		t.Errorf("Extra=%v missing x_future_audit_payload", nc.Extra)
	}
	for _, known := range []string{"model", "messages", "tools", "mcp_servers"} {
		if _, ok := nc.Extra[known]; ok {
			t.Errorf("Extra leaked known key %q", known)
		}
	}
}

// TestExtractResponse_ToolUseBlock covers an assistant response that
// contains a tool_use block alongside (or instead of) text. The
// invocation lands on ToolCallSegments.
func TestExtractResponse_ToolUseBlock(t *testing.T) {
	body := []byte(`{
		"id":"msg_2","type":"message","role":"assistant","model":"claude-sonnet-4-6",
		"content":[
			{"type":"text","text":"Looking up the weather …"},
			{"type":"tool_use","id":"toolu_xyz","name":"get_weather","input":{"city":"NYC"}}
		],
		"stop_reason":"tool_use"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/v1/messages")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Looking up the weather …" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"get_weather"`) {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
	if nc.Metadata["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason=%q", nc.Metadata["stop_reason"])
	}
	if nc.Metadata["model"] != "claude-sonnet-4-6" {
		t.Errorf("model=%q", nc.Metadata["model"])
	}
	if nc.Metadata["id"] != "msg_2" {
		t.Errorf("id=%q", nc.Metadata["id"])
	}
}

// TestExtractResponse_StopReasonAndSequence pins terminal-state
// metadata: stop_reason ("end_turn" / "max_tokens" / "stop_sequence" /
// "tool_use" / "pause_turn" / "refusal") and stop_sequence string when
// the model hit a configured stop word.
func TestExtractResponse_StopReasonAndSequence(t *testing.T) {
	body := []byte(`{
		"content":[{"type":"text","text":"final."}],
		"stop_reason":"stop_sequence",
		"stop_sequence":"END"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/v1/messages")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if nc.Metadata["stop_reason"] != "stop_sequence" {
		t.Errorf("stop_reason=%q", nc.Metadata["stop_reason"])
	}
	if nc.Metadata["stop_sequence"] != "END" {
		t.Errorf("stop_sequence=%q", nc.Metadata["stop_sequence"])
	}
}

// TestExtractResponse_Extra covers the response-side defence-in-depth.
func TestExtractResponse_Extra(t *testing.T) {
	body := []byte(`{
		"content":[{"type":"text","text":"hi"}],
		"x_future_response_field":{"new":"audit"}
	}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/v1/messages")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if _, ok := nc.Extra["x_future_response_field"]; !ok {
		t.Errorf("Extra missing x_future_response_field: %v", nc.Extra)
	}
}

// TestExtractStreamChunk_ContentBlockStart_ToolUse covers the initial
// frame announcing a tool_use content block. The id + name + initial
// (possibly empty) input must reach ToolCallSegments so hooks see the
// invocation start, even before subsequent input_json_delta frames
// stream the arguments.
func TestExtractStreamChunk_ContentBlockStart_ToolUse(t *testing.T) {
	chunk := []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v1/messages")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"get_weather"`) {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
}

// TestExtractStreamChunk_InputJsonDelta covers tool-argument streaming.
// Each delta carries a partial_json fragment; downstream pipeline
// accumulates fragments to reconstruct the full arguments JSON.
func TestExtractStreamChunk_InputJsonDelta(t *testing.T) {
	chunk := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":\"NY"}}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v1/messages")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 {
		t.Fatalf("ToolCallSegments len=%d", len(nc.ToolCallSegments))
	}
	if !strings.Contains(nc.ToolCallSegments[0], `partial_json`) {
		t.Errorf("ToolCallSegments[0]=%q missing partial_json fragment", nc.ToolCallSegments[0])
	}
}

// TestExtractStreamChunk_MessageDeltaStopReason pins that message_delta
// events carry the terminal stop_reason / stop_sequence into Metadata.
func TestExtractStreamChunk_MessageDeltaStopReason(t *testing.T) {
	chunk := []byte(`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":12}}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v1/messages")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if nc.Metadata["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason=%q", nc.Metadata["stop_reason"])
	}
}

// TestExtractStreamChunk_NoMetadataAllocOnEmpty pins the optimisation
// that empty Metadata stays nil rather than allocating an empty map —
// the streaming hot path is sensitive to per-chunk allocation.
func TestExtractStreamChunk_NoMetadataAllocOnEmpty(t *testing.T) {
	chunk := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v1/messages")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if nc.Metadata != nil {
		t.Errorf("Metadata must be nil for empty-meta delta; got %v", nc.Metadata)
	}
}

// TestExtractStreamChunk_ContentBlockStart_TextSkipped pins that a
// content_block_start whose block is text (not tool_use) does not leak
// into ToolCallSegments — only tool_use blocks announce tool calls.
func TestExtractStreamChunk_ContentBlockStart_TextSkipped(t *testing.T) {
	chunk := []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v1/messages")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 0 {
		t.Errorf("ToolCallSegments must stay empty for text block start: %v", nc.ToolCallSegments)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments must stay empty for text block start: %v", nc.Segments)
	}
}

package canonicalbridge

import (
	"context"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// textChunk returns a canonical text-delta chunk.
func textChunk(delta string) provcore.Chunk {
	return provcore.Chunk{Delta: delta}
}

// doneChunk returns a canonical end-of-stream chunk with optional usage.
func doneChunk(prompt, completion int) provcore.Chunk {
	p, c := prompt, completion
	total := p + c
	return provcore.Chunk{
		Done: true,
		Usage: &provcore.Usage{
			PromptTokens:     &p,
			CompletionTokens: &c,
			TotalTokens:      &total,
		},
	}
}

// --- openAIStreamEncoder ---

func TestOpenAIStreamEncoder_TextDelta(t *testing.T) {
	enc := newOpenAIStreamEncoder("claude-opus-4-7")
	ctx := context.Background()

	// First Write emits a role-assignment chunk followed by the content chunk.
	b, err := enc.Write(ctx, textChunk("hello"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	// Must carry all required OpenAI envelope fields.
	for _, want := range []string{`"object":"chat.completion.chunk"`, `"model":"claude-opus-4-7"`, `"id":"chatcmpl-`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing envelope field %q in %q", want, s)
		}
	}
	if !strings.Contains(s, `"content":"hello"`) {
		t.Errorf("expected content:hello in %q", s)
	}
	if !strings.HasPrefix(s, "data: ") {
		t.Errorf("expected SSE data: prefix, got %q", s)
	}
}

func TestOpenAIStreamEncoder_DoneEmitsFinishReason(t *testing.T) {
	enc := newOpenAIStreamEncoder("claude-opus-4-7")
	ctx := context.Background()
	// Trigger headerSent so the first Write below is not the role-assignment chunk.
	enc.headerSent = true

	b, err := enc.Write(ctx, doneChunk(10, 5))
	if err != nil {
		t.Fatal(err)
	}
	// Done must emit a finish_reason=stop chunk (LivePipeline separately appends [DONE]).
	s := string(b)
	if !strings.Contains(s, `"finish_reason":"stop"`) {
		t.Errorf("Done chunk must contain finish_reason:stop; got %q", s)
	}
	if !strings.Contains(s, `"object":"chat.completion.chunk"`) {
		t.Errorf("Done chunk must carry OpenAI envelope; got %q", s)
	}
}

func TestOpenAIStreamEncoder_ToolCallDelta(t *testing.T) {
	enc := newOpenAIStreamEncoder("claude-opus-4-7")
	chunk := provcore.Chunk{
		ToolCallDeltas: []provcore.ToolCallDelta{
			{Index: 0, ID: "tc_1", Name: "my_func", Arguments: `{"x":1}`},
		},
	}
	b, _ := enc.Write(context.Background(), chunk)
	s := string(b)
	if !strings.Contains(s, "tool_calls") {
		t.Errorf("expected tool_calls in %q", s)
	}
	if !strings.Contains(s, "my_func") {
		t.Errorf("expected function name my_func in %q", s)
	}
	if !strings.Contains(s, `"object":"chat.completion.chunk"`) {
		t.Errorf("tool call chunk must carry OpenAI envelope; got %q", s)
	}
}

// --- anthropicStreamEncoder ---

func TestAnthropicStreamEncoder_FullSequence(t *testing.T) {
	enc := newAnthropicStreamEncoder()
	ctx := context.Background()

	// First chunk: text delta — should trigger message_start + ping + content_block_start + content_block_delta
	b, err := enc.Write(ctx, textChunk("Hello"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "message_start") {
		t.Errorf("first chunk should emit message_start; got %q", s)
	}
	if !strings.Contains(s, "ping") {
		t.Errorf("first chunk should emit ping; got %q", s)
	}
	if !strings.Contains(s, "content_block_start") {
		t.Errorf("first text delta should emit content_block_start; got %q", s)
	}
	if !strings.Contains(s, "text_delta") {
		t.Errorf("should emit text_delta; got %q", s)
	}
	if !strings.Contains(s, "Hello") {
		t.Errorf("should include delta text; got %q", s)
	}

	// Second chunk: more text — only content_block_delta, no message_start/ping.
	b2, _ := enc.Write(ctx, textChunk(" world"))
	s2 := string(b2)
	if strings.Contains(s2, "message_start") {
		t.Errorf("second chunk must not re-emit message_start; got %q", s2)
	}
	if !strings.Contains(s2, "text_delta") {
		t.Errorf("second chunk should have text_delta; got %q", s2)
	}

	// Done chunk: content_block_stop + message_delta + message_stop.
	b3, _ := enc.Write(ctx, doneChunk(10, 5))
	s3 := string(b3)
	if !strings.Contains(s3, "content_block_stop") {
		t.Errorf("done should emit content_block_stop; got %q", s3)
	}
	if !strings.Contains(s3, "message_delta") {
		t.Errorf("done should emit message_delta; got %q", s3)
	}
	if !strings.Contains(s3, "message_stop") {
		t.Errorf("done should emit message_stop; got %q", s3)
	}
	if !strings.Contains(s3, "end_turn") {
		t.Errorf("done should include end_turn stop_reason; got %q", s3)
	}
}

func TestAnthropicStreamEncoder_ToolCall(t *testing.T) {
	enc := newAnthropicStreamEncoder()
	ctx := context.Background()

	toolChunk := provcore.Chunk{
		ToolCallDeltas: []provcore.ToolCallDelta{
			{Index: 0, ID: "tc_abc", Name: "get_weather", Arguments: `{"city":"NYC"}`},
		},
	}
	b, _ := enc.Write(ctx, toolChunk)
	s := string(b)
	if !strings.Contains(s, "tool_use") {
		t.Errorf("expected tool_use content_block_start; got %q", s)
	}
	if !strings.Contains(s, "get_weather") {
		t.Errorf("expected tool name in output; got %q", s)
	}
	if !strings.Contains(s, "input_json_delta") {
		t.Errorf("expected input_json_delta for arguments; got %q", s)
	}

	// Done without text block open — no content_block_stop for text.
	b2, _ := enc.Write(ctx, doneChunk(5, 3))
	s2 := string(b2)
	if !strings.Contains(s2, "message_stop") {
		t.Errorf("done must always emit message_stop; got %q", s2)
	}
}

func TestAnthropicStreamEncoder_HeaderSentOnce(t *testing.T) {
	enc := newAnthropicStreamEncoder()
	ctx := context.Background()
	// Emit two text chunks; the SSE "event: message_start" line must only appear in the first.
	// (The string "message_start" also appears inside the data JSON payload, so we match the
	// event: line to count only the header occurrences.)
	b1, _ := enc.Write(ctx, textChunk("a"))
	b2, _ := enc.Write(ctx, textChunk("b"))
	combined := string(b1) + string(b2)
	if strings.Count(combined, "event: message_start") != 1 {
		t.Errorf("SSE event: message_start line should appear exactly once; got:\n%s", combined)
	}
	if strings.Contains(string(b2), "event: message_start") {
		t.Errorf("second chunk must not re-emit message_start event; got %q", b2)
	}
}

// --- geminiStreamEncoder ---

func TestGeminiStreamEncoder_TextDelta(t *testing.T) {
	enc := &geminiStreamEncoder{}
	b, _ := enc.Write(context.Background(), textChunk("hi"))
	s := string(b)
	if !strings.Contains(s, `"text":"hi"`) {
		t.Errorf("expected text:hi in Gemini frame; got %q", s)
	}
	if !strings.Contains(s, "candidates") {
		t.Errorf("expected candidates array; got %q", s)
	}
}

func TestGeminiStreamEncoder_Done(t *testing.T) {
	enc := &geminiStreamEncoder{}
	b, _ := enc.Write(context.Background(), doneChunk(8, 4))
	s := string(b)
	if !strings.Contains(s, "STOP") {
		t.Errorf("done frame should have finishReason STOP; got %q", s)
	}
	if !strings.Contains(s, "usageMetadata") {
		t.Errorf("done frame should carry usageMetadata; got %q", s)
	}
}

// --- cohereStreamEncoder ---

func TestCohereStreamEncoder_Sequence(t *testing.T) {
	enc := &cohereStreamEncoder{}
	ctx := context.Background()

	b1, _ := enc.Write(ctx, textChunk("hello"))
	s1 := string(b1)
	if !strings.Contains(s1, "message-start") {
		t.Errorf("first chunk should include message-start; got %q", s1)
	}
	if !strings.Contains(s1, "content-start") {
		t.Errorf("first text delta should include content-start; got %q", s1)
	}
	if !strings.Contains(s1, "content-delta") {
		t.Errorf("should include content-delta; got %q", s1)
	}

	b2, _ := enc.Write(ctx, doneChunk(5, 2))
	s2 := string(b2)
	if !strings.Contains(s2, "content-end") {
		t.Errorf("done should emit content-end; got %q", s2)
	}
	if !strings.Contains(s2, "message-end") {
		t.Errorf("done should emit message-end; got %q", s2)
	}
	if !strings.Contains(s2, "COMPLETE") {
		t.Errorf("message-end should have COMPLETE finish_reason; got %q", s2)
	}
}

// --- replicateStreamEncoder ---

func TestReplicateStreamEncoder_TextDelta(t *testing.T) {
	enc := &replicateStreamEncoder{}
	b, _ := enc.Write(context.Background(), textChunk("token"))
	s := string(b)
	if !strings.Contains(s, "event: output") {
		t.Errorf("expected event:output; got %q", s)
	}
	if !strings.Contains(s, "token") {
		t.Errorf("expected token text in data; got %q", s)
	}
}

func TestReplicateStreamEncoder_Done(t *testing.T) {
	enc := &replicateStreamEncoder{}
	b, _ := enc.Write(context.Background(), doneChunk(3, 1))
	s := string(b)
	if !strings.Contains(s, "event: done") {
		t.Errorf("expected event:done; got %q", s)
	}
}

// TestNewChatCompletionsStreamEncoder pins the public accessor
// that returns the chat-completions SSE encoder directly (NOT via
// NewStreamTranscoder). The handler's auto-upgrade hook uses this
// because (ingress=FormatOpenAI, target=FormatOpenAI) would otherwise
// pick nil (passthrough), but the upstream is producing Responses SSE
// (parsed by openai.responsesStreamSession into canonical chunks)
// that must be re-encoded into chat-completions SSE for the client.
func TestNewChatCompletionsStreamEncoder(t *testing.T) {
	enc := NewChatCompletionsStreamEncoder("gpt-5.2")
	if enc == nil {
		t.Fatal("NewChatCompletionsStreamEncoder returned nil")
	}
	b, err := enc.Write(context.Background(), provcore.Chunk{Delta: "hello"})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !strings.Contains(string(b), `"content":"hello"`) {
		t.Errorf("chat-completions encoder did not emit content delta; got %q", string(b))
	}
	if !strings.Contains(string(b), `"object":"chat.completion.chunk"`) {
		t.Errorf("chat-completions encoder did not emit chat.completion.chunk envelope; got %q", string(b))
	}
}

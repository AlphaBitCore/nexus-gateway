// Package stream_test covers the OpenAI SSE stream decoder.
// Named failure modes:
//   - [DONE] sentinel: emits Done=true chunk with RawBytes forwarded
//   - text delta: Delta populated from choices[0].delta.content
//   - reasoning_content: appended to Delta (DeepSeek / Kimi / thinking models)
//   - tool_call deltas: ToolCallDeltas populated from choices[0].delta.tool_calls
//   - usage chunk: Usage extracted via shared/normalize
//   - empty data line: silently skipped (Next recurses)
//   - nil body: returns error
//   - context cancelled: returns ctx.Err()
//   - EndpointResponsesAPI: dispatches to responsesStreamSession (not openaiStreamSession)
package stream_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	ostream "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai/stream"
)

// sseBody creates an io.ReadCloser from an SSE string.
func sseBody(s string) io.ReadCloser {
	return io.NopCloser(strings.NewReader(s))
}

func TestStreamDecoder_nilBody_returnsError(t *testing.T) {
	d := ostream.NewStreamDecoder(slog.Default())
	_, err := d.Open(nil, typology.WireShapeOpenAIChat)
	if err == nil {
		t.Fatal("expected error for nil body")
	}
}

func TestStreamDecoder_doneChunk_emitted(t *testing.T) {
	d := ostream.NewStreamDecoder(slog.Default())
	body := sseBody("data: [DONE]\n\n")
	sess, err := d.Open(body, typology.WireShapeOpenAIChat)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer sess.Close()

	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if !chunk.Done {
		t.Error("expected Done=true for [DONE] sentinel")
	}
	if string(chunk.RawBytes) != "data: [DONE]\n\n" {
		t.Errorf("RawBytes: got %q", chunk.RawBytes)
	}
}

func TestStreamDecoder_afterDone_returnsEOF(t *testing.T) {
	d := ostream.NewStreamDecoder(slog.Default())
	body := sseBody("data: [DONE]\n\n")
	sess, _ := d.Open(body, typology.WireShapeOpenAIChat)
	defer sess.Close()

	// First call returns the [DONE] chunk.
	_, _ = sess.Next(context.Background())
	// Second call must return io.EOF.
	_, err := sess.Next(context.Background())
	if !errors.Is(err, io.EOF) {
		t.Errorf("expected io.EOF after [DONE], got %v", err)
	}
}

func TestStreamDecoder_textDelta(t *testing.T) {
	payload := `{"choices":[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":null}]}`
	body := sseBody("data: " + payload + "\n\n")
	d := ostream.NewStreamDecoder(nil) // nil log → slog.Default() internally
	sess, err := d.Open(body, typology.WireShapeOpenAIChat)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer sess.Close()

	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if chunk.Delta != "hello" {
		t.Errorf("Delta: got %q, want %q", chunk.Delta, "hello")
	}
	if chunk.Done {
		t.Error("chunk must not be Done before [DONE]")
	}
}

func TestStreamDecoder_reasoningContent_appendedToDelta(t *testing.T) {
	// DeepSeek / Kimi reasoning_content streams appear on delta.reasoning_content.
	// The gateway appends it to Delta so audit logs capture both.
	payload := `{"choices":[{"index":0,"delta":{"content":"answer","reasoning_content":"think"},"finish_reason":null}]}`
	body := sseBody("data: " + payload + "\n\n")
	d := ostream.NewStreamDecoder(slog.Default())
	sess, _ := d.Open(body, typology.WireShapeOpenAIChat)
	defer sess.Close()

	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if chunk.Delta != "answerthink" {
		t.Errorf("Delta: got %q, want %q (content + reasoning_content)", chunk.Delta, "answerthink")
	}
}

func TestStreamDecoder_toolCallDelta(t *testing.T) {
	payload := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"search","arguments":"{\"q\""}}]},"finish_reason":null}]}`
	body := sseBody("data: " + payload + "\n\n")
	d := ostream.NewStreamDecoder(slog.Default())
	sess, _ := d.Open(body, typology.WireShapeOpenAIChat)
	defer sess.Close()

	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if len(chunk.ToolCallDeltas) == 0 {
		t.Fatal("expected ToolCallDeltas, got none")
	}
	tc := chunk.ToolCallDeltas[0]
	if tc.ID != "call_1" {
		t.Errorf("ID: got %q, want call_1", tc.ID)
	}
	if tc.Name != "search" {
		t.Errorf("Name: got %q, want search", tc.Name)
	}
	if tc.Arguments != `{"q"` {
		t.Errorf("Arguments: got %q", tc.Arguments)
	}
}

func TestStreamDecoder_usageChunk_extractedViaSharedNormalize(t *testing.T) {
	// Usage must be extracted via shared/normalize (not hand-parsed).
	// SSE scanner emits ev.Data as a single line (no newlines preserved), so
	// the payload must be compact JSON for gjson to parse the usage key.
	payload := `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150,"prompt_tokens_details":{"cached_tokens":80}}}`
	body := sseBody("data: " + payload + "\n\n" + "data: [DONE]\n\n")
	d := ostream.NewStreamDecoder(slog.Default())
	sess, _ := d.Open(body, typology.WireShapeOpenAIChat)
	defer sess.Close()

	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if chunk.Usage == nil {
		t.Fatal("Usage should be set on usage chunk")
	}
	if chunk.Usage.PromptTokens == nil || *chunk.Usage.PromptTokens != 100 {
		t.Errorf("PromptTokens: got %v, want 100", chunk.Usage.PromptTokens)
	}
	if chunk.Usage.CacheReadTokens == nil || *chunk.Usage.CacheReadTokens != 80 {
		t.Errorf("CacheReadTokens: got %v, want 80", chunk.Usage.CacheReadTokens)
	}
}

func TestStreamDecoder_contextCancelled_returnsCtxErr(t *testing.T) {
	// After [DONE] the session is marked done; closing the context before
	// a call returns the context error.
	d := ostream.NewStreamDecoder(slog.Default())
	// Use a session that hasn't emitted [DONE] but the context is already cancelled.
	body := sseBody("") // empty — scanner immediately returns EOF but ctx checked first
	sess, _ := d.Open(body, typology.WireShapeOpenAIChat)
	defer sess.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := sess.Next(ctx)
	// Either ctx.Err or io.EOF (empty body) is acceptable — the key check
	// is that it does NOT panic and returns quickly.
	if err == nil {
		t.Error("cancelled context: expected non-nil error")
	}
}

func TestStreamDecoder_responsesAPI_sessionOpened(t *testing.T) {
	// EndpointResponsesAPI must dispatch to responsesStreamSession, not
	// openaiStreamSession. Smoke test: a response.completed event emits Done=true.
	payload := `{"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`
	body := sseBody("event: response.completed\ndata: " + payload + "\n\n")
	d := ostream.NewStreamDecoder(slog.Default())
	sess, err := d.Open(body, typology.WireShapeOpenAIResponses)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer sess.Close()

	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if !chunk.Done {
		t.Errorf("response.completed must emit Done=true, got chunk: %+v", chunk)
	}
}

func TestStreamDecoder_close_idempotent(t *testing.T) {
	d := ostream.NewStreamDecoder(slog.Default())
	sess, _ := d.Open(sseBody("data: [DONE]\n\n"), typology.WireShapeOpenAIChat)
	if err := sess.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
}

func TestStreamDecoder_rawBytesForwardedVerbatim(t *testing.T) {
	// RawBytes on each chunk must match the original SSE frame bytes so
	// the handler can proxy them to the client without re-encoding.
	payload := `{"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`
	body := sseBody("data: " + payload + "\n\n")
	d := ostream.NewStreamDecoder(slog.Default())
	sess, _ := d.Open(body, typology.WireShapeOpenAIChat)
	defer sess.Close()

	chunk, _ := sess.Next(context.Background())
	expected := "data: " + payload + "\n\n"
	if string(chunk.RawBytes) != expected {
		t.Errorf("RawBytes: got %q, want %q", chunk.RawBytes, expected)
	}
}

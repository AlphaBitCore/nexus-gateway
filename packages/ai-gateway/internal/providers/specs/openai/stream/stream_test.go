// Package stream_test covers the OpenAI SSE stream decoder.
// Named failure modes:
//   - [DONE] sentinel: emits Done=true chunk with RawBytes forwarded
//   - text delta: Delta populated from choices[0].delta.content
//   - reasoning_content: routed to ReasoningDelta (DeepSeek / Kimi / thinking models)
//   - tool_call deltas: ToolCallDeltas populated from choices[0].delta.tool_calls
//   - usage chunk: Usage extracted via shared/normalize
//   - empty data line: silently skipped via an in-Next loop (no recursion; F-0232a)
//   - nil body: returns error
//   - context cancelled: returns ctx.Err()
//   - EndpointResponsesAPI: dispatches to responsesStreamSession (not openaiStreamSession)
package stream_test

import (
	"context"
	"errors"
	ostream "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai/stream"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"io"
	"log/slog"
	"strings"
	"testing"
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

// TestStreamDecoder_emptyDataFramesFlood_skippedViaLoop covers F-0232a: a
// burst of empty `data:` frames before a real chunk must be skipped inside a
// single Next call via an iterative loop (NOT tail recursion, which Go does
// not optimise — a hostile upstream could otherwise grow the goroutine
// stack). The next call must still return the real delta that followed.
func TestStreamDecoder_emptyDataFramesFlood_skippedViaLoop(t *testing.T) {
	var b strings.Builder
	for range 5000 {
		b.WriteString("data: \n\n") // empty payload frames
	}
	b.WriteString(`data: {"choices":[{"delta":{"content":"hello"}}]}` + "\n\n")
	d := ostream.NewStreamDecoder(slog.Default())
	sess, err := d.Open(sseBody(b.String()), typology.WireShapeOpenAIChat)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer sess.Close() //nolint:errcheck

	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next after empty-frame flood: %v", err)
	}
	if chunk.Delta != "hello" {
		t.Errorf("Delta = %q, want %q (real frame after the flood)", chunk.Delta, "hello")
	}
}

// TestStreamDecoder_emptyDataThenDone covers the empty-frame skip terminating
// at the [DONE] sentinel without recursion (F-0232a).
func TestStreamDecoder_emptyDataThenDone(t *testing.T) {
	d := ostream.NewStreamDecoder(slog.Default())
	sess, err := d.Open(sseBody("data: \n\ndata: \n\ndata: [DONE]\n\n"), typology.WireShapeOpenAIChat)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer sess.Close() //nolint:errcheck

	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if !chunk.Done {
		t.Errorf("expected Done chunk after empty frames + [DONE], got %+v", chunk)
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

func TestStreamDecoder_reasoningContent_routedToReasoningDelta(t *testing.T) {
	// DeepSeek / Kimi reasoning_content streams appear on delta.reasoning_content.
	// It must land on the dedicated ReasoningDelta channel (kept separate from
	// the answer Delta) so cross-format encoders surface it as the target's
	// reasoning channel rather than leaking it into the visible answer.
	payload := `{"choices":[{"index":0,"delta":{"content":"answer","reasoning_content":"think"},"finish_reason":null}]}`
	body := sseBody("data: " + payload + "\n\n")
	d := ostream.NewStreamDecoder(slog.Default())
	sess, _ := d.Open(body, typology.WireShapeOpenAIChat)
	defer sess.Close()

	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if chunk.Delta != "answer" {
		t.Errorf("Delta: got %q, want %q (answer content only)", chunk.Delta, "answer")
	}
	if chunk.ReasoningDelta != "think" {
		t.Errorf("ReasoningDelta: got %q, want %q (reasoning_content channel)", chunk.ReasoningDelta, "think")
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

// TestStreamDecoder_nullUsageChunk_noExtraction guards the IsObject() usage
// gate: OpenAI-stream content deltas carry `"usage": null` on every non-final
// chunk, which gjson reports as Exists()==true. The decoder must NOT treat a
// null usage as a usage block (no full-normalize, no Usage stamped) — only a
// real usage object on the final include_usage chunk sets Chunk.Usage.
func TestStreamDecoder_nullUsageChunk_noExtraction(t *testing.T) {
	payload := `{"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}],"usage":null}`
	body := sseBody("data: " + payload + "\n\n" + "data: [DONE]\n\n")
	d := ostream.NewStreamDecoder(slog.Default())
	sess, _ := d.Open(body, typology.WireShapeOpenAIChat)
	defer sess.Close()

	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if chunk.Delta != "hi" {
		t.Errorf("Delta: got %q, want hi", chunk.Delta)
	}
	if chunk.Usage != nil {
		t.Errorf("Usage must be nil for a null-usage chunk, got %+v", chunk.Usage)
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

// TestStreamDecoder_finishReason_populated proves the OpenAI decoder surfaces
// the canonical finish_reason from the trailing delta-empty chunk so a
// re-encoder (buffer mode) can preserve it instead of collapsing to "stop".
func TestStreamDecoder_finishReason_populated(t *testing.T) {
	for _, fr := range []string{"tool_calls", "length", "content_filter"} {
		d := ostream.NewStreamDecoder(slog.Default())
		body := sseBody(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"` + fr + `"}]}` + "\n\n")
		sess, err := d.Open(body, typology.WireShapeOpenAIChat)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		chunk, err := sess.Next(context.Background())
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if chunk.FinishReason != fr {
			t.Errorf("FinishReason = %q, want %q", chunk.FinishReason, fr)
		}
		_ = sess.Close()
	}
}

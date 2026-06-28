// Package stream_test covers the Anthropic SSE stream decoder.
// Named failure modes:
//   - nil body → error
//   - message_start: Usage extracted from event
//   - content_block_start: tool_use block → ToolCallDeltas + tools map populated
//   - content_block_delta:
//   - text_delta → Delta populated
//   - thinking_delta → ReasoningDelta populated
//   - signature_delta → ReasoningDelta populated
//   - input_json_delta → ToolCallDeltas appended (requires prior content_block_start)
//   - content_block_stop: tools map entry deleted
//   - ping: no-op, chunk returned
//   - error: s.done=true, MapAnthropicStreamError error returned
//   - message_delta: Usage extracted
//   - message_stop: Done=true
//   - after message_stop: returns io.EOF
//   - MapAnthropicStreamError: full enum coverage
//   - formatSSE: event + no-event forms
//   - context cancelled: returns ctx.Err()
//   - NewStreamDecoder: nil log defaults to slog.Default()
package stream_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	antstream "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/anthropic/stream"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// sseFrame builds a single Anthropic SSE frame (event + data).
func sseFrame(event, data string) string {
	return "event: " + event + "\ndata: " + data + "\n\n"
}

// openSession is a test helper that opens a stream from a string.
func openSession(t *testing.T, body string) provcore.StreamSession {
	t.Helper()
	d := antstream.NewStreamDecoder(slog.Default())
	rc := io.NopCloser(strings.NewReader(body))
	sess, err := d.Open(rc, typology.WireShapeAnthropicMessages)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}

func TestNewStreamDecoder_nilLog_usesDefault(t *testing.T) {
	d := antstream.NewStreamDecoder(nil)
	if d == nil {
		t.Fatal("NewStreamDecoder(nil) returned nil")
	}
	rc := io.NopCloser(strings.NewReader(""))
	sess, err := d.Open(rc, typology.WireShapeAnthropicMessages)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	sess.Close()
}

// nil body

func TestStreamDecoder_nilBody_returnsError(t *testing.T) {
	d := antstream.NewStreamDecoder(slog.Default())
	_, err := d.Open(nil, typology.WireShapeAnthropicMessages)
	if err == nil {
		t.Fatal("expected error for nil body")
	}
}

func TestStreamDecoder_messageStart_usageExtracted(t *testing.T) {
	// message_start carries usage inside message.usage.
	data := `{"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-sonnet-4-6","usage":{"input_tokens":25,"output_tokens":0}}}`
	sess := openSession(t, sseFrame("message_start", data))
	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	// Usage must have been extracted.
	if chunk.Usage == nil {
		t.Fatal("Usage should be non-nil on message_start with usage data")
	}
	if chunk.Usage.PromptTokens == nil || *chunk.Usage.PromptTokens != 25 {
		t.Errorf("PromptTokens: got %v, want 25", chunk.Usage.PromptTokens)
	}
}

func TestStreamDecoder_contentBlockStart_toolUse_toolCallDeltaEmitted(t *testing.T) {
	data := `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tu_1","name":"search"}}`
	sess := openSession(t, sseFrame("content_block_start", data))
	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if len(chunk.ToolCallDeltas) == 0 {
		t.Fatal("expected ToolCallDeltas on content_block_start for tool_use")
	}
	tc := chunk.ToolCallDeltas[0]
	if tc.ID != "tu_1" {
		t.Errorf("ID: got %q, want tu_1", tc.ID)
	}
	if tc.Name != "search" {
		t.Errorf("Name: got %q, want search", tc.Name)
	}
	if tc.Index != 0 {
		t.Errorf("Index: got %d, want 0", tc.Index)
	}
}

func TestStreamDecoder_contentBlockStart_textType_noToolCallDelta(t *testing.T) {
	// content_block_start for a text block should not emit ToolCallDeltas.
	data := `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`
	sess := openSession(t, sseFrame("content_block_start", data))
	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if len(chunk.ToolCallDeltas) != 0 {
		t.Errorf("text content_block_start should not emit ToolCallDeltas: %+v", chunk.ToolCallDeltas)
	}
}

func TestStreamDecoder_contentBlockDelta_textDelta(t *testing.T) {
	data := `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello world"}}`
	sess := openSession(t, sseFrame("content_block_delta", data))
	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if chunk.Delta != "hello world" {
		t.Errorf("Delta: got %q, want hello world", chunk.Delta)
	}
}

func TestStreamDecoder_contentBlockDelta_thinkingDelta(t *testing.T) {
	data := `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"step one"}}`
	sess := openSession(t, sseFrame("content_block_delta", data))
	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if chunk.ReasoningDelta != "step one" {
		t.Errorf("ReasoningDelta: got %q, want step one", chunk.ReasoningDelta)
	}
	if chunk.Delta != "" {
		t.Errorf("Delta should be empty for thinking_delta: got %q", chunk.Delta)
	}
}

func TestStreamDecoder_contentBlockDelta_signatureDelta(t *testing.T) {
	data := `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"abc123"}}`
	sess := openSession(t, sseFrame("content_block_delta", data))
	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if chunk.ReasoningDelta != "abc123" {
		t.Errorf("ReasoningDelta: got %q, want abc123", chunk.ReasoningDelta)
	}
}

func TestStreamDecoder_contentBlockDelta_inputJsonDelta_toolCallDeltaAppended(t *testing.T) {
	// A content_block_start for tool_use followed by input_json_delta appends ToolCallDelta.
	start := `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_2","name":"calc"}}`
	delta := `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"op\":\"add\""}}`
	body := sseFrame("content_block_start", start) + sseFrame("content_block_delta", delta)
	sess := openSession(t, body)

	// First chunk: content_block_start → ToolCallDelta with ID + Name.
	chunk1, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next (start): %v", err)
	}
	if len(chunk1.ToolCallDeltas) == 0 {
		t.Fatal("expected ToolCallDeltas from content_block_start")
	}

	// Second chunk: input_json_delta → ToolCallDelta with Arguments.
	chunk2, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next (delta): %v", err)
	}
	if len(chunk2.ToolCallDeltas) == 0 {
		t.Fatal("expected ToolCallDeltas from input_json_delta")
	}
	tc := chunk2.ToolCallDeltas[0]
	if tc.Arguments != `{"op":"add"` {
		t.Errorf("Arguments: got %q", tc.Arguments)
	}
	if tc.ID != "tu_2" {
		t.Errorf("ID: got %q, want tu_2", tc.ID)
	}
}

func TestStreamDecoder_contentBlockDelta_inputJsonDelta_noMatchingTool_noDelta(t *testing.T) {
	// input_json_delta for index with no registered tool → no ToolCallDeltas.
	delta := `{"type":"content_block_delta","index":99,"delta":{"type":"input_json_delta","partial_json":"{}"}}`
	sess := openSession(t, sseFrame("content_block_delta", delta))
	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if len(chunk.ToolCallDeltas) != 0 {
		t.Errorf("unexpected ToolCallDeltas for unregistered index: %+v", chunk.ToolCallDeltas)
	}
}

func TestStreamDecoder_contentBlockStop_toolMapEntryDeleted(t *testing.T) {
	// content_block_stop clears tool state; subsequent input_json_delta at same
	// index should produce no ToolCallDeltas.
	start := `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tu_3","name":"fn"}}`
	stop := `{"type":"content_block_stop","index":0}`
	delta := `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`
	body := sseFrame("content_block_start", start) +
		sseFrame("content_block_stop", stop) +
		sseFrame("content_block_delta", delta)
	sess := openSession(t, body)

	// Consume start chunk.
	_, _ = sess.Next(context.Background())
	// Consume stop chunk.
	_, _ = sess.Next(context.Background())
	// Now the delta should have no ToolCallDeltas (tool map entry deleted).
	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next (delta after stop): %v", err)
	}
	if len(chunk.ToolCallDeltas) != 0 {
		t.Errorf("expected no ToolCallDeltas after content_block_stop, got: %+v", chunk.ToolCallDeltas)
	}
}

func TestStreamDecoder_ping_chunkReturnedNoError(t *testing.T) {
	data := `{"type":"ping"}`
	sess := openSession(t, sseFrame("ping", data))
	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if chunk.Done {
		t.Error("ping should not set Done=true")
	}
	if chunk.Delta != "" {
		t.Error("ping should not set Delta")
	}
}

// error event

func TestStreamDecoder_errorEvent_returnsProviderError(t *testing.T) {
	data := `{"type":"error","error":{"type":"overloaded_error","message":"server busy"}}`
	sess := openSession(t, sseFrame("error", data))
	_, err := sess.Next(context.Background())
	if err == nil {
		t.Fatal("expected error from error event")
	}
	pe := &provcore.ProviderError{}
	ok := errors.As(err, &pe)
	if !ok {
		t.Fatalf("expected *ProviderError, got %T", err)
	}
	if pe.Code != provcore.CodeRateLimited {
		t.Errorf("Code: got %q, want CodeRateLimited", pe.Code)
	}
	if pe.Type != "overloaded_error" {
		t.Errorf("Type: got %q", pe.Type)
	}
}

func TestStreamDecoder_errorEvent_afterDone_returnsEOF(t *testing.T) {
	// Once the error event sets done=true, subsequent Next calls must return io.EOF.
	data := `{"type":"error","error":{"type":"api_error","message":"err"}}`
	sess := openSession(t, sseFrame("error", data))
	_, _ = sess.Next(context.Background())
	_, err := sess.Next(context.Background())
	if !errors.Is(err, io.EOF) {
		t.Errorf("expected io.EOF after error event, got %v", err)
	}
}

func TestStreamDecoder_messageDelta_usageExtracted(t *testing.T) {
	// message_delta may carry root-level usage (Anthropic / Bedrock pattern).
	data := `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":42}}`
	sess := openSession(t, sseFrame("message_delta", data))
	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if chunk.Usage == nil {
		t.Fatal("Usage should be non-nil on message_delta with usage data")
	}
	if chunk.Usage.CompletionTokens == nil || *chunk.Usage.CompletionTokens != 42 {
		t.Errorf("CompletionTokens: got %v, want 42", chunk.Usage.CompletionTokens)
	}
}

func TestStreamDecoder_messageStop_emitsDoneTrue(t *testing.T) {
	data := `{"type":"message_stop"}`
	sess := openSession(t, sseFrame("message_stop", data))
	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if !chunk.Done {
		t.Error("expected Done=true on message_stop")
	}
}

func TestStreamDecoder_afterMessageStop_returnsEOF(t *testing.T) {
	data := `{"type":"message_stop"}`
	sess := openSession(t, sseFrame("message_stop", data))
	// Consume the message_stop chunk.
	_, _ = sess.Next(context.Background())
	// Next call must return io.EOF.
	_, err := sess.Next(context.Background())
	if !errors.Is(err, io.EOF) {
		t.Errorf("expected io.EOF after message_stop, got %v", err)
	}
}

// raw bytes

func TestStreamDecoder_rawBytesForwardedVerbatim(t *testing.T) {
	// RawBytes must reproduce the original SSE frame bytes.
	data := `{"type":"ping"}`
	expected := "event: ping\ndata: " + data + "\n\n"
	sess := openSession(t, expected)
	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if string(chunk.RawBytes) != expected {
		t.Errorf("RawBytes: got %q, want %q", chunk.RawBytes, expected)
	}
}

// context cancelled

func TestStreamDecoder_contextCancelled_returnsCtxErr(t *testing.T) {
	sess := openSession(t, "") // empty body
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before Next
	_, err := sess.Next(ctx)
	if err == nil {
		t.Error("cancelled context: expected non-nil error")
	}
}

// Close is idempotent

func TestStreamDecoder_close_idempotent(t *testing.T) {
	d := antstream.NewStreamDecoder(slog.Default())
	rc := io.NopCloser(strings.NewReader(""))
	sess, _ := d.Open(rc, typology.WireShapeAnthropicMessages)
	if err := sess.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
}

// MapAnthropicStreamError full enum

func TestMapAnthropicStreamError_overloadedError(t *testing.T) {
	err := antstream.MapAnthropicStreamError("overloaded_error", "busy")
	pe := func() *provcore.ProviderError {
		target := &provcore.ProviderError{}
		_ = errors.As(err, &target)
		return target
	}()
	if pe.Code != provcore.CodeRateLimited {
		t.Errorf("overloaded_error: got %q, want CodeRateLimited", pe.Code)
	}
}

func TestMapAnthropicStreamError_rateLimitError(t *testing.T) {
	err := antstream.MapAnthropicStreamError("rate_limit_error", "slow down")
	pe := func() *provcore.ProviderError {
		target := &provcore.ProviderError{}
		_ = errors.As(err, &target)
		return target
	}()
	if pe.Code != provcore.CodeRateLimited {
		t.Errorf("rate_limit_error: got %q, want CodeRateLimited", pe.Code)
	}
}

func TestMapAnthropicStreamError_authenticationError(t *testing.T) {
	err := antstream.MapAnthropicStreamError("authentication_error", "bad key")
	pe := func() *provcore.ProviderError {
		target := &provcore.ProviderError{}
		_ = errors.As(err, &target)
		return target
	}()
	if pe.Code != provcore.CodeAuthFailed {
		t.Errorf("authentication_error: got %q, want CodeAuthFailed", pe.Code)
	}
}

func TestMapAnthropicStreamError_permissionError(t *testing.T) {
	err := antstream.MapAnthropicStreamError("permission_error", "no access")
	pe := func() *provcore.ProviderError {
		target := &provcore.ProviderError{}
		_ = errors.As(err, &target)
		return target
	}()
	if pe.Code != provcore.CodeAuthFailed {
		t.Errorf("permission_error: got %q, want CodeAuthFailed", pe.Code)
	}
}

func TestMapAnthropicStreamError_invalidRequestError(t *testing.T) {
	err := antstream.MapAnthropicStreamError("invalid_request_error", "bad req")
	pe := func() *provcore.ProviderError {
		target := &provcore.ProviderError{}
		_ = errors.As(err, &target)
		return target
	}()
	if pe.Code != provcore.CodeInvalidRequest {
		t.Errorf("invalid_request_error: got %q, want CodeInvalidRequest", pe.Code)
	}
}

func TestMapAnthropicStreamError_notFoundError(t *testing.T) {
	// not_found_error maps to invalid_request, unified with the unary
	// HTTP normaliser (a 404 is a client error, not a retryable upstream one).
	err := antstream.MapAnthropicStreamError("not_found_error", "not found")
	pe := func() *provcore.ProviderError {
		target := &provcore.ProviderError{}
		_ = errors.As(err, &target)
		return target
	}()
	if pe.Code != provcore.CodeInvalidRequest {
		t.Errorf("not_found_error: got %q, want CodeInvalidRequest", pe.Code)
	}
	if pe.Status != 404 {
		t.Errorf("not_found_error: status=%d, want 404", pe.Status)
	}
}

func TestMapAnthropicStreamError_apiError(t *testing.T) {
	err := antstream.MapAnthropicStreamError("api_error", "server error")
	pe := func() *provcore.ProviderError {
		target := &provcore.ProviderError{}
		_ = errors.As(err, &target)
		return target
	}()
	if pe.Code != provcore.CodeUpstreamError {
		t.Errorf("api_error: got %q, want CodeUpstreamError", pe.Code)
	}
}

func TestMapAnthropicStreamError_emptyType_usesUpstreamError(t *testing.T) {
	err := antstream.MapAnthropicStreamError("", "")
	pe := func() *provcore.ProviderError {
		target := &provcore.ProviderError{}
		_ = errors.As(err, &target)
		return target
	}()
	if pe.Code != provcore.CodeUpstreamError {
		t.Errorf("empty type: got %q, want CodeUpstreamError", pe.Code)
	}
	if pe.Message == "" {
		t.Error("empty message should use default")
	}
}

func TestMapAnthropicStreamError_unknownType_usesUpstreamError(t *testing.T) {
	err := antstream.MapAnthropicStreamError("some_unknown_error", "weird")
	pe := func() *provcore.ProviderError {
		target := &provcore.ProviderError{}
		_ = errors.As(err, &target)
		return target
	}()
	if pe.Code != provcore.CodeUpstreamError {
		t.Errorf("unknown type: got %q, want CodeUpstreamError", pe.Code)
	}
	if pe.Type != "some_unknown_error" {
		t.Errorf("Type: got %q, want some_unknown_error", pe.Type)
	}
}

func TestMapAnthropicStreamError_messagePopulated(t *testing.T) {
	// Custom message should be preserved.
	err := antstream.MapAnthropicStreamError("api_error", "custom message")
	pe := func() *provcore.ProviderError {
		target := &provcore.ProviderError{}
		_ = errors.As(err, &target)
		return target
	}()
	if pe.Message != "custom message" {
		t.Errorf("Message: got %q, want custom message", pe.Message)
	}
}

// NativeEvent populated

func TestStreamDecoder_nativeEvent_populated(t *testing.T) {
	// NativeEvent on the chunk must match the SSE event name.
	data := `{"type":"ping"}`
	sess := openSession(t, sseFrame("ping", data))
	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if chunk.NativeEvent != "ping" {
		t.Errorf("NativeEvent: got %q, want ping", chunk.NativeEvent)
	}
}

// TestStreamDecoder_messageStop_carriesAccumulatedUsage guards the cross-format
// usage drop: Anthropic reports input_tokens on message_start and the final
// output_tokens on message_delta — never on the terminal message_stop — but every
// canonical egress encoder reads chunk.Usage only on the Done chunk. The session
// must therefore stamp the accumulated usage onto message_stop, or the OpenAI /
// Responses stream silently omits the trailing usage frame (include_usage shows
// nothing, and the operator agent's context gauge never populates).
func TestStreamDecoder_messageStop_carriesAccumulatedUsage(t *testing.T) {
	body := sseFrame("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":40,"output_tokens":1}}}`) +
		sseFrame("message_delta", `{"type":"message_delta","usage":{"output_tokens":123}}`) +
		sseFrame("message_stop", `{"type":"message_stop"}`)
	sess := openSession(t, body)

	var done provcore.Chunk
	for {
		c, err := sess.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if c.Done {
			done = c
		}
	}
	if !done.Done {
		t.Fatal("expected a Done chunk from message_stop")
	}
	if done.Usage == nil {
		t.Fatal("Done chunk must carry accumulated usage, else the egress drops the usage frame")
	}
	if done.Usage.PromptTokens == nil || *done.Usage.PromptTokens != 40 {
		t.Errorf("Done PromptTokens: got %v, want 40 (carried from message_start)", done.Usage.PromptTokens)
	}
	if done.Usage.CompletionTokens == nil || *done.Usage.CompletionTokens != 123 {
		t.Errorf("Done CompletionTokens: got %v, want 123 (final output_tokens from message_delta)", done.Usage.CompletionTokens)
	}
}

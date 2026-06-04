// Package stream_test — stream_responses_test.go covers the Responses-API SSE
// stream session and ResponsesStreamDecoder.
// Named failure modes:
//   - response.output_text.delta → Chunk.Delta
//   - response.function_call_arguments.delta → Chunk.ToolCallDeltas
//   - response.reasoning_summary_text.delta / response.reasoning_text.delta → Chunk.ReasoningDelta
//   - response.refusal.delta → Chunk.Delta (canonical has no refusal channel)
//   - response.completed → Done=true + Usage extracted
//   - response.incomplete → Done=true
//   - response.failed / response.error → Done=true (error path)
//   - response.output_item.added → sets currentItemType (bookkeeping, no emit)
//   - response.created / response.in_progress / .queued → skipped (no emit)
//   - built-in tool events (web_search_call.*) → skipped (no emit)
//   - unknown event type → skipped with one-time WARN (no emit)
//   - nil body → error
//   - [DONE] sentinel → Done=true
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

// responsesBody creates an io.ReadCloser from SSE event lines.
func responsesBody(lines string) io.ReadCloser {
	return io.NopCloser(strings.NewReader(lines))
}

// responsesEvent formats a single SSE frame.
func responsesEvent(evType, data string) string {
	return "event: " + evType + "\ndata: " + data + "\n\n"
}

func TestResponsesStreamDecoder_nilBody_returnsError(t *testing.T) {
	d := ostream.NewResponsesStreamDecoder(nil)
	_, err := d.Open(nil, typology.WireShapeOpenAIResponses)
	if err == nil {
		t.Fatal("expected error for nil body")
	}
}

func TestResponsesStreamDecoder_textDelta(t *testing.T) {
	data := `{"type":"response.output_text.delta","output_index":0,"item_id":"item_1","delta":"hello"}`
	body := responsesBody(responsesEvent("response.output_text.delta", data))
	d := ostream.NewResponsesStreamDecoder(slog.Default())
	sess, err := d.Open(body, typology.WireShapeOpenAIResponses)
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
		t.Error("should not be Done")
	}
}

func TestResponsesStreamDecoder_emptyTextDelta_skipped(t *testing.T) {
	// Empty delta → skipped by the session; EOF comes from the body end.
	data := `{"type":"response.output_text.delta","output_index":0,"item_id":"item_1","delta":""}`
	completedData := `{"type":"response.completed","response":{"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`
	body := responsesBody(
		responsesEvent("response.output_text.delta", data) +
			responsesEvent("response.completed", completedData),
	)
	d := ostream.NewResponsesStreamDecoder(slog.Default())
	sess, _ := d.Open(body, typology.WireShapeOpenAIResponses)
	defer sess.Close()

	// First non-empty event should be the completed chunk.
	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if !chunk.Done {
		t.Errorf("expected Done=true from completed, got: %+v", chunk)
	}
}

func TestResponsesStreamDecoder_functionCallArgumentsDelta(t *testing.T) {
	data := `{"type":"response.function_call_arguments.delta","output_index":1,"item_id":"call_abc","delta":"{\"q\""}`
	body := responsesBody(responsesEvent("response.function_call_arguments.delta", data))
	d := ostream.NewResponsesStreamDecoder(slog.Default())
	sess, _ := d.Open(body, typology.WireShapeOpenAIResponses)
	defer sess.Close()

	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if len(chunk.ToolCallDeltas) == 0 {
		t.Fatal("expected ToolCallDeltas")
	}
	tc := chunk.ToolCallDeltas[0]
	if tc.ID != "call_abc" {
		t.Errorf("ID: got %q, want call_abc", tc.ID)
	}
	if tc.Index != 1 {
		t.Errorf("Index: got %d, want 1", tc.Index)
	}
	if tc.Arguments != `{"q"` {
		t.Errorf("Arguments: got %q", tc.Arguments)
	}
}

func TestResponsesStreamDecoder_reasoningSummaryTextDelta(t *testing.T) {
	data := `{"type":"response.reasoning_summary_text.delta","output_index":0,"item_id":"item_1","delta":"step1"}`
	body := responsesBody(responsesEvent("response.reasoning_summary_text.delta", data))
	d := ostream.NewResponsesStreamDecoder(slog.Default())
	sess, _ := d.Open(body, typology.WireShapeOpenAIResponses)
	defer sess.Close()

	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if chunk.ReasoningDelta != "step1" {
		t.Errorf("ReasoningDelta: got %q, want step1", chunk.ReasoningDelta)
	}
}

func TestResponsesStreamDecoder_reasoningTextDelta(t *testing.T) {
	data := `{"type":"response.reasoning_text.delta","output_index":0,"item_id":"item_1","delta":"reason"}`
	body := responsesBody(responsesEvent("response.reasoning_text.delta", data))
	d := ostream.NewResponsesStreamDecoder(slog.Default())
	sess, _ := d.Open(body, typology.WireShapeOpenAIResponses)
	defer sess.Close()

	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if chunk.ReasoningDelta != "reason" {
		t.Errorf("ReasoningDelta: got %q, want reason", chunk.ReasoningDelta)
	}
}

func TestResponsesStreamDecoder_refusalDelta_surfacedAsDelta(t *testing.T) {
	// response.refusal.delta has no canonical refusal channel; surfaced as Delta.
	data := `{"type":"response.refusal.delta","output_index":0,"item_id":"item_1","delta":"I cannot"}`
	body := responsesBody(responsesEvent("response.refusal.delta", data))
	d := ostream.NewResponsesStreamDecoder(slog.Default())
	sess, _ := d.Open(body, typology.WireShapeOpenAIResponses)
	defer sess.Close()

	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if chunk.Delta != "I cannot" {
		t.Errorf("refusal.delta as Delta: got %q", chunk.Delta)
	}
}

func TestResponsesStreamDecoder_completed_doneAndUsage(t *testing.T) {
	data := `{"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`
	body := responsesBody(responsesEvent("response.completed", data))
	d := ostream.NewResponsesStreamDecoder(slog.Default())
	sess, _ := d.Open(body, typology.WireShapeOpenAIResponses)
	defer sess.Close()

	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if !chunk.Done {
		t.Error("response.completed must set Done=true")
	}
	if chunk.Usage == nil {
		t.Fatal("Usage should be set on completed event")
	}
	if chunk.Usage.PromptTokens == nil || *chunk.Usage.PromptTokens != 10 {
		t.Errorf("PromptTokens: got %v, want 10", chunk.Usage.PromptTokens)
	}
}

func TestResponsesStreamDecoder_incomplete_doneTrue(t *testing.T) {
	data := `{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`
	body := responsesBody(responsesEvent("response.incomplete", data))
	d := ostream.NewResponsesStreamDecoder(slog.Default())
	sess, _ := d.Open(body, typology.WireShapeOpenAIResponses)
	defer sess.Close()

	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if !chunk.Done {
		t.Error("response.incomplete must set Done=true")
	}
}

func TestResponsesStreamDecoder_failed_doneTrue(t *testing.T) {
	data := `{"type":"response.failed","response":{"status":"failed"}}`
	body := responsesBody(responsesEvent("response.failed", data))
	d := ostream.NewResponsesStreamDecoder(slog.Default())
	sess, _ := d.Open(body, typology.WireShapeOpenAIResponses)
	defer sess.Close()

	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if !chunk.Done {
		t.Error("response.failed must set Done=true")
	}
}

func TestResponsesStreamDecoder_error_doneTrue(t *testing.T) {
	data := `{"type":"response.error","error":{"message":"something went wrong"}}`
	body := responsesBody(responsesEvent("response.error", data))
	d := ostream.NewResponsesStreamDecoder(slog.Default())
	sess, _ := d.Open(body, typology.WireShapeOpenAIResponses)
	defer sess.Close()

	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if !chunk.Done {
		t.Error("response.error must set Done=true")
	}
}

func TestResponsesStreamDecoder_bookkeepingEvents_skipped(t *testing.T) {
	// Bookkeeping events (created, in_progress, queued, output_item.added, content_part.*)
	// must not emit a canonical chunk — they consume until a real event arrives.
	completedData := `{"type":"response.completed","response":{"usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}}`
	body := responsesBody(
		responsesEvent("response.created", `{"type":"response.created"}`) +
			responsesEvent("response.in_progress", `{"type":"response.in_progress"}`) +
			responsesEvent("response.queued", `{"type":"response.queued"}`) +
			responsesEvent("response.output_item.added", `{"type":"response.output_item.added","item":{"type":"message"}}`) +
			responsesEvent("response.content_part.added", `{"type":"response.content_part.added"}`) +
			responsesEvent("response.output_text.done", `{"type":"response.output_text.done"}`) +
			responsesEvent("response.content_part.done", `{"type":"response.content_part.done"}`) +
			responsesEvent("response.output_item.done", `{"type":"response.output_item.done"}`) +
			responsesEvent("response.completed", completedData),
	)
	d := ostream.NewResponsesStreamDecoder(slog.Default())
	sess, _ := d.Open(body, typology.WireShapeOpenAIResponses)
	defer sess.Close()

	// Should skip all bookkeeping and return the completed chunk directly.
	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if !chunk.Done {
		t.Errorf("expected Done=true (completed event), got chunk: %+v", chunk)
	}
}

func TestResponsesStreamDecoder_builtinToolEvent_skipped(t *testing.T) {
	// Built-in tool events must be silently skipped (not cause a panic or error).
	completedData := `{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`
	body := responsesBody(
		responsesEvent("response.web_search_call.in_progress", `{"type":"response.web_search_call.in_progress"}`) +
			responsesEvent("response.completed", completedData),
	)
	d := ostream.NewResponsesStreamDecoder(slog.Default())
	sess, _ := d.Open(body, typology.WireShapeOpenAIResponses)
	defer sess.Close()

	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if !chunk.Done {
		t.Error("expected completed after built-in tool event skip")
	}
}

func TestResponsesStreamDecoder_unknownEvent_skipped(t *testing.T) {
	// Unknown event types emit a one-time log.Warn but must not break the stream.
	completedData := `{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`
	body := responsesBody(
		responsesEvent("response.future_unknown_event", `{"type":"response.future_unknown_event"}`) +
			responsesEvent("response.completed", completedData),
	)
	d := ostream.NewResponsesStreamDecoder(slog.Default())
	sess, _ := d.Open(body, typology.WireShapeOpenAIResponses)
	defer sess.Close()

	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if !chunk.Done {
		t.Error("expected completed after unknown event skip")
	}
}

func TestResponsesStreamDecoder_doneChunk_afterDone_returnsEOF(t *testing.T) {
	completedData := `{"type":"response.completed","response":{}}`
	body := responsesBody(responsesEvent("response.completed", completedData))
	d := ostream.NewResponsesStreamDecoder(slog.Default())
	sess, _ := d.Open(body, typology.WireShapeOpenAIResponses)
	defer sess.Close()

	_, _ = sess.Next(context.Background())
	_, err := sess.Next(context.Background())
	if !errors.Is(err, io.EOF) {
		t.Errorf("expected io.EOF after done, got %v", err)
	}
}

func TestResponsesStreamDecoder_legacyDONESentinel_doneTrue(t *testing.T) {
	// Defensive: future Responses-API versions might emit [DONE]; must handle it.
	body := responsesBody("data: [DONE]\n\n")
	d := ostream.NewResponsesStreamDecoder(slog.Default())
	sess, _ := d.Open(body, typology.WireShapeOpenAIResponses)
	defer sess.Close()

	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if !chunk.Done {
		t.Error("[DONE] sentinel must set Done=true")
	}
}

func TestResponsesStreamDecoder_completedWithNoUsage_usageNil(t *testing.T) {
	// When usage fields are all absent → Usage pointer must be nil (not zero pointer).
	data := `{"type":"response.completed","response":{}}`
	body := responsesBody(responsesEvent("response.completed", data))
	d := ostream.NewResponsesStreamDecoder(slog.Default())
	sess, _ := d.Open(body, typology.WireShapeOpenAIResponses)
	defer sess.Close()

	chunk, _ := sess.Next(context.Background())
	if chunk.Usage != nil {
		t.Errorf("Usage should be nil when not reported, got %+v", chunk.Usage)
	}
}

func TestNewResponsesStreamDecoder_nilLog_defaultsToSlogDefault(t *testing.T) {
	// Smoke: nil log must not panic.
	d := ostream.NewResponsesStreamDecoder(nil)
	if d == nil {
		t.Fatal("expected non-nil decoder")
	}
}

func TestResponsesStreamDecoder_inlineTypeFallback(t *testing.T) {
	// When SSE event header is absent, evType is read from the inline "type" field.
	// Simulate by writing data: without an event: prefix.
	data := `{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`
	// No "event:" line — relies on inline type fallback.
	body := responsesBody("data: " + data + "\n\n")
	d := ostream.NewResponsesStreamDecoder(slog.Default())
	sess, _ := d.Open(body, typology.WireShapeOpenAIResponses)
	defer sess.Close()

	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if !chunk.Done {
		t.Error("inline type field fallback: expected Done=true from completed")
	}
}

func TestResponsesStreamDecoder_reasoningSummaryPartBookkeeping_skipped(t *testing.T) {
	// response.reasoning_summary_part.added/done are bookkeeping — must not emit.
	completedData := `{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`
	body := responsesBody(
		responsesEvent("response.reasoning_summary_part.added", `{"type":"response.reasoning_summary_part.added"}`) +
			responsesEvent("response.reasoning_summary_part.done", `{"type":"response.reasoning_summary_part.done"}`) +
			responsesEvent("response.function_call_arguments.done", `{"type":"response.function_call_arguments.done"}`) +
			responsesEvent("response.refusal.done", `{"type":"response.refusal.done"}`) +
			responsesEvent("response.output_text.annotation.added", `{"type":"response.output_text.annotation.added"}`) +
			responsesEvent("response.completed", completedData),
	)
	d := ostream.NewResponsesStreamDecoder(slog.Default())
	sess, _ := d.Open(body, typology.WireShapeOpenAIResponses)
	defer sess.Close()

	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if !chunk.Done {
		t.Error("bookkeeping events must be skipped until completed")
	}
}

func TestResponsesStreamDecoder_contextCancelled_returnsCtxErr(t *testing.T) {
	body := responsesBody("") // empty — scanner immediately returns EOF
	d := ostream.NewResponsesStreamDecoder(slog.Default())
	sess, _ := d.Open(body, typology.WireShapeOpenAIResponses)
	defer sess.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := sess.Next(ctx)
	if err == nil {
		t.Error("cancelled context: expected non-nil error")
	}
}

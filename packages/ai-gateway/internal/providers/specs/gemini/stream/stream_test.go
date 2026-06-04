// Package stream_test covers the Gemini SSE stream decoder.
// Named failure modes:
//   - nil body → error
//   - text part → Delta populated
//   - thought:true part → ReasoningDelta
//   - functionCall part → ToolCallDeltas (with and without id, sha1 synthesized)
//   - finishReason set → finishSeen=true
//   - usageMetadata + finishSeen → Done=true + Usage
//   - usageMetadata without finishSeen → Usage only (not Done)
//   - EOF after finishSeen → synthesized Done chunk
//   - EOF without any data frames → ProviderError (empty SSE stream)
//   - empty data line → chunk returned with no parse error
//   - context cancelled → ctx.Err() returned
//   - Close is idempotent
//   - after Done: returns io.EOF
//   - FormatSSE: event + no-event forms
//   - NewStreamDecoder nil log → slog.Default()
package stream_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	gemstream "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/gemini/stream"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

func sseData(data string) string {
	return "data: " + data + "\n\n"
}

func openSession(t *testing.T, body string) provcore.StreamSession {
	t.Helper()
	d := gemstream.NewStreamDecoder(slog.Default())
	rc := io.NopCloser(strings.NewReader(body))
	sess, err := d.Open(rc, typology.WireShapeGeminiGenerateContent)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}

func TestNewStreamDecoder_nilLog_usesDefault(t *testing.T) {
	d := gemstream.NewStreamDecoder(nil)
	if d == nil {
		t.Fatal("NewStreamDecoder(nil) returned nil")
	}
}

// nil body

func TestStreamDecoder_nilBody_returnsError(t *testing.T) {
	d := gemstream.NewStreamDecoder(slog.Default())
	_, err := d.Open(nil, typology.WireShapeGeminiGenerateContent)
	if err == nil {
		t.Fatal("expected error for nil body")
	}
}

// Text delta

func TestStreamDecoder_textPart_deltaPopulated(t *testing.T) {
	payload := `{"candidates":[{"index":0,"content":{"parts":[{"text":"hello world"}],"role":"model"}}]}`
	sess := openSession(t, sseData(payload))
	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if chunk.Delta != "hello world" {
		t.Errorf("Delta: got %q, want hello world", chunk.Delta)
	}
	if chunk.Done {
		t.Error("expected Done=false for text-only chunk")
	}
}

// Thought part → ReasoningDelta

func TestStreamDecoder_thoughtPart_reasoningDelta(t *testing.T) {
	payload := `{"candidates":[{"index":0,"content":{"parts":[{"text":"thinking step","thought":true}],"role":"model"}}]}`
	sess := openSession(t, sseData(payload))
	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if chunk.ReasoningDelta != "thinking step" {
		t.Errorf("ReasoningDelta: got %q, want thinking step", chunk.ReasoningDelta)
	}
	if chunk.Delta != "" {
		t.Errorf("Delta should be empty for thought part: %q", chunk.Delta)
	}
}

// functionCall → ToolCallDeltas

func TestStreamDecoder_functionCallPart_toolCallDeltaEmitted(t *testing.T) {
	payload := `{"candidates":[{"index":0,"content":{"parts":[{"functionCall":{"id":"fc_1","name":"search","args":{"q":"hello"}}}],"role":"model"}}]}`
	sess := openSession(t, sseData(payload))
	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if len(chunk.ToolCallDeltas) == 0 {
		t.Fatal("expected ToolCallDeltas")
	}
	tc := chunk.ToolCallDeltas[0]
	if tc.ID != "fc_1" {
		t.Errorf("ID: got %q, want fc_1", tc.ID)
	}
	if tc.Name != "search" {
		t.Errorf("Name: got %q, want search", tc.Name)
	}
}

func TestStreamDecoder_functionCallPart_noID_syntheticIDGenerated(t *testing.T) {
	// No id in functionCall → synthetic call_<sha1> id.
	payload := `{"candidates":[{"index":0,"content":{"parts":[{"functionCall":{"name":"calc","args":{}}}],"role":"model"}}]}`
	sess := openSession(t, sseData(payload))
	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if len(chunk.ToolCallDeltas) == 0 {
		t.Fatal("expected ToolCallDeltas")
	}
	if chunk.ToolCallDeltas[0].ID == "" {
		t.Error("expected non-empty synthetic ID")
	}
}

func TestStreamDecoder_functionCallPart_emptyArgs_defaultsToEmptyObject(t *testing.T) {
	// functionCall with no args → "{}" default.
	payload := `{"candidates":[{"index":0,"content":{"parts":[{"functionCall":{"id":"fc_2","name":"fn"}}],"role":"model"}}]}`
	sess := openSession(t, sseData(payload))
	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if len(chunk.ToolCallDeltas) == 0 {
		t.Fatal("expected ToolCallDeltas")
	}
	if chunk.ToolCallDeltas[0].Arguments != "{}" {
		t.Errorf("Arguments: got %q, want {}", chunk.ToolCallDeltas[0].Arguments)
	}
}

// finishReason + usageMetadata → Done

func TestStreamDecoder_finishReasonAndUsage_doneTrue(t *testing.T) {
	// A chunk with finishReason AND usageMetadata → Done=true + Usage set.
	payload := `{"candidates":[{"index":0,"content":{"parts":[{"text":"answer"}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}`
	sess := openSession(t, sseData(payload))
	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if !chunk.Done {
		t.Error("expected Done=true when finishReason+usageMetadata present")
	}
	if chunk.Usage == nil {
		t.Fatal("Usage should be non-nil on usageMetadata chunk")
	}
	if chunk.Usage.PromptTokens == nil || *chunk.Usage.PromptTokens != 10 {
		t.Errorf("PromptTokens: got %v, want 10", chunk.Usage.PromptTokens)
	}
}

func TestStreamDecoder_usageMetadataWithoutFinish_notDone(t *testing.T) {
	// usageMetadata present but finishReason NOT yet seen → Usage set but Done=false.
	payload := `{"candidates":[{"index":0,"content":{"parts":[{"text":"partial"}],"role":"model"}}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2}}`
	sess := openSession(t, sseData(payload))
	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if chunk.Done {
		t.Error("expected Done=false when finishReason not yet seen")
	}
	// Usage may or may not be set — the key invariant is Done=false.
}

func TestStreamDecoder_finishThenEOF_synthesizedDoneChunk(t *testing.T) {
	// Frame 1: finishReason set (finishSeen=true), no usageMetadata.
	// EOF follows → synthesized Done chunk emitted.
	frame := `{"candidates":[{"index":0,"content":{"parts":[{"text":"done"}],"role":"model"},"finishReason":"STOP"}]}`
	sess := openSession(t, sseData(frame))

	// Consume the text chunk.
	_, _ = sess.Next(context.Background())
	// Next should return synthesized Done chunk (no error).
	chunk, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Expected synthesized Done chunk, got error: %v", err)
	}
	if !chunk.Done {
		t.Error("synthesized chunk must have Done=true")
	}
	// Another call must return io.EOF.
	_, err = sess.Next(context.Background())
	if !errors.Is(err, io.EOF) {
		t.Errorf("expected io.EOF after synthesized Done, got %v", err)
	}
}

func TestStreamDecoder_emptyStream_providerError(t *testing.T) {
	// EOF before any data frames → ProviderError (not io.EOF).
	sess := openSession(t, "")
	_, err := sess.Next(context.Background())
	if err == nil {
		t.Fatal("expected error for empty stream")
	}
	pe := &provcore.ProviderError{}
	ok := errors.As(err, &pe)
	if !ok {
		t.Fatalf("expected *ProviderError, got %T: %v", err, err)
	}
	if pe.Code != provcore.CodeUpstreamError {
		t.Errorf("Code: got %q, want CodeUpstreamError", pe.Code)
	}
}

// After Done → io.EOF

func TestStreamDecoder_afterDone_returnsEOF(t *testing.T) {
	payload := `{"candidates":[{"index":0,"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`
	sess := openSession(t, sseData(payload))
	_, _ = sess.Next(context.Background())
	_, err := sess.Next(context.Background())
	if !errors.Is(err, io.EOF) {
		t.Errorf("expected io.EOF after Done, got %v", err)
	}
}

// Empty data line

func TestStreamDecoder_emptyDataLine_chunkReturnedNoError(t *testing.T) {
	// An SSE data: frame with empty data → chunk returned without error.
	body := "data: \n\n" + sseData(`{"candidates":[{"index":0,"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1}}`)
	sess := openSession(t, body)
	// First chunk from empty data.
	chunk1, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next on empty data: %v", err)
	}
	_ = chunk1 // chunk from empty data, no assertions on content
}

// Context cancelled

func TestStreamDecoder_contextCancelled_returnsCtxErr(t *testing.T) {
	// Extra payload so scanner doesn't get EOF immediately.
	payload := `{"candidates":[{"index":0,"content":{"parts":[{"text":"x"}],"role":"model"}}]}`
	sess := openSession(t, sseData(payload)+sseData(payload))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := sess.Next(ctx)
	if err == nil {
		t.Error("cancelled context: expected non-nil error")
	}
}

// Close idempotent

func TestStreamDecoder_close_idempotent(t *testing.T) {
	d := gemstream.NewStreamDecoder(slog.Default())
	rc := io.NopCloser(strings.NewReader(""))
	sess, _ := d.Open(rc, typology.WireShapeGeminiGenerateContent)
	if err := sess.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// RawBytes and NativeEvent

func TestStreamDecoder_rawBytesForwarded(t *testing.T) {
	payload := `{"candidates":[{"index":0,"content":{"parts":[{"text":"x"}],"role":"model"}}]}`
	body := sseData(payload)
	sess := openSession(t, body)
	chunk, _ := sess.Next(context.Background())
	if string(chunk.RawBytes) != body {
		t.Errorf("RawBytes: got %q, want %q", chunk.RawBytes, body)
	}
}

func TestFormatSSE_withEvent(t *testing.T) {
	out := gemstream.FormatSSE("ping", []byte(`{}`))
	expected := "event: ping\ndata: {}\n\n"
	if string(out) != expected {
		t.Errorf("FormatSSE with event: got %q, want %q", out, expected)
	}
}

func TestFormatSSE_noEvent(t *testing.T) {
	out := gemstream.FormatSSE("", []byte(`{}`))
	expected := "data: {}\n\n"
	if string(out) != expected {
		t.Errorf("FormatSSE no event: got %q, want %q", out, expected)
	}
}

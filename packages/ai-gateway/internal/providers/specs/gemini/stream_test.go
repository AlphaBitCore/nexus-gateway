package gemini

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// TestStreamDecoder_DoneOnTrailingUsageFrame covers the split-frame case where
// finishReason arrives in chunk N and usageMetadata in chunk N+1. The consumer
// must see both chunks and only the second must carry Done == true (SDD
// e28-s6 T-GEM-STREAM).
func TestStreamDecoder_DoneOnTrailingUsageFrame(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"text":"Hello"}]},"finishReason":"STOP"}]}`,
		``,
		`data: {"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3,"totalTokenCount":8}}`,
		``,
	}, "\n")
	dec := NewStreamDecoder(nil)
	sess, err := dec.Open(io.NopCloser(strings.NewReader(raw)), typology.WireShapeGeminiGenerateContent)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close() //nolint:errcheck

	var chunks []provcore.Chunk
	for {
		ch, err := sess.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		chunks = append(chunks, ch)
	}

	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(chunks))
	}
	if chunks[0].Done {
		t.Error("first chunk (text + finishReason) must not be Done")
	}
	if chunks[0].Delta != "Hello" {
		t.Errorf("first chunk delta=%q want %q", chunks[0].Delta, "Hello")
	}
	if !chunks[1].Done {
		t.Error("second chunk (usage trailer) must be Done")
	}
	if chunks[1].Usage == nil || chunks[1].Usage.PromptTokens == nil || *chunks[1].Usage.PromptTokens != 5 {
		t.Errorf("second chunk usage missing or wrong: %+v", chunks[1].Usage)
	}
}

// TestStreamDecoder_DoneSynthesizedAtEOF covers the case where finishReason
// arrives but the upstream stream EOFs without a trailing usage frame. The
// decoder must synthesize a final Done chunk so the SSE consumer can emit
// `data: [DONE]\n\n`. Bug: returning (chunk, io.EOF) together caused
// chunkSSEReader to drop the chunk; this regression test pins the fix.
func TestStreamDecoder_DoneSynthesizedAtEOF(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"text":"Hi"}]},"finishReason":"STOP"}]}`,
		``,
	}, "\n")
	dec := NewStreamDecoder(nil)
	sess, err := dec.Open(io.NopCloser(strings.NewReader(raw)), typology.WireShapeGeminiGenerateContent)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close() //nolint:errcheck

	var chunks []provcore.Chunk
	for i := range 4 {
		ch, err := sess.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		chunks = append(chunks, ch)
	}

	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks (content + synthesized Done), got %d", len(chunks))
	}
	if chunks[0].Done {
		t.Error("content chunk must not be Done")
	}
	if chunks[0].Delta != "Hi" {
		t.Errorf("content delta=%q want %q", chunks[0].Delta, "Hi")
	}
	if !chunks[1].Done {
		t.Error("synthesized trailer must be Done")
	}
}

// TestStreamDecoder_NoFinishReasonNoSynthesis ensures the EOF path does not
// emit a synthetic Done when no finishReason was ever observed (e.g. upstream
// errored out mid-stream). The consumer just sees EOF without a Done frame.
func TestStreamDecoder_NoFinishReasonNoSynthesis(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"text":"x"}]}}]}`,
		``,
	}, "\n")
	dec := NewStreamDecoder(nil)
	sess, err := dec.Open(io.NopCloser(strings.NewReader(raw)), typology.WireShapeGeminiGenerateContent)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close() //nolint:errcheck

	var sawDone bool
	for i := range 4 {
		ch, err := sess.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		if ch.Done {
			sawDone = true
		}
	}
	if sawDone {
		t.Error("must not synthesize Done without observing finishReason first")
	}
}

// TestStreamDecoder_ThoughtPartsRouteToReasoningDelta covers the streaming
// path: when a Gemini SSE frame carries a candidate part
// with thought=true, the chunk emitted to consumers must carry that text
// in ReasoningDelta — not Delta. Interleaved thought + non-thought parts
// within a single frame are split across the two fields preserving
// order so downstream encoders (canonicalbridge stream_encoders.go) can
// emit reasoning_content / thinking_delta frames distinct from visible
// content frames.
func TestStreamDecoder_ThoughtPartsRouteToReasoningDelta(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"text":"Thinking part one. ","thought":true}]}}]}`,
		``,
		`data: {"candidates":[{"content":{"parts":[{"text":"Visible chunk."}]}}]}`,
		``,
		`data: {"candidates":[{"content":{"parts":[{"text":"More thought.","thought":true},{"text":" Final visible."}]},"finishReason":"STOP"}]}`,
		``,
		`data: {"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3,"thoughtsTokenCount":8,"totalTokenCount":16}}`,
		``,
	}, "\n")
	dec := NewStreamDecoder(nil)
	sess, err := dec.Open(io.NopCloser(strings.NewReader(raw)), typology.WireShapeGeminiGenerateContent)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close() //nolint:errcheck

	var chunks []provcore.Chunk
	for {
		ch, err := sess.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		chunks = append(chunks, ch)
	}

	// Expected: 4 chunks — thought-only, visible-only, mixed-thought-and-visible (with finishReason), usage trailer (Done).
	if len(chunks) != 4 {
		t.Fatalf("expected 4 chunks, got %d", len(chunks))
	}

	// Chunk 0: pure thought.
	if chunks[0].Delta != "" {
		t.Errorf("chunk[0].Delta = %q, want empty (pure thought)", chunks[0].Delta)
	}
	if chunks[0].ReasoningDelta != "Thinking part one. " {
		t.Errorf("chunk[0].ReasoningDelta = %q, want %q", chunks[0].ReasoningDelta, "Thinking part one. ")
	}

	// Chunk 1: pure visible.
	if chunks[1].Delta != "Visible chunk." {
		t.Errorf("chunk[1].Delta = %q, want %q", chunks[1].Delta, "Visible chunk.")
	}
	if chunks[1].ReasoningDelta != "" {
		t.Errorf("chunk[1].ReasoningDelta = %q, want empty", chunks[1].ReasoningDelta)
	}

	// Chunk 2: interleaved within single frame.
	if chunks[2].Delta != " Final visible." {
		t.Errorf("chunk[2].Delta = %q, want %q", chunks[2].Delta, " Final visible.")
	}
	if chunks[2].ReasoningDelta != "More thought." {
		t.Errorf("chunk[2].ReasoningDelta = %q, want %q", chunks[2].ReasoningDelta, "More thought.")
	}

	// Chunk 3: usage trailer.
	if !chunks[3].Done {
		t.Error("chunk[3] must be Done")
	}
	if chunks[3].Usage == nil || chunks[3].Usage.ReasoningTokens == nil || *chunks[3].Usage.ReasoningTokens != 8 {
		t.Errorf("chunk[3] usage missing or wrong ReasoningTokens: %+v", chunks[3].Usage)
	}
}

package canonicalbridge

import (
	"context"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/tidwall/gjson"
)

// ptrInt is a tiny helper for the test fixtures below.
func ptrInt(i int) *int { return &i }

// extractSSEEvents splits a raw SSE byte slice into (event, data) pairs
// in order.
type sseFrame struct {
	event string
	data  string
}

func extractSSEEvents(raw []byte) []sseFrame {
	var out []sseFrame
	for _, block := range strings.Split(string(raw), "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var f sseFrame
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "event: ") {
				f.event = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				f.data = strings.TrimPrefix(line, "data: ")
			}
		}
		out = append(out, f)
	}
	return out
}

// TestResponsesStreamEncoder_TextChunks: text-only canonical chunks
// produce response.created → response.in_progress → output_item.added
// (message) → content_part.added → output_text.delta(×2) → close events
// → response.completed.
func TestResponsesStreamEncoder_TextChunks(t *testing.T) {
	enc := newResponsesStreamEncoder("gpt-5.2")
	var allOut []byte
	for _, c := range []provcore.Chunk{
		{Delta: "Hello"},
		{Delta: " world."},
		{Done: true, Usage: &provcore.Usage{
			PromptTokens:     ptrInt(5),
			CompletionTokens: ptrInt(2),
			TotalTokens:      ptrInt(7),
		}},
	} {
		out, err := enc.Write(context.Background(), c)
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
		allOut = append(allOut, out...)
	}
	frames := extractSSEEvents(allOut)
	wantOrder := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}
	if len(frames) != len(wantOrder) {
		t.Fatalf("expected %d frames, got %d:\n%s", len(wantOrder), len(frames), string(allOut))
	}
	for i, want := range wantOrder {
		if frames[i].event != want {
			t.Errorf("frame[%d].event = %q, want %q", i, frames[i].event, want)
		}
	}
	// Delta payloads.
	if got := gjson.Get(frames[4].data, "delta").String(); got != "Hello" {
		t.Errorf("frame[4].delta = %q", got)
	}
	if got := gjson.Get(frames[5].data, "delta").String(); got != " world." {
		t.Errorf("frame[5].delta = %q", got)
	}
	// Usage block on completed.
	if got := gjson.Get(frames[8].data, "response.usage.input_tokens").Int(); got != 5 {
		t.Errorf("completed.response.usage.input_tokens = %d", got)
	}
	if got := gjson.Get(frames[8].data, "response.usage.output_tokens").Int(); got != 2 {
		t.Errorf("completed.response.usage.output_tokens = %d", got)
	}
}

// TestResponsesStreamEncoder_SequenceNumberMonotonic pins that every
// emitted event carries a unique, monotonic sequence_number starting
// at 0.
func TestResponsesStreamEncoder_SequenceNumberMonotonic(t *testing.T) {
	enc := newResponsesStreamEncoder("gpt-5.2")
	var allOut []byte
	for _, c := range []provcore.Chunk{
		{Delta: "a"},
		{Delta: "b"},
		{Done: true},
	} {
		out, _ := enc.Write(context.Background(), c)
		allOut = append(allOut, out...)
	}
	frames := extractSSEEvents(allOut)
	var prev int64 = -1
	for i, f := range frames {
		seq := gjson.Get(f.data, "sequence_number").Int()
		if seq != prev+1 {
			t.Errorf("frame[%d] event=%s sequence_number=%d (want %d): %s", i, f.event, seq, prev+1, f.data)
		}
		prev = seq
	}
}

// TestResponsesStreamEncoder_ReasoningThenText: a reasoning chunk
// followed by a text chunk causes the encoder to open + close a
// reasoning item before opening the message item — preserving the
// canonical OpenAI emission order.
func TestResponsesStreamEncoder_ReasoningThenText(t *testing.T) {
	enc := newResponsesStreamEncoder("gpt-5.2")
	var allOut []byte
	for _, c := range []provcore.Chunk{
		{ReasoningDelta: "think"},
		{Delta: "answer"},
		{Done: true},
	} {
		out, _ := enc.Write(context.Background(), c)
		allOut = append(allOut, out...)
	}
	frames := extractSSEEvents(allOut)
	wantOrder := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added", // reasoning item opens
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta", // "think"
		"response.reasoning_summary_part.done",  // close reasoning before opening message
		"response.output_item.done",
		"response.output_item.added", // message item opens
		"response.content_part.added",
		"response.output_text.delta", // "answer"
		"response.content_part.done", // close message
		"response.output_item.done",
		"response.completed",
	}
	if len(frames) != len(wantOrder) {
		t.Fatalf("expected %d frames, got %d:\n%s", len(wantOrder), len(frames), string(allOut))
	}
	for i, want := range wantOrder {
		if frames[i].event != want {
			t.Errorf("frame[%d].event = %q, want %q", i, frames[i].event, want)
		}
	}
}

// TestResponsesStreamEncoder_FunctionCallTwoParts: two argument deltas
// for the same tool call emit a single output_item.added and two
// function_call_arguments.delta events.
func TestResponsesStreamEncoder_FunctionCallTwoParts(t *testing.T) {
	enc := newResponsesStreamEncoder("gpt-5.2")
	var allOut []byte
	for _, c := range []provcore.Chunk{
		{ToolCallDeltas: []provcore.ToolCallDelta{{Index: 0, ID: "call_a", Name: "get_weather", Arguments: `{"city":`}}},
		{ToolCallDeltas: []provcore.ToolCallDelta{{Index: 0, Arguments: `"Tokyo"}`}}},
		{Done: true},
	} {
		out, _ := enc.Write(context.Background(), c)
		allOut = append(allOut, out...)
	}
	frames := extractSSEEvents(allOut)
	var (
		addedCount, argsDeltaCount, doneCount int
	)
	for _, f := range frames {
		switch f.event {
		case "response.output_item.added":
			addedCount++
		case "response.function_call_arguments.delta":
			argsDeltaCount++
		case "response.function_call_arguments.done":
			doneCount++
		}
	}
	if addedCount != 1 {
		t.Errorf("expected 1 output_item.added, got %d:\n%s", addedCount, string(allOut))
	}
	if argsDeltaCount != 2 {
		t.Errorf("expected 2 function_call_arguments.delta, got %d", argsDeltaCount)
	}
	if doneCount != 1 {
		t.Errorf("expected 1 function_call_arguments.done, got %d", doneCount)
	}
}

// TestResponsesStreamEncoder_EmptyChunkAfterHeader: a Write that
// receives a chunk with no fields (nothing to emit) after the first
// real chunk does not produce any bytes.
func TestResponsesStreamEncoder_EmptyChunkAfterHeader(t *testing.T) {
	enc := newResponsesStreamEncoder("gpt-5.2")
	_, _ = enc.Write(context.Background(), provcore.Chunk{Delta: "x"}) // header + delta
	out, err := enc.Write(context.Background(), provcore.Chunk{})      // empty
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("empty chunk produced bytes: %s", string(out))
	}
}

// TestResponsesEncoder_FinishReasonToIncomplete proves the Responses encoder
// maps the canonical finish_reason onto the terminal event (HIGH-2): length /
// content_filter become response.incomplete with an incomplete_details.reason;
// empty / stop stay response.completed.
func TestResponsesEncoder_FinishReasonToIncomplete(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		fr         string
		wantEvent  string
		wantReason string
	}{
		{"", "response.completed", ""},
		{"stop", "response.completed", ""},
		{"length", "response.incomplete", "max_output_tokens"},
		{"content_filter", "response.incomplete", "content_filter"},
	}
	for _, c := range cases {
		enc := newResponsesStreamEncoder("m")
		out := string(mustWrite(t, enc, ctx, provcore.Chunk{Done: true, FinishReason: c.fr}))
		if !strings.Contains(out, c.wantEvent) {
			t.Errorf("fr=%q: want event %s, got %s", c.fr, c.wantEvent, out)
		}
		if c.wantReason != "" && !strings.Contains(out, `"reason":"`+c.wantReason+`"`) {
			t.Errorf("fr=%q: want incomplete reason %s, got %s", c.fr, c.wantReason, out)
		}
	}
}

func mustWrite(t *testing.T, enc StreamTranscoder, ctx context.Context, c provcore.Chunk) []byte {
	t.Helper()
	b, err := enc.Write(ctx, c)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	return b
}

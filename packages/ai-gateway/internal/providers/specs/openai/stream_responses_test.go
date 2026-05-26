package openai_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai"
)

// fakeBody is an io.ReadCloser over a string for test transcripts.
type fakeBody struct{ r *strings.Reader }

func newFakeBody(s string) *fakeBody           { return &fakeBody{r: strings.NewReader(s)} }
func (f *fakeBody) Read(p []byte) (int, error) { return f.r.Read(p) }
func (f *fakeBody) Close() error               { return nil }

// drain pulls every chunk from a session until io.EOF and returns them.
func drain(t *testing.T, s provcore.StreamSession) []provcore.Chunk {
	t.Helper()
	var out []provcore.Chunk
	for {
		c, err := s.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		out = append(out, c)
	}
	return out
}

// TestResponsesStreamSession_TextDeltas pins the simplest forward path:
// response.created → output_item.added(message) → content_part.added →
// 2 × output_text.delta → response.completed yields 2 canonical chunks
// with Delta set + 1 final Done chunk with Usage.
func TestResponsesStreamSession_TextDeltas(t *testing.T) {
	transcript := `event: response.created
data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress","model":"gpt-5.2"}}

event: response.in_progress
data: {"type":"response.in_progress"}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","status":"in_progress"}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"Hello"}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":" world."}

event: response.output_text.done
data: {"type":"response.output_text.done","output_index":0,"content_index":0,"text":"Hello world."}

event: response.content_part.done
data: {"type":"response.content_part.done","output_index":0,"content_index":0}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","status":"completed"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","model":"gpt-5.2","usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}

`
	dec := openai.NewResponsesStreamDecoder(slog.Default())
	sess, err := dec.Open(newFakeBody(transcript), typology.WireShapeOpenAIResponses)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = sess.Close() }()
	chunks := drain(t, sess)
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks (2 deltas + 1 done), got %d: %+v", len(chunks), chunks)
	}
	if chunks[0].Delta != "Hello" {
		t.Errorf("chunks[0].Delta = %q", chunks[0].Delta)
	}
	if chunks[1].Delta != " world." {
		t.Errorf("chunks[1].Delta = %q", chunks[1].Delta)
	}
	if !chunks[2].Done {
		t.Errorf("chunks[2].Done = false")
	}
	if chunks[2].Usage == nil || chunks[2].Usage.PromptTokens == nil || *chunks[2].Usage.PromptTokens != 5 {
		t.Errorf("chunks[2].Usage PromptTokens missing/wrong: %+v", chunks[2].Usage)
	}
	if chunks[2].Usage.CompletionTokens == nil || *chunks[2].Usage.CompletionTokens != 2 {
		t.Errorf("chunks[2].Usage CompletionTokens missing/wrong: %+v", chunks[2].Usage)
	}
}

// TestResponsesStreamSession_FunctionCallDeltas pins function call
// argument streaming.
func TestResponsesStreamSession_FunctionCallDeltas(t *testing.T) {
	transcript := `event: response.created
data: {"type":"response.created","response":{"id":"resp_2","status":"in_progress"}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_abc","name":"get_weather"}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"call_abc","delta":"{\"city\":"}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"call_abc","delta":"\"Tokyo\"}"}

event: response.function_call_arguments.done
data: {"type":"response.function_call_arguments.done","output_index":0,"item_id":"call_abc","arguments":"{\"city\":\"Tokyo\"}"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_2","status":"completed","usage":{"input_tokens":8,"output_tokens":4,"total_tokens":12}}}

`
	dec := openai.NewResponsesStreamDecoder(slog.Default())
	sess, _ := dec.Open(newFakeBody(transcript), typology.WireShapeOpenAIResponses)
	defer func() { _ = sess.Close() }()
	chunks := drain(t, sess)
	// Expect: 2 tool call delta chunks + 1 done.
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks, got %d: %+v", len(chunks), chunks)
	}
	if len(chunks[0].ToolCallDeltas) != 1 || chunks[0].ToolCallDeltas[0].Arguments != `{"city":` {
		t.Errorf("chunks[0].ToolCallDeltas = %+v", chunks[0].ToolCallDeltas)
	}
	if len(chunks[1].ToolCallDeltas) != 1 || chunks[1].ToolCallDeltas[0].Arguments != `"Tokyo"}` {
		t.Errorf("chunks[1].ToolCallDeltas = %+v", chunks[1].ToolCallDeltas)
	}
	if !chunks[2].Done {
		t.Errorf("chunks[2].Done = false")
	}
}

// TestResponsesStreamSession_ReasoningDeltas pins reasoning_summary_text
// + reasoning_text deltas route into chunk.ReasoningDelta.
func TestResponsesStreamSession_ReasoningDeltas(t *testing.T) {
	transcript := `event: response.created
data: {"type":"response.created","response":{"id":"resp_3"}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}

event: response.reasoning_summary_part.added
data: {"type":"response.reasoning_summary_part.added","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}

event: response.reasoning_summary_text.delta
data: {"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"thinking..."}

event: response.reasoning_summary_text.done
data: {"type":"response.reasoning_summary_text.done","output_index":0,"summary_index":0,"text":"thinking..."}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_3","status":"completed","usage":{"input_tokens":5,"output_tokens":3,"output_tokens_details":{"reasoning_tokens":3}}}}

`
	dec := openai.NewResponsesStreamDecoder(slog.Default())
	sess, _ := dec.Open(newFakeBody(transcript), typology.WireShapeOpenAIResponses)
	defer func() { _ = sess.Close() }()
	chunks := drain(t, sess)
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks (1 reasoning + 1 done), got %d", len(chunks))
	}
	if chunks[0].ReasoningDelta != "thinking..." {
		t.Errorf("chunks[0].ReasoningDelta = %q", chunks[0].ReasoningDelta)
	}
	if !chunks[1].Done {
		t.Errorf("chunks[1].Done = false")
	}
	if chunks[1].Usage == nil || chunks[1].Usage.ReasoningTokens == nil || *chunks[1].Usage.ReasoningTokens != 3 {
		t.Errorf("Usage.ReasoningTokens missing/wrong: %+v", chunks[1].Usage)
	}
}

// TestResponsesStreamSession_FailedTerminatesStream pins
// response.failed → Done=true with no further reads.
func TestResponsesStreamSession_FailedTerminatesStream(t *testing.T) {
	transcript := `event: response.failed
data: {"type":"response.failed","response":{"id":"resp_x","status":"failed","error":{"message":"boom"}}}

`
	dec := openai.NewResponsesStreamDecoder(slog.Default())
	sess, _ := dec.Open(newFakeBody(transcript), typology.WireShapeOpenAIResponses)
	defer func() { _ = sess.Close() }()
	chunks := drain(t, sess)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk (failed→done), got %d", len(chunks))
	}
	if !chunks[0].Done {
		t.Errorf("expected Done=true on failed terminal")
	}
}

// TestResponsesStreamSession_UnknownEventDoesNotAbort pins that an
// unknown future event type does NOT stop the stream — it's silently
// skipped (with a one-shot WARN).
func TestResponsesStreamSession_UnknownEventDoesNotAbort(t *testing.T) {
	transcript := `event: response.created
data: {"type":"response.created","response":{"id":"resp_u"}}

event: response.something_brand_new
data: {"type":"response.something_brand_new","data":"future"}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message"}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"hi"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_u","status":"completed"}}

`
	dec := openai.NewResponsesStreamDecoder(slog.Default())
	sess, _ := dec.Open(newFakeBody(transcript), typology.WireShapeOpenAIResponses)
	defer func() { _ = sess.Close() }()
	chunks := drain(t, sess)
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks (delta + done), got %d: %+v", len(chunks), chunks)
	}
	if chunks[0].Delta != "hi" {
		t.Errorf("delta lost after unknown event: %q", chunks[0].Delta)
	}
}

// TestResponsesStreamSession_BuiltinEventsIgnored pins that built-in
// tool events (web_search_call.in_progress etc.) are silently dropped
// from canonical without aborting.
func TestResponsesStreamSession_BuiltinEventsIgnored(t *testing.T) {
	transcript := `event: response.created
data: {"type":"response.created"}

event: response.web_search_call.in_progress
data: {"type":"response.web_search_call.in_progress","output_index":0}

event: response.web_search_call.completed
data: {"type":"response.web_search_call.completed","output_index":0}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"type":"message"}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"found it"}

event: response.completed
data: {"type":"response.completed","response":{"id":"r","status":"completed"}}

`
	dec := openai.NewResponsesStreamDecoder(slog.Default())
	sess, _ := dec.Open(newFakeBody(transcript), typology.WireShapeOpenAIResponses)
	defer func() { _ = sess.Close() }()
	chunks := drain(t, sess)
	// Only the output_text.delta + completed should produce chunks.
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(chunks))
	}
}

// TestResponsesStreamSession_NilBody pins the explicit Open error.
func TestResponsesStreamSession_NilBody(t *testing.T) {
	dec := openai.NewResponsesStreamDecoder(nil)
	if _, err := dec.Open(nil, typology.WireShapeOpenAIResponses); err == nil {
		t.Error("expected error on nil body")
	}
}

// TestStreamDecoder_EndpointDispatch pins the binding behavior:
// the unified openai.StreamDecoder.Open returns a Responses-
// grammar session when endpoint=EndpointResponsesAPI and the legacy
// chat-completions session otherwise. The auto-upgrade hook in the
// handler flips req.WireShape to EndpointResponsesAPI before the
// adapter dispatches, so this dispatch is the load-bearing connection
// that makes the upgraded stream pipeline parse the right SSE shape.
func TestStreamDecoder_EndpointDispatch(t *testing.T) {
	dec := openai.NewStreamDecoder(slog.Default())

	// Responses-API endpoint → responsesStreamSession.
	responsesTranscript := `event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"hi"}

event: response.completed
data: {"type":"response.completed","response":{"id":"r","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}

`
	sess, err := dec.Open(newFakeBody(responsesTranscript), typology.WireShapeOpenAIResponses)
	if err != nil {
		t.Fatalf("Open responses endpoint: %v", err)
	}
	chunks := drain(t, sess)
	_ = sess.Close()
	if len(chunks) != 2 {
		t.Fatalf("responses endpoint produced %d chunks; want 2 (delta + done)", len(chunks))
	}
	if chunks[0].Delta != "hi" {
		t.Errorf("responses delta = %q, want 'hi' (responsesStreamSession was NOT picked)", chunks[0].Delta)
	}

	// Default chat-completions endpoint → openaiStreamSession.
	chatTranscript := `data: {"choices":[{"delta":{"content":"yo"}}]}

data: [DONE]

`
	sess2, err := dec.Open(newFakeBody(chatTranscript), typology.WireShapeOpenAIChat)
	if err != nil {
		t.Fatalf("Open chat-completions endpoint: %v", err)
	}
	chunks2 := drain(t, sess2)
	_ = sess2.Close()
	if len(chunks2) == 0 || chunks2[0].Delta != "yo" {
		t.Errorf("chat-completions endpoint dispatch broken — chunks: %+v", chunks2)
	}
}

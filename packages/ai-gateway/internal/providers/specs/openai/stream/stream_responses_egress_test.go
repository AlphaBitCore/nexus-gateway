// Package stream_test — stream_responses_egress_test.go covers the
// content-sniffing /v1/responses egress session: it classifies the first SSE
// frame and either forwards genuine Responses frames verbatim (with a canonical
// tee for accounting) or decodes a chat.completion upstream into canonical so
// the proxy can re-shape it into Responses events with a terminal event.
package stream_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	ostream "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai/stream"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

func openResponsesEgress(t *testing.T, sse string) provcore.StreamSession {
	t.Helper()
	d := ostream.NewStreamDecoder(slog.Default())
	sess, err := d.Open(io.NopCloser(strings.NewReader(sse)), typology.WireShapeOpenAIResponses)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return sess
}

func drainSession(t *testing.T, sess provcore.StreamSession) []provcore.Chunk {
	t.Helper()
	var out []provcore.Chunk
	for {
		c, err := sess.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		out = append(out, c)
		if c.Done {
			break
		}
	}
	return out
}

// TestResponsesEgress_ChatFirst_DecodesCanonical is the core bug: a
// /v1/responses ingress whose upstream actually emits chat.completion.chunk
// frames must be DECODED into canonical chunks (not dropped, not forwarded
// verbatim) so the proxy's Responses encoder can re-shape them — the session
// resolves chat mode and never marks the chunks Verbatim.
func TestResponsesEgress_ChatFirst_DecodesCanonical(t *testing.T) {
	sse := "data: " + `{"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hel"}}]}` + "\n\n" +
		"data: " + `{"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"lo"}}]}` + "\n\n" +
		"data: " + `{"object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}` + "\n\n" +
		"data: [DONE]\n\n"
	chunks := drainSession(t, openResponsesEgress(t, sse))

	var text strings.Builder
	sawDone := false
	for _, c := range chunks {
		if c.Verbatim {
			t.Fatalf("chat-upstream chunks must NOT be marked Verbatim (would leak chat frames to a Responses client): %+v", c)
		}
		text.WriteString(c.Delta)
		if c.Done {
			sawDone = true
		}
	}
	if text.String() != "Hello" {
		t.Fatalf("decoded text = %q, want %q", text.String(), "Hello")
	}
	if !sawDone {
		t.Fatal("stream must terminate with a Done chunk so the encoder emits response.completed")
	}
}

// TestResponsesEgress_ResponsesFirst_VerbatimAndUsage is the raw-byte copier:
// a genuine Responses upstream (incl. a built-in web_search_call event) is
// forwarded byte-for-byte (Verbatim + RawBytes) so built-in-tool events reach
// the client, while usage is still teed from response.completed.
func TestResponsesEgress_ResponsesFirst_VerbatimAndUsage(t *testing.T) {
	webSearch := `{"type":"response.web_search_call.results","output_index":0,"results":[{"url":"https://x"}]}`
	sse := "event: response.created\ndata: " + `{"type":"response.created","response":{"id":"resp_1"}}` + "\n\n" +
		"event: response.output_text.delta\ndata: " + `{"type":"response.output_text.delta","delta":"hi"}` + "\n\n" +
		"event: response.web_search_call.results\ndata: " + webSearch + "\n\n" +
		"event: response.completed\ndata: " + `{"type":"response.completed","response":{"usage":{"input_tokens":7,"output_tokens":4,"total_tokens":11}}}` + "\n\n"
	chunks := drainSession(t, openResponsesEgress(t, sse))

	var rawAll strings.Builder
	var finalUsage *provcore.Usage
	sawText := false
	for _, c := range chunks {
		if !c.Verbatim {
			t.Fatalf("genuine Responses frames must be Verbatim: %+v", c)
		}
		rawAll.Write(c.RawBytes)
		if c.Delta == "hi" {
			sawText = true
		}
		if c.Usage != nil {
			finalUsage = c.Usage
		}
	}
	if !sawText {
		t.Fatal("output_text.delta must surface a canonical Delta for the enforcement lane")
	}
	if !strings.Contains(rawAll.String(), "response.web_search_call.results") {
		t.Fatalf("built-in web_search_call event must reach the client byte-for-byte; raw=%s", rawAll.String())
	}
	if finalUsage == nil || finalUsage.TotalTokens == nil || *finalUsage.TotalTokens != 11 {
		t.Fatalf("usage must be teed from response.completed; got %+v", finalUsage)
	}
}

// TestResponsesEgress_Copier_CanonicalTee covers the canonical fields the
// enforcement lane reads off a verbatim copier chunk: reasoning + refusal
// deltas, function-call argument deltas, and a tool_calls finish_reason on the
// terminal frame once a function call was seen.
func TestResponsesEgress_Copier_CanonicalTee(t *testing.T) {
	sse := "event: response.created\ndata: " + `{"type":"response.created"}` + "\n\n" +
		"event: response.reasoning_summary_text.delta\ndata: " + `{"type":"response.reasoning_summary_text.delta","delta":"because"}` + "\n\n" +
		"event: response.refusal.delta\ndata: " + `{"type":"response.refusal.delta","delta":"no"}` + "\n\n" +
		"event: response.function_call_arguments.delta\ndata: " + `{"type":"response.function_call_arguments.delta","output_index":2,"item_id":"call_9","delta":"{\"a\":1}"}` + "\n\n" +
		"event: response.completed\ndata: " + `{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"
	chunks := drainSession(t, openResponsesEgress(t, sse))

	var reasoning, refusal, finish string
	var tool *provcore.ToolCallDelta
	for i := range chunks {
		c := chunks[i]
		reasoning += c.ReasoningDelta
		if c.FinishReason != "" {
			finish = c.FinishReason
		}
		if len(c.ToolCallDeltas) > 0 {
			tool = &c.ToolCallDeltas[0]
		}
		if c.Delta == "no" {
			refusal = c.Delta
		}
	}
	if reasoning != "because" {
		t.Fatalf("reasoning delta = %q, want %q", reasoning, "because")
	}
	if refusal != "no" {
		t.Fatal("refusal.delta must surface a canonical Delta")
	}
	if tool == nil || tool.ID != "call_9" || tool.Index != 2 || tool.Arguments != `{"a":1}` {
		t.Fatalf("function_call_arguments.delta must tee to a ToolCallDelta; got %+v", tool)
	}
	if finish != "tool_calls" {
		t.Fatalf("terminal finish_reason after a function call = %q, want tool_calls", finish)
	}
}

// TestResponsesEgress_Copier_TerminalReasons covers the incomplete/failed
// terminal mappings: content_filter and length on response.incomplete, and a
// Done chunk on response.failed.
func TestResponsesEgress_Copier_TerminalReasons(t *testing.T) {
	cases := []struct {
		name, event, terminal, wantFinish string
	}{
		{"incomplete_content_filter", "response.incomplete", `{"type":"response.incomplete","response":{"incomplete_details":{"reason":"content_filter"}}}`, "content_filter"},
		{"incomplete_length", "response.incomplete", `{"type":"response.incomplete","response":{"incomplete_details":{"reason":"max_output_tokens"}}}`, "length"},
		{"failed", "response.failed", `{"type":"response.failed","response":{}}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sse := "event: response.created\ndata: " + `{"type":"response.created"}` + "\n\n" +
				"event: " + tc.event + "\ndata: " + tc.terminal + "\n\n"
			chunks := drainSession(t, openResponsesEgress(t, sse))
			last := chunks[len(chunks)-1]
			if !last.Done {
				t.Fatalf("terminal event must produce a Done chunk; got %+v", last)
			}
			if last.FinishReason != tc.wantFinish {
				t.Fatalf("FinishReason = %q, want %q", last.FinishReason, tc.wantFinish)
			}
		})
	}
}

// TestResponsesEgress_Copier_EdgeFrames covers an event-less frame (type only
// in data), an empty-data keep-alive that is skipped, and the EOF returned when
// Next is called after the stream is Done.
func TestResponsesEgress_Copier_EdgeFrames(t *testing.T) {
	sse := "event: response.created\ndata: " + `{"type":"response.created"}` + "\n\n" +
		"data: " + `{"type":"response.output_text.delta","delta":"x"}` + "\n\n" +
		"event: ping\ndata:\n\n" +
		"event: response.completed\ndata: " + `{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"
	sess := openResponsesEgress(t, sse)
	chunks := drainSession(t, sess)
	if _, err := sess.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("Next after Done must return io.EOF, got %v", err)
	}
	var text string
	for _, c := range chunks {
		if !c.Verbatim {
			t.Fatalf("copier frames must be Verbatim: %+v", c)
		}
		text += c.Delta
	}
	if text != "x" {
		t.Fatalf("event-less output_text.delta must still decode a canonical Delta; got %q", text)
	}
}

type errReadCloser struct{ err error }

func (e errReadCloser) Read([]byte) (int, error) { return 0, e.err }
func (e errReadCloser) Close() error             { return nil }

// TestResponsesEgress_ScannerError: a non-EOF read error surfaces from Next.
func TestResponsesEgress_ScannerError(t *testing.T) {
	boom := errors.New("boom")
	d := ostream.NewStreamDecoder(slog.Default())
	sess, err := d.Open(errReadCloser{err: boom}, typology.WireShapeOpenAIResponses)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_, err = sess.Next(context.Background())
	if err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("a non-EOF read error must surface from Next, got %v", err)
	}
}

// TestResponsesEgress_ContextCancel: a cancelled context surfaces the error
// from Next rather than a chunk.
func TestResponsesEgress_ContextCancel(t *testing.T) {
	sess := openResponsesEgress(t, "event: response.created\ndata: "+`{"type":"response.created"}`+"\n\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sess.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled context must surface context.Canceled, got %v", err)
	}
}

// TestResponsesEgress_GarbageFirst_FailsClosedToChat: an unclassifiable first
// frame must NOT be forwarded verbatim — the session falls back to the chat
// decode lane (canonical, non-Verbatim) so the proxy re-encodes a valid
// Responses envelope rather than leaking garbage.
func TestResponsesEgress_GarbageFirst_FailsClosedToChat(t *testing.T) {
	sse := "data: " + `{"unexpected":"garbage"}` + "\n\n" + "data: [DONE]\n\n"
	chunks := drainSession(t, openResponsesEgress(t, sse))
	for _, c := range chunks {
		if c.Verbatim {
			t.Fatalf("unclassifiable frame must fail closed to chat mode (never Verbatim): %+v", c)
		}
	}
}

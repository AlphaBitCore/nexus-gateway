package codecs

import (
	"context"
	"strings"
	"testing"

	core "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// TestOpenAIResponses_EmptyTerminalSplicesDeltas pins the codex behaviour: its
// response.completed terminal event carries an EMPTY output[], and the actual
// assistant reply lives only in the streamed output_text.delta frames. The
// folder must splice the accumulated deltas in rather than trusting the empty
// terminal — otherwise the assistant text is silently dropped (it was, on every
// codex turn). Verified against a real on-host capture.
func TestOpenAIResponses_EmptyTerminalSplicesDeltas(t *testing.T) {
	sse := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","object":"response","status":"in_progress","model":"gpt-5.4"}}`,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"Hello "}`,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"world 4444"}`,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.4","output":[],"usage":{"input_tokens":10,"output_tokens":3,"total_tokens":13}}}`,
		``,
	}, "\n")

	n := NewOpenAIResponsesNormalizer()
	p, err := n.Normalize(context.Background(), []byte(sse), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(p.Messages) == 0 || len(p.Messages[0].Content) == 0 {
		t.Fatalf("no assistant message extracted: %+v", p.Messages)
	}
	got := p.Messages[len(p.Messages)-1].Content[len(p.Messages[len(p.Messages)-1].Content)-1].Text
	if got != "Hello world 4444" {
		t.Fatalf("assistant text = %q, want %q (deltas must be spliced when terminal output[] is empty)", got, "Hello world 4444")
	}
	// usage from the terminal must be preserved alongside the spliced text.
	if p.Usage == nil || p.Usage.TotalTokens == nil || *p.Usage.TotalTokens != 13 {
		t.Errorf("usage lost when splicing deltas: %+v", p.Usage)
	}
}

// TestOpenAIResponses_TerminalWithTextUnchanged guards the standard-OpenAI path:
// when the terminal output[] already carries the assistant output_text, it stays
// authoritative and the deltas are NOT double-appended.
func TestOpenAIResponses_TerminalWithTextUnchanged(t *testing.T) {
	sse := strings.Join([]string{
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"Hi"}`,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hi there"}]}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`,
		``,
	}, "\n")

	n := NewOpenAIResponsesNormalizer()
	p, err := n.Normalize(context.Background(), []byte(sse), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	// Exactly one assistant message with the terminal's authoritative text.
	if len(p.Messages) != 1 {
		t.Fatalf("want 1 message, got %d: %+v", len(p.Messages), p.Messages)
	}
	full := ""
	for _, c := range p.Messages[0].Content {
		full += c.Text
	}
	if full != "Hi there" {
		t.Fatalf("assistant text = %q, want %q (terminal authoritative, no delta double-append)", full, "Hi there")
	}
}

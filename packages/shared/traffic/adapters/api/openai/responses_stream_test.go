package openai

import (
	"context"
	"strings"
	"testing"
)

// A Responses event is a self-describing envelope with no `choices`, so the chat
// extractor reads nothing out of one. What the streaming compliance substrate
// scans is exactly what this returns, so an event whose text lands in no channel
// is text delivered to the client and scanned by nothing.
func TestResponsesStreamEventRoutesTextToItsChannel(t *testing.T) {
	const secret = "call me on 555-867-5309"

	cases := []struct {
		name      string
		frame     string
		content   string
		reasoning string
		toolArgs  string
		finish    string
	}{
		{
			name:    "assistant text delta is scannable content",
			frame:   `{"type":"response.output_text.delta","delta":"` + secret + `"}`,
			content: secret,
		},
		{
			name:      "reasoning summary delta is reasoning, not content",
			frame:     `{"type":"response.reasoning_summary_text.delta","delta":"` + secret + `"}`,
			reasoning: secret,
		},
		{
			name:     "function-call arguments are a tool call, not prose",
			frame:    `{"type":"response.function_call_arguments.delta","delta":"{\"city\":"}`,
			toolArgs: `{"city":`,
		},
		{
			// The done events repeat the ACCUMULATED value under their own field
			// name. Counting them as well would scan the same characters twice and
			// ask the splice to mask one span in two different frames.
			name:  "the done event repeats accumulated text and must contribute nothing",
			frame: `{"type":"response.output_text.done","text":"` + secret + `"}`,
		},
		{
			name:  "lifecycle events carry no fragment",
			frame: `{"type":"response.output_item.added","item":{"type":"message"}}`,
		},
		{
			name:   "the terminal event surfaces its status the way chat surfaces finish_reason",
			frame:  `{"type":"response.completed","response":{"status":"completed","output":[]}}`,
			finish: "completed",
		},
		{
			// An event kind this code has never seen. Anything that is not named
			// reasoning or function-call arguments is treated as assistant-visible
			// text, so a newly shipped text item is scanned by default rather than
			// silently skipped.
			name:    "an unknown text event defaults to the scanned channel",
			frame:   `{"type":"response.output_audio_transcript.delta","delta":"` + secret + `"}`,
			content: secret,
		},
		{
			name:  "an empty delta is not a fragment",
			frame: `{"type":"response.output_text.delta","delta":""}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nc, err := (&Adapter{}).ExtractStreamChunk(context.Background(), []byte(tc.frame), "/v1/responses")
			if err != nil {
				t.Fatalf("ExtractStreamChunk: %v", err)
			}
			if got := strings.Join(nc.Segments, ""); got != tc.content {
				t.Errorf("content = %q, want %q", got, tc.content)
			}
			if got := strings.Join(nc.ReasoningSegments, ""); got != tc.reasoning {
				t.Errorf("reasoning = %q, want %q", got, tc.reasoning)
			}
			if got := strings.Join(nc.ToolCallSegments, ""); got != tc.toolArgs {
				t.Errorf("tool args = %q, want %q", got, tc.toolArgs)
			}
			if got := nc.Metadata["finish_reason"]; got != tc.finish {
				t.Errorf("finish_reason = %q, want %q", got, tc.finish)
			}
		})
	}
}

// A chat chunk must keep going through the chat extractor. The Responses
// dispatch keys on a top-level `type` beginning with `response.`, and a chat
// chunk has no top-level type at all — but the two grammars travel the same
// function, so the separation is worth pinning rather than assuming.
func TestChatChunkIsNotMistakenForAResponsesEvent(t *testing.T) {
	const said = "hello"
	frame := `{"object":"chat.completion.chunk","choices":[{"delta":{"content":"` + said + `"},"finish_reason":"stop"}]}`

	nc, err := (&Adapter{}).ExtractStreamChunk(context.Background(), []byte(frame), "/v1/chat/completions")
	if err != nil {
		t.Fatalf("ExtractStreamChunk: %v", err)
	}
	if got := strings.Join(nc.Segments, ""); got != said {
		t.Fatalf("content = %q, want %q — the chat wire stopped being read", got, said)
	}
	if got := nc.Metadata["finish_reason"]; got != "stop" {
		t.Errorf("finish_reason = %q, want %q", got, "stop")
	}
}

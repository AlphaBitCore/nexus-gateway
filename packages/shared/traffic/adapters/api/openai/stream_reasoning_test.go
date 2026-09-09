package openai

import (
	"context"
	"strings"
	"testing"
)

// The OpenAI-compatible chat wire has two spellings for the same channel:
// `reasoning_content` (OpenAI, DeepSeek) and `reasoning` (xAI, OpenRouter). No
// captured corpus in this repo carries the second one — the vendors that emit it
// were never recorded — so a corpus-driven test cannot observe it, and a gate
// that cannot observe a channel is not guarding it. This drives both spellings
// through the real adapter instead.
//
// It is not only an audit-fidelity question. What the compliance substrate scans
// on a streaming response is what ExtractStreamChunk reports, so a spelling the
// adapter does not read is chain-of-thought delivered to the client and scanned
// by nothing.
func TestStreamReasoningSpellingsBothReachTheReasoningChannel(t *testing.T) {
	cases := []struct {
		name  string
		field string
		who   string
	}{
		{"openai and deepseek", "reasoning_content", "OpenAI / DeepSeek"},
		{"xai and openrouter", "reasoning", "xAI / OpenRouter"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const secret = "the reasoning said 123-45-6789"
			frame := `{"choices":[{"delta":{"` + tc.field + `":"` + secret + `"}}]}`

			nc, err := (&Adapter{}).ExtractStreamChunk(context.Background(), []byte(frame), "/v1/chat/completions")
			if err != nil {
				t.Fatalf("ExtractStreamChunk: %v", err)
			}
			got := strings.Join(nc.ReasoningSegments, "")
			if got != secret {
				t.Fatalf("delta.%s (%s) produced ReasoningSegments %q, want %q — that vendor's "+
					"chain of thought reaches the client unscanned and unaudited",
					tc.field, tc.who, got, secret)
			}
			// It must NOT land in Segments: the two lists are consumed differently
			// (transcripts, UI rendering, and the rewrite path all treat reasoning
			// separately), so putting it in the wrong one is its own defect.
			if c := strings.Join(nc.Segments, ""); c != "" {
				t.Errorf("delta.%s also produced content Segments %q; reasoning is not "+
					"assistant-visible content and has no rewrite slot", tc.field, c)
			}
		})
	}
}

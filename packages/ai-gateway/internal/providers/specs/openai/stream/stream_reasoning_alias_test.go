// Package stream_test — the `reasoning` spelling of the chain-of-thought channel.
// Named failure modes:
//   - reasoning (xAI / OpenRouter spelling): routed to ReasoningDelta, never to Delta
//   - both spellings on one delta: reasoning_content wins, transcript not doubled
package stream_test

import (
	"context"
	"log/slog"
	"testing"

	ostream "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai/stream"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// TestStreamDecoder_reasoningAlias_routedToReasoningDelta pins the alternate wire
// name for the reasoning channel.
//
// Why it is load-bearing rather than cosmetic: response hooks run on the CANONICAL
// waist (the enforcing stream consumes the canonical chunk subscription
// pre-transcode), so a channel this decoder drops is one the compliance pipeline can
// never scan. Under an enforcing response scope the wire is then REBUILT from the
// canonical chunk, so the dropped text does not reach the client either — while the
// unenforced lane forwards the upstream frame byte-for-byte and keeps it. Dropping a
// channel here therefore makes turning compliance ON change the response body.
//
// The shared normalize codec already accepts both spellings (its Delta.Reasoning
// field cites xAI and OpenRouter); this keeps the streaming decoder from disagreeing
// with the fold about the same wire format.
func TestStreamDecoder_reasoningAlias_routedToReasoningDelta(t *testing.T) {
	for _, tc := range []struct {
		name       string
		delta      string
		wantReason string
		wantAnswer string
	}{
		{
			name:       "alias alongside answer content",
			delta:      `{"content":"answer","reasoning":"think"}`,
			wantReason: "think",
			wantAnswer: "answer",
		},
		{
			name:       "alias with no answer content",
			delta:      `{"reasoning":"only thinking"}`,
			wantReason: "only thinking",
			wantAnswer: "",
		},
		{
			// Both spellings on one delta: reasoning_content wins, matching
			// firstNonEmptyString(ReasoningContent, Reasoning) in the normalize
			// fold. Concatenating both would double the transcript.
			name:       "both spellings, reasoning_content wins",
			delta:      `{"reasoning_content":"canonical","reasoning":"alias"}`,
			wantReason: "canonical",
			wantAnswer: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := sseBody("data: " + `{"choices":[{"index":0,"delta":` + tc.delta + `,"finish_reason":null}]}` + "\n\n")
			d := ostream.NewStreamDecoder(slog.Default())
			sess, _ := d.Open(body, typology.WireShapeOpenAIChat)
			defer sess.Close()

			chunk, err := sess.Next(context.Background())
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
			if chunk.ReasoningDelta != tc.wantReason {
				t.Errorf("ReasoningDelta: got %q, want %q", chunk.ReasoningDelta, tc.wantReason)
			}
			if chunk.Delta != tc.wantAnswer {
				t.Errorf("Delta: got %q, want %q", chunk.Delta, tc.wantAnswer)
			}
		})
	}
}

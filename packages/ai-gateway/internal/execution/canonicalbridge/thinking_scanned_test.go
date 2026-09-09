package canonicalbridge_test

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// The compliance prescan reads Delta, RefusalDelta and ReasoningDelta. It does
// NOT read NexusThinking[].Thinking, and NexusThinking IS delivered — the
// Anthropic encoder writes it out as a thinking block.
//
// That is safe today for one reason only: the Anthropic decoder sets
// ReasoningDelta on every thinking_delta AND accumulates the same bytes into
// the block it later republishes on signature_delta. So every byte that appears
// in Thinking was already scanned when it arrived on ReasoningDelta.
//
// Nothing enforced that. A decoder that populated Thinking without having
// emitted the same text on ReasoningDelta would deliver unscanned reasoning,
// and no existing test would notice — the channel comparison sees the text
// (it IS delivered) and the prescan tests never look at this field.
//
// Adding Thinking to the scan surface would be the wrong fix: the prescan would
// then count and rescan bytes it has already seen, on the per-byte path. The
// right one is to hold the invariant that makes the omission safe.
func TestThinkingRepublishesOnlyAlreadyScannedText(t *testing.T) {
	// Two of the three files named thinking_* turn out to carry only text_delta
	// — the names promise more than the captures hold. That is recorded rather
	// than silently tolerated: the counter below fails if NO capture exercises
	// a republished thinking block, so the day the one that does goes stale,
	// this stops reporting a pass.
	withSubjects := 0
	for _, corpus := range []string{"anthropic_thinking_block", "anthropic_thinking_text", "anthropic_thinking_tools"} {
		t.Run(corpus, func(t *testing.T) {
			raw := readCorpus(t, corpus)
			sess, err := anthropicOpen(io.NopCloser(strings.NewReader(raw)), typology.WireShapeAnthropicMessages)
			if err != nil {
				t.Fatalf("open %s: %v", corpus, err)
			}
			defer sess.Close()

			var reasoning strings.Builder
			var republished []string
			for {
				chunk, err := sess.Next(context.Background())
				if err != nil {
					break
				}
				reasoning.WriteString(chunk.ReasoningDelta)
				for _, b := range chunk.NexusThinking {
					if b.Thinking != "" {
						republished = append(republished, b.Thinking)
					}
				}
				if chunk.Done {
					break
				}
			}

			scanned := reasoning.String()
			for i, text := range republished {
				if !strings.Contains(scanned, text) {
					t.Errorf("NexusThinking[%d] carries %d bytes that never appeared on "+
						"ReasoningDelta.\nthinking: %q\nscanned:  %q\n"+
						"The prescan reads ReasoningDelta and not this field, so those bytes are "+
						"delivered to the client without ever being looked at.",
						i, len(text), clip(text), clip(scanned))
				}
			}
			if len(republished) > 0 {
				withSubjects++
			}
			t.Logf("%s: %d republished thinking block(s), %d bytes scanned on ReasoningDelta",
				corpus, len(republished), len(scanned))
		})
	}
	if withSubjects == 0 {
		t.Error("no capture republished a thinking block, so every subtest above asserted " +
			"nothing. The invariant this gate exists for — that Thinking only ever repeats text " +
			"already seen on ReasoningDelta — is now untested.")
	}
}

// TestRedactedThinkingCarriesNoReadableText pins the other producer: a
// redacted_thinking block is Anthropic's own opaque blob, so it fills
// RedactedData and must leave Thinking empty — otherwise unscanned readable text
// rides in on a field named for the fact that it has none, and the Anthropic
// encoder republishes it to the client.
//
// It drives the REAL decoder. The first version of this test built the block
// literal itself and then asserted about that literal, so no production symbol
// was involved and it could not go red for any change at all. Mutation caught
// it: adding `Thinking: <text>` to the decoder's redacted_thinking branch left
// it green.
func TestRedactedThinkingCarriesNoReadableText(t *testing.T) {
	// A stream that opens a redacted_thinking block, then a normal thinking
	// block whose signature_delta forces the republish. The second block is what
	// makes the assertion reachable: redacted blocks are emitted at
	// content_block_start, ordinary ones only when their signature arrives.
	const sse = "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"m1","model":"claude-opus-4","usage":{"input_tokens":1}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"AAAAopaque"}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"thinking_delta","thinking":"visible reasoning"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"signature_delta","signature":"sig-1"}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	sess, err := anthropicOpen(io.NopCloser(strings.NewReader(sse)), typology.WireShapeAnthropicMessages)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()

	redactedSeen, republishedSeen := 0, 0
	for {
		chunk, err := sess.Next(context.Background())
		if err != nil {
			break
		}
		for i, b := range chunk.NexusThinking {
			if b.RedactedData != "" {
				redactedSeen++
				if b.Thinking != "" {
					t.Errorf("block %d carries both redacted data and readable thinking (%q); "+
						"the redacted branch must not populate text the prescan never sees",
						i, b.Thinking)
				}
			} else {
				republishedSeen++
			}
		}
	}
	// Both arms have to have been exercised, or the assertion above ran zero
	// times and this reports a pass for having decoded nothing.
	if redactedSeen == 0 {
		t.Error("the decoder produced no redacted_thinking block, so the assertion never ran")
	}
	if republishedSeen == 0 {
		t.Error("the decoder produced no ordinary republished block, so the stream this test " +
			"feeds no longer exercises the branch it is contrasting against")
	}
}

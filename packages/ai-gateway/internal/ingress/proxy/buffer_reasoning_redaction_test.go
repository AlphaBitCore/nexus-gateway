package proxy

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	normcodecs "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/codecs"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// The buffered-stream path builds its canonical body BY HAND rather than through
// a codec, and it used to leave the reasoning channel out of it on the stated
// grounds that the non-stream path did not scan reasoning either.
//
// That premise stopped being true when the response stage moved to the canonical
// waist. This path kept the old coverage, which left it as the one gateway path
// still re-emitting reasoning no policy could touch — on exactly the models that
// put most of their content there.
//
// The chain under test is the whole one, because each link was individually
// plausible and the gap only exists between them: accumulate → canonical body →
// canonical decode → redact → rewrite → synthetic chunk back to the wire.
func TestBufferedStreamReasoningIsScannedAndRedacted(t *testing.T) {
	const secret = "alice@example.com"

	acc := newCanonicalStreamAccumulator("deepseek-reasoner")
	acc.add(provcore.Chunk{ReasoningDelta: "the user wrote from " + secret})
	acc.add(provcore.Chunk{Delta: "I will not repeat it."})
	body := acc.canonicalBody()

	// 1. The reasoning must be in the canonical body at all. Without this the
	//    rest of the chain has nothing to work on and would pass vacuously.
	if got := gjson.GetBytes(body, "choices.0.message.reasoning_content").String(); !strings.Contains(got, secret) {
		t.Fatalf("reasoning_content = %q — the canonical body does not carry the reasoning, "+
			"so no hook can see it and no redaction can reach it", got)
	}

	// 2. The canonical DECODE must produce it as a reasoning block. A body that
	//    carries the text in a channel the decoder skips is the same gap one
	//    layer down.
	payload, err := normcodecs.NewOpenAIChatNormalizer().Normalize(context.Background(), body, normcore.Meta{
		AdapterType:  string(provcore.FormatOpenAI),
		Direction:    normcore.DirectionResponse,
		EndpointPath: "/v1/chat/completions",
		ContentType:  "application/json",
	})
	if err != nil {
		t.Fatalf("canonical decode: %v", err)
	}
	var reasoningBlocks int
	for _, m := range payload.Messages {
		for _, b := range m.Content {
			if b.Type == normcore.ContentReasoning && strings.Contains(b.Text, secret) {
				reasoningBlocks++
			}
		}
	}
	if reasoningBlocks == 0 {
		t.Fatalf("the canonical decode produced no reasoning block carrying the secret; "+
			"payload = %+v", payload.Messages)
	}

	// 3. A redaction applied to that block must reach the wire body.
	for mi := range payload.Messages {
		for bi := range payload.Messages[mi].Content {
			b := &payload.Messages[mi].Content[bi]
			b.Text = strings.ReplaceAll(b.Text, secret, "[REDACTED]")
		}
	}
	rewritten, n, err := normcodecs.RewriteCanonicalResponseContent(body, payload)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if n == 0 {
		t.Fatal("the rewrite reported no writes for a body whose reasoning was masked")
	}

	// 4. And the chunk that goes back onto the client's wire must carry the
	//    MASKED reasoning. Reading it from the accumulator instead of from the
	//    rewritten body is the specific way this last link failed.
	ch := syntheticChunkFromCanonical(rewritten)
	if strings.Contains(ch.ReasoningDelta, secret) {
		t.Errorf("the synthetic chunk carries the unredacted reasoning (%q) — the redaction "+
			"was applied and then read around", ch.ReasoningDelta)
	}
	if !strings.Contains(ch.ReasoningDelta, "[REDACTED]") {
		t.Errorf("ReasoningDelta = %q, want the mask — the reasoning channel was dropped "+
			"between the rewrite and the wire", ch.ReasoningDelta)
	}
	// The control: the visible answer, which always worked, must still work.
	if ch.Delta != "I will not repeat it." {
		t.Errorf("Delta = %q — the visible answer changed, so this test would have passed "+
			"for the wrong reason", ch.Delta)
	}
}

// A stream with no reasoning must not gain an empty channel: emitting
// `reasoning_content: ""` would make every non-reasoning response decode to a
// reasoning block, and the rewrite pairs slots to blocks by position.
func TestBufferedStreamOmitsTheReasoningChannelWhenThereIsNone(t *testing.T) {
	acc := newCanonicalStreamAccumulator("gpt-4o")
	acc.add(provcore.Chunk{Delta: "hello"})
	body := acc.canonicalBody()
	if gjson.GetBytes(body, "choices.0.message.reasoning_content").Exists() {
		t.Errorf("a response with no reasoning grew a reasoning_content field: %s", body)
	}
}

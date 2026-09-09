package tlsbump

import (
	"context"
	"testing"

	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	normcodecs "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/codecs"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// The inflight redact path hands the hook pipeline's content blocks to an
// adapter's RewriteRequestBody, which writes block i into its own wire's text
// slot i. That contract holds only while the blocks came from the SAME
// adapter's ExtractRequest. Once the registry became the preferred decoder the
// blocks usually come from a canonical decode instead, and canonical block
// order is not the adapter's slot order — measured on a real Anthropic request,
// the rewriter got 9 blocks, wrote 3 slots, left a document block's SSN on the
// wire, overwrote a tool_result slot with a different block's text, and the
// audit row still said action=redact.
//
// These tests pin the predicate that decides whether the positional contract
// applies. They are written against the ACTUAL producers rather than against
// the protocol string, so that a producer changing its tag fails here instead
// of silently re-opening the corruption.
func TestPositionalRewriteAlignedTracksItsProducers(t *testing.T) {
	t.Run("adapter extraction fallback is aligned", func(t *testing.T) {
		// PayloadFromTextSegments is the only producer whose ordering is the
		// adapter's own — it is built FROM the adapter's ExtractRequest output.
		p := hookcore.PayloadFromTextSegments([]string{"one", "two"})
		if p == nil {
			t.Fatal("PayloadFromTextSegments returned nil for a non-empty input")
		}
		if !positionalRewriteAligned(p) {
			t.Fatalf("the adapter-extraction fallback must stay aligned, else every "+
				"non-registry redact silently stops rewriting; got protocol %q", p.Protocol)
		}
	})

	t.Run("no decode at all is aligned", func(t *testing.T) {
		// The pre-registry shape the positional contract was written for.
		if !positionalRewriteAligned(nil) {
			t.Fatal("a nil payload means no canonical decode happened; the positional " +
				"contract is the only one available and must remain usable")
		}
	})

	t.Run("a real canonical decode is NOT aligned", func(t *testing.T) {
		// Drive an actual normalizer rather than hand-stamping a Protocol
		// string: a test that asserts against a literal would keep passing if
		// the decoder started tagging its output differently.
		body := []byte(`{"model":"claude-x","max_tokens":16,"messages":[` +
			`{"role":"user","content":[{"type":"text","text":"look at the claim"},` +
			`{"type":"document","source":{"type":"text","media_type":"text/plain","data":"SSN 222-33-4444"}}]}]}`)
		n := normcodecs.NewAnthropicMessagesNormalizer()
		p, err := n.Normalize(context.Background(), body, normalize.Meta{
			AdapterType:  "anthropic",
			Direction:    normalize.DirectionRequest,
			EndpointPath: "/v1/messages",
			ContentType:  "application/json",
		})
		if err != nil {
			t.Fatalf("normalize a real anthropic request: %v", err)
		}
		if p.Protocol == "synthetic" {
			t.Fatalf("the canonical decoder must not claim the adapter-extraction tag; " +
				"if it does, the predicate cannot tell the two apart at all")
		}
		if positionalRewriteAligned(&p) {
			t.Fatalf("a canonical decode (protocol %q) was treated as positionally "+
				"aligned — this is the state that wrote a redaction into the wrong wire "+
				"slot and left the document block's SSN upstream", p.Protocol)
		}
	})

	t.Run("the two producers are actually distinguishable", func(t *testing.T) {
		// Anti-vacuity: if both sides ever produced the same tag the predicate
		// would be a constant, and the two subtests above could both pass while
		// testing nothing.
		flat := hookcore.PayloadFromTextSegments([]string{"x"})
		body := []byte(`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"x"}]}`)
		canon, err := normcodecs.NewAnthropicMessagesNormalizer().Normalize(
			context.Background(), body, normalize.Meta{
				AdapterType:  "anthropic",
				Direction:    normalize.DirectionRequest,
				EndpointPath: "/v1/messages",
				ContentType:  "application/json",
			})
		if err != nil {
			t.Fatalf("normalize: %v", err)
		}
		if flat.Protocol == canon.Protocol {
			t.Fatalf("both producers tag %q — the alignment predicate has nothing to "+
				"decide on and the positional corruption is unguarded", flat.Protocol)
		}
	})
}

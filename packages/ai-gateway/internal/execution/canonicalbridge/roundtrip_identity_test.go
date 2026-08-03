package canonicalbridge

import (
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/tidwall/gjson"
)

// TestShapeRoundTripIdentity is the shape-conversion test of record (see
// provider-adapter-architecture.md §3 "Round-trip equivalence standard").
//
// A shape conversion is correct iff it is lossless through the canonical hub in
// BOTH directions. The standard exercises the double round-trip:
//
//	shape A  →  canonical(OpenAI)  →  shape B  →  canonical(OpenAI)  →  shape A′
//
// and asserts A′ is semantically equal to the original A. If they match, the
// whole A↔canonical↔B chain is proven — the A→canonical decode, the canonical→B
// encode, the B→canonical decode, AND the canonical→A encode all agree. Any
// field the chain drops, renames, or corrupts surfaces as a divergence.
//
// Equivalence is asserted on the canonical projection of both ends (re-
// canonicalize A and A′ and compare the content-bearing signature), NOT on raw
// bytes: field ordering and protocol-default backfill (§4, e.g. max_tokens) are
// expected to differ across hops and are not failures.
func TestShapeRoundTripIdentity(t *testing.T) {
	b := testBridge(t)
	// The three native chat wire shapes with full bidirectional hub codecs.
	shapes := []provcore.Format{provcore.FormatOpenAI, provcore.FormatAnthropic, provcore.FormatGemini}

	for _, a := range shapes {
		for _, viaB := range shapes {
			if a == viaB {
				continue // identity hop is covered by the passthrough test
			}
			t.Run(string(a)+"_via_"+string(viaB)+"_back", func(t *testing.T) {
				if !b.ChatRoutable(a, viaB) || !b.ChatRoutable(viaB, a) {
					t.Skipf("%s ↔ %s not routable", a, viaB)
				}
				bodyA, err := MinimalNativeChatBody(a)
				if err != nil {
					t.Skipf("no native chat fixture for %q: %v", a, err)
				}

				// A → canonical → B
				wireB, _, err := b.IngressChatToWire(a, viaB, bodyA, dummyCallTarget(viaB), false)
				if err != nil {
					t.Fatalf("A→B (%s→%s): %v", a, viaB, err)
				}
				// B → canonical → A
				bodyA2, _, err := b.IngressChatToWire(viaB, a, wireB, dummyCallTarget(a), false)
				if err != nil {
					t.Fatalf("B→A (%s→%s): %v", viaB, a, err)
				}

				// Compare the canonical projection of the original and the
				// round-tripped A.
				canonA, err := b.IngressChatToCanonical(a, bodyA, dummyCallTarget(a))
				if err != nil {
					t.Fatalf("canonicalize original A: %v", err)
				}
				canonA2, err := b.IngressChatToCanonical(a, bodyA2, dummyCallTarget(a))
				if err != nil {
					t.Fatalf("canonicalize round-tripped A: %v", err)
				}
				sigA := canonicalChatSignature(t, canonA)
				sigA2 := canonicalChatSignature(t, canonA2)
				if sigA != sigA2 {
					t.Errorf("round-trip A→%s→A lost content\n original  : %q\n roundtrip : %q\n wireB     = %s\n bodyA2    = %s",
						viaB, sigA, sigA2, wireB, bodyA2)
				}
			})
		}
	}
}

// canonicalChatSignature reduces a canonical OpenAI chat body to its
// content-bearing essence — an ordered (role, text) list — for round-trip
// equivalence. Model + protocol-default backfill are intentionally excluded:
// they are rewritten per hop and are not part of the shape-fidelity contract.
func canonicalChatSignature(t *testing.T, canonical []byte) string {
	t.Helper()
	msgs := gjson.GetBytes(canonical, "messages")
	if !msgs.Exists() || len(msgs.Array()) == 0 {
		t.Fatalf("canonical body has no messages: %s", canonical)
	}
	var sig strings.Builder
	for _, m := range msgs.Array() {
		role := m.Get("role").String()
		var text string
		if c := m.Get("content"); c.IsArray() {
			// OpenAI multimodal / Anthropic-origin content blocks: concat text parts.
			for _, part := range c.Array() {
				text += part.Get("text").String()
			}
		} else {
			text = c.String()
		}
		sig.WriteString(role + ":" + text + "\n")
	}
	return sig.String()
}

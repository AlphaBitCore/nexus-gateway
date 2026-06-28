package core_test

import (
	"context"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/codecs"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// TestEveryAllFormatsHasTier1Normalizer is the
// cross-service compliance assertion that guarantees ai-gateway's
// chat-routing matrix and shared/transport/normalize/codecs stay in
// lockstep.
//
// The PreHookCallback path (responseprehook.Build) feeds the
// Registry with Meta.AdapterType = strings.ToLower(target.AdapterType)
// where target.AdapterType is exactly a provcore.Format string. If a
// new Format lands in core/types.go AllFormats() without a
// corresponding codecs.RegisterDefaultAIBuiltins() entry, every SSE
// response on that wire shape silently falls to Tier 2/3
// (pattern-extract or generic-http) and the audit row's
// normalized_response loses adapter-specific structure (model name,
// tool_calls, reasoning segments). The user sees a flat-text payload
// instead of a structured ai-chat claim.
//
// The test asserts: for every Format in AllFormats(), the shared
// Registry resolves a Tier 1 normalizer that claims with confidence
// above the routing threshold (i.e. it is the right normalizer, not a
// generic-http catch-all).
//
// On failure the message names the missing Format so the fix is "add
// it to register.go's openAICompatible list (or a dedicated key)".
func TestEveryAllFormatsHasTier1Normalizer(t *testing.T) {
	reg := normcore.NewRegistry()
	codecs.RegisterDefaultAIBuiltins(reg)

	for _, format := range provcore.AllFormats() {
		// capture
		t.Run(string(format), func(t *testing.T) {
			meta := normcore.Meta{
				AdapterType: string(format),
				// Use a per-family body that the corresponding Tier 1
				// normalizer recognises as its own (otherwise we can't
				// tell Tier 1 routing from Tier 3 fallback). The chosen
				// shapes are the minimum schema needed for each spec
				// to claim with full confidence on the request leg.
				ContentType: "application/json",
				Direction:   normcore.DirectionRequest,
			}
			body := canonicalRequestBodyFor(format)

			payload, err := reg.Normalize(context.Background(), body, meta)
			if err != nil {
				t.Fatalf("Format %q: Registry.Normalize returned error %v — no Tier 1 entry?", format, err)
			}

			// Tier 3 generic-http stamps Protocol="generic-http"; Tier 2
			// pattern-extract stamps Protocol="pattern-extract". A real
			// Tier 1 claim stamps the family-specific protocol
			// (openai-chat, anthropic-messages, gemini-generate,
			// openai-embeddings, voyage-embeddings, etc.). A fall-
			// through to Tier 2 / 3 indicates the canonical-bridge map
			// drift this test exists to catch.
			if payload.Protocol == "generic-http" || payload.Protocol == "pattern-extract" {
				t.Fatalf("Format %q: fell through to %q (Tier 2/3) — add this Format to codecs.RegisterDefaultAIBuiltins or its openAICompatible list", format, payload.Protocol)
			}
		})
	}
}

// canonicalRequestBodyFor returns a minimal valid request body that
// the wire family's Tier 1 normalizer recognises as its own. Used
// only by TestEveryAllFormatsHasTier1Normalizer to probe routing.
func canonicalRequestBodyFor(f provcore.Format) []byte {
	switch f {
	case provcore.FormatAnthropic, provcore.FormatBedrock:
		// AnthropicMessagesNormalizer: max_tokens + messages.
		return []byte(`{"model":"claude-3-sonnet","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`)
	case provcore.FormatGemini, provcore.FormatVertex:
		// GeminiGenerateNormalizer: contents + parts.
		return []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	case provcore.FormatVoyage:
		// Voyage is an embeddings-only adapter; canonical body is
		// {input, model} — same shape OpenAIEmbeddingsNormalizer
		// accepts. The voyage adapter-key resolves to
		// VoyageEmbeddingsNormalizer which expects this shape.
		return []byte(`{"model":"voyage-3","input":"hello"}`)
	default:
		// All other Formats route to NewOpenAIChatNormalizer on the
		// chat path. Minimum claim shape = model + messages[].
		return []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`)
	}
}

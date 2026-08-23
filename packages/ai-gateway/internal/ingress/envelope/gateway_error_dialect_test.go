package envelope

import (
	"encoding/json"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// allIngressFormats is every format the gateway can see on an ingress. Listed
// rather than derived because the point is to notice a NEW one: adding a format
// without deciding whether it has an error dialect is exactly how cohere ended
// up on the upstream encoder.
var allIngressFormats = []provcore.Format{
	provcore.FormatOpenAI, provcore.FormatDeepSeek, provcore.FormatGLM,
	provcore.FormatAzureOpenAI, provcore.FormatAnthropic, provcore.FormatGemini,
	provcore.FormatMiniMax, provcore.FormatBedrock, provcore.FormatVertex,
	provcore.FormatCohere, provcore.FormatHuggingFace, provcore.FormatReplicate,
	provcore.FormatMistral, provcore.FormatXai, provcore.FormatGroq,
	provcore.FormatPerplexity, provcore.FormatTogether, provcore.FormatFireworks,
	provcore.FormatMoonshot, provcore.FormatVoyage, provcore.FormatOpenAIResponses,
}

// TestGatewayErrorNeverTakesTheUpstreamEncoder pins the decision that produced
// two envelope shapes for one code in production.
//
// /v1/rerank answered SPEND_LIMIT_EXCEEDED with "param": null while
// /v1/images/generations answered the same code with no param key at all. Both
// errors are the gateway's own — a spend ceiling it enforced before dispatch —
// and both went through the same writeCodecErr. What differed was the ingress:
// the writers branched on IsOpenAIFamily(), cohere is not in that set, and the
// non-family branch falls through EncodeErrorEnvelopeForIngress's default onto
// encodeOpenAIErrorEnvelope — the encoder for UPSTREAM bodies, which stamps
// "param": null unconditionally because that is what a real OpenAI error
// carries.
//
// The question a gateway error has to answer is not "is this ingress in the
// OpenAI family" but "does this ingress have an error dialect of its own".
// Only anthropic, gemini/vertex and the Responses API do. Everything else,
// including any format added tomorrow, must reach the gateway builder.
func TestGatewayErrorNeverTakesTheUpstreamEncoder(t *testing.T) {
	// A code that maps to no param. The gateway builder omits the key; the
	// upstream encoder writes it as null. That difference is the fingerprint.
	const code = "SPEND_LIMIT_EXCEEDED"

	// The expectation is written HERE, not read from hasOwnErrorDialect. A test
	// that asks the predicate which branch to assert cannot catch the predicate
	// being wrong — the first version of this test did exactly that, and a
	// mutation restoring the old family-based decision passed it, because
	// cohere simply took the other branch and was never checked.
	ownDialect := map[provcore.Format]bool{
		provcore.FormatAnthropic:       true,
		provcore.FormatGemini:          true,
		provcore.FormatVertex:          true,
		provcore.FormatOpenAIResponses: true,
	}

	for _, f := range allIngressFormats {
		t.Run(string(f), func(t *testing.T) {
			body := GatewayErrorBodyForIngress(f, 400, code, `field "n" is out of range`, "")

			var outer map[string]any
			if err := json.Unmarshal(body, &outer); err != nil {
				t.Fatalf("body is not JSON: %s", body)
			}

			if ownDialect[f] {
				// Anthropic wraps in {"type":"error",...}; Gemini/Vertex and the
				// Responses API keep their own error object. The one thing none
				// of them may do is come back as the plain OpenAI upstream shape.
				inner, _ := outer["error"].(map[string]any)
				if f == provcore.FormatAnthropic && outer["type"] != "error" {
					t.Fatalf("anthropic ingress lost its envelope: %s", body)
				}
				if inner != nil {
					if _, hasParam := inner["param"]; hasParam {
						t.Fatalf("ingress %q has its own dialect but answered the OpenAI "+
							"upstream shape: %s", f, body)
					}
				}
				return
			}

			inner, ok := outer["error"].(map[string]any)
			if !ok {
				t.Fatalf("ingress %q has no error dialect, so it must answer the OpenAI "+
					"shape; got: %s", f, body)
			}
			if _, present := inner["param"]; present {
				t.Fatalf("ingress %q took the UPSTREAM encoder: %q maps to no param, "+
					"so the gateway builder omits the key — a null param means the "+
					"provider-error writer produced this. Body: %s", f, code, body)
			}
			if inner["code"] != code {
				t.Fatalf("ingress %q lost the code: got %v, want %q (%s)", f, inner["code"], code, body)
			}
		})
	}
}

// TestExactlyFourIngressesHaveTheirOwnErrorDialect keeps the predicate honest.
// If a format grows a dialect, this test is where that is recorded; if one is
// added to the set by accident, this is where it is caught.
func TestExactlyFourIngressesHaveTheirOwnErrorDialect(t *testing.T) {
	var own []provcore.Format
	for _, f := range allIngressFormats {
		if hasOwnErrorDialect(f) {
			own = append(own, f)
		}
	}
	want := []provcore.Format{
		provcore.FormatAnthropic, provcore.FormatGemini,
		provcore.FormatVertex, provcore.FormatOpenAIResponses,
	}
	if len(own) != len(want) {
		t.Fatalf("ingresses with their own error dialect = %v, want %v", own, want)
	}
	for i := range want {
		if own[i] != want[i] {
			t.Fatalf("ingresses with their own error dialect = %v, want %v", own, want)
		}
	}
}

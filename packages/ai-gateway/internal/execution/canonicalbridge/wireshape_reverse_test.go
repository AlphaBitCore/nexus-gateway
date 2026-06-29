package canonicalbridge

import (
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// forwardEgressChatFormats is every provider Format that the forward egress
// map ResponseCanonicalToIngress can reshape canonical chat into. Kept in
// lockstep with the switch in ResponseCanonicalToIngress (bridge.go). If a
// new ingress response codec is added there, add the Format here and the
// reverse-map parity assertion below will force wireShapeToFormatEndpoint to
// cover its chat wire shape too.
var forwardEgressChatFormats = []provcore.Format{
	provcore.FormatOpenAI,
	provcore.FormatAnthropic,
	provcore.FormatGemini,
	provcore.FormatVertex,
	provcore.FormatOpenAIResponses,
}

// TestWireShapeToFormatEndpoint_CoversForwardEgressMap is the parity
// guard: every chat wire shape that the forward egress map
// (ResponseCanonicalToIngress) can produce must be reversible by
// wireShapeToFormatEndpoint, and the reverse must round-trip back to the
// originating Format. Without this, ResponseAcrossFormats silently fails the
// `to` lookup for non-OpenAI/non-Anthropic readers and the cache-HIT path
// serves a wrong-shape body (proxy_cache_hits.go fallback).
func TestWireShapeToFormatEndpoint_CoversForwardEgressMap(t *testing.T) {
	for _, f := range forwardEgressChatFormats {
		var shape typology.WireShape
		if f == provcore.FormatOpenAIResponses {
			// Responses-API has no chatWireShapeForFormat entry; its native
			// chat wire shape is openai-responses.
			shape = typology.WireShapeOpenAIResponses
		} else {
			shape = chatWireShapeForFormat(f)
		}
		gotFormat, gotEndpoint, ok := wireShapeToFormatEndpoint(shape)
		if !ok {
			t.Fatalf("format %q (chat wire shape %q): wireShapeToFormatEndpoint returned ok=false; forward egress map handles it but reverse map does not", f, shape)
		}
		if gotFormat != f {
			t.Errorf("format %q chat wire shape %q: reverse map yielded format %q, want %q", f, shape, gotFormat, f)
		}
		if gotEndpoint != shape {
			t.Errorf("format %q chat wire shape %q: reverse map yielded endpoint %q, want the same wire shape %q", f, shape, gotEndpoint, shape)
		}
	}
}

// TestWireShapeToFormatEndpoint_CoversAllChatAndEmbeddingsShapes asserts the
// reverse map covers every chat-kind and embeddings-kind wire shape in the
// closed typology enumeration. Audio / image / batches surfaces (and the
// bedrock-invoke raw passthrough) intentionally return ok=false because the
// bridge has no canonical pipeline for them.
func TestWireShapeToFormatEndpoint_CoversAllChatAndEmbeddingsShapes(t *testing.T) {
	for _, w := range typology.AllWireShapes {
		kind := typology.KindFromWireShape(w)
		_, _, ok := wireShapeToFormatEndpoint(w)
		wantOK := (kind == typology.EndpointKindChat || kind == typology.EndpointKindEmbeddings) &&
			w != typology.WireShapeBedrockInvoke
		if ok != wantOK {
			t.Errorf("wire shape %q (kind %q): wireShapeToFormatEndpoint ok=%v, want %v", w, kind, ok, wantOK)
		}
	}
}

// TestWireShapeToFormatEndpoint_RejectsNonCanonicalSurfaces locks the
// negative contract: surfaces without a chat/embeddings canonical pipeline
// must return ok=false so ResponseAcrossFormats errors (and the caller falls
// back to serving the stored bytes) rather than mis-decoding.
func TestWireShapeToFormatEndpoint_RejectsNonCanonicalSurfaces(t *testing.T) {
	for _, w := range []typology.WireShape{
		typology.WireShapeOpenAIAudioSpeech,
		typology.WireShapeOpenAIAudioTranscriptions,
		typology.WireShapeOpenAIImages,
		typology.WireShapeOpenAIBatches,
		typology.WireShapeBedrockInvoke,
		typology.WireShapeNone,
	} {
		if _, _, ok := wireShapeToFormatEndpoint(w); ok {
			t.Errorf("wire shape %q: expected ok=false (no canonical pipeline), got ok=true", w)
		}
	}
}

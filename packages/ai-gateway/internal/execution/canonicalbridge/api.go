package canonicalbridge

import (
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// API is the subset of [Bridge] the AI Gateway handler depends on.
// Production wires *Bridge directly; tests substitute fakes to drive
// otherwise-unreachable error arms (ResponseCanonicalToIngress reshape
// failure, IngressChatToCanonical decode failure, NewStreamTranscoder
// returning a custom transcoder for assertion purposes) without
// standing up the full per-format SchemaCodec registry.
//
// Kept additive-only and surface-equivalent: every method here mirrors
// the matching *Bridge receiver verbatim. Adding a new caller in the
// handler that needs another Bridge method requires extending this
// interface in the same PR (compile-time check below catches missing
// methods on *Bridge; the inverse is enforced by review).
type API interface {
	// EndpointRoutable extends routing compatibility beyond chat when
	// embeddings or model listing keep the legacy OpenAI-only
	// translation rule.
	EndpointRoutable(ep typology.WireShape, ingress, target provcore.Format) bool
	// ServesResponses reports whether the resolved target serves the
	// OpenAI /v1/responses wire end-to-end. override is the per-provider
	// downgrade-only signal (nil = adapter RequestShapes default).
	ServesResponses(target provcore.Format, override *bool) bool
	// IngressChatToCanonical converts the client ingress JSON to
	// canonical OpenAI chat.completions request JSON.
	IngressChatToCanonical(ingress provcore.Format, body []byte, ct provcore.CallTarget) ([]byte, error)
	// ResponseCanonicalToIngress converts canonical OpenAI
	// chat.completion response JSON into the client's ingress
	// response shape.
	ResponseCanonicalToIngress(ingress provcore.Format, canonical []byte) ([]byte, error)
	// ResponseAcrossFormats reshapes a response body from one wire
	// shape to another via canonical chat.completion. Used by the
	// cache HIT path when the cached entry's origin ingress shape
	// differs from the requesting ingress shape (cross-ingress shape
	// contamination fix).
	ResponseAcrossFormats(from typology.WireShape, to typology.WireShape, body []byte) ([]byte, error)
	// NewStreamTranscoder returns a StreamTranscoder for the given
	// ingress→target pair, or nil when the pair is a passthrough
	// that does not require re-encoding.
	//
	// Note: this method takes provcore.Format (not typology.WireShape)
	// because the SSE encoder grammar is per-Format-family — every
	// OpenAI-family wire shape (openai-chat, openai-completions-legacy,
	// openai-embeddings) shares the OpenAI SSE grammar. The codec
	// interface methods (EncodeRequest / DecodeResponse) take WireShape
	// because they convert between wire-shape-specific byte layouts.
	// This asymmetry is intentional — the SSE wrapper protocol and the
	// JSON body shape are two different things.
	NewStreamTranscoder(ingress, target provcore.Format, model string) StreamTranscoder
	// ChatWireShapeForTarget returns the native chat-completions wire
	// shape a target provider format serves (e.g. FormatAnthropic →
	// WireShapeAnthropicMessages; every OpenAI-family format →
	// WireShapeOpenAIChat). Callers that have already canonicalized a
	// request use this to tell the target adapter's codec which wire
	// shape to encode to, instead of leaking the caller's ingress shape.
	ChatWireShapeForTarget(target provcore.Format) typology.WireShape
	// EmbeddingsWireShapeForTarget is the embeddings-kind counterpart of
	// ChatWireShapeForTarget.
	EmbeddingsWireShapeForTarget(target provcore.Format) typology.WireShape
	// StripInternalCarriersForTarget removes canonical-only carrier fields
	// (today: nexus_thinking) that must not egress to an OpenAI-wire target's
	// verbatim identity codec. The cache-prep primary egress leg calls this
	// after canonicalizing; the failover leg gets it inside IngressChatToWire.
	StripInternalCarriersForTarget(canon []byte, target provcore.Format) []byte
	// IngressEmbeddingsToCanonical converts a client embeddings ingress body
	// to canonical OpenAI /v1/embeddings request JSON (embeddings counterpart
	// of IngressChatToCanonical).
	IngressEmbeddingsToCanonical(ingress provcore.Format, body []byte, ct provcore.CallTarget) ([]byte, error)
	// ResponseCanonicalToIngressEmbeddings converts a canonical OpenAI
	// embeddings response into the caller's ingress embeddings shape
	// (embeddings counterpart of ResponseCanonicalToIngress).
	ResponseCanonicalToIngressEmbeddings(ingress provcore.Format, canonical []byte) ([]byte, error)
	// ImagesWireShapeForTarget is the image-kind counterpart of
	// ChatWireShapeForTarget (OpenAI-family → openai-images; Gemini →
	// the image leg of :generateContent).
	ImagesWireShapeForTarget(target provcore.Format) typology.WireShape
	// IngressImagesToCanonical validates an OpenAI-shaped images ingress
	// body (validation + identity — the image canonical IS the ingress
	// shape). Cross-format lane only.
	IngressImagesToCanonical(ingress provcore.Format, body []byte, ct provcore.CallTarget) ([]byte, error)
	// IngressImagesToWire converts a canonical images body to the target
	// wire, surfacing the codec's coercion rewrites so failover legs keep
	// their x-nexus-coerced markers (images counterpart of
	// IngressChatToWire; validates via IngressImagesToCanonical first).
	IngressImagesToWire(ingress, target provcore.Format, body []byte, ct provcore.CallTarget) ([]byte, []string, error)
	// RerankWireShapeForTarget is the rerank-kind counterpart of
	// ChatWireShapeForTarget (Cohere → cohere-rerank; Voyage → voyage-rerank).
	// Consumed by both the prepare stage and the executor call-time rewrite.
	RerankWireShapeForTarget(target provcore.Format) typology.WireShape
	// IngressRerankToCanonical validates a Cohere-shaped rerank ingress body
	// (validation + identity — the rerank canonical IS the Cohere ingress
	// shape, since OpenAI ships no rerank API). Cross-format lane only.
	IngressRerankToCanonical(ingress provcore.Format, body []byte, ct provcore.CallTarget) ([]byte, error)
	// IngressRerankToWire converts a canonical (Cohere-shaped) rerank body to
	// the target wire (rerank counterpart of IngressImagesToWire; validates via
	// IngressRerankToCanonical first).
	IngressRerankToWire(ingress, target provcore.Format, body []byte, ct provcore.CallTarget) ([]byte, []string, error)
}

// Compile-time assertion that the production type satisfies the API.
var _ API = (*Bridge)(nil)

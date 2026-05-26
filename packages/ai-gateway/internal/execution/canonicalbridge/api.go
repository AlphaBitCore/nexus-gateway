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
	// TargetNativelyServesResponsesAPI reports whether a target
	// provider wire format natively serves /v1/responses.
	TargetNativelyServesResponsesAPI(target provcore.Format) bool
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
	// contamination fix, Option B2).
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
}

// Compile-time assertion that the production type satisfies the API.
var _ API = (*Bridge)(nil)

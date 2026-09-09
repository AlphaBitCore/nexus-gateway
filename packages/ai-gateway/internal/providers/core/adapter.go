package core

import (
	"context"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// PreparedBody is everything [Adapter.PrepareBody] produces for one request
// apart from the error: the wire bytes, and the facts about them that only the
// codec that built them knows.
//
// URLOverride is the EncodeResult.URLOverride the codec emitted for this body
// (e.g. the Gemini embedding codec selects ":embedContent" vs
// ":batchEmbedContents" by input shape). Callers that reuse the prepared body
// on the cache-MISS fast path MUST pass it back into ExecuteWithBody so the
// override reaches the dispatched URL — without it a batch-shaped Gemini body
// lands on the single-embed action and 400s.
//
// PromptCacheMarked says the codec turned on the provider's prompt cache for
// this body. It rides here rather than being re-derived by the caller because
// the decision depends on whether the CALLER already sent a cache marker,
// which only the codec inspects — a second copy of that predicate outside the
// codec is a second copy that can disagree with the bytes actually sent.
type PreparedBody struct {
	Body              []byte
	Rewrites          []string
	URLOverride       string
	PromptCacheMarked bool
}

// Adapter is the uniform contract every provider implementation exposes
// to the rest of the gateway. The concrete implementation is the
// composed [specAdapter] that wraps an [AdapterSpec].
type Adapter interface {
	// Format returns the adapter's native wire format. Used by the
	// registry for lookup and by the executor to decide whether a
	// request's BodyFormat matches the adapter (passthrough fast path)
	// or must be translated via the spec's SchemaCodec.
	Format() Format

	// SupportsShape reports whether this adapter natively serves the
	// given typology.WireShape at the codec boundary. The canonical
	// bridge consults this to decide same-shape passthrough vs
	// cross-format canonical translation. See AdapterSpec.RequestShapes
	// for the contract; an adapter with no explicit RequestShapes
	// declaration is treated as supporting WireShapeOpenAIChat only.
	SupportsShape(shape typology.WireShape) bool

	// Execute invokes the upstream provider. If req.Stream is true,
	// the returned Response.Stream is populated and Response.Body is nil.
	// Execute returns a non-nil *ProviderError on upstream failure; the
	// returned error is always *ProviderError (wrapping any transport
	// error) so callers can branch on ProviderError.Code.
	Execute(ctx context.Context, req Request) (*Response, error)

	// Probe is a health check. It must not mutate CallTarget. Cheap,
	// idempotent, and wrapped in a short timeout by the implementation.
	Probe(ctx context.Context, target CallTarget) (*ProbeResult, error)

	// PrepareBody is the pure-function part of Execute up to but
	// excluding the network call. Idempotent; no side effects.
	PrepareBody(req Request) (PreparedBody, error)

	// ExecuteWithBody is Execute with the body already prepared by
	// PrepareBody. The cache layer calls this on a MISS so PrepareBody
	// runs exactly once per request.
	//
	// Contract:
	//   - body is the final wire bytes for the upstream; this method
	//     does not run the codec / passthrough rewrite again. Callers
	//     that synthesize their own body MUST replicate what
	//     PrepareBody would produce, including any model-alias rewrite.
	//   - body == nil is permitted iff req.WireShape == EndpointModels
	//     (which uses GET with no body); for any POST endpoint a non-
	//     nil body is required.
	//   - rewrites is propagated verbatim into Response.Coerced and
	//     surfaced as the x-nexus-coerced audit header.
	//   - urlOverride is the PrepareBody URLOverride for this body; pass
	//     "" when the transport's default URL applies.
	ExecuteWithBody(ctx context.Context, req Request, body []byte, rewrites []string, urlOverride string) (*Response, error)
}

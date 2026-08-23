package proxy

// proxy_egress_reshape.go holds the response leg of the round-trip invariant
// "request: A→canonical→B; response: B→canonical→A" — turning a canonical
// (OpenAI-shaped) non-stream response back into the caller's ingress wire
// shape, and removing the gateway's own namespace on the way out.
//
// Split from proxy_upstream.go, which owns fetching from the upstream and
// handling the response that comes back. Reshaping is neither: it is a pure
// function of (ingress shape, body), and it is the frame the client-facing
// guarantee is stated at, so it is worth finding on its own.

import (
	"fmt"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/canonicalext"
	openairesponses "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai/responses"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// egressReshapeNonStream reshapes a CANONICAL (OpenAI) non-stream response body
// back to the caller's ingress wire shape — the response leg of the round-trip
// invariant "request: A→canonical→B; response: B→canonical→A"
// (provider-adapter-architecture.md §3).
//
// The body is canonical on BOTH live response paths: the adapter's
// SchemaCodec.DecodeResponse decodes the upstream B-shape to canonical OpenAI
// (specAdapter.Execute returns CanonicalBody), so handleNonStream's result.Body
// is canonical, and the broker collects/serves the same canonical bytes. The
// reshape is therefore driven SOLELY by the ingress shape A — never by
// ingress-vs-target. (The prior per-path gates — direct "ingress != target",
// broker "WireShape==OpenAIChat" — both returned canonical OpenAI for a native
// non-OpenAI ingress: anthropic /v1/messages + gemini /v1beta got `choices[]`
// instead of `content[]`/`candidates[]`.)
//
// NOT for the cache HIT path: handleNonStreamHit reads the L1 entry which is
// stored POST-reshape in the writer's ORIGIN wire shape, so it reshapes via the
// OriginWireShape gate, not this helper.
//
// Two skip cases, both correct because the body is already in shape A:
//   - OpenAI-family chat/embeddings ingress: canonical IS the ingress shape, so
//     this is the identity — short-circuit (avoids a no-op call + preserves the
//     same-format passthrough optimisation).
//   - /v1/responses NATIVE passthrough (target serves responses-api natively):
//     the body is already Responses-shape; re-encoding via EncodeResponsesResponse
//     would double-encode and strip output[].content[].text.
func (h *Handler) egressReshapeNonStream(ingress Ingress, target routingcore.RoutingTarget, body []byte) ([]byte, error) {
	shaped, err := h.egressReshapeNonStreamInner(ingress, target, body)
	if err != nil {
		return nil, err
	}
	// The last frame the live non-stream response passes through before it
	// becomes client bytes and the audit copy.
	//
	// Three of this function's five arms return the body verbatim, and canonical
	// is the OpenAI-wire caller's shape, so on those arms whatever a codec left in
	// the gateway's namespace is what the caller receives. The other two arms are
	// projections — they rebuild the client body from named fields — and drop it
	// for free; stripping here rather than per-arm means a future arm is covered
	// the day it is written instead of the day someone remembers it.
	//
	// Codecs are expected not to write the namespace into a response at all (the
	// four that did were deleted, since nothing read three of them and the fourth
	// duplicated a canonical usage field). What remains is
	// nexus.ext.openai.responses.*, a real carrier the Responses egress encoder
	// consumes — which an OpenAI-chat caller routed to a Responses-API target
	// would otherwise receive.
	return canonicalext.Strip(shaped), nil
}

func (h *Handler) egressReshapeNonStreamInner(ingress Ingress, target routingcore.RoutingTarget, body []byte) ([]byte, error) {
	if h.deps.CanonicalBridge == nil || len(body) == 0 {
		return body, nil
	}
	if ingress.WireShape == typology.WireShapeOpenAIResponses {
		// Content-authoritative egress: the wire shape follows the ACTUAL
		// response bytes, never the target Format. A Responses-shape body is
		// already in the client's shape (verbatim — zero-loss for built-in
		// tools); a chat.completion body is canonical and re-encodes to the
		// Responses output[] grammar via EncodeResponsesResponse. An
		// unclassifiable body must NEVER be forwarded verbatim to a
		// /v1/responses client — fail closed with a 502 so a chat-shaped or
		// garbage reply can't leak in the wrong wire shape.
		switch openairesponses.ClassifyNonStreamBody(body) {
		case openairesponses.ClassResponses:
			return body, nil
		case openairesponses.ClassChat:
			return h.deps.CanonicalBridge.ResponseCanonicalToIngress(ingress.BodyFormat, body)
		default:
			return nil, fmt.Errorf("egress: unclassifiable /v1/responses upstream body; refusing verbatim passthrough")
		}
	}
	if ingress.BodyFormat.IsOpenAIFamily() {
		// Canonical == OpenAI shape == the caller's shape. Identity.
		return body, nil
	}
	if typology.KindFromWireShape(ingress.WireShape) == typology.EndpointKindEmbeddings {
		return h.deps.CanonicalBridge.ResponseCanonicalToIngressEmbeddings(ingress.BodyFormat, body)
	}
	if typology.KindFromWireShape(ingress.WireShape) == typology.EndpointKindRerank {
		// Rerank's canonical shape IS the wire shape. OpenAI ships no rerank
		// API, so the canonical form was defined as Cohere's — there is no
		// translation to perform, and the body is already what the caller
		// asked for. Without this the reshape fell through to the chat
		// encoder, which cannot represent a results[] body and answered 502
		// "upstream response could not be reshaped for ingress format" on
		// every rerank call that got far enough to receive one.
		return body, nil
	}
	return h.deps.CanonicalBridge.ResponseCanonicalToIngress(ingress.BodyFormat, body)
}

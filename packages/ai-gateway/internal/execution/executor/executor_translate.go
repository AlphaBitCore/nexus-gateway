package executor

// executor_translate.go — the per-target cross-format translation seam
// of the failover loop: converting the caller's ingress-format body
// into the resolved target's wire, per endpoint kind, via the
// canonical bridge. Split out of executeInner so the dispatch loop
// stays readable and each kind's side channels are documented in one
// place.

import (
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// bridgeTranslation is a translated wire body plus the codec side
// channels that only exist at translation time and MUST be threaded
// explicitly to the attempt (adapter.Execute's same-format passthrough
// can never re-derive them):
//
//   - urlOverride — an embeddings codec's endpoint-selection action
//     suffix (Gemini :embedContent vs :batchEmbedContents). Dropping it
//     POSTs the batch body ({"requests":[…]}) to the single-embed URL
//     and Gemini 400s.
//   - rewrites — the codec's coercion markers (per-model contract rules on
//     chat/embeddings; the image codec's size→aspectRatio, quality/style/user
//     drops, response_format degradations; the rerank codec's top_n→top_k).
//     Dropping them silently strips the caller-visible x-nexus-coerced
//     contract on failover targets, where the prepare stage's rewrite
//     plumbing never ran.
type bridgeTranslation struct {
	body        []byte
	urlOverride string
	rewrites    []string
}

// bridgeTranslateForTarget converts the ingress-format base body into
// the target's wire for the endpoint kinds the bridge can translate
// (chat, embeddings, image generation). A nil, nil return means the
// kind has no bridge translation — the caller keeps the original body
// (matching the historical no-matching-case behavior).
func (e *TargetExecutor) bridgeTranslateForTarget(ingressKind typology.EndpointKind, base provcore.Request, callTarget provcore.CallTarget) (*bridgeTranslation, error) {
	switch ingressKind {
	case typology.EndpointKindChat:
		wireBody, rewrites, err := e.bridge.IngressChatToWire(base.BodyFormat, callTarget.Format, base.Body, callTarget, base.Stream)
		if err != nil {
			return nil, err
		}
		return &bridgeTranslation{body: wireBody, rewrites: rewrites}, nil
	case typology.EndpointKindEmbeddings:
		wireBody, urlOverride, rewrites, err := e.bridge.IngressEmbeddingsToWire(base.BodyFormat, callTarget.Format, base.Body, callTarget)
		if err != nil {
			return nil, err
		}
		return &bridgeTranslation{body: wireBody, urlOverride: urlOverride, rewrites: rewrites}, nil
	case typology.EndpointKindImageGeneration:
		// Validates via IngressImagesToCanonical first — the closed-set
		// checks (n bound, nexus reject, prompt shape) bind on this
		// failover lane exactly as on the prepare stage.
		wireBody, rewrites, err := e.bridge.IngressImagesToWire(base.BodyFormat, callTarget.Format, base.Body, callTarget)
		if err != nil {
			return nil, err
		}
		return &bridgeTranslation{body: wireBody, rewrites: rewrites}, nil
	case typology.EndpointKindRerank:
		// Validates via IngressRerankToCanonical first — the Cohere-shaped
		// canonical checks (query/documents/top_n) bind on this failover lane
		// exactly as on the prepare stage. Without this case a Voyage failover
		// keeps the Cohere-shaped body (top_n never → top_k) and 400s upstream.
		wireBody, rewrites, err := e.bridge.IngressRerankToWire(base.BodyFormat, callTarget.Format, base.Body, callTarget)
		if err != nil {
			return nil, err
		}
		return &bridgeTranslation{body: wireBody, rewrites: rewrites}, nil
	}
	return nil, nil
}

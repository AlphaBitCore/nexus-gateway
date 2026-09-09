package proxy

import (
	"context"
	"fmt"
	"log/slog"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	normcodecs "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/codecs"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// canonicalRequestForHooks produces the two things the request stage needs to
// work at the waist: the canonical chat body, and the canonical payload decoded
// from it.
//
// Hooks operate on the canonical spec in both directions. The request arriving
// here is in whatever wire shape the client speaks; canonicalizing it once means
// every scanning hook sees the same structure — assistant history with its
// reasoning, tool calls with their arguments, tool results, media — regardless
// of which of the fifteen ingress formats produced it. The alternative, which
// this replaces, asked the format-aware traffic adapter for a flat list of text
// segments, a model that cannot represent a tool result or a reasoning turn at
// all, so those channels went upstream unscanned on every format.
//
// The canonical body is also what makes the write-back possible, and here it is
// the caller's own bytes: a redaction edits the payload, one rewriter puts the
// edited content back into those bytes, and nothing is re-encoded.
//
// A nil return means the canonical lane is unavailable for this request (no
// bridge, no registry, or a body the codec cannot read). The caller falls back
// to the format-aware extraction, which is weaker but fail-closed on rewrite.
func (h *Handler) canonicalRequestForHooks(ctx context.Context, ingressFormat provcore.Format, body []byte, logger *slog.Logger) ([]byte, *normcore.NormalizedPayload) {
	if h == nil || h.deps == nil || h.deps.NormalizeRegistry == nil || len(body) == 0 {
		return nil, nil
	}
	// OpenAI-family ingress only, and the reason is the WRITE-BACK, not the scan.
	//
	// A redaction here edits the canonical payload, and the edited canonical has
	// to reach the client's wire. For an OpenAI-family ingress that is free: the
	// canonical chat body IS that wire, so the write-back is an in-place edit of
	// the bytes the caller sent and nothing can be lost.
	//
	// For Anthropic it is not free, and the difference is not cosmetic. Encoding
	// canonical back to the Anthropic wire REBUILDS the request, and canonical
	// chat does not model `cache_control` (prompt caching stops working — the
	// bill goes up on exactly the requests a policy touched), `metadata.user_id`,
	// `mcp_servers`, `container`, or `content[].citations`. Worse, a server tool
	// (`{"type":"web_search_20250305"}`) comes back as a client-side custom tool
	// with no `type`, so the upstream emits a tool_use and blocks on a
	// tool_result the client cannot produce. That corrupts the conversation
	// rather than degrading it, and it does so silently.
	//
	// The principle this enforces is the same one the response rewriter states:
	// a redaction EDITS the body it was decoded from, it never reconstructs it.
	// Reconstruction loses whatever the intermediate shape does not model. The
	// non-OpenAI ingresses keep the format-aware extraction and its surgical
	// sjson write-back, which loses nothing — at the cost of the weaker scan
	// model, which is the pre-existing state and is tracked.
	if !ingressFormat.IsOpenAIFamily() {
		return nil, nil
	}
	// There is no canonicalization step, and that is the point of the gate above:
	// for an OpenAI-family ingress the request body ALREADY IS the canonical chat
	// body. Calling the bridge here would be an identity with an error arm
	// nothing can reach, and it would suggest the lane could be widened by
	// swapping a call — which is exactly what made it lossy.
	canon := body
	payload, err := h.deps.NormalizeRegistry.Normalize(ctx, canon, normcore.Meta{
		AdapterType:  string(provcore.FormatOpenAI),
		Direction:    normcore.DirectionRequest,
		EndpointPath: "/v1/chat/completions",
		ContentType:  "application/json",
	})
	// A decode error and a decode by the WRONG codec are the same outcome here,
	// and in practice only the second one happens: the registry picks a codec by
	// sniffing and its last resort is the generic HTTP-JSON codec, which succeeds
	// on any object. So a body the chat codec cannot read does not fail — it
	// comes back as kind "http-json" with no messages, a decode that succeeded
	// while decoding nothing this lane can use. Accepting that would be the
	// silent downgrade the lane exists to remove: the hook would believe it holds
	// the canonical chat spec. Checking the protocol is what makes the check
	// real; checking only the error would pass everything.
	if err == nil && payload.Protocol != openAIChatProtocol {
		err = fmt.Errorf("registry decoded the canonical body as %q, not %q",
			payload.Protocol, openAIChatProtocol)
	}
	if err != nil {
		logExtractFailure(logger, "request", "canonical", "/v1/chat/completions", len(canon), err)
		if h.deps.Metrics != nil {
			h.deps.Metrics.RecordTrafficExtract(string(ingressFormat), "request", "error")
		}
		return nil, nil
	}
	if h.deps.Metrics != nil {
		h.deps.Metrics.RecordTrafficExtract(string(ingressFormat), "request", "success")
	}
	return canon, &payload
}

// openAIChatProtocol is the Protocol tag the chat normalizer stamps. It is the
// only decode the REQUEST lane accepts, because the rewriter that puts a
// redaction back walks the chat shape and nothing else.
const openAIChatProtocol = "openai-chat"

// openAIResponsesProtocol is the second canonical RESPONSE wire shape. A hook
// still sees one payload — both shapes decode into it — but the rewriter that
// writes a redaction back has one arm per shape, because writing bytes is the
// job that has to know where the text lives.
const openAIResponsesProtocol = "openai-responses"

// applyCanonicalRequestRedaction turns the pipeline's spans into edited
// canonical content, writes that content back into the canonical request body,
// and hands the result to the client's own codec to put back on their wire.
//
// Three steps, each owned by the layer that knows the shape — the same division
// the response side uses:
//
//   - normalize.ApplySpans resolves each span's canonical block address and
//     edits the text. Compliance addresses canonical blocks and knows nothing
//     about wires.
//   - the codec writes the edited content back into the canonical slots it was
//     decoded from, pairing each slot with the block type that belongs in it.
//   - the bridge encodes canonical back to the ingress wire, which is the
//     encode that ingress format's codec already performs on the cross-format
//     lane.
//
// Every failure is an error, never a fallback to the original bytes: the policy
// demanded a redaction, so forwarding the unredacted body would send upstream
// exactly the content that was masked.
func (h *Handler) applyCanonicalRequestRedaction(canonBody []byte, payload *normcore.NormalizedPayload, spans []normcore.TransformSpan) ([]byte, int, error) {
	if payload == nil {
		return nil, 0, fmt.Errorf(
			"canonical request redaction: the policy produced a redaction but no canonical " +
				"payload was available to apply it to")
	}
	edited, _ := normcore.ApplySpans(*payload, spans)
	// The rewrite edits the caller's own bytes in place — there is no encode back
	// to a wire, because the body never left it. A rebuild would lose every
	// field the canonical shape does not model, which is why this lane is
	// OpenAI-family only.
	return normcodecs.RewriteCanonicalRequestContent(canonBody, edited)
}

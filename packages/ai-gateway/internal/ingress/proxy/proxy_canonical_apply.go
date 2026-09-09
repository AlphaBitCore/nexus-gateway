package proxy

import (
	"fmt"

	"github.com/tidwall/gjson"

	normcodecs "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/codecs"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// applyCanonicalRedaction turns the pipeline's spans into edited canonical
// content and asks the codec to write that content back into the canonical
// body.
//
// Two steps, each owned by the layer that knows the shape:
//
//   - normalize.ApplySpans resolves each span's canonical block address
//     ("messages.<i>.content.<j>") and edits the text. Compliance addresses
//     canonical blocks; it does not know or care what wire they came from.
//   - the codec writes the edited content back into the wire slots it decoded
//     them from, pairing each slot with the block type that belongs in it.
//
// Everything outside the content channels is left byte-identical, which is why
// a redaction cannot delete an envelope field.
//
// A nil payload with spans to apply is fail-closed. The canonical locus is
// strong-compliance — the gateway is not in the host packet path — so a
// redaction the policy demanded but could not be applied must never deliver the
// original.
//
// It dispatches on the PAYLOAD's protocol rather than on a path, because the
// payload is what says which shape the body was decoded from — and the body is
// what gets edited. There are exactly two canonical response shapes, and each
// has an in-place rewriter.
//
// It used to decode the /v1/responses shape to canonical chat, redact there, and
// re-encode. That reconstructed the body, and reconstruction loses everything
// the intermediate shape does not model — previous_response_id, the citation
// annotations a web-search answer must display, reasoning encrypted_content, and
// the item ids, which came back regenerated. Editing in place loses none of it.
func (h *Handler) applyCanonicalRedaction(body []byte, payload *normcore.NormalizedPayload, spans []normcore.TransformSpan) ([]byte, int, error) {
	if payload == nil {
		return nil, 0, fmt.Errorf(
			"canonical redaction: the policy produced a redaction but no canonical payload was " +
				"available to apply it to")
	}

	edited, _ := normcore.ApplySpans(*payload, spans)
	if payload.Protocol == openAIResponsesProtocol {
		return normcodecs.RewriteCanonicalResponsesContent(body, edited)
	}
	return normcodecs.RewriteCanonicalResponseContent(body, edited)
}

// canonicalBodyHasContent asks the presence question of whichever canonical
// response shape the body is in.
//
// It dispatches on the bytes, not on the ingress, for the same reason the decode
// does: a cross-format cache HIT serves canonical chat to a /v1/responses
// reader. Getting this wrong is not cosmetic — the chat check looks for
// `choices[]`, so asking it about an `output[]` body returns "no content", and
// the fail-closed guard then reads the most dangerous case as the safest one.
func canonicalBodyHasContent(body []byte) bool {
	if !gjson.GetBytes(body, "choices").Exists() && gjson.GetBytes(body, "output").Exists() {
		return normcodecs.CanonicalResponsesHasContent(body)
	}
	return normcodecs.CanonicalResponseHasContent(body)
}

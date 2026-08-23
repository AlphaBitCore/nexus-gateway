package guardrail

// redact_body.go — the sanitized copy of a /v1/guardrail request body, for
// audit storage.
//
// The audit row may persist evaluated text only under the rules every other
// endpoint follows: under approve the raw body, under redact or block the
// sanitized copy and nothing else — redact.StorageRawBodyChecked fail-safes the
// raw bytes to NULL for those two actions, so a record that offers no sanitized
// copy stores no body at all.
//
// The projection is the one this package already owns for `redacted_content`,
// reused rather than reimplemented: the stored body and the echoed verdict
// therefore cannot disagree about what was masked.

import (
	"github.com/goccy/go-json"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// RedactedRequestBody returns req re-serialized with every evaluated segment
// replaced by its sanitized text, or nil when it cannot produce one the caller
// may trust. nil means "store no body" — never "store the raw body".
//
// Segment order follows Request.Segments(), which drops empty message contents;
// those messages are carried through untouched, since they hold no text to mask.
func RedactedRequestBody(req *Request, spans []normalize.TransformSpan) []byte {
	if req == nil {
		return nil
	}
	segs := req.Segments()
	if len(segs) == 0 {
		return nil
	}
	sanitized, _ := normalize.ApplySpans(*BuildNormalized(segs), spans)
	masked := maskedSegments(sanitized, len(segs))
	if masked == nil {
		return nil
	}

	out := *req
	if req.Content != "" {
		out.Content = masked[0]
	} else {
		msgs := make([]Message, len(req.Messages))
		copy(msgs, req.Messages)
		next := 0
		for i := range msgs {
			if msgs[i].Content == "" {
				continue
			}
			msgs[i].Content = masked[next]
			next++
		}
		out.Messages = msgs
	}
	// out is a struct of strings built from decoded input, so re-marshaling
	// cannot fail; the error is structurally dead.
	b, _ := json.Marshal(&out)
	return b
}

// maskedSegments reads the sanitized text back out of an ApplySpans result, one
// entry per input segment, or nil if the payload no longer has the shape
// BuildNormalized gave it.
//
// It reads the content blocks directly rather than going through TextProjection,
// which drops empty-text blocks: a span that masks a whole segment away would
// then shift every later segment onto the wrong field, and a mask placed on the
// wrong field is worse than no stored body at all. The shape check is the guard
// for the same failure arriving from the other side — ApplySpans adding, dropping
// or retyping a block — which is why it returns nil rather than best-effort.
func maskedSegments(p normalize.NormalizedPayload, want int) []string {
	if len(p.Messages) != 1 || len(p.Messages[0].Content) != want {
		return nil
	}
	out := make([]string, want)
	for i, b := range p.Messages[0].Content {
		if b.Type != normalize.ContentText {
			return nil
		}
		out[i] = b.Text
	}
	return out
}

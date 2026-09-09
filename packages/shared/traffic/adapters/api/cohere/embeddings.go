package cohere

import (
	"strconv"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// Cohere's embed request carries the documents to vectorise in `texts[]`, its
// own spelling of the field OpenAI calls `input`:
//
//	{"model":"embed-v4.0","input_type":"search_document","texts":["…","…"]}
//
// The adapter used to have no branch for it. ExtractRequest looked for
// `messages`, found none, and answered ErrUnknownSchema — so the hook pipeline
// received no content and the documents went upstream scanned by nothing. On the
// intercepting substrates the adapter is chosen by the HOST the client dialled,
// so any tool embedding through Cohere directly was outside every content
// policy, and nothing about the request looked different from a clean one.
//
// This mirrors the rerank branch that already sits beside it: a shape check, an
// extraction, and a rewrite that walks the same slots in the same order.

// isEmbedBody reports whether the body is an embed request. `texts` is the
// required field of that wire and appears in no other Cohere request shape, so
// its presence is the discriminator — the same style as isRerankBody.
func isEmbedBody(body []byte) bool {
	return gjson.GetBytes(body, "texts").IsArray()
}

// extractEmbedRequest pulls every document out of texts[].
func extractEmbedRequest(body []byte) traffic.NormalizedContent {
	var segments []string
	gjson.GetBytes(body, "texts").ForEach(func(_, item gjson.Result) bool {
		if item.Type == gjson.String {
			segments = append(segments, item.Str)
		}
		return true
	})
	meta := map[string]string{}
	if m := gjson.GetBytes(body, "model"); m.Type == gjson.String && m.Str != "" {
		meta["model"] = m.Str
	}
	return traffic.NormalizedContent{Segments: segments, Metadata: meta}
}

// rewriteEmbedRequest writes redacted documents back into texts[], consuming one
// segment per STRING element in the order the extractor produced them.
func rewriteEmbedRequest(body []byte, content traffic.NormalizedContent) ([]byte, int, error) {
	if len(content.Segments) == 0 {
		return body, 0, nil
	}
	out := body
	// The array position and the segment position are separate counters, and the
	// position is counted here rather than read from the callback: gjson hands an
	// ARRAY iterator a zero-value key, and an empty path element makes sjson
	// append instead of replace.
	ai, si, patched := 0, 0, 0
	var err error
	gjson.GetBytes(body, "texts").ForEach(func(_, item gjson.Result) bool {
		pos := ai
		ai++
		if item.Type != gjson.String {
			return true
		}
		if si >= len(content.Segments) {
			return false
		}
		out, err = sjson.SetBytes(out, "texts."+strconv.Itoa(pos), content.Segments[si])
		if err != nil {
			return false
		}
		si++
		patched++
		return true
	})
	if err != nil {
		return nil, 0, err
	}
	return out, patched, nil
}

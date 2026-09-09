package gemini

import (
	"strconv"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// Gemini's embedding endpoints carry their text in a `content` object rather
// than the `contents` array the generateContent wire uses, in two shapes:
//
//	embedContent:        {"model":"…","content":{"parts":[{"text":"…"}]}}
//	batchEmbedContents:  {"requests":[{"model":"…","content":{"parts":[…]}}]}
//
// The adapter had a branch for neither. ExtractRequest looked for `contents`,
// found none, and answered ErrUnknownSchema, so the hook pipeline received no
// content and the documents went upstream scanned by nothing. On the
// intercepting substrates the adapter is picked by the HOST the client dialled,
// so anything embedding through Gemini directly sat outside every content policy
// — and a request nobody scanned is indistinguishable from a clean one.
//
// One character separates the two wires (`content` vs `contents`), which is
// exactly the kind of near-miss a shape check has to be explicit about: the
// discriminator below requires the singular key to be an OBJECT, so a
// generateContent body can never take this path.

// isEmbedBody reports whether the body is one of the two embedding shapes.
func isEmbedBody(body []byte) bool {
	if gjson.GetBytes(body, "content").IsObject() {
		return true
	}
	return gjson.GetBytes(body, "requests").IsArray()
}

// embedTextPaths returns the gjson path of every text slot the embedding body
// carries, in walk order. Extraction and rewrite both consume this list, so the
// two cannot disagree about which slot a segment belongs to — the failure mode
// that a second hand-written walk invites.
func embedTextPaths(body []byte) []string {
	var paths []string
	// The array position is counted here rather than read off the callback:
	// gjson hands an ARRAY iterator a zero-value key, and an empty path element
	// makes sjson append instead of replace.
	appendParts := func(prefix string) {
		pos := 0
		gjson.GetBytes(body, prefix+"parts").ForEach(func(_, part gjson.Result) bool {
			at := pos
			pos++
			if t := part.Get("text"); t.Type == gjson.String && t.Str != "" {
				paths = append(paths, prefix+"parts."+strconv.Itoa(at)+".text")
			}
			return true
		})
	}

	if gjson.GetBytes(body, "content").IsObject() {
		appendParts("content.")
		return paths
	}
	reqs := gjson.GetBytes(body, "requests")
	if !reqs.IsArray() {
		return nil
	}
	i := 0
	reqs.ForEach(func(_, _ gjson.Result) bool {
		appendParts("requests." + strconv.Itoa(i) + ".content.")
		i++
		return true
	})
	return paths
}

// extractEmbedRequest pulls the text of every embedding slot.
func extractEmbedRequest(body []byte) traffic.NormalizedContent {
	paths := embedTextPaths(body)
	segments := make([]string, 0, len(paths))
	for _, p := range paths {
		segments = append(segments, gjson.GetBytes(body, p).Str)
	}
	meta := map[string]string{}
	if m := gjson.GetBytes(body, "model"); m.Type == gjson.String && m.Str != "" {
		meta["model"] = m.Str
	}
	return traffic.NormalizedContent{Segments: segments, Metadata: meta}
}

// rewriteEmbedRequest writes redacted text back into the same slots, in the same
// order the extractor produced them.
func rewriteEmbedRequest(body []byte, content traffic.NormalizedContent) ([]byte, int, error) {
	paths := embedTextPaths(body)
	if len(paths) == 0 || len(content.Segments) == 0 {
		return body, 0, nil
	}
	out := body
	patched := 0
	for i, p := range paths {
		if i >= len(content.Segments) {
			break
		}
		next, err := sjson.SetBytes(out, p, content.Segments[i])
		if err != nil {
			return nil, 0, err
		}
		out = next
		patched++
	}
	return out, patched, nil
}

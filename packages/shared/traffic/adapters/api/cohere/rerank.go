package cohere

import (
	"fmt"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// rerank.go — the compliance seam for /v1/rerank on the Cohere traffic adapter.
// Rerank bodies are {model, query, documents:[]string} with no `messages`, so
// the chat extractor rejects them; without this the hook pipeline would scan
// NOTHING and forward unredacted query + documents upstream. Documents are the
// caller's retrieved corpus (the bulk-PII carrier), so BOTH the query and
// every document are extracted for scanning, and a redact decision rewrites
// them in place — the provider never sees the PII while ranking still works on
// the redacted text.

// isRerankBody reports whether body is a rerank request (query + documents,
// no messages) rather than a Cohere chat body.
func isRerankBody(body []byte) bool {
	if gjson.GetBytes(body, "messages").Exists() {
		return false
	}
	return gjson.GetBytes(body, "query").Type == gjson.String &&
		gjson.GetBytes(body, "documents").IsArray()
}

// extractRerankRequest emits the query followed by every string document as
// scannable segments, in a fixed order the rewrite mirrors exactly.
func extractRerankRequest(body []byte) traffic.NormalizedContent {
	docs := gjson.GetBytes(body, "documents").Array()
	segments := make([]string, 0, 1+len(docs))
	segments = append(segments, gjson.GetBytes(body, "query").Str)
	for _, d := range docs {
		if d.Type == gjson.String {
			segments = append(segments, d.Str)
		}
	}
	meta := map[string]string{}
	if model := gjson.GetBytes(body, "model"); model.Type == gjson.String {
		meta["model"] = model.Str
	}
	return traffic.NormalizedContent{Segments: segments, Metadata: meta}
}

// rewriteRerankRequest writes redacted segments back to `query` and each
// string `documents[i]`, mirroring extractRerankRequest's iteration order
// (query first, then string documents in array order) so index i always maps
// to the segment scanned from that slot. Extra/short segments are handled
// guard-and-continue like the chat rewriter.
func rewriteRerankRequest(body []byte, content traffic.NormalizedContent) ([]byte, int, error) {
	if !gjson.ValidBytes(body) {
		return nil, 0, traffic.ErrMalformed
	}
	out := body
	segIdx := 0
	written := 0
	var err error

	if segIdx < len(content.Segments) {
		out, err = sjson.SetBytes(out, "query", content.Segments[segIdx])
		if err != nil {
			return nil, written, fmt.Errorf("cohere: rewrite query: %w", err)
		}
		segIdx++
		written++
	}

	docs := gjson.GetBytes(out, "documents").Array()
	for i := range docs {
		if docs[i].Type != gjson.String {
			continue
		}
		if segIdx >= len(content.Segments) {
			break
		}
		p := fmt.Sprintf("documents.%d", i)
		out, err = sjson.SetBytes(out, p, content.Segments[segIdx])
		if err != nil {
			return nil, written, fmt.Errorf("cohere: rewrite %s: %w", p, err)
		}
		segIdx++
		written++
	}
	return out, written, nil
}

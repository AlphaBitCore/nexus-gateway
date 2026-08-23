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

// rerankDocText reports the scannable text of one `documents[i]` element and
// the JSON path to write a redaction back to.
//
// Cohere accepts both spellings — a bare string, and an object whose `text`
// carries the document — and scores them alike. Extraction and rewrite MUST
// agree on which elements are scannable and in what order, because a segment's
// position is the only thing tying it back to the slot it came from; they
// therefore share this one decision rather than each testing the shape.
//
// Elements with no text at all (a number, a bool, null, an object without a
// string `text`) are not scannable and are skipped by both. Skipping them is
// right — there is nothing to read — where skipping an object that DOES carry
// text would forward it unscanned.
func rerankDocText(idx int, d gjson.Result) (path, text string, ok bool) {
	switch {
	case d.Type == gjson.String:
		return fmt.Sprintf("documents.%d", idx), d.Str, true
	case d.IsObject():
		if t := d.Get("text"); t.Type == gjson.String {
			return fmt.Sprintf("documents.%d.text", idx), t.Str, true
		}
	}
	return "", "", false
}

// extractRerankRequest emits the query followed by every document that
// carries text, in a fixed order the rewrite mirrors exactly.
func extractRerankRequest(body []byte) traffic.NormalizedContent {
	docs := gjson.GetBytes(body, "documents").Array()
	segments := make([]string, 0, 1+len(docs))
	segments = append(segments, gjson.GetBytes(body, "query").Str)
	for i, d := range docs {
		if _, text, ok := rerankDocText(i, d); ok {
			segments = append(segments, text)
		}
	}
	meta := map[string]string{}
	if model := gjson.GetBytes(body, "model"); model.Type == gjson.String {
		meta["model"] = model.Str
	}
	return traffic.NormalizedContent{Segments: segments, Metadata: meta}
}

// rewriteRerankRequest writes redacted segments back to `query` and each
// text-carrying `documents[i]`, mirroring extractRerankRequest's iteration order
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
		p, _, ok := rerankDocText(i, docs[i])
		if !ok {
			continue
		}
		if segIdx >= len(content.Segments) {
			break
		}
		out, err = sjson.SetBytes(out, p, content.Segments[segIdx])
		if err != nil {
			return nil, written, fmt.Errorf("cohere: rewrite %s: %w", p, err)
		}
		segIdx++
		written++
	}
	return out, written, nil
}

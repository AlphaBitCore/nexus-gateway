// writer_splice.go — the audit Writer's body-splice marshal helpers: the
// post-encode body splice that keeps a large captured body from paying a
// body-sized re-encode allocation, plus the binary-wire and un-spliced
// marshal variants. Split from writer_batch.go, which owns the per-record
// marshal entry point, chunking, and the framed publish path.
package audit

import (
	"bytes"
	"encoding/base64"

	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"

	json "github.com/goccy/go-json"
)

// Body-splice markers. A large inline captured body (any encoding) would otherwise
// make Body.MarshalJSON allocate a fresh ~body-sized envelope per body (the dominant
// audit re-encode allocation). Before encoding, each such body is armed via
// DetachForSplice to emit one of these tiny markers; after encoding, the marker's
// wire form is replaced in-place with the real body bytes rendered for the body's
// encoding (verbatim / escaped / base64). The markers are valid JSON
// strings (so Encoding=raw stays valid) and distinctive enough that a collision
// with real content is astronomically unlikely — and if one ever occurs, the
// occurrence-count check falls the record back to a plain (un-spliced) re-encode,
// so correctness never depends on the marker being unique.
var (
	reqBodySpliceMarker  = []byte(`"__nexus_body_splice_req_7b3f9e2a__"`)
	respBodySpliceMarker = []byte(`"__nexus_body_splice_resp_7b3f9e2a__"`)
)

// splicedBody pairs a detached body's real bytes with its encoding so the
// post-encode splice renders it correctly (verbatim raw / escaped text / base64).
// A zero value (real == nil) means nothing was detached for that direction.
type splicedBody struct {
	real []byte
	enc  sharedaudit.BodyEncoding
}

// estimateSplicedLen returns an upper-ish estimate of a detached body's rendered
// wire length, used only to pre-grow the splice buffer (correctness does not
// depend on it — an under-estimate just costs one append regrowth). raw renders
// verbatim; base64 is exact (EncodedLen + quotes); text is the body length plus a
// margin for the rare escape (SSE/text bodies escape few bytes).
func estimateSplicedLen(s splicedBody) int {
	switch s.enc {
	case sharedaudit.EncodingRaw:
		return len(s.real)
	case sharedaudit.EncodingText:
		return len(s.real) + len(s.real)/4 + 16
	default: // base64
		return base64.StdEncoding.EncodedLen(len(s.real)) + 2
	}
}

// spliceBodyMarkers writes enc into out with each present marker replaced by its
// detached body rendered for its encoding (audit.AppendInlineForSplice). Returns
// false if a marker is missing or appears more than once (a content collision) —
// the caller then falls back to a plain re-encode. enc is the marker-encoded
// message (bodies are tiny markers), so this scan is cheap. The whole spliced
// record is built into out's available buffer via append and written once, so a
// large body is rendered straight into the pooled buffer with no body-sized
// intermediate allocation.
func spliceBodyMarkers(out *bytes.Buffer, enc []byte, req, resp splicedBody) bool {
	if req.real != nil && bytes.Count(enc, reqBodySpliceMarker) != 1 {
		return false
	}
	if resp.real != nil && bytes.Count(enc, respBodySpliceMarker) != 1 {
		return false
	}
	size := len(enc)
	if req.real != nil {
		size += estimateSplicedLen(req) - len(reqBodySpliceMarker)
	}
	if resp.real != nil {
		size += estimateSplicedLen(resp) - len(respBodySpliceMarker)
	}
	out.Grow(size)
	dst := out.AvailableBuffer()
	rest := enc
	for len(rest) > 0 {
		ri, si := -1, -1
		if req.real != nil {
			ri = bytes.Index(rest, reqBodySpliceMarker)
		}
		if resp.real != nil {
			si = bytes.Index(rest, respBodySpliceMarker)
		}
		switch {
		case ri >= 0 && (si < 0 || ri < si):
			dst = append(dst, rest[:ri]...)
			dst = sharedaudit.AppendInlineForSplice(dst, req.real, req.enc)
			rest = rest[ri+len(reqBodySpliceMarker):]
			req.real = nil
		case si >= 0:
			dst = append(dst, rest[:si]...)
			dst = sharedaudit.AppendInlineForSplice(dst, resp.real, resp.enc)
			rest = rest[si+len(respBodySpliceMarker):]
			resp.real = nil
		default:
			dst = append(dst, rest...)
			rest = nil
		}
	}
	out.Write(dst)
	return true
}

// marshalRecordBinary encodes rec as one binary TLV record (the NEXUS_AUDIT_WIRE
// =binary path). It needs no splice machinery: AppendBinary writes each body's raw
// (already-compressed) frame straight into the pooled buffer with no base64 and no
// body-sized re-encode — the splice optimization exists only to keep the JSON path
// from re-escaping a large body, which the binary path never does. dst aliases the
// pooled buffer's backing when the record fits its pre-grown capacity (the common
// case); an oversized record reallocates dst (the pooled buffer is still returned,
// just not backing the result), so the caller's reclaim contract is unchanged.
func (w *Writer) marshalRecordBinary(rec *Record) (data []byte, buf *bytes.Buffer, ok bool) {
	msg := w.recordToMessage(rec)
	buf = msgBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	dst := msg.AppendBinary(buf.AvailableBuffer())
	// Bodies are now copied into dst; reclaim the pooled body buffer (mirrors the
	// JSON path's post-encode reclaim). On publish failure the retry re-publishes
	// these bytes (rec.marshaled), never re-reading the reclaimed body.
	w.reclaimRecordBody(rec)
	return dst, buf, true
}

// marshalRecordPlain is the un-spliced encode path: a fresh recordToMessage (the
// caller's msg had its bodies detached) + a plain encode. It is the correctness
// fallback for the rare marker collision; the buffer-hold contract is identical
// to marshalRecord (caller reclaims the returned buffer post-publish).
func (w *Writer) marshalRecordPlain(rec *Record) (data []byte, buf *bytes.Buffer, ok bool) {
	msg := w.recordToMessage(rec)
	buf = msgBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	if err := json.NewEncoder(buf).Encode(msg); err != nil {
		w.logger.Error("audit: marshal failed", "requestId", rec.RequestID, "error", err)
		reclaimMsgBuf(buf)
		return nil, nil, false
	}
	b := buf.Bytes()
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
	}
	w.reclaimRecordBody(rec) // body now copied into b; reclaim the pooled buffer
	return b, buf, true
}

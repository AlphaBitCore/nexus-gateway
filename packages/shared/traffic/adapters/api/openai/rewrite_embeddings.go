package openai

import (
	"strconv"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// RewriteEmbeddingsInput writes redacted segments back into an embeddings
// request's top-level `input`, mirroring extractEmbeddingsRequest's walk: the
// single-string shape takes segment 0, the array shape takes one segment per
// STRING element in order.
//
// It is exported because the OpenAI embeddings request shape is the one several
// vendors serve verbatim — voyage among them — and a second copy of this walk in
// each of them is a second place for the extractor and the rewriter to drift
// apart.
//
// Rewriting this was previously declined outright, with the comment that
// returning ErrRewriteUnsupported "makes the AG proxy fall through to forwarding
// the original body with a warn log". That has not been true for some time: the
// proxy fails CLOSED on an unsupported rewrite, because forwarding the original
// would send upstream exactly the content a policy had just masked. So the
// consequence of declining was not a leak — it was that a redact policy on an
// embeddings request could only ever REFUSE it. `input` is a flat list of
// strings; there is nothing here a redaction can lose.
//
// Token-array inputs (`[[1,2],[3,4]]`, the pre-tokenised form) are left alone.
// The extractor produces no segment for them, so they consume no slot, and a
// token array carries no text a policy can address.
func RewriteEmbeddingsInput(body []byte, content traffic.NormalizedContent) ([]byte, int, error) {
	if !gjson.ValidBytes(body) {
		return nil, 0, traffic.ErrMalformed
	}
	input := gjson.GetBytes(body, "input")
	if !input.Exists() {
		return nil, 0, traffic.ErrUnknownSchema
	}
	return rewriteStringOrStringArray(body, "input", input, content.Segments)
}

// rewriteStringOrStringArray replaces a field that is either a string or an
// array of strings, consuming one segment per replaced slot. Shared so a vendor
// whose embeddings field is spelled differently (Cohere's `texts`) reuses the
// same slot accounting rather than restating it.
func rewriteStringOrStringArray(body []byte, path string, cur gjson.Result, segments []string) ([]byte, int, error) {
	if len(segments) == 0 {
		return body, 0, nil
	}
	out := body
	patched := 0

	if cur.Type == gjson.String {
		next, err := sjson.SetBytes(out, path, segments[0])
		if err != nil {
			return nil, 0, err
		}
		return next, 1, nil
	}
	if !cur.IsArray() {
		// Neither a string nor an array: a shape the extractor produced no
		// segment for, so there is no slot to write and nothing was masked.
		return body, 0, nil
	}

	// The array position and the segment position are different counters: a
	// non-string element advances the first and not the second. gjson hands an
	// ARRAY iterator a zero-value key, so the position is counted here rather
	// than read off the callback — reading it gives an empty path element, which
	// sjson treats as an append and leaves the original value in place beside the
	// masked one.
	ai, si := 0, 0
	var err error
	cur.ForEach(func(_, item gjson.Result) bool {
		pos := ai
		ai++
		if item.Type != gjson.String {
			// Token arrays and other non-string elements hold no slot, exactly as
			// the extractor skipped them.
			return true
		}
		if si >= len(segments) {
			return false
		}
		out, err = sjson.SetBytes(out, path+"."+strconv.Itoa(pos), segments[si])
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

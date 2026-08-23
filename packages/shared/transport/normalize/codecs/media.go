package codecs

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/locator"
)

// Shared media helpers. Every codec builds MediaRef through these rather
// than parsing data URIs and sizing base64 by hand — six divergent
// hand-rolled implementations are exactly what produced the defects this
// unification removes.

// modalityFromMime maps a mime type to one of the four modalities. An
// unknown or empty mime is a file — the honest answer, and the one that
// still gets a download. This is what stops audio and PDFs from being
// labelled images (measured defect #7).
func modalityFromMime(mime string) string {
	m := strings.ToLower(strings.TrimSpace(mime))
	if i := strings.IndexByte(m, ';'); i >= 0 {
		m = strings.TrimSpace(m[:i])
	}
	switch {
	case strings.HasPrefix(m, "image/"):
		return core.ModalityImage
	case strings.HasPrefix(m, "audio/"):
		return core.ModalityAudio
	case strings.HasPrefix(m, "video/"):
		return core.ModalityVideo
	default:
		return core.ModalityFile
	}
}

// inlineOrExternal classifies a wire field that may hold either inline
// bytes as a data URI or a remote URL. Three codecs decode this same
// ambiguous field, and each deciding independently is how they came to
// disagree — so the decision lives here once.
//
// A `data:` URI is never treated as external, even when it is not base64:
// putting the payload into MediaRef.URL would be the measured defect
// (a whole data URI stored in a reference field) in a narrower form. A
// non-base64 data URI is inline content we cannot address by the base64
// container, so it is reported absent with its declared mime rather than
// copied anywhere.
func inlineOrExternal(urlStr, locatorPath, modality string) *core.MediaRef {
	// Normalise before any prefix test. Every prefix comparison below is
	// against a client-controlled string, and one leading space walked a
	// whole data URI past all of them into the URL field.
	urlStr = locator.TrimIgnorable(urlStr)
	if urlStr == "" {
		return &core.MediaRef{Modality: modality, Source: core.MediaAbsent}
	}
	if locator.HasSchemeFold(urlStr, "data:") {
		if mime, payload, ok := locator.ParseDataURI(urlStr); ok {
			ref := capturedMedia(mime, payload, locator.DataURI(locatorPath))
			if modality != "" && mime == "" {
				ref.Modality = modality
			}
			return ref
		}
		mime := urlStr[len("data:"):]
		if i := strings.IndexAny(mime, ";,"); i >= 0 {
			mime = mime[:i]
		}
		m := modality
		if m == "" {
			m = modalityFromMime(mime)
		}
		return &core.MediaRef{Modality: m, Mime: mime, Source: core.MediaAbsent}
	}
	return externalMedia("", urlStr, modality)
}

// capturedMedia builds a MediaRef for base64 bytes held at locator inside
// the captured body.
//
// Two invariants are enforced here rather than left to each codec, because
// eight codecs each enforcing them separately is how they came to disagree
// in the first place:
//
//   - Source=captured implies a locator. Without one the bytes exist but
//     are unaddressable, so the ref degrades to absent rather than
//     promising a download nothing could serve.
//   - A size is only reported for a payload that could actually decode.
//     `decodedSize` is pure arithmetic and will happily size a payload
//     that is not base64 at all, which fabricates a byte count for a
//     download that would fail. Undecodable payloads are marked truncated
//     with no size, which is the state the card explains and offers no
//     control for.
func capturedMedia(mime, b64, path string) *core.MediaRef {
	ref := &core.MediaRef{
		Modality: modalityFromMime(mime),
		Mime:     mime,
		Source:   core.MediaCaptured,
		Locator:  path,
	}
	if path == "" {
		ref.Source = core.MediaAbsent
		return ref
	}
	if b64 == "" {
		// The media object exists but carries no data. That is "never
		// there", not "arrived broken" — and offering a locator would
		// promise a download that resolves to nothing.
		//
		// The cause is still named. "Absent" alone reads as "the provider
		// sent no media", which is a different fact from "the provider sent
		// a media object with an empty payload" — the second is a wire
		// oddity worth seeing rather than smoothing over.
		ref.Source = core.MediaAbsent
		ref.Locator = ""
		ref.Cause = "empty-payload"
		return ref
	}
	if !locator.ValidBase64(b64) {
		ref.Truncated = true
		ref.Cause = "undecodable-base64"
		return ref
	}
	ref.SizeBytes = locator.DecodedSize(b64)
	return ref
}

// externalMedia builds a MediaRef for a remote URL the gateway never
// fetched and never will. The URL is recorded inert.
//
// The data-URI guard lives HERE rather than in the callers: every caller
// passes a client-controlled string, and a `data:` URI reaching the URL
// field is the measured headline defect — a whole payload stored in a
// reference field. Guarding two of six call sites would leave the other
// four as the same defect on a different wire.
func externalMedia(mime, url, modality string) *core.MediaRef {
	url = locator.TrimIgnorable(url)
	if url == "" {
		// The same degradation inlineOrExternal and capturedMedia already
		// make: a reference to nothing is absent, not an external card the
		// reader would try to open.
		return &core.MediaRef{Modality: modality, Mime: mime, Source: core.MediaAbsent}
	}
	if locator.HasSchemeFold(url, "data:") {
		declared := url[len("data:"):]
		if i := strings.IndexAny(declared, ";,"); i >= 0 {
			declared = declared[:i]
		}
		if mime == "" {
			mime = declared
		}
		// Modality is resolved AFTER the mime is recovered, or the ref
		// contradicts itself: audio/wav bytes labelled a file.
		if modality == "" {
			modality = modalityFromMime(mime)
		}
		return &core.MediaRef{Modality: modality, Mime: mime, Source: core.MediaAbsent}
	}
	// Everything the classifier did NOT recognise passes here, and this is
	// the only line that does not depend on recognising it. Three
	// discriminators have been walked past one equivalence class at a time;
	// a length bound holds however the value is spelled, so the payload
	// cannot reach the field even when the next classifier is wrong too.
	if len(url) > maxURLBytes {
		// Truncated, not blanked. Blanking made an oversized reference
		// indistinguishable from "the provider sent no media", and threw
		// away the one thing worth keeping — which host was referenced.
		// Truncated+Cause is the shape capturedMedia already uses for
		// undecodable bytes, and mediaHasBytes refuses to offer a control
		// for a truncated ref, so nothing becomes openable by keeping it.
		if modality == "" {
			modality = core.ModalityFile
		}
		return &core.MediaRef{
			Modality:  modality,
			Source:    core.MediaExternal,
			URL:       truncateRunes(url, urlEvidenceBytes),
			Truncated: true,
			Cause:     "oversized-url",
		}
	}
	if modality == "" {
		modality = modalityFromMime(mime)
	}
	return &core.MediaRef{
		Modality: modality,
		Mime:     mime,
		Source:   core.MediaExternal,
		URL:      url,
	}
}

// providerRefMedia builds a MediaRef for content the provider holds under
// its own file id / URI.
func providerRefMedia(mime, ref, modality string) *core.MediaRef {
	if modality == "" {
		modality = modalityFromMime(mime)
	}
	return &core.MediaRef{
		Modality:    modality,
		Mime:        mime,
		Source:      core.MediaProviderRef,
		ProviderRef: ref,
	}
}

// mediaBlock wraps a MediaRef as a content block.
func mediaBlock(ref *core.MediaRef) core.ContentBlock {
	return core.ContentBlock{Type: core.ContentMedia, MediaRef: ref}
}

// payloadSafeJSON renders a wire structure as text with payloads elided.
//
// Serialising an unrecognised part is how a reader learns what arrived, and
// every codec does it. But a part can carry bytes, and rendering it whole
// puts the payload into a text block that feeds compliance scanning and the
// stored normalized text — the measured headline defect, reachable through
// as many doors as there are default branches.
//
// A per-value length cap is NOT sufficient, which was learned by
// measurement: a payload split across sixty-four short strings, hidden in
// JSON keys, or written as an array of integers walks straight through one.
// The bound that actually holds is a budget on the RENDERED OUTPUT: keys,
// values, punctuation and all. Once it is spent, rendering stops and says
// so. Whatever shape the input takes, the text block cannot exceed the
// budget, so no payload fits inside it.
func payloadSafeJSON(v any) string {
	var b strings.Builder
	w := &budgetWriter{b: &b, left: payloadSafeBudget}
	w.write(v, 0)
	if w.spent {
		b.WriteString(" …[truncated, payload elided]")
	}
	return b.String()
}

const (
	// Generous for a content part's structure and its short text fields,
	// far below anything a payload could hide in.
	payloadSafeBudget = 1024
	// A synthetic rendering never legitimately needs more than this: a
	// block type, a field name, a short scalar.
	//
	// This bounds one block, NOT the aggregate: nothing here limits how
	// many parts a body carries, so N unknown parts still yield N bounded
	// blocks. That is accepted rather than fixed — a declared text part
	// preserves its content verbatim by design, so an attacker splitting a
	// payload across parts gains no reach they did not already have, and
	// the total stays under the body cap either way.
	payloadSafeMaxValue = 96
	payloadSafeMaxDepth = 8

	// urlEvidenceBytes is how much of an oversized value is KEPT. Enough to
	// carry scheme, host and the start of the path — which is the whole
	// point of retaining it — and far too little to be a payload.
	urlEvidenceBytes = 256

	// maxURLBytes bounds what may be RECORDED as a URL.
	//
	// Three discriminators have now failed to keep a payload out of this
	// field: a bare prefix test, then the same test after TrimSpace, then
	// that after aligning the whitespace set. Each fix was defeated one
	// equivalence class over — Unicode format characters are not
	// White_Space, and `DATA:` is a legal spelling of the same URI. The
	// question "is this string a URI" is open-ended; "is it longer than any
	// URI" is not.
	//
	// The ceiling comes from the practical browser limit (~2 KiB, which
	// signed URLs approach) with headroom — NOT from a measurement of long
	// provider URLs, because none exists in this tree: the captured wire
	// fixtures carry URLs of 26 and 67 bytes. An earlier version of this
	// comment claimed such a measurement. The boundary is pinned by test
	// instead of asserted here.
	maxURLBytes = 4096
)

// budgetWriter renders a decoded JSON value under a total output budget.
type budgetWriter struct {
	b     *strings.Builder
	left  int
	spent bool
}

func (w *budgetWriter) put(s string) {
	if w.spent {
		return
	}
	if len(s) > w.left {
		w.b.WriteString(truncateRunes(s, w.left))
		w.left = 0
		w.spent = true
		return
	}
	w.b.WriteString(s)
	w.left -= len(s)
}

func (w *budgetWriter) write(v any, depth int) {
	if w.spent {
		return
	}
	if depth > payloadSafeMaxDepth {
		w.put("…")
		return
	}
	switch t := v.(type) {
	case string:
		// Two guards, both required. The budget stops a payload split
		// across many short values; this stops one long value from
		// spending the whole budget on its own prefix. Neither alone is
		// enough — that was established by measurement, twice.
		if len(t) > payloadSafeMaxValue {
			w.put(fmt.Sprintf("\"[elided %d-char value]\"", len(t)))
			return
		}
		w.put(strconv.Quote(t))
	case map[string]any:
		// Sorted so the rendering is deterministic: an audit record that
		// changes shape between reads is not a record.
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		w.put("{")
		for i, k := range keys {
			if i > 0 {
				w.put(",")
			}
			if len(k) > payloadSafeMaxValue {
				w.put(fmt.Sprintf("\"[elided %d-char key]\"", len(k)))
			} else {
				w.put(strconv.Quote(k))
			}
			w.put(":")
			w.write(t[k], depth+1)
			if w.spent {
				break
			}
		}
		w.put("}")
	case []any:
		w.put("[")
		for i, e := range t {
			if i > 0 {
				w.put(",")
			}
			w.write(e, depth+1)
			if w.spent {
				break
			}
		}
		w.put("]")
	case nil:
		w.put("null")
	default:
		w.put(fmt.Sprintf("%v", t))
	}
}

// truncateRunes cuts at a rune boundary at or before n bytes. Slicing a
// string by byte index splits a multi-byte character, and the resulting
// invalid UTF-8 is rejected outright by a Postgres text column — so a
// wire-controlled value with any non-ASCII in it would fail the audit write
// rather than merely render oddly.
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// payloadSafeRaw renders raw wire bytes under the same budget. Used where a
// body failed to decode into the expected shape and the codec would
// otherwise fall back to the verbatim bytes.
func payloadSafeRaw(raw []byte) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		if len(raw) > payloadSafeBudget {
			return fmt.Sprintf("[unparseable body, %d bytes]", len(raw))
		}
		return string(raw)
	}
	return payloadSafeJSON(v)
}

// payloadSafeText bounds a synthetic text block — a marker, a summary, a
// kept tool payload.
//
// It bounds the WHOLE result, not one argument. Bounding only `body` while
// concatenating a caller-supplied `prefix` is how the guard introduced to
// prevent unbounded text grew an unbounded path on its own line: every
// prefix here is wire-derived (a block type, a key list), so both halves are
// attacker-controlled and both must be bounded.
func payloadSafeText(prefix, body string) string {
	if len(prefix) > payloadSafeMaxValue {
		prefix = truncateRunes(prefix, payloadSafeMaxValue) + "…"
	}
	if len(body) > payloadSafeMaxValue {
		body = fmt.Sprintf("[elided %d-char value]", len(body))
	}
	// No whole-result bound: with both halves already capped at
	// payloadSafeMaxValue the sum cannot approach the budget, so such a
	// line would never execute — and a guard that never runs cannot be
	// falsified, which is how the two live guards above went untested
	// behind it. Each is asserted directly instead.
	return prefix + body
}

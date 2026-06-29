// Package audit: body_splice.go — the body-splice rendering rules shared between
// MarshalJSON and the producer's post-encode splice. A large captured body of any
// inline encoding (raw / text / base64) would otherwise be re-encoded into a fresh
// body-sized slice every time its record is marshaled — the single largest audit
// allocator under streaming load. Splicing defers the body bytes out of the goccy
// encode: MarshalJSON emits a tiny marker, and AppendInlineForSplice renders the
// real bytes (verbatim / JSON-escaped / base64) directly into the producer's
// pooled output buffer, so the body-sized allocation never happens.
//
// The render rules here MUST stay byte-equivalent (after JSON decode) to the
// non-spliced MarshalJSON branches in body.go, since a record may take either path.
package audit

import (
	"encoding/base64"
	"unicode/utf8"
)

// SpliceMinBodyBytes is the smallest inline body worth splicing — below it the
// marker swap + post-encode scan cost outweighs the saved allocation, so the body
// is left to encode inline.
const SpliceMinBodyBytes = 128

// DetachForSplice arms b for the producer's splice path: MarshalJSON will emit
// marker as the inlineBytes wire value instead of rendering InlineBytes, and the
// caller splices the returned real bytes back in afterwards via
// AppendInlineForSplice(_, real, enc). Returns ok=false (and leaves b untouched)
// for bodies that encode cheaply anyway — absent/spill kinds, small bodies, or an
// unknown encoding — so the caller skips them.
//
// The body's Encoding is preserved on the wire, so the consumer decodes the
// spliced-in bytes correctly. marker must be a valid JSON string and distinctive
// enough that a content collision is astronomically unlikely; the producer
// verifies the marker appears exactly once and falls back to a plain re-encode
// otherwise, so correctness never depends on uniqueness.
func (b *Body) DetachForSplice(marker []byte) (real []byte, enc BodyEncoding, ok bool) {
	if b.Kind != BodyInline {
		return nil, "", false
	}
	if len(b.InlineBytes) < SpliceMinBodyBytes {
		return nil, "", false
	}
	switch b.Encoding {
	case EncodingRaw, EncodingText, EncodingBase64, EncodingZstd, EncodingS2:
		// Compressed encodings (zstd/s2) splice too: AppendInlineForSplice
		// compresses the ORIGINAL InlineBytes directly into the output buffer,
		// so the body never pays the json.Marshal(string(base64)) escape scan +
		// the envelope-marshal body copy that the non-spliced path incurs.
	default:
		return nil, "", false
	}
	b.spliceMarker = marker
	return b.InlineBytes, b.Encoding, true
}

// AppendInlineForSplice appends real's wire form for encoding enc to dst, matching
// the corresponding MarshalJSON branch:
//   - raw    → the bytes verbatim (they are valid JSON).
//   - text   → an escaped JSON string (valid UTF-8 non-JSON).
//   - base64 → a quoted base64 string (binary / NUL-bearing).
//
// Rendering directly into the caller's (pooled) buffer is what makes the splice
// allocation-free for the body — there is no intermediate body-sized slice.
func AppendInlineForSplice(dst, real []byte, enc BodyEncoding) []byte {
	switch enc {
	case EncodingRaw:
		return append(dst, real...)
	case EncodingText:
		return appendJSONString(dst, real)
	case EncodingZstd:
		// real is the ORIGINAL captured body; emit "<base64-of-zstd>" directly.
		// base64 is JSON-safe (no escape scan needed). Matches MarshalJSON's
		// EncodingZstd branch (quoted base64 of the zstd frame) after decode.
		dst = append(dst, '"')
		dst = compressInlineToBase64(dst, real)
		return append(dst, '"')
	case EncodingS2:
		dst = append(dst, '"')
		dst = compressInlineS2ToBase64(dst, real)
		return append(dst, '"')
	default: // EncodingBase64 (and the empty default MarshalJSON treats as base64)
		dst = append(dst, '"')
		dst = base64.StdEncoding.AppendEncode(dst, real)
		return append(dst, '"')
	}
}

const hexDigits = "0123456789abcdef"

// jsLineSep2028, jsLineSep2029 are U+2028 / U+2029 — valid in a JSON string but
// escaped by encoding/json's default policy (JS string safety). Matching that here
// keeps spliced text output identical to the non-spliced goccy form.
const (
	jsLineSep2028 = 0x2028
	jsLineSep2029 = 0x2029
)

// appendJSONString appends src as a quoted, escaped JSON string to dst. The escape
// set matches encoding/json's HTML-safe default (the form goccy produced on the
// non-spliced text path): ", \, the short escapes \b \f \n \r \t, \u00xx for the
// remaining C0 control bytes, < > & escaped, and U+2028/U+2029 escaped. Other
// bytes — including all multibyte UTF-8 — pass through verbatim (valid inside a
// JSON string). src is assumed valid UTF-8 (the text encoding guarantees it); a
// stray invalid byte is emitted as the U+FFFD replacement escape, never producing
// invalid JSON.
func appendJSONString(dst, src []byte) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(src); {
		if c := src[i]; c < utf8.RuneSelf {
			if htmlSafe(c) {
				i++
				continue
			}
			if start < i {
				dst = append(dst, src[start:i]...)
			}
			dst = append(dst, '\\')
			switch c {
			case '\\', '"':
				dst = append(dst, c)
			case '\n':
				dst = append(dst, 'n')
			case '\r':
				dst = append(dst, 'r')
			case '\t':
				dst = append(dst, 't')
			default:
				// Remaining C0 control byte (incl. backspace 0x08 and form-feed 0x0c,
				// which goccy renders as u-escapes, NOT \b / \f), or < > &.
				dst = append(dst, 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xF])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRune(src[i:])
		if r == utf8.RuneError && size == 1 {
			if start < i {
				dst = append(dst, src[start:i]...)
			}
			dst = append(dst, '\\', 'u', 'f', 'f', 'f', 'd')
			i += size
			start = i
			continue
		}
		if r == jsLineSep2028 || r == jsLineSep2029 {
			if start < i {
				dst = append(dst, src[start:i]...)
			}
			dst = append(dst, '\\', 'u', '2', '0', '2', hexDigits[r&0xF])
			i += size
			start = i
			continue
		}
		i += size
	}
	if start < len(src) {
		dst = append(dst, src[start:]...)
	}
	return append(dst, '"')
}

// htmlSafe reports whether byte c (< utf8.RuneSelf) may be emitted verbatim inside
// a JSON string under the HTML-safe escaping policy. Escaped: C0 controls, the
// quote and backslash, and < > & (which encoding/json escapes by default).
func htmlSafe(c byte) bool {
	return c >= 0x20 && c != '"' && c != '\\' && c != '<' && c != '>' && c != '&'
}

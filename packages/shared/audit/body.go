// Package audit: body.go — wire-format-safe container for captured request /
// response bodies on traffic_event_payload.
//
// The Hub-bound audit message used to type the body field as
// `json.RawMessage`. That worked for ai-gateway's JSON request/response shape
// but exploded on compliance-proxy and agent SSE traffic, multipart uploads,
// and any byte sequence that wasn't already valid JSON: `json.Marshal` of the
// envelope would call `RawMessage.MarshalJSON`, which validates the bytes and
// errors out the moment it saw a `\x1b` (ANSI escape), an unescaped CR, or
// any other non-JSON byte. The MQ writer's `continue`-on-error path then
// silently dropped the entire audit row.
//
// `Body` makes the captured payload a structured discriminator — `Kind`
// distinguishes "absent" (capture disabled or zero-length), "inline" (body
// fits within the inline size threshold and travels with the message), and
// "spill" (body was written to `shared/spillstore` and the message carries a
// reference). Inline bytes are base64-encoded on the wire when the bytes are
// not themselves valid JSON, so any byte sequence round-trips losslessly.
package audit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	stdjson "encoding/json"
	"unicode/utf8"
)

// BodyKind discriminates the storage form of a captured body.
type BodyKind string

const (
	BodyAbsent BodyKind = "absent"
	BodyInline BodyKind = "inline"
	BodySpill  BodyKind = "spill"
)

// BodyEncoding records how `InlineBytes` is laid out on the wire.
//   - "raw"    — bytes are valid JSON, embedded as a JSON value (verbatim splice,
//     no escape). The cheapest form; gated by json.Valid.
//   - "text"   — bytes are valid UTF-8 but not valid JSON (SSE, plain text);
//     embedded as an escaped JSON string. Benchmarked ~14% smaller and
//     lower-alloc than base64 for SSE bodies, and the envelope stays
//     valid JSON.
//   - "base64" — bytes are not valid UTF-8 (binary) or carry a NUL; base64-encoded
//     for transport.
type BodyEncoding string

const (
	EncodingRaw    BodyEncoding = "raw"
	EncodingText   BodyEncoding = "text"
	EncodingBase64 BodyEncoding = "base64"
	// EncodingZstd — InlineBytes is the ORIGINAL captured body; the wire form is
	// base64 of its zstd frame. The producer compresses at marshal time (off the
	// request path); the Hub persists the base64-of-zstd string verbatim into the
	// inline_*_body column (no decompress on ingest) and only the view layer
	// decompresses. Chosen by NewInlineBody for large bodies when compression is
	// enabled for the process (SetInlineCompression).
	EncodingZstd BodyEncoding = "zstd"
	// EncodingS2 — like EncodingZstd but the frame is S2 (klauspost/compress/s2),
	// a Snappy-derived codec ~3-5x faster to encode than zstd at a lower ratio.
	// Chosen by NewInlineBody when AI_GATEWAY_AUDIT_CODEC=s2; self-describing so
	// s2 and zstd bodies coexist with no migration (each reader dispatches on its
	// own tag).
	EncodingS2 BodyEncoding = "s2"
)

// SpillRef points to a body that was stored out-of-band (large payloads).
// Backend / Key tuples are opaque to the audit pipeline; resolution happens
// via `shared/spillstore.SpillStore.Get`.
//
// Truncated is set when the backend hit its per-object cap before exhausting
// the upstream reader; the audit pipeline then knows the persisted bytes
// are a prefix of the original payload, not the whole thing. The outer
// Body still carries its own Truncated for inline payloads — the SpillRef
// flag covers the spill-backend-specific case.
type SpillRef struct {
	Backend     string `json:"backend"`               // "localfs" | "s3" | …
	Key         string `json:"key"`                   // backend-specific key
	Size        int64  `json:"size"`                  // bytes
	SHA256      string `json:"sha256,omitempty"`      // hex-encoded
	ContentType string `json:"contentType,omitempty"` // hint for renderers
	Truncated   bool   `json:"truncated,omitempty"`
}

// Body is the discriminated container persisted on `traffic_event_payload`.
// Producers call `NewInlineBody` / `NewSpillBody` / `EmptyBody`; consumers
// read `Kind` and dispatch on it.
type Body struct {
	Kind        BodyKind     `json:"kind"`
	Encoding    BodyEncoding `json:"encoding,omitempty"` // only meaningful for Inline
	InlineBytes []byte       `json:"-"`                  // not serialized directly — see MarshalJSON
	SpillRef    *SpillRef    `json:"spillRef,omitempty"`
	SizeBytes   int64        `json:"sizeBytes,omitempty"` // pre-truncation size
	Truncated   bool         `json:"truncated,omitempty"`
	ContentType string       `json:"contentType,omitempty"`

	// rawValidated records that NewInlineBody already confirmed InlineBytes is
	// valid JSON when it chose Encoding=raw. It lets MarshalJSON skip the
	// O(len(body)) json.Valid re-scan on the hot audit marshal path — that
	// re-scan, run once per captured body per record, measured as ~20% of total
	// gateway CPU under load, yet it only re-checks what the constructor already
	// proved. Validation is a once-at-construction boundary, not a per-marshal
	// cost. Unexported and untagged, so it never touches the wire form; bodies
	// built by direct struct literal (no constructor) leave it false and are
	// re-validated at marshal time, preserving the defensive error contract for
	// those callers.
	rawValidated bool

	// inlineIsRawFrame is set by the binary-wire decoder (ReadBodyBinary) to record
	// that InlineBytes already holds the RAW compressed frame for an s2/zstd body —
	// not the base64 wire string the JSON path carries. ColumnPayload then stores it
	// straight into the BYTEA column without a base64 decode. False for every
	// JSON-decoded or freshly-constructed body, so existing paths are unchanged.
	// Unexported and untagged — never reaches the wire.
	inlineIsRawFrame bool

	// spliceMarker, when non-nil, makes MarshalJSON emit this tiny byte sequence
	// as the `inlineBytes` wire value INSTEAD of rendering InlineBytes (escape /
	// base64 / verbatim). The real bytes are spliced back into the encoded record
	// afterwards by the producer (see audit.AppendInlineForSplice), so a large
	// captured body never pays a body-sized re-encode allocation on the hot audit
	// marshal path. The envelope still carries the body's TRUE Encoding, so the
	// consumer's UnmarshalJSON decodes the spliced-in bytes correctly. Set via
	// DetachForSplice; unexported and untagged so it never reaches the wire.
	spliceMarker []byte
}

// EmptyBody returns the zero-value body that means "no payload captured".
func EmptyBody() Body {
	return Body{Kind: BodyAbsent}
}

// NewInlineBody returns a body whose bytes travel with the audit message.
// The encoding is auto-detected three ways: valid JSON → "raw" (verbatim
// splice), valid UTF-8 non-JSON → "text" (escaped string), binary / NUL →
// "base64". `contentType` is a free-form hint stored on the row for UI
// rendering ("application/json", "text/event-stream", "multipart/form-data",
// "application/octet-stream", …).
func NewInlineBody(b []byte, sizeBytes int64, truncated bool, contentType string) Body {
	if len(b) == 0 {
		return EmptyBody()
	}
	// When this process produces compressed audit bodies, a body at or above the
	// size floor is marked EncodingZstd: InlineBytes stays the ORIGINAL bytes and
	// the zstd+base64 happens lazily in MarshalJSON (the async marshal worker), so
	// no compression CPU lands on the request path. The pre-compression encoding
	// (raw/text/base64) is immaterial downstream — the view layer re-classifies
	// the decompressed bytes — so the three-way json.Valid classification is
	// SKIPPED entirely here when the body will be compressed (that O(n) scan was
	// measured at ~12% of gateway CPU under load and is pure waste once the result
	// is overridden to zstd).
	if inlineCompressionEnabled() && len(b) >= inlineCompressionMinBytes() {
		enc := EncodingZstd
		if inlineCodecS2() {
			enc = EncodingS2
		}
		return Body{
			Kind:        BodyInline,
			Encoding:    enc,
			InlineBytes: b,
			SizeBytes:   sizeBytes,
			Truncated:   truncated,
			ContentType: contentType,
		}
	}
	// Three-way classification (benchmarked, json.Valid-gated hybrid):
	//   valid JSON           → raw   (verbatim nested splice, no escape — the
	//                                 cheapest; this is why json.Valid stays: a
	//                                 50 KB JSON request body escaped as a string
	//                                 instead measured 7.5x slower + 2x alloc).
	//   valid UTF-8, no NUL   → text  (escaped JSON string — ~14% smaller + lower
	//                                 alloc than base64 for SSE/text bodies).
	//   otherwise            → base64 (binary / NUL-bearing).
	// stdlib encoding/json.Valid is a zero-alloc stack scanner (goccy's decodes
	// into interface{}, ~4x body alloc — never use it here).
	var enc BodyEncoding
	switch {
	case stdjson.Valid(b):
		enc = EncodingRaw
	case utf8.Valid(b) && bytes.IndexByte(b, 0) < 0:
		enc = EncodingText
	default:
		enc = EncodingBase64
	}
	return Body{
		Kind:        BodyInline,
		Encoding:    enc,
		InlineBytes: b,
		SizeBytes:   sizeBytes,
		Truncated:   truncated,
		ContentType: contentType,
		// raw ⟺ json.Valid(b) just passed, so the marshal path can trust it.
		rawValidated: enc == EncodingRaw,
	}
}

// NewSpillBody returns a body that lives in a `SpillStore` backend.
// `originalSize` is the pre-spill size of the captured stream (always the
// full size — there is no truncation in the spill path; oversized streams
// are capped at the per-backend hard limit and `Truncated` reflects that).
func NewSpillBody(ref *SpillRef, originalSize int64, truncated bool, contentType string) Body {
	if ref == nil {
		return EmptyBody()
	}
	return Body{
		Kind:        BodySpill,
		SpillRef:    ref,
		SizeBytes:   originalSize,
		Truncated:   truncated,
		ContentType: contentType,
	}
}

// SHA256Hex returns the lowercase hex-encoded SHA-256 of `b`. Used by spill
// callers to populate `SpillRef.SHA256` deterministically before Put.
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

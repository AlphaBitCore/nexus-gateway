// Package audit: body_column.go — the persisted-column (BYTEA `inline_*_body`)
// codec for captured bodies: the inline_*_encoding discriminators and the
// encode/decode helpers the Hub ingest + view layers use. Distinct from
// body_json.go, which carries the NATS wire (JSON message) codec.
package audit

import (
	"bytes"
	"encoding/base64"
	"unicode/utf8"
)

// Column-encoding discriminators for the BYTEA `inline_*_body` columns on
// traffic_event_payload. They are the persisted-row counterpart of the wire
// `BodyEncoding` (which describes the NATS message form), and are stored in the
// sibling `inline_*_encoding` columns so a reader knows how to read the body
// column back. The column is BYTEA: it holds RAW bytes (no base64), so the +33%
// base64 inflation a TEXT column once forced for binary / compressed bodies is
// gone — which is the whole point, since audit-body disk WRITE bandwidth is the
// single-box no-loss bottleneck.
const (
	// BodyColumnText — the column holds the captured body's RAW bytes, which are
	// valid UTF-8 with no NUL. The common case; reads back with no decode. (Under
	// BYTEA "text" vs "binary" is informational — both read verbatim — but the tag
	// is preserved so renderers can tell text from binary cheaply.)
	BodyColumnText = "text"
	// BodyColumnBinary — the column holds the captured body's RAW bytes that are
	// not text-safe (binary, embedded NUL, or non-UTF-8). A BYTEA column stores
	// these losslessly with no base64. Replaces the former base64-in-TEXT path.
	BodyColumnBinary = "binary"
	// BodyColumnBase64 — LEGACY read-only: pre-BYTEA rows stored base64 of a
	// non-text body in the old TEXT column. Kept on the READ path so a migrated
	// table's old rows still decode; ColumnPayload never produces it anymore.
	BodyColumnBase64 = "base64"
	// BodyColumnZstd — the column holds the RAW zstd frame of the captured body
	// (the former form was base64 of the frame in a TEXT column). The Hub
	// base64-decodes the wire EncodingZstd form to the raw frame at ingest
	// (ColumnPayload); the view layer zstd-decompresses (DecodeBodyForColumn).
	BodyColumnZstd = "zstd"
	// BodyColumnS2 — the column holds the RAW S2 frame. Like BodyColumnZstd but
	// S2-decompressed on read (DecodeBodyForColumn).
	BodyColumnS2 = "s2"
)

// ColumnPayload returns the persisted-column (BYTEA) form of an inline body and
// its inline_*_encoding discriminator. A compressed body (EncodingS2 /
// EncodingZstd) arrives from the wire as base64 of its frame — InlineBytes
// carries that base64 string from UnmarshalJSON — so it is base64-DECODED here to
// the RAW frame, which is what the BYTEA column stores (33% fewer bytes than the
// former base64-in-TEXT). The decode lands on the Hub ingest worker, which has
// CPU headroom. A malformed wire base64 (must not happen) falls back to storing
// the bytes verbatim as binary so the row is never lost. Any non-compressed
// inline body is rendered via EncodeBodyForColumn. Callers must only use this for
// Kind==BodyInline bodies.
func (b Body) ColumnPayload() (payload []byte, encoding string) {
	switch b.Encoding {
	case EncodingS2:
		// Binary wire already delivered the raw frame — store it verbatim.
		if b.inlineIsRawFrame {
			return b.InlineBytes, BodyColumnS2
		}
		if frame, ok := base64DecodeFrame(b.InlineBytes); ok {
			return frame, BodyColumnS2
		}
		return b.InlineBytes, BodyColumnBinary
	case EncodingZstd:
		if b.inlineIsRawFrame {
			return b.InlineBytes, BodyColumnZstd
		}
		if frame, ok := base64DecodeFrame(b.InlineBytes); ok {
			return frame, BodyColumnZstd
		}
		return b.InlineBytes, BodyColumnBinary
	default:
		return EncodeBodyForColumn(b.InlineBytes)
	}
}

// EncodeBodyForColumn renders captured bytes for the BYTEA `inline_*_body` column
// and returns the matching `inline_*_encoding` discriminator. The column is
// BYTEA, so EVERY byte sequence is stored RAW — UTF-8 text and arbitrary binary
// alike — with no base64: PostgreSQL keeps the bytes as-is (no parse / validate /
// tree-store on insert, which was the audit-drain bottleneck), and a BYTEA column
// accepts NUL and non-UTF-8 losslessly where a TEXT column rejected them
// (SQLSTATE 22021) and forced the +33% base64 inflation. The text/binary tag is
// informational (both read back verbatim) but is preserved so renderers can tell
// text from binary cheaply; utf8.Valid plus the explicit no-NUL check draws that
// line (NUL is itself valid UTF-8).
func EncodeBodyForColumn(b []byte) (payload []byte, encoding string) {
	if utf8.Valid(b) && bytes.IndexByte(b, 0) < 0 {
		return b, BodyColumnText
	}
	return b, BodyColumnBinary
}

// DecodeBodyForColumn inverts the persist path: given the stored BYTEA column
// bytes and their `inline_*_encoding` discriminator, it returns the original
// captured body bytes. "zstd"/"s2" decompress the RAW frame directly (no
// base64-decode — the frame is stored raw under BYTEA); "text"/"binary"/empty are
// the body verbatim. "base64" is the LEGACY pre-BYTEA form (old rows in a
// migrated table) and is base64-decoded for back-compat. A malformed/unreadable
// payload yields nil rather than an error — the caller treats an unreadable audit
// body as absent, never as a hard failure of the surrounding read.
func DecodeBodyForColumn(payload []byte, encoding string) []byte {
	switch encoding {
	case BodyColumnZstd:
		// Raw zstd frame → original bytes. Malformed yields nil (absent).
		return decompressInline(payload)
	case BodyColumnS2:
		// Raw S2 frame → original bytes. Same fail-soft contract.
		return decompressInlineS2(payload)
	case BodyColumnBase64:
		// Legacy: base64 text persisted in the old TEXT column. After a Text→BYTEA
		// migration the column bytes are the base64 ASCII, so decoding still works.
		decoded, err := base64.StdEncoding.DecodeString(string(payload))
		if err != nil {
			return nil
		}
		return decoded
	default: // BodyColumnText, BodyColumnBinary, "" — raw bytes, verbatim.
		return payload
	}
}

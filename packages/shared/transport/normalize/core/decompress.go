package core

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"io"

	"github.com/klauspost/compress/zstd"
)

// maxDecompressed bounds the audit-time decompressor so a hostile body
// can't blow memory. 64 MiB is well above any realistic AI response.
const maxDecompressed = 64 << 20

// MaybeGunzip detects gzip / zlib / zstd magic bytes and decompresses.
// Exported so codecs tests can verify decompression behaviour.
// Returns (raw, false) for anything unrecognised.
func MaybeGunzip(raw []byte) ([]byte, bool) { return maybeGunzip(raw) }

// maybeGunzip detects gzip / zlib / zstd magic bytes and decompresses.
// Returns (raw, false) for anything unrecognised so the caller falls
// back to the original bytes. Producers (cp/agent) sometimes capture a
// compressed wire body before the transport layer decompresses it; the
// normalizer would otherwise see compressed bytes and fail to parse.
// Brotli (`Content-Encoding: br`) is intentionally NOT handled here —
// it has no reliable magic-byte signature and would require a new
// third-party dependency in `shared`; ask before adding.
func maybeGunzip(raw []byte) ([]byte, bool) {
	if len(raw) < 2 {
		return raw, false
	}
	switch {
	case raw[0] == 0x1f && raw[1] == 0x8b:
		// gzip
		gz, err := gzip.NewReader(bytes.NewReader(raw))
		return drainDecompressor(gz, err, raw)
	case raw[0] == 0x78 && (raw[1] == 0x01 || raw[1] == 0x5e || raw[1] == 0x9c || raw[1] == 0xda):
		// zlib (deflate with header) — common Content-Encoding: deflate
		zr, err := zlib.NewReader(bytes.NewReader(raw))
		return drainDecompressor(zr, err, raw)
	case len(raw) >= 4 && raw[0] == 0x28 && raw[1] == 0xb5 && raw[2] == 0x2f && raw[3] == 0xfd:
		// zstd
		zr, err := zstd.NewReader(bytes.NewReader(raw), zstd.WithDecoderMaxMemory(maxDecompressed))
		var rc io.ReadCloser
		if zr != nil {
			rc = zr.IOReadCloser()
		}
		return drainDecompressor(rc, err, raw)
	}
	return raw, false
}

// drainDecompressor is the shared tail of every maybeGunzip branch:
// given a freshly constructed decompressing reader and its constructor
// error, it reads the decompressed bytes under the maxDecompressed
// bound. Any failure — constructor error or a body that fails mid-read
// (truncated capture, corrupt frame) — falls back to the original raw
// bytes so the caller's normalizers see SOMETHING rather than nothing.
func drainDecompressor(rc io.ReadCloser, err error, raw []byte) ([]byte, bool) {
	if err != nil {
		return raw, false
	}
	defer func() { _ = rc.Close() }()
	out, err := io.ReadAll(io.LimitReader(rc, maxDecompressed))
	if err != nil {
		return raw, false
	}
	return out, true
}

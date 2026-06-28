// Package bodydecompress provides Decompress — the single source of
// truth used by every data-plane service (agent / compliance-proxy /
// ai-gateway / nexus-hub) for unwrapping upstream response bodies
// that Go's http.Transport did not auto-decompress. Distinct package
// name avoids clashing with shared/transport/http (the client logging
// wrapper, not body utilities).
//
// Go's net/http only auto-decompresses gzip when the request didn't
// carry an explicit Accept-Encoding. Anything the origin chooses to
// send back as br / zstd / deflate (chatgpt.com via Cloudflare ships
// br by default) stays compressed in resp.Body. Audit / normalize /
// usage-extract callers that try to JSON-parse those bytes directly
// see ErrUnsupported / decode errors and silently lose the row's
// content. A real incident traced exactly this — Hub
// received the decompressed payload via decompressForCapture but
// agent's runtimeNormalize fed raw brotli bytes to the registry.
//
// Keep this package import-free except for stdlib + the two upstream
// codec libraries; data-plane services have very tight dep surfaces
// and a heavy import here costs in every binary.
package bodydecompress

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// DefaultMaxDecompressedBytes bounds the expanded output of Decompress when
// the caller passes maxDecompressed <= 0. It defends against a decompression
// bomb: Go's http.Transport does not auto-decompress br / zstd, so a malicious
// or compromised upstream can ship a small (≤10 MiB) body that an unbounded
// io.ReadAll(decoder) would expand to multiple GB and OOM the data-plane
// service. 50 MiB leaves generous headroom over the 10 MiB compressed-read cap
// (payloadcapture DefaultMaxRequestBytes / DefaultMaxResponseBytes) so a
// legitimately large body — e.g. a batch-embedding response — still decodes,
// while a GB-scale bomb is rejected. Kept as a package-local constant so this
// package stays import-free except for stdlib + the two codec libraries.
const DefaultMaxDecompressedBytes int64 = 50 * 1024 * 1024 // 50 MiB

// Decompress returns a decompressed copy of body when the response
// carried a Content-Encoding that Go's http.Transport did not already
// unwrap. Idempotent: when resp.Uncompressed is true (transport
// already decompressed gzip) or body is empty, returns body unchanged.
//
// maxDecompressed bounds the expanded output: every decoder is wrapped in an
// io.LimitReader so a decompression bomb can never allocate more than
// maxDecompressed+1 bytes. When the expanded output would exceed
// maxDecompressed, Decompress returns the ORIGINAL (still-compressed) body and
// truncated=true — the safe fallback that stores opaque, debuggable bytes
// rather than a partial/corrupt decompressed buffer, matching the
// corrupt-stream contract below. maxDecompressed <= 0 uses
// DefaultMaxDecompressedBytes so a caller that forgets to set it still gets a
// bound rather than an unbounded read.
//
// On decompression failure (corrupt stream, partial bytes) returns the
// original body with truncated=false — caller decides whether to treat as
// opaque or surface an error. The fallback prevents a single decoder hiccup
// from dropping an audit row to NULL silently; the stored bytes will
// look like base64-encoded binary downstream, which is debuggable.
//
// Supports gzip / deflate / br / zstd — the four Content-Encoding
// values Cloudflare / nginx / AWS ALB / Anthropic / OpenAI use in
// production. Unknown encodings return body unchanged.
func Decompress(body []byte, resp *http.Response, maxDecompressed int64) (out []byte, truncated bool) {
	if resp == nil || resp.Uncompressed || len(body) == 0 {
		return body, false
	}
	if maxDecompressed <= 0 {
		maxDecompressed = DefaultMaxDecompressedBytes
	}
	enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	switch enc {
	case "gzip":
		r, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return body, false
		}
		defer func() { _ = r.Close() }()
		return readBounded(r, body, maxDecompressed)
	case "deflate":
		r := flate.NewReader(bytes.NewReader(body))
		defer func() { _ = r.Close() }()
		return readBounded(r, body, maxDecompressed)
	case "br":
		return readBounded(brotli.NewReader(bytes.NewReader(body)), body, maxDecompressed)
	case "zstd":
		r, err := zstd.NewReader(bytes.NewReader(body))
		if err != nil {
			return body, false
		}
		defer r.Close()
		return readBounded(r, body, maxDecompressed)
	}
	return body, false
}

// readBounded drains r into a buffer capped at maxDecompressed+1 bytes so a
// decompression bomb cannot allocate without limit. It returns:
//   - (decompressed, false) on success;
//   - (original, false) on a read error (corrupt/truncated stream) — the
//     opaque-bytes fallback;
//   - (original, true) when the expanded output exceeds maxDecompressed (bomb)
//     — the original compressed bytes are returned so no partial buffer leaks
//     downstream as if it were valid.
func readBounded(r io.Reader, original []byte, maxDecompressed int64) (out []byte, truncated bool) {
	decoded, err := io.ReadAll(io.LimitReader(r, maxDecompressed+1))
	if err != nil {
		return original, false
	}
	if int64(len(decoded)) > maxDecompressed {
		return original, true
	}
	return decoded, false
}

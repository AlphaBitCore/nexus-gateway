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
// content. The 2026-05-24 incident (#76) traced exactly this — Hub
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

// Decompress returns a decompressed copy of body when the response
// carried a Content-Encoding that Go's http.Transport did not already
// unwrap. Idempotent: when resp.Uncompressed is true (transport
// already decompressed gzip) or body is empty, returns body unchanged.
//
// On decompression failure (corrupt stream, partial bytes) returns the
// original body — caller decides whether to treat as opaque or
// surface an error. The fallback prevents a single decoder hiccup
// from dropping an audit row to NULL silently; the stored bytes will
// look like base64-encoded binary downstream, which is debuggable.
//
// Supports gzip / deflate / br / zstd — the four Content-Encoding
// values Cloudflare / nginx / AWS ALB / Anthropic / OpenAI use in
// production. Unknown encodings return body unchanged.
func Decompress(body []byte, resp *http.Response) []byte {
	if resp == nil || resp.Uncompressed || len(body) == 0 {
		return body
	}
	enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	switch enc {
	case "gzip":
		r, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return body
		}
		defer func() { _ = r.Close() }()
		out, err := io.ReadAll(r)
		if err != nil {
			return body
		}
		return out
	case "deflate":
		r := flate.NewReader(bytes.NewReader(body))
		defer func() { _ = r.Close() }()
		out, err := io.ReadAll(r)
		if err != nil {
			return body
		}
		return out
	case "br":
		out, err := io.ReadAll(brotli.NewReader(bytes.NewReader(body)))
		if err != nil {
			return body
		}
		return out
	case "zstd":
		r, err := zstd.NewReader(bytes.NewReader(body))
		if err != nil {
			return body
		}
		defer r.Close()
		out, err := io.ReadAll(r)
		if err != nil {
			return body
		}
		return out
	}
	return body
}

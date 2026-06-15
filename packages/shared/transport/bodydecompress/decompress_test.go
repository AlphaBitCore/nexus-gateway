package bodydecompress_test

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"net/http"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/bodydecompress"
)

// payload is a representative response body — a JSON object large enough that
// every codec actually compresses it, so a successful round-trip proves the
// decoder ran rather than the fallback returning the input unchanged.
var payload = []byte(`{"id":"chatcmpl-123","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"the quick brown fox jumps over the lazy dog, repeatedly and at length"}}],"usage":{"prompt_tokens":11,"completion_tokens":42}}`)

func gzipBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(b); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func flateBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, flate.DefaultCompression)
	if err != nil {
		t.Fatalf("flate new: %v", err)
	}
	if _, err := w.Write(b); err != nil {
		t.Fatalf("flate write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("flate close: %v", err)
	}
	return buf.Bytes()
}

func brotliBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := brotli.NewWriter(&buf)
	if _, err := w.Write(b); err != nil {
		t.Fatalf("brotli write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("brotli close: %v", err)
	}
	return buf.Bytes()
}

func zstdBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("zstd new: %v", err)
	}
	if _, err := w.Write(b); err != nil {
		t.Fatalf("zstd write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zstd close: %v", err)
	}
	return buf.Bytes()
}

func respWith(encoding string) *http.Response {
	h := http.Header{}
	if encoding != "" {
		h.Set("Content-Encoding", encoding)
	}
	return &http.Response{Header: h}
}

// TestDecompressRoundTrip asserts each supported Content-Encoding is decoded
// back to the exact original bytes — the observable behaviour callers depend on
// (audit/normalize/usage-extract get readable JSON, not compressed bytes).
func TestDecompressRoundTrip(t *testing.T) {
	cases := []struct {
		name     string
		encoding string
		compress func(*testing.T, []byte) []byte
	}{
		{"gzip", "gzip", gzipBytes},
		{"deflate", "deflate", flateBytes},
		{"br", "br", brotliBytes},
		{"zstd", "zstd", zstdBytes},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			compressed := tc.compress(t, payload)
			if bytes.Equal(compressed, payload) {
				t.Fatalf("%s: fixture not actually compressed", tc.name)
			}
			got, truncated := bodydecompress.Decompress(compressed, respWith(tc.encoding), 0)
			if truncated {
				t.Fatalf("%s: small payload must not be flagged truncated", tc.name)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("%s: decompressed mismatch\n got=%q\nwant=%q", tc.name, got, payload)
			}
		})
	}
}

// TestDecompressCaseAndWhitespace covers the header normalisation: a real
// upstream may send "GZIP" or " gzip" and the decoder must still fire.
func TestDecompressCaseAndWhitespace(t *testing.T) {
	compressed := gzipBytes(t, payload)
	for _, enc := range []string{"GZIP", "  gzip  ", "Gzip"} {
		got, _ := bodydecompress.Decompress(compressed, respWith(enc), 0)
		if !bytes.Equal(got, payload) {
			t.Fatalf("encoding %q: not decoded; got %q", enc, got)
		}
	}
}

// TestDecompressCorruptFallsBackToOriginal is the load-bearing failure mode
// from the 2026 brotli incident: a decoder error must return the input bytes
// unchanged (so the row is stored as opaque/debuggable) rather than dropping
// content. Each codec is fed bytes that announce its encoding but are invalid.
func TestDecompressCorruptFallsBackToOriginal(t *testing.T) {
	garbage := []byte("this is not a valid compressed stream at all, definitely")
	for _, enc := range []string{"gzip", "deflate", "br", "zstd"} {
		got, truncated := bodydecompress.Decompress(garbage, respWith(enc), 0)
		if truncated {
			t.Fatalf("encoding %q: corrupt stream is a decode failure, not an overflow", enc)
		}
		if !bytes.Equal(got, garbage) {
			t.Fatalf("encoding %q: corrupt stream should return original; got %q", enc, got)
		}
	}
}

// TestDecompressValidHeaderTruncatedBody hits the read-time failure (as opposed
// to reader-construction failure): the stream's header parses, so the decoder
// is built, but the payload is truncated mid-stream so io.ReadAll errors. The
// original bytes must still be returned (same fallback contract as corrupt).
func TestDecompressValidHeaderTruncatedBody(t *testing.T) {
	t.Run("gzip", func(t *testing.T) {
		full := gzipBytes(t, payload)
		// Keep the 10-byte gzip header + a few data bytes, drop the rest
		// (including the CRC/ISIZE trailer) so NewReader succeeds and ReadAll
		// fails on the incomplete deflate block.
		clipped := full[:15]
		got, _ := bodydecompress.Decompress(clipped, respWith("gzip"), 0)
		if !bytes.Equal(got, clipped) {
			t.Fatalf("truncated gzip should fall back to original bytes; got %q", got)
		}
	})
	t.Run("zstd", func(t *testing.T) {
		full := zstdBytes(t, payload)
		clipped := full[:12]
		got, _ := bodydecompress.Decompress(clipped, respWith("zstd"), 0)
		if !bytes.Equal(got, clipped) {
			t.Fatalf("truncated zstd should fall back to original bytes; got %q", got)
		}
	})
}

// TestDecompressPassthrough covers every branch that returns the body unchanged
// without attempting a decode.
func TestDecompressPassthrough(t *testing.T) {
	t.Run("nil response", func(t *testing.T) {
		if got, tr := bodydecompress.Decompress(payload, nil, 0); tr || !bytes.Equal(got, payload) {
			t.Fatalf("nil resp should pass through")
		}
	})
	t.Run("already uncompressed by transport", func(t *testing.T) {
		r := respWith("gzip")
		r.Uncompressed = true
		// body is the raw (still-gzipped) bytes; because Uncompressed is set we
		// must NOT touch it — assert it is returned verbatim.
		compressed := gzipBytes(t, payload)
		if got, tr := bodydecompress.Decompress(compressed, r, 0); tr || !bytes.Equal(got, compressed) {
			t.Fatalf("Uncompressed=true should pass through untouched")
		}
	})
	t.Run("empty body", func(t *testing.T) {
		if got, tr := bodydecompress.Decompress([]byte{}, respWith("gzip"), 0); tr || len(got) != 0 {
			t.Fatalf("empty body should pass through, got %q", got)
		}
	})
	t.Run("unknown encoding", func(t *testing.T) {
		if got, tr := bodydecompress.Decompress(payload, respWith("snappy"), 0); tr || !bytes.Equal(got, payload) {
			t.Fatalf("unknown encoding should pass through")
		}
	})
	t.Run("no content-encoding header", func(t *testing.T) {
		if got, tr := bodydecompress.Decompress(payload, respWith(""), 0); tr || !bytes.Equal(got, payload) {
			t.Fatalf("missing header should pass through")
		}
	})
}

// TestDecompressBombBounded is the load-bearing security failure mode (F-0278):
// a small compressed payload that expands far beyond the cap must be REJECTED —
// Decompress returns the original (still-compressed) bytes and truncated=true,
// having allocated no more than maxDecompressed+1 bytes for the expansion. The
// same fixture decoded with a generous bound proves the rejection is caused by
// the bound, not by a decode error.
func TestDecompressBombBounded(t *testing.T) {
	// 4 MiB of a single byte compresses to a tiny payload under every codec but
	// expands well past the small cap below — a stand-in for a real bomb.
	bomb := bytes.Repeat([]byte("A"), 4*1024*1024)
	const smallCap = 64 * 1024 // 64 KiB
	cases := []struct {
		name     string
		encoding string
		compress func(*testing.T, []byte) []byte
	}{
		{"gzip", "gzip", gzipBytes},
		{"deflate", "deflate", flateBytes},
		{"br", "br", brotliBytes},
		{"zstd", "zstd", zstdBytes},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			compressed := tc.compress(t, bomb)
			if int64(len(compressed)) > smallCap {
				t.Fatalf("%s: fixture compressed size %d exceeds cap %d; not a valid bomb test",
					tc.name, len(compressed), smallCap)
			}
			// Over the small cap: must be rejected with the original bytes.
			got, truncated := bodydecompress.Decompress(compressed, respWith(tc.encoding), smallCap)
			if !truncated {
				t.Fatalf("%s: decompression bomb must be flagged truncated", tc.name)
			}
			if !bytes.Equal(got, compressed) {
				t.Fatalf("%s: on overflow the original compressed bytes must be returned, not a partial buffer", tc.name)
			}
			// Generous cap: the very same fixture decodes fully, proving the
			// rejection above was the bound — not a corrupt stream.
			full, tr := bodydecompress.Decompress(compressed, respWith(tc.encoding), int64(len(bomb))+1)
			if tr {
				t.Fatalf("%s: fixture must decode cleanly under a generous bound", tc.name)
			}
			if !bytes.Equal(full, bomb) {
				t.Fatalf("%s: generous-bound decode mismatch (len got=%d want=%d)", tc.name, len(full), len(bomb))
			}
		})
	}
}

// TestDecompressDefaultBoundApplied asserts the maxDecompressed<=0 sentinel is
// coerced to the package default rather than meaning "unbounded": a payload
// that expands past the default must be rejected.
func TestDecompressDefaultBoundApplied(t *testing.T) {
	// Expand past DefaultMaxDecompressedBytes (50 MiB) so the default cap trips.
	big := bytes.Repeat([]byte("A"), int(bodydecompress.DefaultMaxDecompressedBytes)+1024)
	compressed := gzipBytes(t, big)
	got, truncated := bodydecompress.Decompress(compressed, respWith("gzip"), 0)
	if !truncated {
		t.Fatal("maxDecompressed<=0 must apply the default bound, not run unbounded")
	}
	if !bytes.Equal(got, compressed) {
		t.Fatal("on default-bound overflow the original compressed bytes must be returned")
	}
}

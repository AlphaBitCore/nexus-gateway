// Package audit: compress.go — zstd compression of large captured bodies on the
// audit side-path. A captured request/response body is JSON or text and
// compresses ~3-10x; the audit pipeline is disk-I/O-bound at the NATS broker
// (the producer's publish workers block on the socket write that backs the
// broker's file store), so shrinking each record's body bytes before publish is
// the direct lever on publish throughput. The compression is end-to-end: the
// producer (ai-gateway audit Writer) compresses, the body rides the NATS wire
// compressed, the Hub persists the COMPRESSED bytes verbatim into the
// inline_*_body column (no decompress on the ingest hot path — a pure copy), and
// only the Control-Plane view layer decompresses when an operator opens a row.
//
// The compressed form is carried as base64 of the zstd frame, identically on the
// wire (BodyEncoding "zstd") and in the persisted column (column encoding
// "zstd"), so the Hub's ingest is a verbatim string copy from wire to column —
// it neither decompresses nor re-encodes. Base64 keeps the NATS envelope valid
// JSON and the column a plain TEXT value; the ~33% base64 overhead is applied to
// the already-shrunk compressed bytes, so the net wire/disk footprint is still a
// large reduction over the uncompressed body.
package audit

import (
	"encoding/base64"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/klauspost/compress/s2"
	"github.com/klauspost/compress/zstd"
)

// inlineCodecS2 reports whether captured bodies should be compressed with S2
// (klauspost/compress/s2) instead of zstd. S2 is a Snappy-derived codec that is
// ~3-5x faster to encode than zstd at a lower (but still large) ratio — on a
// CPU-bound single box the audit marshal worker's zstd EncodeAll was measured as
// the dominant gateway-box CPU consumer (~26%), so trading a little wire/disk
// size for far cheaper compression frees CPU for the request path. The codec is
// per-encoded-body and self-describing on the wire/column (BodyEncoding "s2"),
// so a reader decompresses each body by its own tag — zstd and s2 bodies coexist
// (no migration). Read once; default s2 (faster on the audit side-path) unless
// AI_GATEWAY_AUDIT_CODEC=zstd.
var (
	inlineCodecOnce sync.Once
	inlineCodecIsS2 bool
)

// inlineCodecS2 reports whether inline bodies compress with S2 (the default) vs
// zstd. S2 is the default: it is markedly faster on the audit side-path and the
// slightly larger frame is covered by the spool quota, so audit throughput wins
// over compression ratio. AI_GATEWAY_AUDIT_CODEC=zstd opts into zstd (smaller
// frame, more CPU). Resolved once at process start.
func inlineCodecS2() bool {
	inlineCodecOnce.Do(func() {
		inlineCodecIsS2 = !strings.EqualFold(strings.TrimSpace(os.Getenv("AI_GATEWAY_AUDIT_CODEC")), "zstd")
	})
	return inlineCodecIsS2
}

// compressInlineS2ToBase64 S2-compresses src and returns the base64 of the S2
// frame — the wire/column form of an EncodingS2 body. s2.Encode is a stateless,
// concurrency-safe pure function (no encoder pool needed, unlike zstd). dst is an
// optional scratch slice the base64 is appended to.
func compressInlineS2ToBase64(dst, src []byte) []byte {
	cbp := compressedBufPool.Get().(*[]byte)
	compressed := s2.Encode((*cbp)[:0], src)
	n := base64.StdEncoding.EncodedLen(len(compressed))
	start := len(dst)
	if cap(dst)-start < n {
		grown := make([]byte, start, start+n)
		copy(grown, dst)
		dst = grown
	}
	dst = dst[:start+n]
	base64.StdEncoding.Encode(dst[start:], compressed)
	if cap(compressed) <= compressedBufReclaimCap {
		cb := compressed[:0]
		compressedBufPool.Put(&cb)
	}
	return dst
}

// compressInlineS2Raw S2-compresses src and appends the RAW s2 frame (no base64)
// to dst, returning the grown slice. It is the binary-wire counterpart of
// compressInlineS2ToBase64: the binary audit frame carries the raw compressed
// frame verbatim (the Hub stores it straight into the BYTEA column), so the +33%
// base64 inflation and its encode/decode CPU are removed end-to-end.
func compressInlineS2Raw(dst, src []byte) []byte {
	cbp := compressedBufPool.Get().(*[]byte)
	compressed := s2.Encode((*cbp)[:0], src)
	dst = append(dst, compressed...)
	if cap(compressed) <= compressedBufReclaimCap {
		cb := compressed[:0]
		compressedBufPool.Put(&cb)
	}
	return dst
}

// compressInlineZstdRaw zstd-compresses src and appends the RAW zstd frame (no
// base64) to dst. Binary-wire counterpart of compressInlineToBase64.
func compressInlineZstdRaw(dst, src []byte) []byte {
	e := acquireEncoder()
	cbp := compressedBufPool.Get().(*[]byte)
	compressed := e.EncodeAll(src, (*cbp)[:0])
	releaseEncoder(e)
	dst = append(dst, compressed...)
	if cap(compressed) <= compressedBufReclaimCap {
		cb := compressed[:0]
		compressedBufPool.Put(&cb)
	}
	return dst
}

// base64DecodeFrame decodes the base64 WIRE form of a compressed frame (the
// inlineBytes value an EncodingZstd/EncodingS2 body carries over NATS) back to
// the RAW frame bytes, which are what the BYTEA inline_*_body column stores. The
// decode lands on the Hub ingest worker (CPU headroom) so PG writes 33% fewer
// bytes than the former base64-in-TEXT column. Returns ok=false on malformed
// base64 so the caller can fall back to storing the bytes verbatim.
func base64DecodeFrame(b64 []byte) (frame []byte, ok bool) {
	raw, err := base64.StdEncoding.DecodeString(string(b64))
	if err != nil {
		return nil, false
	}
	return raw, true
}

// maxDecompressedInline bounds the output of an inline-body decompression — a
// decompression-bomb backstop. Audit inline bodies are captured under a small
// inline cutoff, so any frame claiming to expand past this ceiling is corrupt or
// hostile; treat it as an unreadable (absent) body rather than letting it exhaust
// memory at the Control-Plane view layer.
const maxDecompressedInline = 64 << 20 // 64 MiB

// decompressInlineS2 S2-decompresses a RAW s2 frame — the BYTEA column form of an
// "s2" body. Returns nil on malformed input (the read-side contract treats an
// unreadable audit body as absent, never a hard failure of the surrounding read)
// or when the declared decompressed size exceeds maxDecompressedInline.
func decompressInlineS2(frame []byte) []byte {
	if n, err := s2.DecodedLen(frame); err != nil || n < 0 || n > maxDecompressedInline {
		return nil
	}
	out, err := s2.Decode(nil, frame)
	if err != nil {
		return nil
	}
	return out
}

// encoderPool holds single-concurrency zstd encoders, one taken per EncodeAll.
// A SHARED encoder is wrong here: klauspost's EncodeAll serialises concurrent
// callers on the encoder's internal state pool (sized by WithEncoderConcurrency),
// so the N audit marshal workers would bottleneck on one encoder — measured as a
// drain collapse (queue backs up, body pool pins multi-GB, almost nothing lands).
// A pool of WithEncoderConcurrency(1) encoders gives each marshal worker its own,
// so compression scales with the workers. The decoder side (DecodeAll) is
// concurrency-safe and low-frequency (view layer only), so a single shared
// decoder suffices. Both are created lazily so a process that never compresses or
// decompresses pays nothing.
// encoderRing is a GC-STABLE pool of single-concurrency zstd encoders. A
// sync.Pool is the wrong primitive here: the runtime drops every sync.Pool
// entry on each GC, so under the audit path's steady compression load (thousands
// of records/sec the box GCs hundreds of times) the pool is repeatedly emptied
// and acquireEncoder falls back to zstd.NewWriter — a multi-MB window allocation
// per miss, measured as a top heap allocator (~5 GB/run). A bounded buffered
// channel survives GC, so a warmed encoder is almost always available and
// NewWriter runs only to fill the ring once. Capacity covers the audit marshal
// fan-out; excess concurrent callers allocate transiently and are dropped on
// release when the ring is full (bounding retained encoders).
var (
	encoderRing = make(chan *zstd.Encoder, 64)
	zstdDecoder atomic.Pointer[zstd.Decoder]
)

// inlineCompression holds the producer-side toggle. Reads/writes are atomic so
// SetInlineCompression (called once at service boot) is safe against the audit
// marshal workers. enabled gates NewInlineBody choosing EncodingZstd; minBytes
// is the smallest body worth compressing (below it the zstd frame overhead and
// CPU outweigh the saved bytes); level is the zstd encoder level. Decoding never
// consults these — a reader decompresses any "zstd" body regardless of whether
// THIS process produces compressed bodies.
var inlineCompression struct {
	enabled  atomic.Bool
	minBytes atomic.Int64
	level    atomic.Int32
}

// DefaultInlineCompressionMinBytes is the default body-size floor for
// compression. Below ~1 KiB the zstd frame header + base64 overhead can exceed
// the saved bytes, and tiny bodies are not the disk-throughput problem.
const DefaultInlineCompressionMinBytes = 1024

// SetInlineCompression configures producer-side inline body compression. Called
// once at service boot from config. level maps to zstd.EncoderLevelFromZstd; an
// out-of-range level falls back to SpeedDefault. minBytes <= 0 uses the default
// floor. Idempotent and safe to call before any audit marshal runs.
func SetInlineCompression(enabled bool, minBytes int, level int) {
	if minBytes <= 0 {
		minBytes = DefaultInlineCompressionMinBytes
	}
	inlineCompression.minBytes.Store(int64(minBytes))
	lvl := zstd.EncoderLevelFromZstd(level)
	inlineCompression.level.Store(int32(lvl))
	inlineCompression.enabled.Store(enabled)
}

// inlineCompressionEnabled reports whether NewInlineBody should mark large
// bodies for zstd compression in this process.
func inlineCompressionEnabled() bool { return inlineCompression.enabled.Load() }

// inlineCompressionMinBytes returns the configured size floor (or the default).
func inlineCompressionMinBytes() int {
	if v := inlineCompression.minBytes.Load(); v > 0 {
		return int(v)
	}
	return DefaultInlineCompressionMinBytes
}

// acquireEncoder returns a single-concurrency encoder from the pool at the
// configured level, allocating one on a pool miss. Returned via releaseEncoder.
func acquireEncoder() *zstd.Encoder {
	select {
	case e := <-encoderRing:
		return e
	default:
	}
	lvl := zstd.EncoderLevel(inlineCompression.level.Load())
	if lvl == 0 {
		lvl = zstd.SpeedDefault
	}
	e, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(lvl), zstd.WithEncoderConcurrency(1))
	if err != nil {
		// NewWriter only errors on a bad option; fall back to a default writer so
		// compression never hard-fails the audit path.
		e, _ = zstd.NewWriter(nil)
	}
	return e
}

// releaseEncoder returns an encoder to the ring for reuse, or drops it to GC
// when the ring is full (bounding the number of retained encoders).
func releaseEncoder(e *zstd.Encoder) {
	select {
	case encoderRing <- e:
	default:
	}
}

// decoder returns the lazily-initialised shared zstd decoder.
func decoder() *zstd.Decoder {
	if d := zstdDecoder.Load(); d != nil {
		return d
	}
	d, err := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(maxDecompressedInline))
	if err != nil {
		return nil
	}
	if !zstdDecoder.CompareAndSwap(nil, d) {
		d.Close()
		return zstdDecoder.Load()
	}
	return d
}

// compressedBufPool reuses the intermediate zstd-frame buffer between
// compressInlineToBase64 calls. EncodeAll(src, nil) allocated a fresh ~body/5
// slice per audit record, which — at thousands of records/sec — was a dominant
// short-lived allocator driving GC frequency (measured: GC pause STW adds to the
// gateway request latency). Pooling the intermediate keeps the working set flat.
var compressedBufPool = sync.Pool{New: func() any { b := make([]byte, 0, 16<<10); return &b }}

// compressedBufReclaimCap bounds a pooled compressed buffer — one oversized body
// must not inflate every pooled buffer thereafter.
const compressedBufReclaimCap = 1 << 20 // 1 MiB

// compressInlineToBase64 zstd-compresses src and returns the base64 of the zstd
// frame — the wire/column form of an EncodingZstd body. dst is an optional
// scratch slice the base64 is appended to (pass nil for a fresh allocation); the
// returned slice is the appended result.
func compressInlineToBase64(dst, src []byte) []byte {
	e := acquireEncoder()
	cbp := compressedBufPool.Get().(*[]byte)
	compressed := e.EncodeAll(src, (*cbp)[:0])
	releaseEncoder(e)
	n := base64.StdEncoding.EncodedLen(len(compressed))
	start := len(dst)
	if cap(dst)-start < n {
		grown := make([]byte, start, start+n)
		copy(grown, dst)
		dst = grown
	}
	dst = dst[:start+n]
	base64.StdEncoding.Encode(dst[start:], compressed)
	// compressed is fully consumed; return its backing buffer to the pool (drop an
	// oversized one to GC so a single large body doesn't inflate the pool).
	if cap(compressed) <= compressedBufReclaimCap {
		cb := compressed[:0]
		compressedBufPool.Put(&cb)
	}
	return dst
}

// decompressInline zstd-decompresses a RAW zstd frame — the BYTEA column form of
// a "zstd" body. Returns nil on any malformed input — the read-side contract
// treats an unreadable audit body as absent, never a hard failure of the read.
func decompressInline(frame []byte) []byte {
	d := decoder()
	if d == nil {
		return nil
	}
	out, err := d.DecodeAll(frame, nil)
	if err != nil {
		return nil
	}
	return out
}

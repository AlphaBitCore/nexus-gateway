package audit

import (
	"bytes"
	"strings"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// TestCompressInlineRaw_RoundTrip covers the binary-wire raw-frame compressors
// (compressInlineS2Raw / compressInlineZstdRaw) and their matching decompressors:
// each appends a RAW (non-base64) compressed frame to dst, which must decompress
// back to the original bytes. Both a compressible and a high-entropy payload are
// exercised so the frame body is non-trivial in each case, and the dst-append
// (grow, not overwrite) contract is asserted via a sentinel prefix.
func TestCompressInlineRaw_RoundTrip(t *testing.T) {
	compressible := []byte(strings.Repeat("the quick brown fox jumps over the lazy dog. ", 300))
	highEntropy := make([]byte, 8192)
	for i := range highEntropy {
		highEntropy[i] = byte((i*97 + 13) % 251)
	}

	for _, src := range [][]byte{compressible, highEntropy, []byte("small"), {}} {
		// S2 raw frame.
		s2Frame := compressInlineS2Raw([]byte("PRE"), src)
		if !bytes.HasPrefix(s2Frame, []byte("PRE")) {
			t.Fatalf("compressInlineS2Raw overwrote dst prefix")
		}
		if got := decompressInlineS2(s2Frame[3:]); !bytes.Equal(got, src) {
			t.Fatalf("s2 raw round-trip mismatch: got %d want %d bytes", len(got), len(src))
		}

		// Zstd raw frame.
		zFrame := compressInlineZstdRaw([]byte("PRE"), src)
		if !bytes.HasPrefix(zFrame, []byte("PRE")) {
			t.Fatalf("compressInlineZstdRaw overwrote dst prefix")
		}
		if got := decompressInline(zFrame[3:]); !bytes.Equal(got, src) {
			t.Fatalf("zstd raw round-trip mismatch: got %d want %d bytes", len(got), len(src))
		}
	}

	// The raw frame for highly compressible data must be smaller than the input —
	// guards against a silent store-only degradation.
	if f := compressInlineS2Raw(nil, compressible); len(f) >= len(compressible) {
		t.Fatalf("s2 raw did not shrink compressible data: %d >= %d", len(f), len(compressible))
	}
	if f := compressInlineZstdRaw(nil, compressible); len(f) >= len(compressible) {
		t.Fatalf("zstd raw did not shrink compressible data: %d >= %d", len(f), len(compressible))
	}
}

// TestBase64DecodeFrame_BadInput covers the malformed-base64 branch: the function
// must report ok=false (so the caller falls back to storing bytes verbatim) and a
// nil frame, never panic.
func TestBase64DecodeFrame_BadInput(t *testing.T) {
	frame, ok := base64DecodeFrame([]byte("!!! not base64 @@@"))
	if ok || frame != nil {
		t.Fatalf("malformed base64 must yield (nil,false), got (%v,%v)", frame, ok)
	}
	// Happy path sanity: a real base64 string decodes to its raw bytes.
	good, ok := base64DecodeFrame([]byte("aGVsbG8=")) // "hello"
	if !ok || string(good) != "hello" {
		t.Fatalf("valid base64 decode failed: %q ok=%v", good, ok)
	}
}

// TestDecompressInline_MalformedReturnsNil covers the error branch of both raw
// decompressors: an unreadable frame yields nil (treated as an absent body), never
// an error or panic.
func TestDecompressInline_MalformedReturnsNil(t *testing.T) {
	if got := decompressInlineS2([]byte("not an s2 frame at all")); got != nil {
		t.Fatalf("malformed s2 frame should decompress to nil, got %d bytes", len(got))
	}
	if got := decompressInline([]byte("not a zstd frame at all")); got != nil {
		t.Fatalf("malformed zstd frame should decompress to nil, got %d bytes", len(got))
	}
}

// TestInlineCompressionMinBytes_DefaultFallback covers the fallback branch where
// the stored floor is non-positive: SetInlineCompression normalizes minBytes<=0 to
// the default, so after such a call the getter returns DefaultInlineCompressionMinBytes.
func TestInlineCompressionMinBytes_DefaultFallback(t *testing.T) {
	t.Cleanup(func() { SetInlineCompression(false, 0, 0) })

	// minBytes <= 0 → normalized to the default floor.
	SetInlineCompression(true, 0, 0)
	if got := inlineCompressionMinBytes(); got != DefaultInlineCompressionMinBytes {
		t.Fatalf("minBytes<=0 should fall back to default %d, got %d", DefaultInlineCompressionMinBytes, got)
	}
	SetInlineCompression(true, -5, 0)
	if got := inlineCompressionMinBytes(); got != DefaultInlineCompressionMinBytes {
		t.Fatalf("negative minBytes should fall back to default, got %d", got)
	}

	// An explicit positive floor is returned verbatim.
	SetInlineCompression(true, 2048, 0)
	if got := inlineCompressionMinBytes(); got != 2048 {
		t.Fatalf("positive floor should be returned verbatim, got %d", got)
	}

	// The getter's own non-positive fallback arm: store 0 directly (bypassing
	// SetInlineCompression's normalization) so inlineCompressionMinBytes returns the
	// default rather than 0 — this is the in-getter guard for the never-configured
	// process (the atomic's zero value).
	inlineCompression.minBytes.Store(0)
	if got := inlineCompressionMinBytes(); got != DefaultInlineCompressionMinBytes {
		t.Fatalf("stored 0 must fall back to default in getter, got %d", got)
	}
}

// TestAcquireReleaseEncoder_PoolMissAndLevel exercises acquireEncoder's pool-miss
// allocation path (a freshly-allocated encoder when the ring is empty) at a
// configured level, and the release-back-to-ring path, asserting the encoder
// actually compresses (a real EncodeAll round-trip).
func TestAcquireReleaseEncoder_PoolMissAndLevel(t *testing.T) {
	t.Cleanup(func() { SetInlineCompression(false, 0, 0) })

	// Drain the ring so the next acquire takes the pool-miss allocation branch.
	drained := drainEncoderRing()
	defer func() {
		for _, e := range drained {
			releaseEncoder(e)
		}
	}()

	for _, lvl := range []int{0, 1, 3, 9} {
		SetInlineCompression(true, 64, lvl)
		// Drain again each iteration so acquire allocates at the configured level.
		for _, e := range drainEncoderRing() {
			_ = e // dropped to GC; we want a guaranteed miss
		}
		e := acquireEncoder()
		if e == nil {
			t.Fatalf("level %d: acquireEncoder returned nil", lvl)
		}
		src := []byte(strings.Repeat("compress me please ", 100))
		frame := e.EncodeAll(src, nil)
		releaseEncoder(e)
		if got := decompressInline(frame); !bytes.Equal(got, src) {
			t.Fatalf("level %d: encoder produced a frame that did not round-trip", lvl)
		}
	}
}

// TestReleaseEncoder_RingFullDropsToGC covers releaseEncoder's default branch: when
// the ring is already full, an extra encoder is dropped (no panic, no block).
func TestReleaseEncoder_RingFullDropsToGC(t *testing.T) {
	saved := drainEncoderRing()
	t.Cleanup(func() {
		drainEncoderRing() // discard the fill encoders we added below
		for _, e := range saved {
			releaseEncoder(e)
		}
	})

	// Fill the ring to capacity with freshly-allocated encoders pushed straight in.
	// (acquire+release would never grow a partially-full ring — acquire pulls the
	// one entry back out, so the length just cycles.)
	for len(encoderRing) < cap(encoderRing) {
		e, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
		if err != nil {
			t.Fatalf("NewWriter: %v", err)
		}
		encoderRing <- e
	}
	if len(encoderRing) != cap(encoderRing) {
		t.Fatalf("ring not full: len=%d cap=%d", len(encoderRing), cap(encoderRing))
	}
	// Releasing one more encoder while the ring is full must hit the default arm and
	// drop it (no block on the full channel, no panic).
	extra, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	releaseEncoder(extra) // ring is full → dropped silently
	if len(encoderRing) != cap(encoderRing) {
		t.Fatalf("ring length changed after full-ring release: len=%d cap=%d", len(encoderRing), cap(encoderRing))
	}
}

// TestDecoder_ReturnsUsableSingleton covers decoder(): the first call lazily builds
// the shared decoder, a second call returns the same cached pointer (the load
// fast-path), and the returned decoder actually decompresses a real frame.
func TestDecoder_ReturnsUsableSingleton(t *testing.T) {
	d1 := decoder()
	if d1 == nil {
		t.Fatal("decoder() returned nil")
	}
	d2 := decoder() // load fast-path (already initialised)
	if d1 != d2 {
		t.Fatal("decoder() must return the same cached singleton")
	}
	src := []byte(strings.Repeat("zstd payload ", 50))
	frame := compressInlineZstdRaw(nil, src)
	out, err := d1.DecodeAll(frame, nil)
	if err != nil || !bytes.Equal(out, src) {
		t.Fatalf("singleton decoder failed to decompress: err=%v match=%v", err, bytes.Equal(out, src))
	}
}

// TestDecoder_ConcurrentInitCASLoser covers the CAS-loss arm of decoder(): when
// several goroutines race the lazy init, exactly one wins CompareAndSwap and the
// losers must Close their throwaway decoder and return the winner's singleton. The
// test resets the package decoder to nil, fans out, and asserts every goroutine
// observes the SAME non-nil decoder (proving the losers discarded their own).
func TestDecoder_ConcurrentInitCASLoser(t *testing.T) {
	prev := zstdDecoder.Swap(nil)
	t.Cleanup(func() { zstdDecoder.Store(prev) })

	const n = 64
	results := make([]*zstd.Decoder, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start // release all at once to maximise the CAS race
			results[idx] = decoder()
		}(i)
	}
	close(start)
	wg.Wait()

	winner := zstdDecoder.Load()
	if winner == nil {
		t.Fatal("decoder singleton is nil after concurrent init")
	}
	for i, d := range results {
		if d != winner {
			t.Fatalf("goroutine %d got a different decoder than the singleton (CAS loser did not adopt the winner)", i)
		}
	}
}

// drainEncoderRing empties the encoder ring non-blockingly and returns what it
// pulled, so a test can force acquireEncoder's pool-miss branch deterministically.
func drainEncoderRing() []*zstd.Encoder {
	var out []*zstd.Encoder
	for {
		select {
		case e := <-encoderRing:
			out = append(out, e)
		default:
			return out
		}
	}
}

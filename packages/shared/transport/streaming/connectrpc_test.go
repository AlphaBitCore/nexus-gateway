package streaming

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// makeConnectRPCFrame produces a Connect-RPC envelope byte slice with the
// given flag byte and payload.
func makeConnectRPCFrame(flag byte, payload []byte) []byte {
	hdr := make([]byte, 5)
	hdr[0] = flag
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	return append(hdr, payload...)
}

// TestReadConnectRPCFrame_StandardFrame — a plain data frame (no flags) returns
// the payload with neither the compressed nor the end-of-stream bit set.
func TestReadConnectRPCFrame_StandardFrame(t *testing.T) {
	frame := makeConnectRPCFrame(0x00, []byte("hello"))
	flags, payload, err := ReadConnectRPCFrame(bytes.NewReader(frame))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if flags&ConnectFlagEndStream != 0 || flags&ConnectFlagCompressed != 0 {
		t.Errorf("flags = 0x%02x, want 0x00 for a plain data frame", flags)
	}
	if !bytes.Equal(payload, []byte("hello")) {
		t.Errorf("payload = %q, want hello", payload)
	}
}

// TestReadConnectRPCFrame_EndOfStreamFlag — bit 0x02 marks the end-of-stream
// (trailer) frame. The compressed bit (0x01) must NOT be mistaken for it — that
// conflation made the reader stop after the first compressed data frame.
func TestReadConnectRPCFrame_EndOfStreamFlag(t *testing.T) {
	frame := makeConnectRPCFrame(ConnectFlagEndStream, []byte(`{"trailer":"x"}`))
	flags, payload, err := ReadConnectRPCFrame(bytes.NewReader(frame))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if flags&ConnectFlagEndStream == 0 {
		t.Errorf("flags = 0x%02x, want end-of-stream bit set", flags)
	}
	if !bytes.Equal(payload, []byte(`{"trailer":"x"}`)) {
		t.Errorf("payload = %q", payload)
	}

	// Regression guard: a compressed data frame (0x01) is NOT end-of-stream.
	cf, _, err := ReadConnectRPCFrame(bytes.NewReader(makeConnectRPCFrame(ConnectFlagCompressed, []byte("data"))))
	if err != nil {
		t.Fatalf("compressed frame err = %v", err)
	}
	if cf&ConnectFlagEndStream != 0 {
		t.Errorf("compressed flag 0x01 misread as end-of-stream (flags=0x%02x)", cf)
	}
	if cf&ConnectFlagCompressed == 0 {
		t.Errorf("flags = 0x%02x, want compressed bit set", cf)
	}
}

// TestReadConnectRPCFrame_ZeroLengthFrame — length=0 returns (flags, nil, nil)
// without attempting to read a body.
func TestReadConnectRPCFrame_ZeroLengthFrame(t *testing.T) {
	frame := makeConnectRPCFrame(ConnectFlagEndStream, nil) // trailer, zero body
	flags, payload, err := ReadConnectRPCFrame(bytes.NewReader(frame))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if flags&ConnectFlagEndStream == 0 {
		t.Errorf("flags = 0x%02x, want end-of-stream bit set", flags)
	}
	if payload != nil {
		t.Errorf("payload = %v, want nil for zero-length frame", payload)
	}
}

// TestMaybeGunzipConnectFrame — compressed frames are decompressed; uncompressed
// frames and undecodable bodies pass through unchanged.
func TestMaybeGunzipConnectFrame(t *testing.T) {
	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	_, _ = gw.Write([]byte("payload text"))
	_ = gw.Close()

	if got := MaybeGunzipConnectFrame(ConnectFlagCompressed, gz.Bytes()); string(got) != "payload text" {
		t.Errorf("compressed frame = %q, want decompressed 'payload text'", got)
	}
	// Not flagged compressed → returned verbatim even if it happens to be gzip.
	if got := MaybeGunzipConnectFrame(0x00, gz.Bytes()); !bytes.Equal(got, gz.Bytes()) {
		t.Errorf("unflagged frame was modified")
	}
	// Flagged compressed but not actually gzip → best-effort returns raw.
	if got := MaybeGunzipConnectFrame(ConnectFlagCompressed, []byte("not-gzip")); string(got) != "not-gzip" {
		t.Errorf("bad gzip = %q, want raw fall-through", got)
	}
}

// TestReadConnectRPCFrame_HeaderEOF — empty reader returns io.EOF on the
// header read.
func TestReadConnectRPCFrame_HeaderEOF(t *testing.T) {
	_, _, err := ReadConnectRPCFrame(bytes.NewReader(nil))
	if !errors.Is(err, io.EOF) {
		t.Errorf("err = %v, want io.EOF", err)
	}
}

// TestReadConnectRPCFrame_ShortHeader — fewer than 5 header bytes yields
// io.ErrUnexpectedEOF from io.ReadFull.
func TestReadConnectRPCFrame_ShortHeader(t *testing.T) {
	_, _, err := ReadConnectRPCFrame(bytes.NewReader([]byte{0x00, 0x00})) // only 2 bytes
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("err = %v, want io.ErrUnexpectedEOF", err)
	}
}

// TestReadConnectRPCFrame_TruncatedPayload — header advertises N bytes but
// only a prefix is available → io.ErrUnexpectedEOF.
func TestReadConnectRPCFrame_TruncatedPayload(t *testing.T) {
	// claim 10-byte body but supply only 3 bytes
	buf := make([]byte, 5+3)
	buf[0] = 0x00
	binary.BigEndian.PutUint32(buf[1:5], 10)
	copy(buf[5:], []byte("abc"))
	_, _, err := ReadConnectRPCFrame(bytes.NewReader(buf))
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("err = %v, want io.ErrUnexpectedEOF", err)
	}
}

// TestPassthroughWithConnectRPCExtract_RelaysBytesAndAccumulates — happy path:
// upstream bytes reach client verbatim and extractor sees each frame payload.
func TestPassthroughWithConnectRPCExtract_RelaysBytesAndAccumulates(t *testing.T) {
	frames := bytes.Buffer{}
	frames.Write(makeConnectRPCFrame(0x00, []byte("alpha")))
	frames.Write(makeConnectRPCFrame(0x00, []byte("beta")))
	frames.Write(makeConnectRPCFrame(0x01, []byte("end")))

	want := frames.Bytes()
	var client bytes.Buffer

	extractor := func(p []byte) string { return string(p) + "|" }
	acc, err := PassthroughWithConnectRPCExtract(
		context.Background(),
		bytes.NewReader(want),
		&client,
		nil,
		extractor,
	)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !bytes.Equal(client.Bytes(), want) {
		t.Errorf("client bytes diverged from upstream")
	}
	if acc != "alpha|beta|end|" {
		t.Errorf("accumulated = %q, want alpha|beta|end|", acc)
	}
}

// TestPassthroughWithConnectRPCExtract_NilExtractor — when extractor is nil,
// the side goroutine just drains and the relay still completes.
func TestPassthroughWithConnectRPCExtract_NilExtractor(t *testing.T) {
	frames := bytes.Buffer{}
	frames.Write(makeConnectRPCFrame(0x01, []byte("payload")))

	var client bytes.Buffer
	acc, err := PassthroughWithConnectRPCExtract(
		context.Background(),
		bytes.NewReader(frames.Bytes()),
		&client,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if acc != "" {
		t.Errorf("nil-extractor accumulated = %q, want empty", acc)
	}
	if !bytes.Equal(client.Bytes(), frames.Bytes()) {
		t.Error("relay diverged")
	}
}

// TestPassthroughWithConnectRPCExtract_GzippedPayloads — a frame flagged
// ConnectFlagCompressed (0x01) is decompressed for the extractor while the
// client still receives the original gzipped wire bytes.
func TestPassthroughWithConnectRPCExtract_GzippedPayloads(t *testing.T) {
	// Build a gzipped payload "hidden message".
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	_, _ = gw.Write([]byte("hidden message"))
	_ = gw.Close()
	gzipped := gzBuf.Bytes()

	frame := makeConnectRPCFrame(0x01, gzipped)

	var client bytes.Buffer
	var got string
	extractor := func(p []byte) string {
		got = string(p)
		return got
	}
	if _, err := PassthroughWithConnectRPCExtract(
		context.Background(),
		bytes.NewReader(frame),
		&client,
		nil,
		extractor,
	); err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != "hidden message" {
		t.Errorf("extractor saw %q, want 'hidden message' (decompressed)", got)
	}
	// Client must receive the raw gzipped wire bytes — owns its own decompression.
	if !bytes.Equal(client.Bytes(), frame) {
		t.Errorf("client got modified bytes; expected raw wire encoding")
	}
}

// TestPassthroughWithConnectRPCExtract_GzippedBadPayload — a frame flagged
// compressed whose bytes are not gzip falls through: extractor gets the raw
// payload, no panic.
func TestPassthroughWithConnectRPCExtract_GzippedBadPayload(t *testing.T) {
	// Non-gzip bytes inside the frame.
	frame := makeConnectRPCFrame(0x01, []byte("not-a-gzip-stream"))

	var client bytes.Buffer
	var got string
	extractor := func(p []byte) string {
		got = string(p)
		return got
	}
	_, err := PassthroughWithConnectRPCExtract(
		context.Background(),
		bytes.NewReader(frame),
		&client,
		nil,
		extractor,
	)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	// gzip.NewReader returns an error — extractor receives original bytes
	// per the fall-through path.
	if got != "not-a-gzip-stream" {
		t.Errorf("got = %q, want raw payload on gzip-decode failure", got)
	}
}

// TestPassthroughWithConnectRPCExtract_CapturedBuffer — when captureBuf is
// non-nil, every relayed byte tees into it as well.
func TestPassthroughWithConnectRPCExtract_CapturedBuffer(t *testing.T) {
	frame := makeConnectRPCFrame(0x01, []byte("hello"))
	var client bytes.Buffer
	cap := NewCappedBuffer(1024)

	if _, err := PassthroughWithConnectRPCExtract(
		context.Background(),
		bytes.NewReader(frame),
		&client,
		cap,
		nil,
	); err != nil {
		t.Fatalf("err = %v", err)
	}
	if !bytes.Equal(client.Bytes(), frame) {
		t.Errorf("client bytes diverged")
	}
	if !bytes.Equal(cap.Bytes(), frame) {
		t.Errorf("captureBuf got %q, want %q", cap.Bytes(), frame)
	}
}

// TestPassthroughWithConnectRPCExtract_ContextCancel — pre-cancelled ctx
// causes the relay to return ctx.Err() without writing anything.
func TestPassthroughWithConnectRPCExtract_ContextCancel(t *testing.T) {
	// Slow reader simulates upstream that doesn't immediately EOF.
	pr, pw := io.Pipe()
	defer pw.Close()
	go func() {
		// Never write; let context cancellation be the only termination signal.
		time.Sleep(50 * time.Millisecond)
	}()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var client bytes.Buffer
	_, err := PassthroughWithConnectRPCExtract(ctx, pr, &client, nil, nil)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// errReader returns the configured error after a single Read.
type errReader struct {
	data []byte
	pos  int
	err  error
}

func (r *errReader) Read(p []byte) (int, error) {
	if r.pos < len(r.data) {
		n := copy(p, r.data[r.pos:])
		r.pos += n
		return n, nil
	}
	return 0, r.err
}

// TestPassthroughWithConnectRPCExtract_UpstreamReadError — a non-EOF read
// error after delivering some bytes is returned to the caller.
func TestPassthroughWithConnectRPCExtract_UpstreamReadError(t *testing.T) {
	frame := makeConnectRPCFrame(0x00, []byte("partial"))
	myErr := errors.New("upstream-read-fail")
	r := &errReader{data: frame, err: myErr}

	var client bytes.Buffer
	_, err := PassthroughWithConnectRPCExtract(
		context.Background(),
		r,
		&client,
		nil,
		func(p []byte) string { return string(p) },
	)
	if !errors.Is(err, myErr) {
		t.Errorf("err = %v, want %v", err, myErr)
	}
	// Client should at least see the relayed prefix.
	if !bytes.Equal(client.Bytes(), frame) {
		t.Errorf("client bytes diverged: %q vs %q", client.Bytes(), frame)
	}
}

// errWriter returns errFail on every Write.
type errWriter struct{ err error }

func (w *errWriter) Write(p []byte) (int, error) { return 0, w.err }

// TestPassthroughWithConnectRPCExtract_ClientWriteError — client Write error
// aborts the relay and is returned.
func TestPassthroughWithConnectRPCExtract_ClientWriteError(t *testing.T) {
	frame := makeConnectRPCFrame(0x01, []byte("payload"))
	myErr := errors.New("client-write-fail")

	_, err := PassthroughWithConnectRPCExtract(
		context.Background(),
		bytes.NewReader(frame),
		&errWriter{err: myErr},
		nil,
		nil,
	)
	if !errors.Is(err, myErr) {
		t.Errorf("err = %v, want %v", err, myErr)
	}
}

// flushOnlyWriter implements both io.Writer and http.Flusher so we can
// verify the relay calls Flush after each upstream chunk.
type flushOnlyWriter struct {
	buf       bytes.Buffer
	flushes   int
	failWrite bool
}

func (f *flushOnlyWriter) Write(p []byte) (int, error) {
	if f.failWrite {
		return 0, errors.New("simulated")
	}
	return f.buf.Write(p)
}

func (f *flushOnlyWriter) Flush() { f.flushes++ }

// TestPassthroughWithConnectRPCExtract_FlushCalled — http.Flusher capable
// clients receive Flush() after each upstream read.
func TestPassthroughWithConnectRPCExtract_FlushCalled(t *testing.T) {
	frame := makeConnectRPCFrame(0x01, []byte("payload"))
	client := &flushOnlyWriter{}

	if _, err := PassthroughWithConnectRPCExtract(
		context.Background(),
		bytes.NewReader(frame),
		client,
		nil,
		nil,
	); err != nil {
		t.Fatalf("err = %v", err)
	}
	if client.flushes == 0 {
		t.Errorf("Flush was never called on flusher-capable client")
	}
	if !bytes.Equal(client.buf.Bytes(), frame) {
		t.Errorf("client bytes diverged")
	}
}

// TestReadConnectRPCFrame_LargePayload — sanity: full-large payload is
// reassembled correctly even with non-power-of-2 size.
func TestReadConnectRPCFrame_LargePayload(t *testing.T) {
	payload := []byte(strings.Repeat("x", 7777))
	frame := makeConnectRPCFrame(0x00, payload)
	_, got, err := ReadConnectRPCFrame(bytes.NewReader(frame))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("payload mismatch: len got=%d want=%d", len(got), len(payload))
	}
}

// TestReadConnectRPCFrame_OversizedLengthRejected — a header declaring a
// payload above MaxConnectRPCFrameLen returns ErrConnectRPCFrameTooLarge
// before any payload allocation. Bodies that are not actually Connect-framed
// can spell multi-GB lengths out of arbitrary bytes, so the guard must fire
// on the declared length alone, never on the (absent) body.
func TestReadConnectRPCFrame_OversizedLengthRejected(t *testing.T) {
	for _, declared := range []uint32{MaxConnectRPCFrameLen + 1, 0xF6F5F4F3} {
		buf := make([]byte, 5)
		buf[0] = 0x00
		binary.BigEndian.PutUint32(buf[1:5], declared)
		_, payload, err := ReadConnectRPCFrame(bytes.NewReader(buf))
		if !errors.Is(err, ErrConnectRPCFrameTooLarge) {
			t.Fatalf("declared=%d: err = %v, want ErrConnectRPCFrameTooLarge", declared, err)
		}
		if payload != nil {
			t.Errorf("declared=%d: payload = %d bytes, want nil", declared, len(payload))
		}
	}
}

// TestReadConnectRPCFrame_CapBoundaryAccepted — a declared length of exactly
// MaxConnectRPCFrameLen passes the guard; the subsequent body read fails on
// the truncated input, proving rejection did not happen at the guard.
func TestReadConnectRPCFrame_CapBoundaryAccepted(t *testing.T) {
	buf := make([]byte, 5+3)
	buf[0] = 0x00
	binary.BigEndian.PutUint32(buf[1:5], MaxConnectRPCFrameLen)
	copy(buf[5:], "abc")
	_, _, err := ReadConnectRPCFrame(bytes.NewReader(buf))
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want io.ErrUnexpectedEOF (boundary length must pass the guard)", err)
	}
}

// TestPassthroughWithConnectRPCExtract_GzipDecompressionCapped — a frame
// whose gzip payload inflates past MaxConnectRPCFrameLen hands the extractor
// at most MaxConnectRPCFrameLen bytes; the relay bytes stay verbatim.
func TestPassthroughWithConnectRPCExtract_GzipDecompressionCapped(t *testing.T) {
	var compressed bytes.Buffer
	gw := gzip.NewWriter(&compressed)
	if _, err := gw.Write(make([]byte, MaxConnectRPCFrameLen+4096)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	frame := makeConnectRPCFrame(0x01, compressed.Bytes())
	var client bytes.Buffer
	var extractorSaw int
	_, err := PassthroughWithConnectRPCExtract(
		context.Background(),
		bytes.NewReader(frame),
		&client,
		nil,
		func(p []byte) string { extractorSaw = len(p); return "" },
	)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !bytes.Equal(client.Bytes(), frame) {
		t.Errorf("client bytes diverged from upstream")
	}
	if extractorSaw != MaxConnectRPCFrameLen {
		t.Errorf("extractor saw %d bytes, want exactly MaxConnectRPCFrameLen (%d)", extractorSaw, MaxConnectRPCFrameLen)
	}
}

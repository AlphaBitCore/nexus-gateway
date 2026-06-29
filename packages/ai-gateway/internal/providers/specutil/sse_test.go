package specutil

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// nopCloserBody is a minimal io.ReadCloser wrapping a strings.Reader so
// tests can drive SSEScanner against a synthetic body and observe Close
// propagation. Tracks Close-count so we can assert at-most-once
// idempotency on the scanner side.
type nopCloserBody struct {
	r         *strings.Reader
	closes    int
	closeErr  error
	readErrAt int // when >0, return readErr after this many reads
	readCount int
	readErr   error
}

func (b *nopCloserBody) Read(p []byte) (int, error) {
	b.readCount++
	if b.readErrAt > 0 && b.readCount >= b.readErrAt {
		return 0, b.readErr
	}
	return b.r.Read(p)
}

func (b *nopCloserBody) Close() error {
	b.closes++
	return b.closeErr
}

func newBody(s string) *nopCloserBody {
	return &nopCloserBody{r: strings.NewReader(s)}
}

// TestSSEScanner_DataOnlyEvents asserts the OpenAI-style "data: …\n\n"
// transcript yields one SSEEvent per blank-line-separated record, with
// the data prefix stripped and the single leading space discarded. This
// is the dominant adapter path — DeepSeek/OpenAI/Moonshot all rely on
// it producing exact wire bytes for the JSON payload.
func TestSSEScanner_DataOnlyEvents(t *testing.T) {
	body := newBody("data: {\"a\":1}\n\ndata: {\"b\":2}\n\ndata: [DONE]\n\n")
	s := NewSSEScanner(body)
	defer s.Close()

	wantData := []string{`{"a":1}`, `{"b":2}`, `[DONE]`}
	for i, want := range wantData {
		ev, err := s.Next()
		if err != nil {
			t.Fatalf("event %d: unexpected err: %v", i, err)
		}
		if ev.Event != "" {
			t.Errorf("event %d: Event=%q, want empty (data-only frame)", i, ev.Event)
		}
		if string(ev.Data) != want {
			t.Errorf("event %d: Data=%q, want %q", i, ev.Data, want)
		}
	}

	if _, err := s.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("after last event: err=%v, want io.EOF", err)
	}
}

// TestSSEScanner_NamedEvents asserts the Anthropic-style "event: X\ndata: Y\n\n"
// transcript carries the named event through to SSEEvent.Event, and that
// the event name is reset between frames so a data-only frame after a
// named frame does NOT inherit the previous Event.
func TestSSEScanner_NamedEvents(t *testing.T) {
	body := newBody(
		"event: message_start\ndata: {\"id\":\"msg_1\"}\n\n" +
			"event: content_block_delta\ndata: {\"index\":0}\n\n" +
			"data: {\"orphan\":true}\n\n",
	)
	s := NewSSEScanner(body)
	defer s.Close()

	ev1, err := s.Next()
	if err != nil || ev1.Event != "message_start" || string(ev1.Data) != `{"id":"msg_1"}` {
		t.Fatalf("frame 1: got Event=%q Data=%q err=%v", ev1.Event, ev1.Data, err)
	}
	ev2, err := s.Next()
	if err != nil || ev2.Event != "content_block_delta" || string(ev2.Data) != `{"index":0}` {
		t.Fatalf("frame 2: got Event=%q Data=%q err=%v", ev2.Event, ev2.Data, err)
	}
	ev3, err := s.Next()
	if err != nil {
		t.Fatalf("frame 3: err=%v", err)
	}
	if ev3.Event != "" {
		t.Errorf("frame 3: Event=%q, want empty — event name must reset between frames", ev3.Event)
	}
	if string(ev3.Data) != `{"orphan":true}` {
		t.Errorf("frame 3: Data=%q", ev3.Data)
	}
}

// TestSSEScanner_MultilineData asserts the SSE spec's multi-line "data:"
// rule: consecutive data lines in one frame join with '\n' between them.
// Some Vertex/Gemini SSE transports rely on this when a single JSON
// payload spans line wraps.
func TestSSEScanner_MultilineData(t *testing.T) {
	body := newBody("data: line-one\ndata: line-two\ndata: line-three\n\n")
	s := NewSSEScanner(body)
	defer s.Close()

	ev, err := s.Next()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(ev.Data) != "line-one\nline-two\nline-three" {
		t.Errorf("Data=%q, want newline-joined multi-line", ev.Data)
	}
}

// TestSSEScanner_SkipsCommentsAndExtraBlankLines asserts (a) comment
// lines starting with ':' (used as SSE heartbeats by some upstreams)
// are silently dropped and (b) a leading run of blank lines before the
// first event does not produce a spurious empty SSEEvent.
func TestSSEScanner_SkipsCommentsAndExtraBlankLines(t *testing.T) {
	body := newBody(
		"\n\n" + // leading blank lines — must not produce empty frame
			": keep-alive ping\n" + // comment line
			": another\n" +
			"data: payload\n\n",
	)
	s := NewSSEScanner(body)
	defer s.Close()

	ev, err := s.Next()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(ev.Data) != "payload" {
		t.Errorf("Data=%q, want payload", ev.Data)
	}
	if _, err := s.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("after only event: err=%v, want io.EOF", err)
	}
}

// TestSSEScanner_TrailingEventWithoutBlankLine asserts the
// "some providers don't emit a final blank line before closing" branch:
// when the underlying body EOFs with buffered data, that last frame
// must still be flushed before the next Next() returns io.EOF. Losing
// it here used to drop the last tool_call delta on Gemini.
func TestSSEScanner_TrailingEventWithoutBlankLine(t *testing.T) {
	body := newBody("data: final\n") // no trailing blank line
	s := NewSSEScanner(body)
	defer s.Close()

	ev, err := s.Next()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(ev.Data) != "final" {
		t.Errorf("Data=%q, want final", ev.Data)
	}
	if _, err := s.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("after trailing flush: err=%v, want io.EOF", err)
	}
}

// TestSSEScanner_TrailingEventNameOnly covers the s.event != "" branch
// of the trailing-flush logic — an SSE stream that ends after an
// "event: X" line but before any data line still yields an event with
// the captured name and empty Data, so callers see the partial frame.
func TestSSEScanner_TrailingEventNameOnly(t *testing.T) {
	body := newBody("event: ping\n")
	s := NewSSEScanner(body)
	defer s.Close()

	ev, err := s.Next()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ev.Event != "ping" || len(ev.Data) != 0 {
		t.Errorf("got Event=%q Data=%q, want Event=ping Data=empty", ev.Event, ev.Data)
	}
}

// TestSSEScanner_EmptyBodyReturnsEOF asserts an empty body yields
// io.EOF on first Next() — adapters use this to distinguish "stream
// closed before any frame" from "scan error".
func TestSSEScanner_EmptyBodyReturnsEOF(t *testing.T) {
	s := NewSSEScanner(newBody(""))
	defer s.Close()

	if _, err := s.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("err=%v, want io.EOF", err)
	}
}

// TestSSEScanner_PooledBufferReuse_NoStaleBytes drives many scanners
// sequentially against the shared buffer pool: a long first stream is fully
// consumed and Closed (returning its buffer), then a shorter second stream that
// reuses the recycled buffer must parse its OWN bytes exactly — never a residue
// of the longer first stream. This is the cross-request-leak guard for the
// per-stream buffer pooling; it fails if Close returns a buffer still aliased by
// emitted data, or if a recycled buffer exposes stale content.
func TestSSEScanner_PooledBufferReuse_NoStaleBytes(t *testing.T) {
	// First stream: long payloads to dirty the buffer well past the second's length.
	long := strings.Repeat("X", 4096)
	first := NewSSEScanner(newBody("data: " + long + "\n\ndata: " + long + "\n\n"))
	for i := range 2 {
		ev, err := first.Next()
		if err != nil {
			t.Fatalf("first stream event %d: %v", i, err)
		}
		if string(ev.Data) != long {
			t.Fatalf("first stream event %d corrupted", i)
		}
	}
	if _, err := first.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("first stream end: want EOF, got %v", err)
	}
	first.Close() // returns the (dirtied) buffer to the pool

	// Second stream: short, distinct payload. If pooling leaked the first
	// stream's bytes, this would read trailing 'X's after "short".
	second := NewSSEScanner(newBody("data: short\n\n"))
	defer second.Close()
	ev, err := second.Next()
	if err != nil {
		t.Fatalf("second stream: %v", err)
	}
	if string(ev.Data) != "short" {
		t.Fatalf("second stream Data=%q, want %q (stale bytes leaked from pool)", ev.Data, "short")
	}
	if _, err := second.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("second stream end: want EOF, got %v", err)
	}
}

// errReader returns a fixed error on Read so we can exercise the
// scanner.Err() path.
type errReader struct{ err error }

func (e errReader) Read(_ []byte) (int, error) { return 0, e.err }
func (errReader) Close() error                 { return nil }

// TestSSEScanner_PropagatesReadError asserts a non-EOF read error from
// the underlying body surfaces through Next() — callers MUST be able
// to distinguish "clean EOF" from "transport broke mid-stream" so that
// stream codecs report the right finish_reason.
func TestSSEScanner_PropagatesReadError(t *testing.T) {
	wantErr := errors.New("transport broke")
	s := NewSSEScanner(errReader{err: wantErr})
	defer s.Close()

	_, err := s.Next()
	if !errors.Is(err, wantErr) {
		t.Errorf("err=%v, want %v", err, wantErr)
	}
}

// TestSSEScanner_Close_Idempotent asserts Close is at-most-once on the
// underlying body and a follow-up Close after the body has already
// been released is a no-op (returns nil, no nil-pointer panic).
func TestSSEScanner_Close_Idempotent(t *testing.T) {
	b := newBody("data: x\n\n")
	s := NewSSEScanner(b)
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if b.closes != 1 {
		t.Errorf("body.closes=%d, want 1 after first Close", b.closes)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v, want nil (body already released)", err)
	}
	if b.closes != 1 {
		t.Errorf("body.closes=%d, want still 1 after second scanner Close", b.closes)
	}
}

// TestSSEScanner_Close_PropagatesBodyError asserts a Close error from
// the underlying body surfaces through SSEScanner.Close — adapters log
// this so a leaked TLS conn doesn't go unnoticed.
func TestSSEScanner_Close_PropagatesBodyError(t *testing.T) {
	wantErr := errors.New("close failed")
	b := newBody("")
	b.closeErr = wantErr
	s := NewSSEScanner(b)
	if err := s.Close(); !errors.Is(err, wantErr) {
		t.Errorf("Close err=%v, want %v", err, wantErr)
	}
}

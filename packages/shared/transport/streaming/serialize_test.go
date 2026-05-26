package streaming

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// failingWriter returns the configured error after writeBeforeErr successful
// writes. Used to exercise WriteSSEEvent's per-field error returns.
type failingWriter struct {
	written        int
	writeBeforeErr int
	err            error
}

func (f *failingWriter) Write(p []byte) (int, error) {
	if f.written >= f.writeBeforeErr {
		return 0, f.err
	}
	f.written++
	return len(p), nil
}

// TestWriteSSEEvent_DefaultMessageEvent_NoEventLine — when Event is "message"
// or empty, the serializer omits the `event:` line (SSE default).
func TestWriteSSEEvent_DefaultMessageEvent_NoEventLine(t *testing.T) {
	var out bytes.Buffer
	if err := WriteSSEEvent(&out, &SSEEvent{Event: "message", Data: "x", Retry: -1}); err != nil {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(out.String(), "event:") {
		t.Errorf("default 'message' event should not emit event: line, got %q", out.String())
	}

	out.Reset()
	if err := WriteSSEEvent(&out, &SSEEvent{Event: "", Data: "y", Retry: -1}); err != nil {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(out.String(), "event:") {
		t.Errorf("empty event should not emit event: line, got %q", out.String())
	}
}

// TestWriteSSEEvent_CustomEventLine — non-default event types ARE emitted.
func TestWriteSSEEvent_CustomEventLine(t *testing.T) {
	var out bytes.Buffer
	if err := WriteSSEEvent(&out, &SSEEvent{Event: "delta", Data: "x", Retry: -1}); err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(out.String(), "event: delta\n") {
		t.Errorf("expected 'event: delta\\n', got %q", out.String())
	}
}

// TestWriteSSEEvent_IDAndRetry — non-empty ID and non-negative Retry are
// emitted on their own lines.
func TestWriteSSEEvent_IDAndRetry(t *testing.T) {
	var out bytes.Buffer
	if err := WriteSSEEvent(&out, &SSEEvent{ID: "42", Retry: 3000, Data: "hello"}); err != nil {
		t.Fatalf("err = %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "id: 42\n") {
		t.Errorf("missing id line, got %q", s)
	}
	if !strings.Contains(s, "retry: 3000\n") {
		t.Errorf("missing retry line, got %q", s)
	}
	if !strings.Contains(s, "data: hello\n") {
		t.Errorf("missing data line, got %q", s)
	}
	if !strings.HasSuffix(s, "\n\n") {
		t.Errorf("event must terminate with blank line, got %q", s)
	}
}

// TestWriteSSEEvent_MultiLineData — embedded \n in Data must produce one
// `data:` line per fragment per the SSE spec.
func TestWriteSSEEvent_MultiLineData(t *testing.T) {
	var out bytes.Buffer
	if err := WriteSSEEvent(&out, &SSEEvent{Data: "line1\nline2\nline3", Retry: -1}); err != nil {
		t.Fatalf("err = %v", err)
	}
	want := "data: line1\ndata: line2\ndata: line3\n\n"
	if out.String() != want {
		t.Errorf("got %q, want %q", out.String(), want)
	}
}

// TestWriteSSEEvent_RetryNegativeOmitted — Retry == -1 (sentinel "not set")
// must NOT emit a retry: line.
func TestWriteSSEEvent_RetryNegativeOmitted(t *testing.T) {
	var out bytes.Buffer
	if err := WriteSSEEvent(&out, &SSEEvent{Data: "x", Retry: -1}); err != nil {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(out.String(), "retry:") {
		t.Errorf("Retry=-1 should suppress retry: line, got %q", out.String())
	}
}

// TestWriteSSEEvent_EventLineWriteError — io error while writing the
// `event:` line is propagated.
func TestWriteSSEEvent_EventLineWriteError(t *testing.T) {
	myErr := errors.New("event-line io fail")
	w := &failingWriter{writeBeforeErr: 0, err: myErr}
	err := WriteSSEEvent(w, &SSEEvent{Event: "custom", Data: "x", Retry: -1})
	if !errors.Is(err, myErr) {
		t.Errorf("err = %v, want wrapping %v", err, myErr)
	}
}

// TestWriteSSEEvent_IDLineWriteError — io error on the `id:` line is
// propagated even when the event line wrote OK.
func TestWriteSSEEvent_IDLineWriteError(t *testing.T) {
	myErr := errors.New("id-line io fail")
	// 1 successful write (event line), then fail on id.
	w := &failingWriter{writeBeforeErr: 1, err: myErr}
	err := WriteSSEEvent(w, &SSEEvent{Event: "custom", ID: "1", Data: "x", Retry: -1})
	if !errors.Is(err, myErr) {
		t.Errorf("err = %v, want wrapping %v", err, myErr)
	}
}

// TestWriteSSEEvent_RetryLineWriteError — io error on the `retry:` line
// is propagated.
func TestWriteSSEEvent_RetryLineWriteError(t *testing.T) {
	myErr := errors.New("retry-line io fail")
	// 2 successful writes (event + id), then fail on retry.
	w := &failingWriter{writeBeforeErr: 2, err: myErr}
	err := WriteSSEEvent(w, &SSEEvent{Event: "custom", ID: "1", Retry: 100, Data: "x"})
	if !errors.Is(err, myErr) {
		t.Errorf("err = %v, want wrapping %v", err, myErr)
	}
}

// TestWriteSSEEvent_DataLineWriteError — io error mid-Data-line is
// propagated.
func TestWriteSSEEvent_DataLineWriteError(t *testing.T) {
	myErr := errors.New("data-line io fail")
	// Only the data line is written when event="message" default, no id, no retry.
	w := &failingWriter{writeBeforeErr: 0, err: myErr}
	err := WriteSSEEvent(w, &SSEEvent{Data: "x", Retry: -1})
	if !errors.Is(err, myErr) {
		t.Errorf("err = %v, want wrapping %v", err, myErr)
	}
}

// TestWriteSSEEvent_TerminatorWriteError — io error on the final blank
// line is propagated.
func TestWriteSSEEvent_TerminatorWriteError(t *testing.T) {
	myErr := errors.New("terminator io fail")
	// event=default, no id, no retry → 1 successful write (data line) then
	// fail on the blank-line terminator.
	w := &failingWriter{writeBeforeErr: 1, err: myErr}
	err := WriteSSEEvent(w, &SSEEvent{Data: "x", Retry: -1})
	if !errors.Is(err, myErr) {
		t.Errorf("err = %v, want wrapping %v", err, myErr)
	}
}

// TestWriteSSEEvent_DiscardWriter_NoError — sanity check that the canonical
// io.Discard writer never errors.
func TestWriteSSEEvent_DiscardWriter_NoError(t *testing.T) {
	if err := WriteSSEEvent(io.Discard, &SSEEvent{Data: "x", Retry: -1}); err != nil {
		t.Errorf("Discard writer err = %v", err)
	}
}

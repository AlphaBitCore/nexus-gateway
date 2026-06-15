package format

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// failingWriter returns errFail after `okBeforeErr` successful Write calls.
type failingWriter struct {
	okBeforeErr int
	n           int
}

var errFail = errors.New("simulated write failure")

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.n >= w.okBeforeErr {
		return 0, errFail
	}
	w.n++
	return len(p), nil
}

// scanReadError force-triggers bufio.Scanner.Err so Parser.Next surfaces
// the underlying io.Reader error instead of EOF.
type scanReadError struct{}

var errScanBoom = errors.New("simulated scanner read error")

func (scanReadError) Read(_ []byte) (int, error) { return 0, errScanBoom }

// Leading empty lines (before any data or event field) must be silently
// skipped, not treated as event terminators.
func TestParser_SkipsLeadingBlankLines(t *testing.T) {
	input := "\n\n\ndata: hello\n\n"
	p := NewParser(strings.NewReader(input))
	evt, err := p.Next()
	if err != nil {
		t.Fatal(err)
	}
	if evt.Data != "hello" {
		t.Errorf("got data %q, want %q", evt.Data, "hello")
	}
}

// EOF reached after data lines but no trailing blank-line terminator
// must surface as a complete event, not be lost.
func TestParser_EOFWithUnterminatedEventSurfacesData(t *testing.T) {
	input := "event: msg\ndata: payload"
	p := NewParser(strings.NewReader(input))
	evt, err := p.Next()
	if err != nil {
		t.Fatal(err)
	}
	if evt.Type != "msg" || evt.Data != "payload" {
		t.Errorf("got type=%q data=%q, want type=msg data=payload", evt.Type, evt.Data)
	}
	// Now drains to EOF.
	_, err = p.Next()
	if !errors.Is(err, io.EOF) {
		t.Errorf("expected EOF on second call, got %v", err)
	}
}

// EOF after a single [DONE] data event sets Done=true on the trailing
// event (covers the EOF-with-accumulated branch).
func TestParser_EOFWithDoneSentinel(t *testing.T) {
	input := "data: [DONE]"
	p := NewParser(strings.NewReader(input))
	evt, err := p.Next()
	if err != nil {
		t.Fatal(err)
	}
	if !evt.Done {
		t.Errorf("expected Done=true on EOF-terminated [DONE], got %+v", evt)
	}
}

func TestParser_SurfacesScannerError(t *testing.T) {
	p := NewParser(scanReadError{})
	_, err := p.Next()
	if err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("expected non-EOF scanner error, got %v", err)
	}
	if !errors.Is(err, errScanBoom) {
		t.Errorf("expected wrapped errScanBoom, got %v", err)
	}
}

// WriteTypedEvent returns the io.Writer error when the `event:` field
// Fprintf fails (covers the first error-return arm).
func TestWriteTypedEvent_EventLineWriteError(t *testing.T) {
	w := &failingWriter{okBeforeErr: 0}
	err := WriteTypedEvent(w, "evt", "payload")
	if !errors.Is(err, errFail) {
		t.Errorf("expected errFail on event-line write, got %v", err)
	}
}

// WriteTypedEvent returns the io.Writer error when a `data:` line
// Fprintf fails after the event line wrote OK (covers the second
// error-return arm).
func TestWriteTypedEvent_DataLineWriteError(t *testing.T) {
	// 1 ok write (event:), then fail on first data: line.
	w := &failingWriter{okBeforeErr: 1}
	err := WriteTypedEvent(w, "evt", "payload")
	if !errors.Is(err, errFail) {
		t.Errorf("expected errFail on data-line write, got %v", err)
	}
}

// WriteError must surface the underlying writer error from the
// WriteEvent call (covers the early-return on err arm).
func TestWriteError_PropagatesWriterError(t *testing.T) {
	w := &failingWriter{okBeforeErr: 0}
	err := WriteError(w, "msg")
	if !errors.Is(err, errFail) {
		t.Errorf("expected errFail from underlying WriteEvent, got %v", err)
	}
}

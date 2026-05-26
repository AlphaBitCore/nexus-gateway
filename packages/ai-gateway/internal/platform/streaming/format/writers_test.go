package format

import (
	"bytes"
	"strings"
	"testing"
)

func TestWriteEvent(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteEvent(&buf, `{"test":true}`); err != nil {
		t.Fatal(err)
	}
	want := "data: {\"test\":true}\n\n"
	if buf.String() != want {
		t.Errorf("got %q, want %q", buf.String(), want)
	}
}

func TestWriteTypedEvent_EmitsEventLine(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteTypedEvent(&buf, "message_start", `{"type":"message_start"}`); err != nil {
		t.Fatal(err)
	}
	want := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n"
	if buf.String() != want {
		t.Errorf("got %q, want %q", buf.String(), want)
	}
}

func TestWriteTypedEvent_EmptyTypeOmitsLine(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteTypedEvent(&buf, "", `{"x":1}`); err != nil {
		t.Fatal(err)
	}
	want := "data: {\"x\":1}\n\n"
	if buf.String() != want {
		t.Errorf("got %q, want %q", buf.String(), want)
	}
}

func TestWriteDone(t *testing.T) {
	var buf bytes.Buffer
	_ = WriteDone(&buf)
	if buf.String() != "data: [DONE]\n\n" {
		t.Errorf("got %q", buf.String())
	}
}

func TestWriteError(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteError(&buf, "stream buffer exceeded maximum size"); err != nil {
		t.Fatal(err)
	}
	// WriteError emits a single SSE event with a JSON error payload
	// followed by the [DONE] marker — clients consuming the stream see
	// a terminal error frame and stop reading.
	out := buf.String()
	if !strings.Contains(out, "stream buffer exceeded maximum size") {
		t.Errorf("output missing error message: %q", out)
	}
	if !strings.Contains(out, "proxy_error") {
		t.Errorf("output missing proxy_error type tag: %q", out)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Errorf("output missing terminal [DONE] marker: %q", out)
	}
}

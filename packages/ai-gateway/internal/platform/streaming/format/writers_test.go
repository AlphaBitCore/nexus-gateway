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

// A `data:` value containing newlines must go out as one `data: ` line per
// line — an SSE client concatenates them back with the newlines restored, so
// emitting the raw value would terminate the frame at the first newline and
// silently truncate the payload. Providers send this for real: OpenAI-style
// error bodies and any pretty-printed JSON an upstream chooses to stream.
func TestWriteTypedEvent_MultiLineDataSplitsPerLine(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteTypedEvent(&buf, "error", "{\n  \"a\": 1\n}"); err != nil {
		t.Fatal(err)
	}
	want := "event: error\ndata: {\ndata:   \"a\": 1\ndata: }\n\n"
	if buf.String() != want {
		t.Errorf("got %q, want %q", buf.String(), want)
	}
}

// A trailing newline in the value yields a final empty `data:` line rather
// than being swallowed, because the SSE client re-joins the lines with "\n"
// and dropping it would change the payload the client reassembles.
func TestWriteTypedEvent_TrailingNewlineKeepsEmptyDataLine(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteTypedEvent(&buf, "", "x\n"); err != nil {
		t.Fatal(err)
	}
	want := "data: x\ndata: \n\n"
	if buf.String() != want {
		t.Errorf("got %q, want %q", buf.String(), want)
	}
}

// A frame wider than the pooled scratch buffer must still be written whole.
// The buffer grows via append for that frame and is not returned oversized,
// but correctness of the wire bytes is what a client sees.
func TestWriteTypedEvent_ValueLargerThanScratchBuffer(t *testing.T) {
	var buf bytes.Buffer
	big := strings.Repeat("z", 4096)
	if err := WriteTypedEvent(&buf, "", big); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "data: "+big+"\n\n" {
		t.Errorf("oversized frame not written verbatim: got %d bytes", buf.Len())
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

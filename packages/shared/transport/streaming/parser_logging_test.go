package streaming

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// These tests pin the C-14 / C-23 contract on the SSE parser's diagnostics.
//
// compliance-proxy and agent are transparent MITM interceptors: they carry
// arbitrary provider wires, and the SSE spec REQUIRES ignoring unrecognized
// fields. So an unrecognized field is normal traffic, not an error. Logging one
// line per occurrence turned normal traffic into a log storm, and because a
// line with no ':' separator makes the WHOLE LINE the field name, the old code
// wrote up to maxSSELineSize (1 MiB) of remotely-controlled bytes per
// occurrence — a log-amplification shape, and on a compliance/DLP product a
// path for raw prompt or model output to reach the logs.

// captureLogs returns a logger writing to buf at the given level.
func captureLogs(buf *bytes.Buffer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: level}))
}

// drain runs the parser to EOF, discarding events.
func drain(t *testing.T, p *SSEParser) {
	t.Helper()
	for {
		_, err := p.Next()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
	}
}

// TestSSEParser_UnknownField_NotLoggedAtDefaultLevel: unrecognized fields must
// be silent at production log level. They are spec-mandated-ignorable, so
// surfacing them at Warn/Info makes normal traffic look broken.
func TestSSEParser_UnknownField_NotLoggedAtDefaultLevel(t *testing.T) {
	var buf bytes.Buffer
	input := "x-provider-seq: 1\ndata: {\"a\":1}\n\nx-provider-seq: 2\ndata: {\"a\":2}\n\n"
	drain(t, NewSSEParserWithLogger(strings.NewReader(input), captureLogs(&buf, slog.LevelInfo)))

	if buf.Len() != 0 {
		t.Fatalf("unrecognized fields must be silent at Info level; got log output:\n%s", buf.String())
	}
}

// TestSSEParser_UnknownField_ReportedOncePerStream: at Debug the diagnostic is
// available, but collapsed to a single report no matter how many lines trip it.
func TestSSEParser_UnknownField_ReportedOncePerStream(t *testing.T) {
	var buf bytes.Buffer
	var sb strings.Builder
	for i := range 50 {
		sb.WriteString("x-provider-seq: ")
		sb.WriteString(string(rune('0' + i%10)))
		sb.WriteString("\ndata: {\"a\":1}\n\n")
	}
	drain(t, NewSSEParserWithLogger(strings.NewReader(sb.String()), captureLogs(&buf, slog.LevelDebug)))

	if got := strings.Count(buf.String(), "unrecognized field"); got != 1 {
		t.Fatalf("expected exactly 1 unrecognized-field report for 50 offending lines, got %d\n%s", got, buf.String())
	}
}

// TestSSEParser_SeparatorlessLine_NeverLogsContent is the C-23 safety contract:
// a line with no ':' must NOT have its content echoed into the log at any
// level, because that content is remotely controlled and may carry user prompt
// or model output.
func TestSSEParser_SeparatorlessLine_NeverLogsContent(t *testing.T) {
	const secret = "SSN-123-45-6789-should-never-be-logged"
	input := secret + "\ndata: {\"a\":1}\n\n"

	for _, level := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError} {
		var buf bytes.Buffer
		drain(t, NewSSEParserWithLogger(strings.NewReader(input), captureLogs(&buf, level)))
		if strings.Contains(buf.String(), secret) {
			t.Fatalf("level %v: separator-less line content leaked into the log:\n%s", level, buf.String())
		}
	}
}

// TestSSEParser_LongUnknownFieldName_Truncated: even a genuine (colon-bearing)
// field name is bounded before it reaches the log, so a hostile upstream cannot
// use the field-name slot as an amplification channel either.
func TestSSEParser_LongUnknownFieldName_Truncated(t *testing.T) {
	long := strings.Repeat("Z", 4096)
	var buf bytes.Buffer
	input := long + ": v\ndata: {\"a\":1}\n\n"
	drain(t, NewSSEParserWithLogger(strings.NewReader(input), captureLogs(&buf, slog.LevelDebug)))

	if strings.Contains(buf.String(), long) {
		t.Fatal("full 4096-char field name reached the log; it must be truncated")
	}
	if !strings.Contains(buf.String(), "unrecognized field") {
		t.Fatalf("expected the (truncated) diagnostic to still be emitted; got:\n%s", buf.String())
	}
	if got := len(buf.String()); got > 512 {
		t.Fatalf("log line is %d bytes; a bounded field name should keep it small", got)
	}
}

// TestSSEParser_KnownFieldsStillParsed guards against the diagnostic changes
// having altered parsing itself: the spec fields must still be honored, and an
// unrecognized field must still be IGNORED rather than becoming data.
func TestSSEParser_KnownFieldsStillParsed(t *testing.T) {
	input := "x-vendor: junk\nevent: content_block_delta\nid: 42\nretry: 250\ndata: hello\n\n"
	p := NewSSEParserWithLogger(strings.NewReader(input), captureLogs(&bytes.Buffer{}, slog.LevelDebug))
	evt, err := p.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if evt.Event != "content_block_delta" {
		t.Errorf("Event = %q, want content_block_delta", evt.Event)
	}
	if evt.ID != "42" {
		t.Errorf("ID = %q, want 42", evt.ID)
	}
	if evt.Retry != 250 {
		t.Errorf("Retry = %d, want 250", evt.Retry)
	}
	if evt.Data != "hello" {
		t.Errorf("Data = %q, want hello (the unrecognized field must not leak into data)", evt.Data)
	}
}

// TestSSEParser_InvalidRetry_ReportedOnceAndValueNotLogged: a malformed retry
// value is also attacker-influenced, so it is deduped and its value is not
// echoed.
func TestSSEParser_InvalidRetry_ReportedOnceAndValueNotLogged(t *testing.T) {
	const junk = "not-a-number-PROMPT-LEAK"
	var buf bytes.Buffer
	input := "retry: " + junk + "\nretry: " + junk + "\ndata: x\n\n"
	drain(t, NewSSEParserWithLogger(strings.NewReader(input), captureLogs(&buf, slog.LevelDebug)))

	if strings.Contains(buf.String(), junk) {
		t.Fatalf("malformed retry value leaked into the log:\n%s", buf.String())
	}
	if got := strings.Count(buf.String(), "invalid retry value"); got != 1 {
		t.Fatalf("expected exactly 1 invalid-retry report for 2 offending lines, got %d", got)
	}
}

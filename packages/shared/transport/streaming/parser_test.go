package streaming

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestSSEParser_BasicEvent(t *testing.T) {
	input := "data: hello\n\n"
	parser := NewSSEParser(strings.NewReader(input))

	evt, err := parser.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.Data != "hello" {
		t.Errorf("expected data='hello', got %q", evt.Data)
	}
	if evt.Event != "message" {
		t.Errorf("expected event='message', got %q", evt.Event)
	}
	if evt.Done {
		t.Error("expected Done=false")
	}

	// Should return EOF on next call.
	_, err = parser.Next()
	if !errors.Is(err, io.EOF) {
		t.Errorf("expected io.EOF, got %v", err)
	}
}

func TestSSEParser_MultiLineData(t *testing.T) {
	input := "data: line1\ndata: line2\ndata: line3\n\n"
	parser := NewSSEParser(strings.NewReader(input))

	evt, err := parser.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := "line1\nline2\nline3"
	if evt.Data != expected {
		t.Errorf("expected data=%q, got %q", expected, evt.Data)
	}
}

func TestSSEParser_EventType(t *testing.T) {
	input := "event: custom\ndata: payload\n\n"
	parser := NewSSEParser(strings.NewReader(input))

	evt, err := parser.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.Event != "custom" {
		t.Errorf("expected event='custom', got %q", evt.Event)
	}
	if evt.Data != "payload" {
		t.Errorf("expected data='payload', got %q", evt.Data)
	}
}

func TestSSEParser_Done(t *testing.T) {
	input := "data: [DONE]\n\n"
	parser := NewSSEParser(strings.NewReader(input))

	evt, err := parser.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !evt.Done {
		t.Error("expected Done=true")
	}
	if evt.Data != "[DONE]" {
		t.Errorf("expected data='[DONE]', got %q", evt.Data)
	}
}

func TestSSEParser_Comments(t *testing.T) {
	input := ": this is a comment\ndata: actual\n\n"
	parser := NewSSEParser(strings.NewReader(input))

	evt, err := parser.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.Data != "actual" {
		t.Errorf("expected data='actual', got %q", evt.Data)
	}
}

func TestSSEParser_EmptyData(t *testing.T) {
	input := "data:\n\n"
	parser := NewSSEParser(strings.NewReader(input))

	evt, err := parser.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.Data != "" {
		t.Errorf("expected empty data, got %q", evt.Data)
	}
}

func TestSSEParser_IDAndRetry(t *testing.T) {
	input := "id: 42\nretry: 3000\ndata: test\n\n"
	parser := NewSSEParser(strings.NewReader(input))

	evt, err := parser.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.ID != "42" {
		t.Errorf("expected id='42', got %q", evt.ID)
	}
	if evt.Retry != 3000 {
		t.Errorf("expected retry=3000, got %d", evt.Retry)
	}
	if evt.Data != "test" {
		t.Errorf("expected data='test', got %q", evt.Data)
	}
}

func TestSSEParser_MultipleEvents(t *testing.T) {
	input := "data: first\n\ndata: second\n\n"
	parser := NewSSEParser(strings.NewReader(input))

	evt1, err := parser.Next()
	if err != nil {
		t.Fatalf("unexpected error on first event: %v", err)
	}
	if evt1.Data != "first" {
		t.Errorf("expected first event data='first', got %q", evt1.Data)
	}

	evt2, err := parser.Next()
	if err != nil {
		t.Fatalf("unexpected error on second event: %v", err)
	}
	if evt2.Data != "second" {
		t.Errorf("expected second event data='second', got %q", evt2.Data)
	}
}

func TestSSEParser_TrailingDataWithoutBlankLine(t *testing.T) {
	// Stream ends without a trailing blank line — parser should still emit the event.
	input := "data: unterminated"
	parser := NewSSEParser(strings.NewReader(input))

	evt, err := parser.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if evt.Data != "unterminated" {
		t.Errorf("expected data='unterminated', got %q", evt.Data)
	}
}

func TestSSEParser_DataWithLeadingSpace(t *testing.T) {
	// Per SSE spec, one leading space after ':' is stripped.
	input := "data:  two spaces\n\n"
	parser := NewSSEParser(strings.NewReader(input))

	evt, err := parser.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// First space is stripped, second remains.
	if evt.Data != " two spaces" {
		t.Errorf("expected data=' two spaces', got %q", evt.Data)
	}
}

func TestSSEParser_EmptyStream(t *testing.T) {
	parser := NewSSEParser(strings.NewReader(""))
	_, err := parser.Next()
	if !errors.Is(err, io.EOF) {
		t.Errorf("expected io.EOF, got %v", err)
	}
}

func TestSSEParser_OnlyComments(t *testing.T) {
	input := ": comment1\n: comment2\n\n"
	parser := NewSSEParser(strings.NewReader(input))

	// Comments with a blank line but no data fields → skip.
	_, err := parser.Next()
	if !errors.Is(err, io.EOF) {
		t.Errorf("expected io.EOF for comment-only stream, got %v", err)
	}
}

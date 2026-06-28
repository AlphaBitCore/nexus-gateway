package format

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// TestParser_PooledBufferReuse_NoStaleBytes runs a long stream to completion,
// Releases its buffer, then parses a short distinct stream that reuses the
// recycled buffer — which must yield only its OWN bytes, never a residue of the
// longer first stream. Cross-stream-leak guard for the per-stream buffer pool.
func TestParser_PooledBufferReuse_NoStaleBytes(t *testing.T) {
	long := strings.Repeat("Z", 4096)
	p1 := NewParser(strings.NewReader("data: " + long + "\n\ndata: [DONE]\n\n"))
	ev, err := p1.Next()
	if err != nil || ev.Data != long {
		t.Fatalf("first stream: data corrupted (err=%v)", err)
	}
	if ev2, _ := p1.Next(); !ev2.Done {
		t.Fatalf("first stream: expected DONE")
	}
	p1.Release() // returns the dirtied buffer to the pool

	p2 := NewParser(strings.NewReader("data: short\n\n"))
	defer p2.Release()
	ev, err = p2.Next()
	if err != nil {
		t.Fatalf("second stream: %v", err)
	}
	if ev.Data != "short" {
		t.Fatalf("second stream Data=%q, want %q (stale bytes leaked from pool)", ev.Data, "short")
	}
}

func TestParser_BasicEvents(t *testing.T) {
	input := "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\ndata: [DONE]\n\n"
	p := NewParser(strings.NewReader(input))

	evt1, err := p.Next()
	if err != nil {
		t.Fatal(err)
	}
	if evt1.Done || !strings.Contains(evt1.Data, "Hello") {
		t.Errorf("event 1: %+v", evt1)
	}

	evt2, err := p.Next()
	if err != nil {
		t.Fatal(err)
	}
	if evt2.Done || !strings.Contains(evt2.Data, "world") {
		t.Errorf("event 2: %+v", evt2)
	}

	evt3, err := p.Next()
	if err != nil {
		t.Fatal(err)
	}
	if !evt3.Done {
		t.Error("event 3 should be DONE")
	}

	_, err = p.Next()
	if !errors.Is(err, io.EOF) {
		t.Errorf("expected EOF, got %v", err)
	}
}

func TestParser_SkipsComments(t *testing.T) {
	input := ": this is a comment\ndata: hello\n\n"
	p := NewParser(strings.NewReader(input))
	evt, err := p.Next()
	if err != nil {
		t.Fatal(err)
	}
	if evt.Data != "hello" {
		t.Errorf("got %q", evt.Data)
	}
}

func TestParser_MultiLineData(t *testing.T) {
	input := "data: line1\ndata: line2\n\n"
	p := NewParser(strings.NewReader(input))
	evt, err := p.Next()
	if err != nil {
		t.Fatal(err)
	}
	if evt.Data != "line1\nline2" {
		t.Errorf("got %q", evt.Data)
	}
}

func TestParser_EmptyStream(t *testing.T) {
	p := NewParser(strings.NewReader(""))
	_, err := p.Next()
	if !errors.Is(err, io.EOF) {
		t.Errorf("expected EOF, got %v", err)
	}
}

// TestParser_PreservesEventField pins the SSE event-line passthrough
// regression: pre-fix Parser.Next discarded `event:` lines, so when
// the gateway re-emitted via WriteEvent the client got `data:`-only
// frames. Anthropic SDK / Claude Code dispatch on the `event:` line
// for typed handlers and rendered nothing. Fix is in two halves —
// Parser captures Type, WriteTypedEvent emits it back.
func TestParser_PreservesEventField(t *testing.T) {
	input := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"Hi\"}}\n\n" +
		"data: [DONE]\n\n"
	p := NewParser(strings.NewReader(input))

	evt1, err := p.Next()
	if err != nil {
		t.Fatal(err)
	}
	if evt1.Type != "message_start" {
		t.Errorf("event 1 Type: want %q, got %q", "message_start", evt1.Type)
	}
	if !strings.Contains(evt1.Data, "message_start") {
		t.Errorf("event 1 Data: %q", evt1.Data)
	}

	evt2, err := p.Next()
	if err != nil {
		t.Fatal(err)
	}
	if evt2.Type != "content_block_delta" {
		t.Errorf("event 2 Type: want %q, got %q", "content_block_delta", evt2.Type)
	}
	if !strings.Contains(evt2.Data, "Hi") {
		t.Errorf("event 2 Data: %q", evt2.Data)
	}

	evt3, err := p.Next()
	if err != nil {
		t.Fatal(err)
	}
	if !evt3.Done {
		t.Errorf("event 3 should be DONE: %+v", evt3)
	}
}

// TestParser_RoundTripWithWriteTypedEvent confirms Parser.Next +
// WriteTypedEvent are inverses, so a passthrough proxy preserves the
// bit-for-bit semantics Anthropic SDK relies on.
func TestParser_RoundTripWithWriteTypedEvent(t *testing.T) {
	input := "event: message_start\ndata: {\"a\":1}\n\nevent: content_block_delta\ndata: {\"a\":2}\n\n"
	p := NewParser(strings.NewReader(input))
	var out bytes.Buffer
	for {
		evt, err := p.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if evt.Done {
			break
		}
		if err := WriteTypedEvent(&out, evt.Type, evt.Data); err != nil {
			t.Fatal(err)
		}
	}
	if out.String() != input {
		t.Errorf("round-trip mismatch:\n got %q\nwant %q", out.String(), input)
	}
}

package streaming

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// Correctness gate for the pooled scan buffer (finding C-12). The risk pooling
// introduces is not a wrong answer on the happy path — it is one stream observing
// another stream's bytes after the buffer is recycled. On a compliance product that
// would splice one tenant's payload into another's audit record, so the tests below
// target that specifically rather than only checking that parsing still works.

// TestSSEParser_RecycledBufferCannotBleedBetweenStreams is the load-bearing test.
// It runs a stream carrying a distinctive secret, releases the parser so the buffer
// goes back to the pool, then runs a SHORTER stream through a parser that will very
// likely be handed the same buffer, and requires that none of the first stream's
// bytes appear in the second stream's events.
func TestSSEParser_RecycledBufferCannotBleedBetweenStreams(t *testing.T) {
	const secret = "SECRET-CANARY-9f3a7c21-DO-NOT-LEAK"

	first := "data: {\"choices\":[{\"delta\":{\"content\":\"" + secret + "\"}}]}\n\n" +
		"data: [DONE]\n\n"
	p1 := NewSSEParser(strings.NewReader(first))
	sawSecret := false
	for {
		evt, err := p1.Next()
		if err != nil {
			break
		}
		if strings.Contains(evt.Data, secret) {
			sawSecret = true
		}
	}
	p1.Release()
	if !sawSecret {
		t.Fatal("first stream never yielded the canary — the test would prove nothing about bleed")
	}

	// Deliberately much shorter, so any residue from the first stream would sit
	// beyond this stream's own content in a shared buffer.
	second := "data: short\n\n"
	for range 8 { // several parsers, to make reuse of the recycled buffer near-certain
		p2 := NewSSEParser(strings.NewReader(second))
		for {
			evt, err := p2.Next()
			if err != nil {
				break
			}
			if strings.Contains(evt.Data, secret) {
				t.Fatalf("recycled buffer bled the previous stream's bytes into %q", evt.Data)
			}
			if strings.Contains(evt.Event, secret) || strings.Contains(evt.ID, secret) {
				t.Fatalf("recycled buffer bled the previous stream's bytes into event=%q id=%q", evt.Event, evt.ID)
			}
			if evt.Data != "short" {
				t.Errorf("second stream data = %q, want %q", evt.Data, "short")
			}
		}
		p2.Release()
	}
}

// TestSSEParser_ReleaseIsIdempotent pins that a double Release cannot hand the same
// slice to two streams at once — which is the way a pool turns into a data race.
func TestSSEParser_ReleaseIsIdempotent(t *testing.T) {
	p := NewSSEParser(strings.NewReader("data: x\n\n"))
	if _, err := p.Next(); err != nil {
		t.Fatalf("Next: %v", err)
	}
	p.Release()
	p.Release() // must be a no-op, not a second Put of the same buffer
	p.Release()
}

// TestSSEParser_NextAfterReleaseFailsLoudly pins the chosen failure mode. Reading
// after Release would scan a buffer that may already belong to another stream, so it
// must not silently succeed and must not silently look like end-of-stream either.
func TestSSEParser_NextAfterReleaseFailsLoudly(t *testing.T) {
	p := NewSSEParser(strings.NewReader("data: a\n\ndata: b\n\n"))
	if _, err := p.Next(); err != nil {
		t.Fatalf("first Next: %v", err)
	}
	p.Release()

	evt, err := p.Next()
	if !errors.Is(err, ErrParserReleased) {
		t.Errorf("Next after Release: err = %v, want ErrParserReleased", err)
	}
	if errors.Is(err, io.EOF) {
		t.Error("Next after Release returned io.EOF — that is indistinguishable from a clean end of stream")
	}
	if evt != nil {
		t.Errorf("Next after Release returned an event: %+v", evt)
	}
}

// TestSSEParser_OversizeFrameStillReturnsThePristineBuffer pins the invariant the
// pool's safety rests on: when a frame exceeds 64 KiB, bufio abandons our slice and
// allocates its own, and the handle we return must still be the original 64 KiB one.
// If the handle were ever replaced by the grown buffer, the pool would slowly fill
// with multi-hundred-KiB slices and the memory saving would invert.
func TestSSEParser_OversizeFrameStillReturnsThePristineBuffer(t *testing.T) {
	big := strings.Repeat("x", 200*1024) // > 64 KiB, < maxSSELineSize
	p := NewSSEParser(strings.NewReader("data: " + big + "\n\n"))

	handleBefore := p.bufp
	capBefore := cap(*handleBefore)

	evt, err := p.Next()
	if err != nil {
		t.Fatalf("Next on an oversize frame: %v", err)
	}
	if len(evt.Data) != len(big) {
		t.Fatalf("oversize frame truncated: got %d bytes, want %d", len(evt.Data), len(big))
	}
	if p.bufp != handleBefore {
		t.Error("the pooled handle was replaced while scanning an oversize frame")
	}
	if got := cap(*p.bufp); got != capBefore {
		t.Errorf("pooled buffer capacity changed from %d to %d — the grown buffer would be returned to the pool", capBefore, got)
	}
	p.Release()
}

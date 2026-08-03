package streaming

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// Correctness gate for the fmt-free WriteSSEEvent (finding C-21). The claim is
// byte-identical output at lower cost, so the test does not restate what the wire
// should look like — it runs the ORIGINAL implementation beside the current one and
// requires the bytes to match exactly. Anything else would encode my belief about
// SSE framing rather than the framing that shipped.

// writeSSEEventReference is verbatim the fmt-based implementation this replaced.
// Oracle only; do not "improve" it. If a future change is meant to alter the wire
// rather than only its cost, this reference must change in the same commit and the
// intent be stated there.
func writeSSEEventReference(w io.Writer, evt *SSEEvent) error {
	if evt.Event != "" && evt.Event != "message" {
		if _, err := fmt.Fprintf(w, "event: %s\n", evt.Event); err != nil {
			return err
		}
	}
	if evt.ID != "" {
		if _, err := fmt.Fprintf(w, "id: %s\n", evt.ID); err != nil {
			return err
		}
	}
	if evt.Retry >= 0 {
		if _, err := fmt.Fprintf(w, "retry: %d\n", evt.Retry); err != nil {
			return err
		}
	}
	for _, line := range strings.Split(evt.Data, "\n") {
		if _, err := fmt.Fprintf(w, "data: %s\n", line); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprint(w, "\n"); err != nil {
		return err
	}
	return nil
}

func serializeCorpus() []*SSEEvent {
	return []*SSEEvent{
		{Event: "message", Data: `{"choices":[{"delta":{"content":"hi"}}]}`, Retry: -1},
		{Event: "", Data: "plain", Retry: -1},
		// Event names that must and must not be emitted.
		{Event: "content_block_delta", Data: "x", Retry: -1},
		{Event: "message", Data: "x", Retry: -1}, // "message" is suppressed
		// ID and retry, including the boundary where retry becomes emittable.
		{Event: "", ID: "42", Data: "x", Retry: -1},
		{Event: "", Data: "x", Retry: 0},
		{Event: "", Data: "x", Retry: 3000},
		{Event: "e", ID: "i", Data: "d", Retry: 7},
		// Multi-line data: each line needs its own prefix.
		{Data: "a\nb", Retry: -1},
		{Data: "a\nb\nc", Retry: -1},
		// Degenerate newline placement — the cases an IndexByte walk can get wrong.
		{Data: "", Retry: -1},
		{Data: "\n", Retry: -1},
		{Data: "\n\n", Retry: -1},
		{Data: "a\n", Retry: -1},
		{Data: "\na", Retry: -1},
		{Data: "a\n\nb", Retry: -1},
		// Content that looks like SSE framing, which must not be re-interpreted.
		{Data: "data: nested", Retry: -1},
		{Data: "event: nested\ndata: more", Retry: -1},
		// The [DONE] sentinel and a done-flagged event.
		{Data: "[DONE]", Done: true, Retry: -1},
		// Bytes that are not valid UTF-8 must pass through unchanged.
		{Data: "\xff\xfe binary", Retry: -1},
		{Data: "emoji 😀 and CJK 密码", Retry: -1},
		// A frame larger than the pooled scratch, so the grow path is exercised.
		{Data: strings.Repeat("x", 8192), Retry: -1},
		{Data: strings.Repeat("y\n", 3000), Retry: -1},
	}
}

// TestWriteSSEEvent_ByteIdenticalToReference is the primary C-21 correctness proof.
func TestWriteSSEEvent_ByteIdenticalToReference(t *testing.T) {
	for i, evt := range serializeCorpus() {
		var got, want bytes.Buffer
		if err := WriteSSEEvent(&got, evt); err != nil {
			t.Fatalf("event %d: WriteSSEEvent: %v", i, err)
		}
		if err := writeSSEEventReference(&want, evt); err != nil {
			t.Fatalf("event %d: reference: %v", i, err)
		}
		if got.String() != want.String() {
			t.Errorf("event %d (%+v):\n got  %q\n want %q", i, evt, got.String(), want.String())
		}
	}
}

// TestWriteSSEEvent_OversizeFrameDoesNotInflateThePool pins the pool hygiene rule:
// a frame that outgrows the scratch makes append allocate a fresh array, and keeping
// that would let one oversized frame permanently enlarge every later frame's buffer.
func TestWriteSSEEvent_OversizeFrameDoesNotInflateThePool(t *testing.T) {
	// Drain whatever is pooled so the capacity observed below is the New() one.
	first := sseWireBufPool.Get().(*[]byte)
	baseCap := cap(*first)
	sseWireBufPool.Put(first)

	var sink bytes.Buffer
	// A single data line several times the scratch size, so append must grow.
	if err := WriteSSEEvent(&sink, &SSEEvent{Data: strings.Repeat("z", baseCap*4), Retry: -1}); err != nil {
		t.Fatalf("oversize write: %v", err)
	}
	if sink.Len() < baseCap*4 {
		t.Fatalf("oversize frame relayed only %d bytes — the fixture no longer outgrows the scratch", sink.Len())
	}

	// Every buffer reachable from the pool must still be the original size. Sample
	// generously: sync.Pool is per-P, so one Get can miss the poisoned entry.
	for i := range 64 {
		bp := sseWireBufPool.Get().(*[]byte)
		if got := cap(*bp); got > baseCap {
			t.Fatalf("sample %d: pooled buffer grew to %d (base %d) — an oversized frame inflated the pool", i, got, baseCap)
		}
		sseWireBufPool.Put(bp)
	}
}

// TestWriteSSEEvent_WriteErrorPropagates pins that a failing writer is reported and
// not swallowed by the buffering — the relay closes the stream on this error.
func TestWriteSSEEvent_WriteErrorPropagates(t *testing.T) {
	want := io.ErrClosedPipe
	err := WriteSSEEvent(alwaysFailWriter{want}, &SSEEvent{Data: "x", Retry: -1})
	if !errors.Is(err, want) {
		t.Errorf("WriteSSEEvent on a failing writer: err = %v, want %v", err, want)
	}
}

type alwaysFailWriter struct{ err error }

func (e alwaysFailWriter) Write([]byte) (int, error) { return 0, e.err }

// FuzzWriteSSEEvent_ByteIdenticalToReference covers the framing the corpus did not
// think of — chiefly newline and prefix placement inside the data payload.
func FuzzWriteSSEEvent_ByteIdenticalToReference(f *testing.F) {
	for _, evt := range serializeCorpus() {
		f.Add(evt.Event, evt.ID, evt.Data, evt.Retry)
	}
	f.Fuzz(func(t *testing.T, event, id, data string, retry int) {
		evt := &SSEEvent{Event: event, ID: id, Data: data, Retry: retry}
		var got, want bytes.Buffer
		if err := WriteSSEEvent(&got, evt); err != nil {
			t.Fatalf("WriteSSEEvent: %v", err)
		}
		if err := writeSSEEventReference(&want, evt); err != nil {
			t.Fatalf("reference: %v", err)
		}
		if got.String() != want.String() {
			t.Fatalf("divergence on event=%q id=%q data=%q retry=%d:\n got  %q\n want %q",
				event, id, data, retry, got.String(), want.String())
		}
	})
}

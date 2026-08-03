package format

import (
	"github.com/goccy/go-json"
	"io"
	"strings"
	"sync"
)

// sseWireBufPool holds the scratch line buffer WriteTypedEvent assembles each
// SSE line into. One frame is a handful of short writes, so the buffer is small
// and its whole purpose is to keep those writes off the heap.
var sseWireBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 512)
		return &b
	},
}

// WriteEvent writes an SSE event with no `event:` field to the writer.
// Use WriteTypedEvent when the upstream supplied a typed event name
// (Anthropic always does); typed clients (Claude Code, Anthropic SDK)
// dispatch on the `event:` line and silently drop frames that arrive
// without one.
func WriteEvent(w io.Writer, data string) error {
	return WriteTypedEvent(w, "", data)
}

// WriteTypedEvent writes an SSE frame preserving the upstream
// `event:` field. Empty eventType skips the line so the wire format
// matches OpenAI-style "event-less" streams. Multi-line `data:`
// values are split per the SSE spec.
//
// This runs on every frame of every inspected stream, on the delivery path, so
// it assembles each line into a pooled buffer rather than going through fmt:
// fmt.Fprintf boxes its arguments into an []any on every call, and
// strings.Split allocates a []string per frame even for the single-line data
// that is nearly all real traffic. packages/shared/transport/streaming's
// WriteSSEEvent is the same technique; the two are separate because their
// callers pass different shapes, not because the cost differs.
//
// The number of Write calls is deliberately unchanged — one per line plus one
// for the terminating blank line. Coalescing a frame into a single Write is
// fewer syscalls but a different observable, and the round-trip tests below
// depend on the current one.
func WriteTypedEvent(w io.Writer, eventType, data string) error {
	// bufp is the pooled handle and is always what goes back to the pool.
	// scratch is the working slice and may be replaced by append if a line
	// outgrows it; that grown array is dropped rather than returned, so one
	// oversized frame cannot permanently inflate the pool.
	bufp := sseWireBufPool.Get().(*[]byte)
	defer sseWireBufPool.Put(bufp)
	scratch := (*bufp)[:0]

	writeLine := func(prefix, value string) error {
		scratch = append(scratch[:0], prefix...)
		scratch = append(scratch, value...)
		scratch = append(scratch, '\n')
		_, err := w.Write(scratch)
		return err
	}

	if eventType != "" {
		if err := writeLine("event: ", eventType); err != nil {
			return err
		}
	}

	// Walked with IndexByte rather than strings.Split so no []string is built,
	// and the single-line case never looks for a second line at all.
	rest := data
	for {
		i := strings.IndexByte(rest, '\n')
		if i < 0 {
			if err := writeLine("data: ", rest); err != nil {
				return err
			}
			break
		}
		if err := writeLine("data: ", rest[:i]); err != nil {
			return err
		}
		rest = rest[i+1:]
	}

	// io.WriteString hands a constant to writers that implement StringWriter —
	// http.ResponseWriter and bytes.Buffer both do — so it does not allocate.
	_, err := io.WriteString(w, "\n")
	return err
}

// WriteDone writes the [DONE] marker. io.WriteString rather than fmt for the
// same reason as above: this is a constant, and fmt would box it.
func WriteDone(w io.Writer) error {
	_, err := io.WriteString(w, "data: [DONE]\n\n")
	return err
}

// WriteError writes an error as an SSE event followed by [DONE].
func WriteError(w io.Writer, message string) error {
	errJSON, _ := json.Marshal(map[string]any{
		"error": map[string]string{
			"message": message,
			"type":    "proxy_error",
		},
	})
	if err := WriteEvent(w, string(errJSON)); err != nil {
		return err
	}
	return WriteDone(w)
}

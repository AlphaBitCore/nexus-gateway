package streaming

import (
	"io"
	"strconv"
	"strings"
	"sync"
)

// sseWireBufPool holds the scratch each SSE line is formatted into before being
// written. Holding *[]byte rather than []byte keeps Get/Put from boxing a slice
// header per frame.
var sseWireBufPool = sync.Pool{New: func() any { b := make([]byte, 0, 4096); return &b }}

// WriteSSEEvent serializes an SSEEvent to the given writer in SSE wire format.
//
// Output is byte-identical to the fmt-based implementation this replaced, and so is
// the WRITE GRANULARITY: one Write per logical line plus one for the terminating
// blank line. Both are pinned — serialize_equivalence_test.go diffs the bytes
// against a verbatim copy of the old implementation, and the pre-existing
// failingWriter tests in serialize_test.go depend on the per-line write count. That
// second set is how an earlier version of this change, which coalesced the whole
// frame into a single Write, was caught: fewer syscalls, but it silently changed an
// observable the suite already protected.
//
// What changed is only the cost. fmt.Fprintf boxed its arguments into an []any on
// every call and strings.Split allocated a []string per frame even for the
// single-line data that is nearly all real traffic. Both are gone. This runs on
// every frame of every inspected stream, on the delivery path.
func WriteSSEEvent(w io.Writer, evt *SSEEvent) error {
	// bufp is the pooled handle and is always what goes back to the pool. scratch is
	// the working slice and may be replaced by append if a line outgrows it; that
	// grown array is simply dropped at the end of the event. Same invariant the
	// pooled SSE scan buffer uses: returning a grown array would let one oversized
	// frame permanently inflate the pool for every later frame.
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

	if evt.Event != "" && evt.Event != "message" {
		if err := writeLine("event: ", evt.Event); err != nil {
			return err
		}
	}
	if evt.ID != "" {
		if err := writeLine("id: ", evt.ID); err != nil {
			return err
		}
	}
	if evt.Retry >= 0 {
		scratch = append(scratch[:0], "retry: "...)
		scratch = strconv.AppendInt(scratch, int64(evt.Retry), 10)
		scratch = append(scratch, '\n')
		if _, err := w.Write(scratch); err != nil {
			return err
		}
	}

	// Data may contain multiple lines; each carries its own "data: " prefix. Walked
	// with IndexByte rather than strings.Split so no []string is built, and the
	// single-line case never looks for a second line at all.
	rest := evt.Data
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

	// Blank line terminates the event. io.WriteString hands a constant to writers
	// that implement StringWriter (http.ResponseWriter and bytes.Buffer both do), so
	// it does not allocate.
	if _, err := io.WriteString(w, "\n"); err != nil {
		return err
	}
	return nil
}

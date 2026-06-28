package specutil

import (
	"bufio"
	"bytes"
	"io"
	"sync"
)

// SSE field prefixes, kept as package-level byte slices so the hot Next() loop
// matches them with bytes.HasPrefix against the scanner's zero-copy Bytes()
// without re-allocating the literals each line.
var (
	sseEventPrefix = []byte("event:")
	sseDataPrefix  = []byte("data:")
)

// sseScanBufPool recycles the 64 KiB initial scanner buffer that every
// NewSSEScanner allocated fresh per stream — one of the largest per-stream
// allocations on the SSE hot path (~13.6 GB/window under streaming load).
//
// The pooled value is the ORIGINAL 64 KiB slice we hand to bufio.Scanner.Buffer.
// If a frame exceeds 64 KiB the scanner allocates its own larger buffer and
// abandons ours; our handle still references the pristine 64 KiB slice, so it is
// always safe (and correctly sized) to return on Close. Every event SSEScanner
// emits is bytes.Clone'd, so no returned data aliases the buffer after Close —
// the recycled buffer can never leak one stream's bytes into another.
var sseScanBufPool = sync.Pool{New: func() any { b := make([]byte, 64*1024); return &b }}

// SSEEvent is one parsed Server-Sent-Event frame. Event is the
// provider's event name (empty for providers that only emit data
// lines). Data is the raw payload bytes with the `data:` prefix
// stripped but newlines between multi-line data lines preserved.
type SSEEvent struct {
	Event string
	Data  []byte
}

// SSEScanner is a cursor over an io.ReadCloser producing SSEEvents
// one at a time. Close propagates to the underlying body. Intended
// for use inside StreamDecoder implementations so every provider
// shares the same wire parser.
type SSEScanner struct {
	body    io.ReadCloser
	scanner *bufio.Scanner
	bufp    *[]byte // pooled 64 KiB scanner buffer handle; returned on Close

	// buf accumulates the current event's lines until the blank-line
	// separator flushes it into an SSEEvent.
	event string
	data  *bytes.Buffer
}

// NewSSEScanner wraps body. The caller is responsible for closing it
// via SSEScanner.Close when done (successful EOF or early abort) — Close
// also returns the pooled scanner buffer for reuse.
func NewSSEScanner(body io.ReadCloser) *SSEScanner {
	bufp := sseScanBufPool.Get().(*[]byte)
	s := bufio.NewScanner(body)
	// SSE frames can exceed the default 64 KiB token limit on providers
	// that emit large JSON payloads (Anthropic tool_use blocks).
	s.Buffer(*bufp, 4*1024*1024)
	return &SSEScanner{body: body, scanner: s, bufp: bufp, data: &bytes.Buffer{}}
}

// Next returns the next SSE event. It returns io.EOF at end of stream.
// Comment lines (starting with ':') are skipped per the SSE spec.
func (s *SSEScanner) Next() (SSEEvent, error) {
	for s.scanner.Scan() {
		// Bytes() returns a slice into the scanner buffer, valid only until the
		// next Scan(). Everything we retain (data → s.data buffer, event name →
		// string) is copied out before the next iteration, so aliasing is safe
		// and the per-line Text() string allocation is eliminated.
		line := s.scanner.Bytes()

		if len(line) == 0 {
			if s.data.Len() == 0 && s.event == "" {
				continue
			}
			ev := SSEEvent{
				Event: s.event,
				Data:  bytes.Clone(bytes.TrimRight(s.data.Bytes(), "\n")),
			}
			s.event = ""
			s.data.Reset()
			return ev, nil
		}

		if line[0] == ':' {
			continue
		}

		switch {
		case bytes.HasPrefix(line, sseEventPrefix):
			s.event = string(bytes.TrimSpace(line[len(sseEventPrefix):]))
		case bytes.HasPrefix(line, sseDataPrefix):
			if s.data.Len() > 0 {
				s.data.WriteByte('\n')
			}
			v := line[len(sseDataPrefix):]
			if len(v) > 0 && v[0] == ' ' {
				v = v[1:]
			}
			s.data.Write(v)
		}
	}

	if err := s.scanner.Err(); err != nil {
		return SSEEvent{}, err
	}

	// Flush any trailing event (some providers do not emit the final
	// blank line before closing the connection).
	if s.data.Len() > 0 || s.event != "" {
		ev := SSEEvent{
			Event: s.event,
			Data:  bytes.Clone(bytes.TrimRight(s.data.Bytes(), "\n")),
		}
		s.event = ""
		s.data.Reset()
		return ev, nil
	}

	return SSEEvent{}, io.EOF
}

// Close releases the underlying body and returns the pooled scanner buffer.
// Both are at-most-once: a second Close is a no-op (no double-Put, no
// nil-body close).
func (s *SSEScanner) Close() error {
	if s.bufp != nil {
		sseScanBufPool.Put(s.bufp)
		s.bufp = nil
	}
	if s.body == nil {
		return nil
	}
	err := s.body.Close()
	s.body = nil
	return err
}

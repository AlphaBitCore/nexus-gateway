package specutil

import (
	"bufio"
	"bytes"
	"io"
	"strings"
)

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

	// buf accumulates the current event's lines until the blank-line
	// separator flushes it into an SSEEvent.
	event string
	data  *bytes.Buffer
}

// NewSSEScanner wraps body. The caller is responsible for closing it
// via SSEScanner.Close when done (successful EOF or early abort).
func NewSSEScanner(body io.ReadCloser) *SSEScanner {
	s := bufio.NewScanner(body)
	// SSE frames can exceed the default 64 KiB token limit on providers
	// that emit large JSON payloads (Anthropic tool_use blocks).
	s.Buffer(make([]byte, 64*1024), 4*1024*1024)
	return &SSEScanner{body: body, scanner: s, data: &bytes.Buffer{}}
}

// Next returns the next SSE event. It returns io.EOF at end of stream.
// Comment lines (starting with ':') are skipped per the SSE spec.
func (s *SSEScanner) Next() (SSEEvent, error) {
	for s.scanner.Scan() {
		line := s.scanner.Text()

		if line == "" {
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

		if strings.HasPrefix(line, ":") {
			continue
		}

		switch {
		case strings.HasPrefix(line, "event:"):
			s.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if s.data.Len() > 0 {
				s.data.WriteByte('\n')
			}
			s.data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
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

// Close releases the underlying body.
func (s *SSEScanner) Close() error {
	if s.body == nil {
		return nil
	}
	err := s.body.Close()
	s.body = nil
	return err
}

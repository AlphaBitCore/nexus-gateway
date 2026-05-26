// Package streaming implements SSE stream processing with checkpoint-based
// compliance hooks for the Nexus Gateway compliance-proxy.
package streaming

import (
	"bufio"
	"io"
	"log/slog"
	"strconv"
	"strings"
)

// SSEEvent represents a single Server-Sent Event.
type SSEEvent struct {
	Event string // event type (default "message")
	Data  string // event data (may span multiple lines)
	ID    string // event ID
	Retry int    // reconnect time in ms (-1 if not set)
	Done  bool   // true if this is a [DONE] marker
}

// maxSSELineSize is the maximum size of a single SSE line. 1MB handles
// base64-encoded images and large tool-call results from AI providers.
const maxSSELineSize = 1024 * 1024 // 1 MB

// SSEParser reads SSE events from an io.Reader.
type SSEParser struct {
	scanner *bufio.Scanner
	logger  *slog.Logger
}

// NewSSEParser creates a parser for the given reader.
func NewSSEParser(r io.Reader) *SSEParser {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64*1024), maxSSELineSize)
	return &SSEParser{
		scanner: s,
		logger:  slog.Default(),
	}
}

// NewSSEParserWithLogger creates a parser with a custom logger.
func NewSSEParserWithLogger(r io.Reader, logger *slog.Logger) *SSEParser {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64*1024), maxSSELineSize)
	return &SSEParser{
		scanner: s,
		logger:  logger,
	}
}

// Next returns the next SSE event, or io.EOF when the stream ends.
// Malformed lines are skipped with a warning log.
func (p *SSEParser) Next() (*SSEEvent, error) {
	var (
		dataLines []string
		eventType string
		id        string
		retry     = -1
		hasData   bool
	)

	for p.scanner.Scan() {
		line := p.scanner.Text()

		// Empty line marks the end of an event.
		if line == "" {
			if !hasData && eventType == "" && id == "" && retry == -1 {
				// No fields accumulated; skip consecutive blank lines.
				continue
			}
			evt := &SSEEvent{
				Event: eventType,
				Data:  strings.Join(dataLines, "\n"),
				ID:    id,
				Retry: retry,
			}
			if evt.Event == "" {
				evt.Event = "message"
			}
			// Detect [DONE] marker.
			trimmed := strings.TrimSpace(evt.Data)
			if trimmed == "[DONE]" {
				evt.Done = true
			}
			return evt, nil
		}

		// Comment lines (starting with ':') are ignored per the SSE spec.
		if strings.HasPrefix(line, ":") {
			continue
		}

		// Split on the first ':'.
		field, value := parseField(line)

		switch field {
		case "data":
			hasData = true
			dataLines = append(dataLines, value)
		case "event":
			eventType = value
		case "id":
			id = value
		case "retry":
			parsed, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				p.logger.Warn("SSE parser: invalid retry value", "value", value)
				continue
			}
			retry = parsed
		default:
			// Unknown field — skip with warning.
			p.logger.Warn("SSE parser: unknown field", "field", field, "line", line)
		}
	}

	// Scanner exhausted. If we have accumulated fields, emit a final event.
	if hasData || eventType != "" || id != "" || retry != -1 {
		evt := &SSEEvent{
			Event: eventType,
			Data:  strings.Join(dataLines, "\n"),
			ID:    id,
			Retry: retry,
		}
		if evt.Event == "" {
			evt.Event = "message"
		}
		trimmed := strings.TrimSpace(evt.Data)
		if trimmed == "[DONE]" {
			evt.Done = true
		}
		return evt, nil
	}

	if err := p.scanner.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

// parseField splits an SSE line into field name and value.
// Per the SSE spec: if the line contains ':', the field is everything before
// the first ':', and the value is everything after (with one leading space
// stripped if present). If no ':', the entire line is the field with empty value.
func parseField(line string) (string, string) {
	idx := strings.IndexByte(line, ':')
	if idx < 0 {
		return line, ""
	}
	field := line[:idx]
	value := line[idx+1:]
	// Strip single leading space after colon per SSE spec.
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	return field, value
}

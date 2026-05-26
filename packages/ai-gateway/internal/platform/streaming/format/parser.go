// Package format provides SSE wire-level primitives (parser + writers +
// per-chunk text extraction) used by ai-gateway's streaming compliance
// pipeline. Pure wire/format concerns — NO dependency on
// shared/policy/hooks/core or anything in the parent streaming
// (compliance) package (#100 enforces this boundary so the format
// surface can evolve independently of the hook executor).
package format

import (
	"bufio"
	"io"
	"strings"
)

// Event represents a single Server-Sent Event.
//
// Type is the value of the SSE `event:` field when the upstream
// emitted one (Anthropic always does — message_start, content_block_delta,
// message_stop, ...). Empty for OpenAI-style streams where every event
// is a default "message". Preserving Type matters because Anthropic SDK
// + Claude Code dispatch on the `event:` line; dropping it produces a
// well-formed body that the client cannot route to a typed handler,
// surfacing as a blank UI even though all `data:` deltas arrived.
type Event struct {
	Type string
	Data string
	Done bool // true if data is [DONE]
}

// Parser reads SSE events from an io.Reader.
type Parser struct {
	scanner *bufio.Scanner
}

const maxSSELineSize = 10 * 1024 * 1024 // 10 MB — matches maxRequestBodySize

// NewParser creates an SSE parser with a buffer large enough for vision
// responses and other large payloads.
func NewParser(r io.Reader) *Parser {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64*1024), maxSSELineSize)
	return &Parser{scanner: s}
}

// Next returns the next SSE event, or io.EOF when the stream ends.
//
// Captures both the `event:` field (into Event.Type) and `data:` lines
// (into Event.Data). `id:` and `retry:` are ignored (the AI Gateway
// does not surface them to consumers). Multi-line `data:` is joined
// with `\n` per the SSE spec.
func (p *Parser) Next() (*Event, error) {
	var dataLines []string
	var eventType string
	hasData := false

	for p.scanner.Scan() {
		line := p.scanner.Text()

		// Empty line = end of event.
		if line == "" {
			if !hasData && eventType == "" {
				continue
			}
			data := strings.Join(dataLines, "\n")
			return &Event{
				Type: eventType,
				Data: data,
				Done: strings.TrimSpace(data) == "[DONE]",
			}, nil
		}

		// Skip comments.
		if strings.HasPrefix(line, ":") {
			continue
		}

		// Parse "<field>: ..." lines.
		if field, value, ok := strings.Cut(line, ":"); ok {
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
			switch field {
			case "data":
				hasData = true
				dataLines = append(dataLines, value)
			case "event":
				eventType = value
				// id:, retry: intentionally ignored.
			}
		}
	}

	// EOF with accumulated data.
	if hasData || eventType != "" {
		data := strings.Join(dataLines, "\n")
		return &Event{
			Type: eventType,
			Data: data,
			Done: strings.TrimSpace(data) == "[DONE]",
		}, nil
	}

	if err := p.scanner.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

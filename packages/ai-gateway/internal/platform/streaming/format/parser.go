// Package format provides SSE wire-level primitives (parser + writers +
// per-chunk text extraction) used by ai-gateway's streaming compliance
// pipeline. Pure wire/format concerns — NO dependency on
// shared/policy/hooks/core or anything in the parent streaming
// (compliance) package — this boundary keeps the format
// surface evolving independently of the hook executor.
package format

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"sync"
)

// colonSep splits an SSE "<field>:<value>" line; package-level so the hot
// Next() loop does not re-allocate the separator each line.
var colonSep = []byte(":")

// parserBufPool recycles the 64 KiB initial scanner buffer NewParser allocated
// fresh per stream — ~13.7 GB/window under streaming load. The pooled value is
// the ORIGINAL 64 KiB slice; if a frame exceeds it bufio.Scanner allocates its
// own larger buffer and abandons ours, so our handle stays a pristine 64 KiB
// slice that is always safe to return on Release. Every Event the parser emits
// builds its Data via strings.Join (a fresh string), so no emitted event aliases
// the buffer — a recycled buffer cannot leak one stream's bytes into another.
var parserBufPool = sync.Pool{New: func() any { b := make([]byte, 64*1024); return &b }}

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
	bufp    *[]byte      // pooled 64 KiB scanner buffer handle; returned on Release
	data    bytes.Buffer // reused per-event data accumulator (newline-joined data lines)
}

const maxSSELineSize = 10 * 1024 * 1024 // 10 MB — matches maxRequestBodySize

// NewParser creates an SSE parser with a buffer large enough for vision
// responses and other large payloads. Call Release when the stream is done to
// return the scanner buffer to the pool.
func NewParser(r io.Reader) *Parser {
	bufp := parserBufPool.Get().(*[]byte)
	s := bufio.NewScanner(r)
	s.Buffer(*bufp, maxSSELineSize)
	return &Parser{scanner: s, bufp: bufp}
}

// Release returns the pooled scanner buffer. At-most-once: a second call is a
// no-op. After Release the Parser must not be used again.
func (p *Parser) Release() {
	if p.bufp != nil {
		parserBufPool.Put(p.bufp)
		p.bufp = nil
	}
}

// Next returns the next SSE event, or io.EOF when the stream ends.
//
// Captures both the `event:` field (into Event.Type) and `data:` lines
// (into Event.Data). `id:` and `retry:` are ignored (the AI Gateway
// does not surface them to consumers). Multi-line `data:` is joined
// with `\n` per the SSE spec.
func (p *Parser) Next() (*Event, error) {
	// data is reused across events; reset its accumulated bytes (not its cap).
	p.data.Reset()
	var eventType string
	hasData := false

	for p.scanner.Scan() {
		// Bytes() aliases the scanner buffer (valid only until the next Scan);
		// data lines are copied into p.data and the event name into a string
		// before the next iteration, so nothing retained aliases the buffer.
		// Eliminates the per-line Text() string allocation.
		line := p.scanner.Bytes()

		// Empty line = end of event.
		if len(line) == 0 {
			if !hasData && eventType == "" {
				continue
			}
			return p.emit(eventType), nil
		}

		// Skip comments.
		if line[0] == ':' {
			continue
		}

		// Parse "<field>:<value>" lines.
		field, value, ok := bytes.Cut(line, colonSep)
		if !ok {
			continue
		}
		if len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}
		switch string(field) { // string(field) does not allocate in a switch
		case "data":
			if hasData {
				p.data.WriteByte('\n')
			}
			hasData = true
			p.data.Write(value)
		case "event":
			eventType = string(value)
			// id:, retry: intentionally ignored.
		}
	}

	// EOF with accumulated data.
	if hasData || eventType != "" {
		return p.emit(eventType), nil
	}

	if err := p.scanner.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

// emit builds an Event from the accumulated data buffer. The Data string is a
// fresh copy, so it remains valid after the next Next() resets p.data.
func (p *Parser) emit(eventType string) *Event {
	data := p.data.String()
	return &Event{
		Type: eventType,
		Data: data,
		Done: strings.TrimSpace(data) == "[DONE]",
	}
}

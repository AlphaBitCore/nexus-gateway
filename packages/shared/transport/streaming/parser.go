// Package streaming implements SSE stream processing with checkpoint-based
// compliance hooks for the Nexus Gateway compliance-proxy.
package streaming

import (
	"bufio"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
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

// maxLoggedFieldName bounds how much of an unrecognized field name reaches the
// log. Real SSE field names are a handful of characters; anything longer is a
// malformed or hostile line, and this is a compliance/DLP product where raw
// intercepted bytes must never land in logs.
const maxLoggedFieldName = 32

// sseScanBufPool recycles the 64 KiB bufio scan buffer. One was allocated per
// stream, which made it the single largest allocation on the inspected-SSE path:
// on a 150-frame reply it is 65,536 of ~228,000 bytes, and a short 3-frame stream
// spent essentially all of its 66 KB here. Same shape the ai-gateway already pools
// in providers/specutil/sse.go.
//
// The pool holds *[]byte rather than []byte so Get/Put do not box a slice header
// into an interface on every stream.
//
// Two invariants make recycling safe, and both are load-bearing:
//
//   - The handle kept in SSEParser is the ORIGINAL 64 KiB slice. If a frame
//     exceeds 64 KiB, bufio allocates its own larger buffer and abandons ours; the
//     handle still points at the pristine slice, so it stays correctly sized and
//     safe to return.
//   - Nothing a parser emits aliases the buffer. Next builds every SSEEvent field
//     from scanner.Text(), which copies, so no returned event can observe another
//     stream's bytes after the buffer is recycled. That matters more here than in a
//     plain relay: this is a compliance product, and cross-stream bleed would put
//     one tenant's payload into another's audit record.
var sseScanBufPool = sync.Pool{New: func() any { b := make([]byte, 0, 64*1024); return &b }}

// ErrParserReleased is returned by Next after Release has been called. It is an
// error rather than a panic or a silent EOF because the alternatives are worse: the
// released buffer may already belong to another stream, so continuing to scan could
// splice a second stream's bytes into this one's events, and a silent EOF would hide
// that. Callers treat it like any other reader error and end the stream.
var ErrParserReleased = errors.New("streaming: SSE parser used after Release")

// SSEParser reads SSE events from an io.Reader.
type SSEParser struct {
	scanner *bufio.Scanner
	logger  *slog.Logger

	// bufp is the pooled scan-buffer handle, returned by Release. nil once
	// released, which also makes Release idempotent — returning the same slice
	// twice would hand it to two streams at once.
	bufp *[]byte

	// unknownFieldReported / retryParseReported collapse per-LINE diagnostics
	// into one report per stream. Both conditions are things a transparent MITM
	// sees routinely — it carries arbitrary provider wires, and the SSE spec
	// *requires* ignoring unrecognized fields — so logging per line turned
	// normal traffic into a log storm (measured: 6.3x slower parse) and, for a
	// line with no separator, wrote the entire line (up to maxSSELineSize, 1 MiB)
	// into the log at WARN.
	unknownFieldReported bool
	retryParseReported   bool
}

// NewSSEParser creates a parser for the given reader.
//
// The caller MUST call Release when done — on clean EOF and on early abort alike —
// or the pooled scan buffer is simply not reused. Release does not touch the
// reader; the parser never owned it.
func NewSSEParser(r io.Reader) *SSEParser {
	return newSSEParser(r, slog.Default())
}

// NewSSEParserWithLogger creates a parser with a custom logger. Same Release
// contract as NewSSEParser.
func NewSSEParserWithLogger(r io.Reader, logger *slog.Logger) *SSEParser {
	return newSSEParser(r, logger)
}

func newSSEParser(r io.Reader, logger *slog.Logger) *SSEParser {
	bufp := sseScanBufPool.Get().(*[]byte)
	s := bufio.NewScanner(r)
	s.Buffer(*bufp, maxSSELineSize)
	return &SSEParser{
		scanner: s,
		logger:  logger,
		bufp:    bufp,
	}
}

// Release returns the pooled scan buffer. It is safe to call more than once and
// safe to call on a parser that never scanned anything. After Release, Next
// returns ErrParserReleased rather than reading from a buffer that may already
// belong to another stream.
//
// Deliberately named Release and not Close: this parser does not own the reader it
// was given, and a Close would imply it does.
func (p *SSEParser) Release() {
	if p == nil || p.bufp == nil {
		return
	}
	sseScanBufPool.Put(p.bufp)
	p.bufp = nil
	p.scanner = nil
}

// Next returns the next SSE event, or io.EOF when the stream ends.
// Malformed lines are skipped with a warning log.
func (p *SSEParser) Next() (*SSEEvent, error) {
	if p.scanner == nil {
		return nil, ErrParserReleased
	}
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
		field, value, hasSeparator := parseField(line)

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
				if !p.retryParseReported {
					p.retryParseReported = true
					p.logger.Debug("SSE parser: invalid retry value; ignoring (reported once per stream)")
				}
				continue
			}
			retry = parsed
		default:
			// Unrecognized field. The SSE spec REQUIRES ignoring these, so this
			// is not an error condition — and on a transparent MITM carrying
			// arbitrary provider wires it is routine. Report once per stream at
			// Debug.
			//
			// The line content is deliberately NOT logged. When the line has no
			// ':' separator the whole line becomes the "field name", so the old
			// per-line Warn wrote up to maxSSELineSize (1 MiB) of
			// remotely-controlled bytes into the log on every occurrence — a log
			// amplification shape, and on a compliance/DLP product a path for raw
			// prompt or model output to reach default-level logs.
			if !p.unknownFieldReported {
				p.unknownFieldReported = true
				if hasSeparator {
					p.logger.Debug("SSE parser: unrecognized field; ignoring per spec (reported once per stream)",
						"field", boundedFieldName(field))
				} else {
					p.logger.Debug("SSE parser: line without a field separator; ignoring per spec (reported once per stream)",
						"lineBytes", len(line))
				}
			}
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

// boundedFieldName truncates an unrecognized field name to a length safe to
// log. A line with no ':' separator yields the whole line as the field name,
// which can be up to maxSSELineSize and is attacker-influenced on a MITM path;
// callers must not log it at all in that case (see the default arm in Next).
func boundedFieldName(field string) string {
	if len(field) <= maxLoggedFieldName {
		return field
	}
	return field[:maxLoggedFieldName] + "…"
}

// parseField splits an SSE line into field name and value.
// Per the SSE spec: if the line contains ':', the field is everything before
// the first ':', and the value is everything after (with one leading space
// stripped if present). If no ':', the entire line is the field with empty value.
//
// hasSeparator reports whether a ':' was present. Callers need it to tell a
// genuine (if unrecognized) field name apart from a separator-less line whose
// "field name" is the entire line — the two must not be logged the same way.
func parseField(line string) (field, value string, hasSeparator bool) {
	idx := strings.IndexByte(line, ':')
	if idx < 0 {
		return line, "", false
	}
	field = line[:idx]
	value = line[idx+1:]
	// Strip single leading space after colon per SSE spec.
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	return field, value, true
}

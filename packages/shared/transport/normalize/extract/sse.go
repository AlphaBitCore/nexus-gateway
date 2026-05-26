package extract

import (
	"bufio"
	"bytes"
	"errors"
	"strings"
)

// MaxSSEScanLine bounds a single SSE line. Matches the existing
// openai_chat / anthropic_messages stream decoders so all three paths
// agree on the upper limit and one moving to share this code does not
// regress on long bodies.
const MaxSSEScanLine = 8 * 1024 * 1024 // 8 MiB

// initSSEScanLine is the bufio.Scanner initial buffer size; grows up to
// MaxSSEScanLine on demand. 64 KiB is enough for typical SSE chunks
// from every observed provider.
const initSSEScanLine = 64 * 1024

// WalkSSE iterates Server-Sent Events frames out of raw bytes. Each
// frame is the canonical `event:` + `data:` block separated from the
// next by a blank line, per W3C eventsource. Continuation `data:` lines
// within the same frame are concatenated into the frame's Data field
// joined by "\n" so the original payload's internal newlines (rare but
// legal) survive.
//
// The supplied fn is called once per frame in stream order. Returning a
// non-nil error from fn stops iteration and propagates the error;
// returning ErrSSEStopWalk gracefully ends iteration without error
// (used by callers who only need the first N frames).
//
// Whitespace between frames is tolerated; comments (lines starting with
// ":") are skipped. The terminal "data: [DONE]" line common to OpenAI
// streams is delivered as a normal data frame — the caller decides what
// to do with [DONE] sentinels.
func WalkSSE(raw []byte, fn func(event, data string) error) error {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, initSSEScanLine), MaxSSEScanLine)
	scanner.Split(bufio.ScanLines)

	var (
		curEvent string
		dataBuf  strings.Builder
		gotData  bool
	)
	flush := func() error {
		if !gotData {
			// blank frame separator hit with no `data:` lines since
			// the last flush — nothing to emit.
			curEvent = ""
			return nil
		}
		err := fn(curEvent, dataBuf.String())
		curEvent = ""
		dataBuf.Reset()
		gotData = false
		return err
	}

	for scanner.Scan() {
		line := scanner.Text()
		// trim CR (some servers emit \r\n line endings)
		line = strings.TrimRight(line, "\r")
		if line == "" {
			// frame boundary
			if err := flush(); err != nil {
				if errors.Is(err, ErrSSEStopWalk) {
					return nil
				}
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			// comment / keep-alive — skip
			continue
		}
		// Match "field:" or "field: value"
		idx := strings.IndexByte(line, ':')
		var field, value string
		if idx < 0 {
			field = line
			value = ""
		} else {
			field = line[:idx]
			value = line[idx+1:]
			// Per spec: single leading space is part of the field
			// delimiter, not the value.
			value = strings.TrimPrefix(value, " ")
		}
		switch field {
		case "event":
			curEvent = value
		case "data":
			if gotData {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(value)
			gotData = true
		case "id", "retry":
			// W3C spec fields — ignored by audit normalizer
		default:
			// Unknown field — ignore (per spec recommendation)
		}
	}
	// Final frame (no trailing blank line)
	if gotData {
		if err := flush(); err != nil && !errors.Is(err, ErrSSEStopWalk) {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}

// ErrSSEStopWalk is a sentinel a WalkSSE callback may return to halt
// iteration without surfacing an error to the WalkSSE caller. Useful
// when only the first frame matters (e.g. extracting the resume token
// from ChatGPT's `resume_conversation_token` opener without scanning
// every delta).
var ErrSSEStopWalk = errors.New("extract: stop sse walk")

// LooksLikeSSE returns true when raw appears to start with an SSE frame.
// Lenient about leading whitespace / blank lines. Used by callers that
// haven't yet decided to walk and want a quick yes/no.
func LooksLikeSSE(raw []byte) bool {
	probe := raw
	if len(probe) > 64 {
		probe = probe[:64]
	}
	s := strings.TrimLeft(string(probe), " \r\n\t")
	return strings.HasPrefix(s, "event:") || strings.HasPrefix(s, "data:")
}

package codecs

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
)

// walkSSEFrames scans a captured SSE byte stream and invokes fn once per
// dispatched FRAME, with the frame's data payload and its `event:` name.
//
// Frame semantics follow the SSE spec, as the sibling walker in
// transport/streaming/extract already does: fields are COLLECTED until the blank
// dispatch separator, and multiple `data:` lines in one frame are joined with "\n".
// A trailing frame with no blank line after it is still dispatched at EOF.
//
// Doing this per-line instead was finding R-14: a W3C-legal multi-line `data:` frame
// produced one callback per line, each carrying a JSON fragment that could not parse,
// so the frame silently lost its Tier-1 decode and the body fell through to Tier 3 —
// hooks then saw the verbatim fallback instead of structured messages. Collecting also
// fixes R-15: the `event:` name now applies to the whole frame regardless of whether it
// appeared before or after the data lines, where previously it bound only to data lines
// that followed it.
//
// Comments (`:` prefix) and the `id:` / `retry:` fields are ignored, per spec.
//
// The walk never stops early on frame content — malformed frames are the callback's
// business (every fold counts them toward its coverage total and moves on). The scanner
// accepts lines up to 8 MiB (64 KiB initial buffer); a longer line aborts the scan and
// the scanner error is returned so callers can weigh the lost tail on their coverage.
func walkSSEFrames(raw []byte, fn func(event, data string)) error {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)

	event := ""
	var data []string
	// dispatch fires the callback for the frame collected so far. A frame with no
	// `data:` line at all (an `event:`-only keepalive, say) dispatches nothing — that
	// matches the previous behaviour, where such a frame produced no callback either.
	dispatch := func() {
		if len(data) > 0 {
			fn(event, strings.Join(data, "\n"))
		}
		event = ""
		data = data[:0]
	}

	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.TrimSpace(line) == "":
			dispatch()
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	// A stream whose last frame is not followed by a blank line still has a frame to
	// dispatch. Dropping it would lose the final frame of any truncated capture — and a
	// truncated capture is the normal shape for an aborted stream.
	dispatch()

	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

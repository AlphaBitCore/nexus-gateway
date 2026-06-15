package codecs

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
)

// walkSSEFrames scans a captured SSE byte stream line by line and invokes
// fn once per `data:` line, with the payload stripped of the prefix and
// trimmed of surrounding whitespace. event carries the frame's `event:`
// name per SSE dispatch semantics: an `event:` line names every `data:`
// line that follows it until the blank dispatch separator resets the
// name to "". Callers that key off the data payload's own discriminator
// (every AI provider stream carries one) simply ignore the parameter.
//
// The walk never stops early on frame content — malformed frames are the
// callback's business (every fold counts them toward its coverage total
// and moves on). The scanner accepts lines up to 8 MiB (64 KiB initial
// buffer); a longer line aborts the scan and the scanner error is
// returned so callers can weigh the lost tail on their coverage.
func walkSSEFrames(raw []byte, fn func(event, data string)) error {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	event := ""
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.TrimSpace(line) == "":
			// Blank line = SSE dispatch boundary; the event name does
			// not carry over to the next frame.
			event = ""
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			fn(event, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

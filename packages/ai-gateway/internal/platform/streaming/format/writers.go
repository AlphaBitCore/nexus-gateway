package format

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// WriteEvent writes an SSE event with no `event:` field to the writer.
// Use WriteTypedEvent when the upstream supplied a typed event name
// (Anthropic always does); typed clients (Claude Code, Anthropic SDK)
// dispatch on the `event:` line and silently drop frames that arrive
// without one.
func WriteEvent(w io.Writer, data string) error {
	return WriteTypedEvent(w, "", data)
}

// WriteTypedEvent writes an SSE frame preserving the upstream
// `event:` field. Empty eventType skips the line so the wire format
// matches OpenAI-style "event-less" streams. Multi-line `data:`
// values are split per the SSE spec.
func WriteTypedEvent(w io.Writer, eventType, data string) error {
	if eventType != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", eventType); err != nil {
			return err
		}
	}
	for _, line := range strings.Split(data, "\n") {
		if _, err := fmt.Fprintf(w, "data: %s\n", line); err != nil {
			return err
		}
	}
	_, err := fmt.Fprint(w, "\n")
	return err
}

// WriteDone writes the [DONE] marker.
func WriteDone(w io.Writer) error {
	_, err := fmt.Fprint(w, "data: [DONE]\n\n")
	return err
}

// WriteError writes an error as an SSE event followed by [DONE].
func WriteError(w io.Writer, message string) error {
	errJSON, _ := json.Marshal(map[string]any{
		"error": map[string]string{
			"message": message,
			"type":    "proxy_error",
		},
	})
	if err := WriteEvent(w, string(errJSON)); err != nil {
		return err
	}
	return WriteDone(w)
}

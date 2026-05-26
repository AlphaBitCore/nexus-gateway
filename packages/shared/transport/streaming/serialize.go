package streaming

import (
	"fmt"
	"io"
	"strings"
)

// WriteSSEEvent serializes an SSEEvent to the given writer in SSE wire format.
func WriteSSEEvent(w io.Writer, evt *SSEEvent) error {
	if evt.Event != "" && evt.Event != "message" {
		if _, err := fmt.Fprintf(w, "event: %s\n", evt.Event); err != nil {
			return err
		}
	}
	if evt.ID != "" {
		if _, err := fmt.Fprintf(w, "id: %s\n", evt.ID); err != nil {
			return err
		}
	}
	if evt.Retry >= 0 {
		if _, err := fmt.Fprintf(w, "retry: %d\n", evt.Retry); err != nil {
			return err
		}
	}
	// Data may contain multiple lines; each must be prefixed with "data: ".
	for _, line := range strings.Split(evt.Data, "\n") {
		if _, err := fmt.Fprintf(w, "data: %s\n", line); err != nil {
			return err
		}
	}
	// Blank line terminates the event.
	if _, err := fmt.Fprint(w, "\n"); err != nil {
		return err
	}
	return nil
}

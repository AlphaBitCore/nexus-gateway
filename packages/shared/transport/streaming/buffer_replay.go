package streaming

// Client-side delivery for the buffer pipeline: re-emitting the frames that were
// held back while the response was inspected.
//
// Split out of buffer.go along that seam — buffer.go owns reading, accumulating and
// deciding; this owns writing the decision's outcome to the client. The split was
// forced by the file-size ratchet and taken here rather than waived because the two
// halves genuinely answer different questions.

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// replay writes the given SSE events to the client, teeing into the
// capture buffer when WithBodyCapture is enabled and flushing after each
// frame for incremental delivery. Resolves the flusher BEFORE wrapping
// in MultiWriter — interface satisfactions don't pass through it.
func (b *BufferPipeline) replay(ctx context.Context, client io.Writer, events []*SSEEvent) error {
	flusher, canFlush := client.(http.Flusher)
	writer := client
	if b.captureBuf != nil {
		writer = io.MultiWriter(client, b.captureBuf)
	}
	for _, evt := range events {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := WriteSSEEvent(writer, evt); err != nil {
			return fmt.Errorf("buffer pipeline: write event: %w", err)
		}
		if canFlush {
			flusher.Flush()
		}
	}
	return nil
}

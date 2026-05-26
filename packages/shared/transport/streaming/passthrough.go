package streaming

import (
	"context"
	"io"
	"net/http"
)

// Passthrough copies an SSE stream directly from upstream to client with no
// compliance inspection. It respects context cancellation and flushes after
// each read if the client supports http.Flusher.
func Passthrough(ctx context.Context, upstream io.Reader, client io.Writer) error {
	flusher, canFlush := client.(http.Flusher)

	buf := make([]byte, 32*1024) // 32 KB read buffer
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		n, readErr := upstream.Read(buf)
		if n > 0 {
			if _, writeErr := client.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return nil
			}
			return readErr
		}
	}
}

// PassthroughWithAccumulator streams upstream bytes to the client unchanged
// while tee-ing them into an SSE parser whose events feed the given
// UsageAccumulator. Bytes delivered to the client are byte-for-byte identical
// to upstream; accumulator parsing runs on a side goroutine so a slow or
// erroring parser never blocks the main relay.
//
// If acc is nil this degrades to Passthrough. The caller is expected to
// Finalize acc after the function returns.
func PassthroughWithAccumulator(ctx context.Context, upstream io.Reader, client io.Writer, acc UsageAccumulator) error {
	if acc == nil {
		return Passthrough(ctx, upstream, client)
	}

	flusher, canFlush := client.(http.Flusher)

	// Side pipe feeds parsed SSE events into the accumulator.
	pr, pw := io.Pipe()
	parseDone := make(chan struct{})
	go func() {
		defer close(parseDone)
		parser := NewSSEParser(pr)
		for {
			evt, err := parser.Next()
			if err != nil {
				// Drain the rest of the pipe so main-loop writes never block.
				_, _ = io.Copy(io.Discard, pr)
				return
			}
			acc.Feed(evt)
			if evt.Done {
				_, _ = io.Copy(io.Discard, pr)
				return
			}
		}
	}()

	buf := make([]byte, 32*1024)
	var mainErr error
	for {
		if err := ctx.Err(); err != nil {
			mainErr = err
			break
		}

		n, readErr := upstream.Read(buf)
		if n > 0 {
			if _, writeErr := client.Write(buf[:n]); writeErr != nil {
				mainErr = writeErr
				break
			}
			if canFlush {
				flusher.Flush()
			}
			// Best-effort feed to parser; ignore pipe errors (parser may have exited).
			_, _ = pw.Write(buf[:n])
		}
		if readErr != nil {
			if readErr != io.EOF {
				mainErr = readErr
			}
			break
		}
	}

	// Closing pw signals EOF to the parser goroutine, which will drain and exit.
	_ = pw.Close()
	<-parseDone
	return mainErr
}

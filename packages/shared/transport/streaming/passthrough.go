package streaming

import (
	"context"
	"io"
	"net/http"
	"sync"
)

// relayBufSize is the read chunk for the passthrough relays. 32 KiB matches
// what these functions have always used; it is large enough that a full SSE
// frame usually lands in one read and small enough to keep time-to-first-byte
// tight on a slow trickle.
const relayBufSize = 32 * 1024

// relayBufPool reuses the relay read buffer across streams. Every SSE response
// that reaches a passthrough relay used to allocate a fresh 32 KiB here, and a
// proxy serving many short streams paid that repeatedly — measured as the
// dominant per-stream cost on short replies.
//
// sync.Pool is the right primitive for this one (playbook §2.2): a plain byte
// slice is cheap to recreate, so losing the pool contents to a GC costs only a
// re-allocation. The GC-stable-ring pattern is reserved for objects whose
// construction is expensive (cgo scratch, zstd encoders).
//
// The buffer never escapes: it is written to the client and to the tee pipe
// synchronously inside the loop, and both have consumed it before the next
// Read overwrites it. So returning it on function exit is safe.
var relayBufPool = sync.Pool{New: func() any {
	b := make([]byte, relayBufSize)
	return &b
}}

func getRelayBuf() *[]byte  { return relayBufPool.Get().(*[]byte) }
func putRelayBuf(b *[]byte) { relayBufPool.Put(b) }

// Passthrough copies an SSE stream directly from upstream to client with no
// compliance inspection. It respects context cancellation and flushes after
// each read if the client supports http.Flusher.
func Passthrough(ctx context.Context, upstream io.Reader, client io.Writer) error {
	flusher, canFlush := client.(http.Flusher)

	bufp := getRelayBuf()
	defer putRelayBuf(bufp)
	buf := *bufp
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
		defer parser.Release()
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

	bufp := getRelayBuf()
	defer putRelayBuf(bufp)
	buf := *bufp
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

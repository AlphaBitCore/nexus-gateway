package proxy

import (
	"net/http"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
)

// streamCaptureTee wraps the streaming ResponseWriter to buffer up to hardCap bytes of
// the response body (for traffic_event capture / hooks) while passing every write
// through to the client unchanged. Past hardCap it stops buffering (tail=true) but
// keeps relaying, so a large stream is never held in memory in full.
//
// The capture buffer is a POOLED backing array (audit.responseBodyPool): the tee
// appends THROUGH the handle, captured() exposes it for the audit record, and the
// buffer is reclaimed at the record's terminal resolution (when stored) or via
// release() (when the body is not stored). This turns the per-stream capture
// allocation — the second-largest streaming-relay allocator — into a pooled reuse.
type streamCaptureTee struct {
	http.ResponseWriter
	hardCap   int64
	written   int64
	bufHandle *[]byte
	tail      bool // true once we have stopped buffering past hardCap
}

func newStreamCaptureTee(w http.ResponseWriter, hardCap int64) *streamCaptureTee {
	if hardCap < 0 {
		hardCap = 0
	}
	return &streamCaptureTee{
		ResponseWriter: w,
		hardCap:        hardCap,
		bufHandle:      audit.AcquireResponseBuffer(),
	}
}

func (w *streamCaptureTee) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if n > 0 && !w.tail {
		writable := w.hardCap - w.written
		switch {
		case writable <= 0:
			w.tail = true
		case int64(n) > writable:
			*w.bufHandle = append(*w.bufHandle, p[:int(writable)]...)
			w.written = w.hardCap
			w.tail = true
		default:
			*w.bufHandle = append(*w.bufHandle, p[:n]...)
			w.written += int64(n)
		}
	}
	return n, err
}

func (w *streamCaptureTee) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *streamCaptureTee) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *streamCaptureTee) captured() []byte         { return *w.bufHandle }
func (w *streamCaptureTee) truncatedBeyondCap() bool { return w.tail }

// handle returns the pooled capture buffer for AttachPooledResponseBody when the
// captured body is stored on the audit record (the writer reclaims it at the
// record's terminal resolution).
func (w *streamCaptureTee) handle() *[]byte { return w.bufHandle }

// release returns the capture buffer to the pool directly — for the path where
// the captured body is NOT stored on an audit record (so no terminal reclaim
// runs). After release the tee must not be read again.
func (w *streamCaptureTee) release() {
	audit.ReleaseResponseBuffer(w.bufHandle)
	w.bufHandle = nil
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

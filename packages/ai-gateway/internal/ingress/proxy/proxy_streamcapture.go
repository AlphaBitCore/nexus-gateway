package proxy

import "net/http"

// streamCaptureTee wraps the streaming ResponseWriter to buffer up to hardCap bytes of
// the response body (for traffic_event capture / hooks) while passing every write
// through to the client unchanged. Past hardCap it stops buffering (tail=true) but
// keeps relaying, so a large stream is never held in memory in full.
type streamCaptureTee struct {
	http.ResponseWriter
	hardCap int64
	written int64
	buf     []byte
	tail    bool // true once we have stopped buffering past hardCap
}

func newStreamCaptureTee(w http.ResponseWriter, hardCap int64) *streamCaptureTee {
	if hardCap < 0 {
		hardCap = 0
	}
	return &streamCaptureTee{
		ResponseWriter: w,
		hardCap:        hardCap,
		buf:            make([]byte, 0, minInt64(hardCap, 16*1024)),
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
			w.buf = append(w.buf, p[:int(writable)]...)
			w.written = w.hardCap
			w.tail = true
		default:
			w.buf = append(w.buf, p[:n]...)
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

func (w *streamCaptureTee) captured() []byte         { return w.buf }
func (w *streamCaptureTee) truncatedBeyondCap() bool { return w.tail }

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

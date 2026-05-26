package streaming

// CappedBuffer is a goroutine-unsafe io.Writer that captures up to maxBytes
// bytes and silently drops the rest. Used by LivePipeline / BufferPipeline
// to record the response bytes streamed to the client so the audit
// emitter can persist a prefix of the SSE body alongside the inline /
// non-stream capture path.
//
// Write always reports it consumed every byte handed in so the surrounding
// io.MultiWriter does not abort the client write when the cap is hit.
// Callers detect overflow via Truncated().
type CappedBuffer struct {
	buf       []byte
	maxBytes  int
	truncated bool
}

func NewCappedBuffer(maxBytes int) *CappedBuffer {
	if maxBytes <= 0 {
		return nil
	}
	return &CappedBuffer{maxBytes: maxBytes}
}

func (b *CappedBuffer) Write(p []byte) (int, error) {
	if b == nil {
		return len(p), nil
	}
	remaining := b.maxBytes - len(b.buf)
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) <= remaining {
		b.buf = append(b.buf, p...)
		return len(p), nil
	}
	b.buf = append(b.buf, p[:remaining]...)
	b.truncated = true
	return len(p), nil
}

// Bytes returns the captured prefix. Empty if capture was disabled or
// nothing was written.
func (b *CappedBuffer) Bytes() []byte {
	if b == nil {
		return nil
	}
	return b.buf
}

// Truncated reports whether at least one Write call had to drop bytes
// because the per-buffer cap was reached.
func (b *CappedBuffer) Truncated() bool {
	if b == nil {
		return false
	}
	return b.truncated
}

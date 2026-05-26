package streaming

import (
	"bytes"
	"sync"
)

// LockedByteBuffer is a goroutine-safe bytes.Buffer used by SSE
// pipelines (BufferPipeline + LivePipeline) to accumulate raw SSE wire
// bytes for the PreHookCallback. The reader goroutine writes via
// io.TeeReader (so writes happen inline during parser.Next); the
// compliance goroutine reads a snapshot at every checkpoint via
// Snapshot() which locks briefly + copies the underlying byte slice
// (so the caller can't observe mid-write torn state).
//
// Exported in #92 so ai-gateway/internal/platform/streaming (which has
// its own LivePipeline impl for cross-format transcoding reasons) can
// reuse the same goroutine-safe accumulator instead of carrying its
// own byte-for-byte copy. Single point of truth for the contract.
type LockedByteBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write satisfies io.Writer for TeeReader.
func (l *LockedByteBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// Snapshot returns a defensive copy of the bytes accumulated so far.
// The caller may safely retain / mutate the returned slice without
// affecting subsequent writes.
func (l *LockedByteBuffer) Snapshot() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	src := l.buf.Bytes()
	out := make([]byte, len(src))
	copy(out, src)
	return out
}

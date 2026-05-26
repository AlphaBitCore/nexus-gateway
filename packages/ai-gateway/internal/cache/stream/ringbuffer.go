package streamcache

import (
	"context"
	"io"
	"sync"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// RingBuffer is an append-only chunk buffer with wake-on-append
// notification. Safe for one writer (the broker leader pump) and
// many concurrent readers. Late-joining readers may start at idx=0
// to replay all chunks already received.
//
// Despite the name, this is not a circular buffer — chunks are kept
// for the full lifetime of the broker so subscribers joining mid-
// stream get the full replay window.
type RingBuffer struct {
	mu      sync.Mutex
	chunks  []provcore.Chunk
	done    bool
	err     error
	waiters []chan struct{}
}

// NewRingBuffer returns an empty, ready-to-use RingBuffer.
func NewRingBuffer() *RingBuffer { return &RingBuffer{} }

// Append adds a chunk and wakes all parked readers. Append after
// AppendTerminal or Fail is a no-op (defensive).
func (r *RingBuffer) Append(chunk provcore.Chunk) {
	r.mu.Lock()
	if r.done || r.err != nil {
		r.mu.Unlock()
		return
	}
	r.chunks = append(r.chunks, chunk)
	r.wakeAllLocked()
	r.mu.Unlock()
}

// AppendTerminal appends the chunk and marks the buffer done.
// Subsequent Read calls past this chunk return io.EOF.
// AppendTerminal after AppendTerminal or Fail is a no-op.
func (r *RingBuffer) AppendTerminal(chunk provcore.Chunk) {
	r.mu.Lock()
	if r.done || r.err != nil {
		r.mu.Unlock()
		return
	}
	r.chunks = append(r.chunks, chunk)
	r.done = true
	r.wakeAllLocked()
	r.mu.Unlock()
}

// Fail marks the buffer as failed; readers past the latest chunk see
// err on their next Read. Already-buffered chunks remain readable.
// Fail after AppendTerminal or after an earlier Fail is a no-op.
func (r *RingBuffer) Fail(err error) {
	r.mu.Lock()
	if r.done || r.err != nil {
		r.mu.Unlock()
		return
	}
	r.err = err
	r.wakeAllLocked()
	r.mu.Unlock()
}

// Snapshot returns a copy of the chunks currently in the buffer.
// Caller mutations on the returned slice do not affect the buffer.
// Typically called by the broker after receiving the terminal chunk
// to persist the timeline to cache.
func (r *RingBuffer) Snapshot() []provcore.Chunk {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]provcore.Chunk, len(r.chunks))
	copy(out, r.chunks)
	return out
}

// Read returns the chunk at idx (or blocks until one is available),
// the next idx to read, and any error.
//
// Returns:
//   - (chunk, idx+1, nil)   when a chunk is available at idx
//   - (zero, idx, io.EOF)   when AppendTerminal has been called and
//     idx is past the end
//   - (zero, idx, err)      when Fail(err) has been called and idx is
//     past the end
//   - (zero, idx, ctx.Err()) when the caller's context is cancelled
//     while parked
func (r *RingBuffer) Read(ctx context.Context, idx int) (provcore.Chunk, int, error) {
	for {
		r.mu.Lock()
		if idx < len(r.chunks) {
			chunk := r.chunks[idx]
			r.mu.Unlock()
			return chunk, idx + 1, nil
		}
		if r.err != nil {
			err := r.err
			r.mu.Unlock()
			return provcore.Chunk{}, idx, err
		}
		if r.done {
			r.mu.Unlock()
			return provcore.Chunk{}, idx, io.EOF
		}
		// Park waiting for the next Append / AppendTerminal / Fail.
		ch := make(chan struct{})
		r.waiters = append(r.waiters, ch)
		r.mu.Unlock()

		select {
		case <-ch:
			// Woken; loop re-checks state.
		case <-ctx.Done():
			return provcore.Chunk{}, idx, ctx.Err()
		}
	}
}

// wakeAllLocked closes every waiter channel and clears the slice.
// Must be called with r.mu held.
func (r *RingBuffer) wakeAllLocked() {
	for _, ch := range r.waiters {
		close(ch)
	}
	r.waiters = nil
}

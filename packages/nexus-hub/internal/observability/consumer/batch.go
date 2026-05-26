package consumer

import (
	"sync"
	"time"
)

// BatchAccumulator collects items of type T and calls a flush function when
// the batch reaches maxSize or flushInterval elapses, whichever comes first.
// Thread-safe for concurrent Add calls.
type BatchAccumulator[T any] struct {
	maxSize       int
	flushInterval time.Duration
	flushFn       func(batch []T) error

	mu      sync.Mutex
	buffer  []T
	timer   *time.Timer
	stopped bool
}

// NewBatchAccumulator creates a new accumulator.
//   - maxSize: flush when buffer reaches this count (e.g. 100)
//   - flushInterval: flush after this duration even if buffer is not full (e.g. 5s)
//   - flushFn: callback invoked with the batch; return error to signal failure.
//     Must be safe for concurrent invocation: while flushFn runs the buffer
//     lock is released so a concurrent Add that fills the buffer to maxSize
//     can enter flushFn on a second goroutine.
func NewBatchAccumulator[T any](maxSize int, flushInterval time.Duration, flushFn func([]T) error) *BatchAccumulator[T] {
	return &BatchAccumulator[T]{
		maxSize:       maxSize,
		flushInterval: flushInterval,
		flushFn:       flushFn,
		buffer:        make([]T, 0, maxSize),
	}
}

// Add appends an item to the buffer. If the buffer reaches maxSize, flush is
// called synchronously. If this is the first item after a flush, the timer
// is started.
func (b *BatchAccumulator[T]) Add(item T) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.stopped {
		return nil
	}

	b.buffer = append(b.buffer, item)

	if len(b.buffer) == 1 {
		b.resetTimerLocked()
	}

	if len(b.buffer) >= b.maxSize {
		return b.flushLocked()
	}
	return nil
}

// Flush forces an immediate flush of any buffered items.
func (b *BatchAccumulator[T]) Flush() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.flushLocked()
}

// Stop marks the accumulator as stopped, flushes remaining items, and
// cancels the timer.
func (b *BatchAccumulator[T]) Stop() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopped = true
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	return b.flushLocked()
}

// flushLocked snapshots and clears the buffer under b.mu, then releases the
// lock for the duration of b.flushFn so concurrent Add calls are not
// serialized behind flush I/O. The lock is reacquired before return so callers
// that use `defer b.mu.Unlock()` remain correct.
//
// Caller must hold b.mu on entry, and b.mu will be held on return.
func (b *BatchAccumulator[T]) flushLocked() error {
	if len(b.buffer) == 0 {
		return nil
	}

	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}

	batch := make([]T, len(b.buffer))
	copy(batch, b.buffer)
	b.buffer = b.buffer[:0]

	b.mu.Unlock()
	defer b.mu.Lock()
	return b.flushFn(batch)
}

// resetTimerLocked starts or resets the flush timer. Caller must hold b.mu.
func (b *BatchAccumulator[T]) resetTimerLocked() {
	if b.timer != nil {
		b.timer.Stop()
	}
	b.timer = time.AfterFunc(b.flushInterval, func() {
		_ = b.Flush()
	})
}

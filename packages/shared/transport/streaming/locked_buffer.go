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
// Exported so ai-gateway/internal/platform/streaming (which has
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

// SnapshotInto copies the accumulated bytes into dst, reusing dst's capacity when it
// is large enough, and returns the filled prefix. It is the allocation-free sibling of
// Snapshot for callers that hand the result to a consumer which does not retain it.
//
// Why this exists (finding C-22): LivePipeline snapshots at EVERY checkpoint, and each
// snapshot copied the entire transcript accumulated so far. Because the checkpoint count
// also grows with the stream, total allocation was quadratic in stream length. Reusing
// one destination makes it amortized O(final size) while the copy itself (a memmove)
// stays. Measured on a 1000-frame stream: B/op 2,677,705 -> 1,474,210, i.e. -44.9 %.
//
// The pre-hook's own cost is NOT this, and the difference matters for anyone tempted to
// chase the rest. Turning the pre-hook off entirely saves 79.6 % of allocated bytes on
// that arm; only the 44.9 % here is copying. The remainder is the normalize work the
// snapshot feeds — every checkpoint re-normalizes the whole cumulative transcript, which
// is quadratic by design, not by implementation. Removing it means changing the
// checkpoint contract, not the buffer.
//
// CONTRACT, and the reason it is a separate method rather than a change to Snapshot:
// the returned slice is only valid until the next SnapshotInto call on the same dst.
// Snapshot's documented promise — that the caller may retain AND mutate the result —
// is relied on by ai-gateway/internal/platform/streaming and must not be weakened, so
// Snapshot is left exactly as it is.
//
// Safe for the pre-hook consumer because no NormalizedPayload can alias the bytes it is
// built from: neither NormalizedPayload nor any type nested in it has a []byte or
// json.RawMessage field, the normalize tree uses no unsafe, and both JSON libraries in
// play copy out of the input (gjson's getBytes re-allocates Raw/Str; goccy decodes into
// fresh values). Verified empirically by overwriting and extending the source buffer
// after normalizing seven payload shapes spanning every tier and observing byte-identical
// output. Do NOT hand the result to a consumer that retains it without re-checking that.
func (l *LockedByteBuffer) SnapshotInto(dst []byte) []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	// append rather than a sized make, for its amortized growth: the transcript is
	// longer at every checkpoint, so a destination sized to exactly len(src) would
	// reallocate on every call. (Measured, both variants: a sized make landed within
	// ~5 % of this one, so the growth strategy is not where the win comes from —
	// reusing the destination at all is. Recorded because the intuition that append
	// would be clearly better did not survive measurement.)
	//
	// dst[:0] re-slices before copying, so no stale tail from a previous, longer
	// snapshot can survive into the returned slice.
	return append(dst[:0], l.buf.Bytes()...)
}

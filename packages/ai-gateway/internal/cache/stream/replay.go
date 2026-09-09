package streamcache

import (
	"context"
	"io"
	"sync/atomic"

	cache "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/core"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// NewReplaySubscription returns a ChunkSubscription that emits the
// cached chunks of entry in order, at full I/O speed. There is no
// inter-frame sleep — replay runs at I/O speed because the upstream
// inference latency is not part of the recorded timeline.
//
// The original chunk granularity from the producing upstream call is
// preserved; SDK streaming UIs see the same multi-chunk arrival
// pattern as a live MISS, just faster. m may be nil to disable
// Prometheus instrumentation.
func NewReplaySubscription(entry *cache.StreamEntry, m *Metrics) ChunkSubscription {
	return &replaySub{chunks: entry.Chunks, metrics: m}
}

type replaySub struct {
	chunks  []cache.ChunkRecord
	metrics *Metrics
	idx     int
	closed  atomic.Bool
}

func (r *replaySub) Next(ctx context.Context) (provcore.Chunk, error) {
	if r.closed.Load() {
		return provcore.Chunk{}, io.EOF
	}
	if err := ctx.Err(); err != nil {
		return provcore.Chunk{}, err
	}
	if r.idx >= len(r.chunks) {
		return provcore.Chunk{}, io.EOF
	}
	return r.take(), nil
}

// TryNext implements ReadySubscription. Every chunk of a replay is already in
// memory, so "ready" is just "not past the end" — the whole timeline is
// available from the first call, which is what lets the reader hand the client
// a completed response in one write instead of one per recorded frame.
//
// Unlike Next it does not consult a context. It never waits, so there is nothing
// for a cancellation to interrupt; a cancelled caller stops asking, and the
// blocking Next it returns to reports ctx.Err() as it always did.
func (r *replaySub) TryNext() (provcore.Chunk, bool) {
	if r.closed.Load() || r.idx >= len(r.chunks) {
		return provcore.Chunk{}, false
	}
	return r.take(), true
}

// take consumes the chunk at the cursor. Both entry points go through it so the
// two can never disagree about which fields a replayed chunk carries — a drift
// that would surface as a HIT whose frames differ from the MISS that recorded
// them, in whichever field the copy forgot.
func (r *replaySub) take() provcore.Chunk {
	rec := r.chunks[r.idx]
	r.idx++
	r.metrics.IncReplayChunks()
	return provcore.Chunk{
		Delta:          rec.Delta,
		ReasoningDelta: rec.ReasoningDelta,
		ToolCallDeltas: rec.ToolCallDeltas,
		NexusThinking:  rec.NexusThinking,
		Usage:          rec.Usage,
		Done:           rec.Done,
		NativeEvent:    rec.NativeEvent,
		// RawBytes preserved verbatim from the producing upstream call so HIT
		// replay is byte-equivalent to a live MISS — full envelope (id,
		// created, model, system_fingerprint, finish_reason, …), not a
		// synthesized minimal frame. The chunkSSEReader's same-ingress fast
		// path consumes RawBytes directly. For cross-ingress replay the
		// canonical Delta / ToolCallDeltas / etc. fields above are used.
		RawBytes: rec.RawBytes,
	}
}

func (r *replaySub) Close() error {
	r.closed.Store(true)
	return nil
}

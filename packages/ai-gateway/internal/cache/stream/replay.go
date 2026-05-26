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
	rec := r.chunks[r.idx]
	r.idx++
	r.metrics.IncReplayChunks()
	return provcore.Chunk{
		Delta:          rec.Delta,
		ReasoningDelta: rec.ReasoningDelta,
		ToolCallDeltas: rec.ToolCallDeltas,
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
	}, nil
}

func (r *replaySub) Close() error {
	r.closed.Store(true)
	return nil
}

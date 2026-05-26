// Package streamcache provides per-cache-key broker fan-out for
// streaming and non-streaming upstream calls, plus replay of cached
// chunk timelines for HIT.
package streamcache

import (
	"context"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// ChunkSubscription is the read side used by both cached-HIT replay
// and live broker fan-out. The downstream pipeline (transcoder,
// LivePipeline, hook, writer) is source-agnostic — it consumes a
// ChunkSubscription regardless of whether chunks come from Redis or
// from a live upstream session.
type ChunkSubscription interface {
	// Next returns the next chunk in order, or io.EOF when the
	// stream finished cleanly, or *provcore.ProviderError on
	// upstream failure / broker-broadcasted error. May also return
	// ctx.Err() if the caller's context is cancelled.
	Next(ctx context.Context) (provcore.Chunk, error)

	// Close releases the subscription. On the broker path this
	// decrements ref-count. Close is idempotent.
	Close() error
}

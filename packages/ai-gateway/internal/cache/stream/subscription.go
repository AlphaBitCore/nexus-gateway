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

// ReadySubscription is the optional non-blocking half of ChunkSubscription. A
// subscription that can answer "is the next chunk already here?" without
// waiting implements it; one that cannot simply does not, and every caller
// behaves exactly as before against it.
//
// It exists for replay. A cached stream is a slice already sitting in memory,
// yet the relay takes one chunk per Read and the pump writes and flushes once
// per chunk — one syscall per frame for a response that finished being produced
// some time ago. That is not the price of being live; it is the live path's
// shape applied where nothing is live.
//
// What it deliberately does NOT offer is a way to WAIT for more. There is no
// window, no linger, no batch size — because on a genuinely live stream the
// reader is always ahead of the model and the only way to find something to
// batch is to delay a token that was ready to go. This repo has already measured
// what holding does to a stream: an 8KB Model-A window turned a typical answer
// into two hundred frames that all arrived after generation finished. TryNext
// can only ever collect what a caller would have found on its next trip anyway.
type ReadySubscription interface {
	// TryNext returns the next chunk when one is ALREADY available and reports
	// ok. It never blocks. ok=false means "nothing is queued right now" and says
	// nothing about whether a chunk will arrive later — the caller must still go
	// back to Next, which is also where io.EOF and stream errors are reported.
	TryNext() (provcore.Chunk, bool)
}

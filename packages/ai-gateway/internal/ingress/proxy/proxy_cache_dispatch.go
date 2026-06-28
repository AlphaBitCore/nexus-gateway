package proxy

import (
	"context"

	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
)

// dispatchStreamMode is the single switch site that routes an SSE
// request to the correct streaming pipeline based on the admin
// streampolicy.Mode. Kept separate from the relay stage
// (stream_relay.go) so the dispatch contract can be unit-tested in
// isolation with a small switch-table assertion.
//
// Three-service alignment: the `default` arm
// MUST fall through to passthrough, matching the same default in
// shared/transport/tlsbump/sse.go's resolveStreamingMode. An unknown
// or future mode enum must NOT silently engage the live (hook-
// running) pipeline against traffic the admin has not explicitly
// opted into — the conservative choice is to relay bytes unchanged
// and let validation surface the bad enum upstream.
func dispatchStreamMode(ctx context.Context, mode streampolicy.Mode, deps runStreamDeps) {
	switch mode {
	case streampolicy.ModeBufferFullBlock:
		runBufferStream(ctx, deps)
	case streampolicy.ModePassThrough:
		runPassthroughStream(ctx, deps)
	case streampolicy.ModeChunkedAsync:
		runLiveStream(ctx, deps)
	default:
		runPassthroughStream(ctx, deps)
	}
}

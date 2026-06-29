package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/streaming"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
)

// runStreamDeps bundles the dependencies the WIRE-consuming SSE dispatchers
// (runLiveStream / runPassthroughStream) need. The dispatch site at the relay
// stage builds it once so dispatchStreamMode is a single switch over a single
// struct (#115).
//
// Buffer mode does NOT use this bundle: it is the canonical S-canon LOCUS and
// consumes the ChunkSubscription pre-transcode directly (runCanonicalBufferStream),
// so the relay stage routes it separately without building the wire reader.
type runStreamDeps struct {
	Deps         *Deps
	AdapterType  string
	Path         string
	AcceptHeader string
	HookRunner   streaming.StreamHookRunner
	HookCtx      *streaming.StreamHookContext
	SSEReader    io.Reader
	// Tee is both an io.Writer and an http.ResponseWriter (LivePipeline needs
	// Flusher). The production streamCaptureTee satisfies both interfaces.
	Tee    http.ResponseWriter
	Logger *slog.Logger
	// EmitDone is a live-mode-only LiveConfig field (OpenAI `data: [DONE]`).
	EmitDone bool
	// HasResponseHooks is false when the stream-entry probe found no
	// response-stage rules. Live mode uses it to skip the Registry-normalize
	// PreHook (and its raw-accumulating TeeReader) — there is no hook to consume
	// the normalized payload, so computing it is pure overhead.
	HasResponseHooks bool
	// MaxBufferBytes resolves admin streampolicy MaxBufferBytes into the live
	// pipeline (LiveConfig.MaxBufferSize). Zero means "use the pipeline's
	// built-in default" (8MB).
	MaxBufferBytes int
}

// dispatchStreamMode is the switch site that routes an SSE request to the
// correct WIRE-consuming streaming pipeline based on the admin
// streampolicy.Mode. Kept separate from the relay stage (stream_relay.go) so
// the dispatch contract can be unit-tested in isolation with a small
// switch-table assertion.
//
// Buffer mode (streampolicy.ModeBufferFullBlock) is NOT dispatched here: it is
// the locked S-canon LOCUS that consumes the CANONICAL ChunkSubscription
// pre-transcode (runCanonicalBufferStream), so the relay stage handles it
// directly without building the wire chunkSSEReader these pipelines read. This
// switch therefore covers only the wire-consuming modes (live / passthrough);
// ModeBufferFullBlock reaching it would (correctly) fall through to passthrough,
// but the relay never routes buffer traffic this way.
//
// Three-service alignment (#115/R2 follow-up): the `default` arm MUST fall
// through to passthrough, matching the same default in
// shared/transport/tlsbump/sse.go's resolveStreamingMode. An unknown or future
// mode enum must NOT silently engage the live (hook-running) pipeline against
// traffic the admin has not explicitly opted into — the conservative choice is
// to relay bytes unchanged and let validation surface the bad enum upstream.
func dispatchStreamMode(ctx context.Context, mode streampolicy.Mode, deps runStreamDeps) {
	switch mode {
	case streampolicy.ModePassThrough:
		runPassthroughStream(ctx, deps)
	case streampolicy.ModeChunkedAsync:
		runLiveStream(ctx, deps)
	default:
		runPassthroughStream(ctx, deps)
	}
}

package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/streaming"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/responseprehook"
	sharedstreaming "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming"
)

// runStreamDeps bundles the dependencies the SSE dispatchers need.
// Shared by both live (runLiveStream) and buffer (runBufferStream)
// modes so the dispatch site at proxy_cache.go's hot path is a single
// if/else over a single struct.
type runStreamDeps struct {
	Deps         *Deps
	AdapterType  string
	Path         string
	AcceptHeader string
	HookRunner   streaming.StreamHookRunner
	HookCtx      *streaming.StreamHookContext
	SSEReader    io.Reader
	// Tee is both an io.Writer (BufferPipeline) and an
	// http.ResponseWriter (LivePipeline needs Flusher). The production
	// streamCaptureTee satisfies both interfaces.
	Tee    http.ResponseWriter
	Logger *slog.Logger
	// HoldBack + EmitDone are live-mode-only LiveConfig fields. Buffer
	// mode ignores them (whole-body read + replay is the only behaviour).
	HoldBack bool
	EmitDone bool
	// HasResponseHooks is false when the stream-entry probe found no
	// response-stage rules. Live mode uses it to skip the Registry-normalize
	// PreHook (and its raw-accumulating TeeReader) — there is no hook to
	// consume the normalized payload, so computing it is pure overhead.
	HasResponseHooks bool
	// MaxBufferBytes resolves admin streampolicy MaxBufferBytes through
	// to both buffer (BufferConfig.MaxBufferSize) and live
	// (LiveConfig.MaxBufferSize) pipelines. Zero means "use the
	// pipeline's built-in default" (8MB in both). An earlier version
	// of runBufferStream passed BufferConfig{} and silently
	// defaulted regardless of the admin-configured 64MB cap; the cap
	// only affected tlsbump callers.
	MaxBufferBytes int
}

// runBufferStream wires shared.BufferPipeline against the ai-gateway
// hookRunner + StreamHookContext + tee.
// admin streamingMode=buffer_full_block drives ai-gateway's SSE
// handler into whole-body-buffer mode (matches the existing tlsbump
// behaviour used by agent + compliance-proxy).
//
// Buffer mode trade-offs vs live (chunked_async):
//
//   - One hook checkpoint at end-of-body instead of per-N-char
//     checkpoints. Catches more context but delays first byte to
//     client.
//
//   - Modify decisions are not supported (BufferPipeline's switch
//     has no Modify branch). When the response hook returns Modify
//     under buffer mode we log a warning + treat as Approve; the
//     bytes are replayed unchanged. This is documented in
//     sse-streaming-compliance-architecture.md "Asymmetries"
//     subsection.
//
//   - OnCheckpoint fires once with the single end-of-stream result;
//     the StreamHookContext callback is still invoked so rec fields
//     populate the same way as live mode.
func runBufferStream(ctx context.Context, d runStreamDeps) {
	// Production always wires SSEReader +
	// Tee; defensive nil-guard so a future caller that forgets one
	// of the deps doesn't nil-deref into a 502. Symmetric with
	// runPassthroughStream's guard.
	if d.SSEReader == nil || d.Tee == nil {
		return
	}
	hookCtx := d.HookCtx
	if hookCtx == nil {
		// Defensive — caller always supplies one in production, but
		// keep the function nil-safe so an experimental wiring path
		// doesn't panic.
		hookCtx = &streaming.StreamHookContext{}
	}
	baseInput := &hookcore.HookInput{
		RequestID:      hookCtx.RequestID,
		Stage:          "response",
		IngressType:    hookCtx.IngressType,
		Path:           hookCtx.Path,
		Method:         hookCtx.Method,
		Model:          hookCtx.Model,
		SourceIP:       hookCtx.SourceIP,
		ProviderRegion: hookCtx.ProviderRegion,
	}

	// Adapter: shared.BufferPipeline takes a PipelineExecutor
	// interface; ai-gateway built its hookRunner as a func value of
	// the same signature. Wrap once so the buffer pipeline sees a
	// type that satisfies the interface.
	executor := bufferModeExecutor{run: d.HookRunner}

	bp := sharedstreaming.NewBufferPipeline(sharedstreaming.BufferConfig{
		MaxBufferSize: d.MaxBufferBytes,
	}, executor, d.Logger)

	// PreHook — install the same Registry-normalize-before-hooks
	// callback the live path uses, so the buffer pipeline's single
	// checkpoint sees structured Normalized rather than flat-text.
	if cb := buildBufferPreHookCallback(ctx, d.Deps, d.AdapterType, d.Path, d.AcceptHeader); cb != nil {
		bp.WithPreHook(cb)
	}

	result, err := bp.Process(ctx, d.SSEReader, d.Tee, baseInput)
	if err != nil {
		d.Logger.Error("buffer pipeline error", "error", err)
	}
	// Mirror live mode's OnCheckpoint callback so rec fields populate
	// regardless of dispatch path — admins reading TrafficEvent rows
	// see ResponseHookDecision / Reason / Tags consistently across
	// modes.
	if result != nil && hookCtx.OnCheckpoint != nil {
		hookCtx.OnCheckpoint(result)
	}
}

// bufferModeExecutor adapts ai-gateway's StreamHookRunner (function
// type) to shared.PipelineExecutor (interface). Same signature shape;
// the adapter is a 1-line method.
//
// The Modify-degradation log + Prometheus counter
// (nexus_streaming_modify_degraded_total) live inside
// shared.BufferPipeline so all three data planes emit the
// same signal from a single registration. This adapter is purely a
// type bridge.
type bufferModeExecutor struct {
	run streaming.StreamHookRunner
}

func (b bufferModeExecutor) Execute(ctx context.Context, input *hookcore.HookInput) *hookcore.CompliancePipelineResult {
	return b.run(ctx, input)
}

// buildBufferPreHookCallback is the buffer-mode counterpart to
// buildStreamPreHookCallback. Same underlying responseprehook.Build
// helper; the only difference is the returned type fits
// sharedstreaming.PreHookCallback (BufferPipeline.WithPreHook accepts
// that type). Both PreHookCallback aliases resolve to
// hookcore.PreHookCallback at the type level so the underlying
// function value is identical.
func buildBufferPreHookCallback(ctx context.Context, deps *Deps, adapterType, path, accept string) sharedstreaming.PreHookCallback {
	if deps == nil {
		return nil
	}
	return responseprehook.Build(responseprehook.Options{
		Ctx:          ctx,
		Registry:     deps.NormalizeRegistry,
		AdapterID:    adapterType,
		EndpointPath: path,
		ContentType:  accept,
		Direction:    normcore.DirectionResponse,
	})
}

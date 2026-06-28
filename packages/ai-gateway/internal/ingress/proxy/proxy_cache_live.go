package proxy

import (
	"context"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/streaming"
)

// runLiveStream wires ai-gateway.LivePipeline (chunked_async) against
// the SSE handler's deps. Symmetric with runBufferStream — both are
// dispatched from the streaming relay stage (stream_relay.go) based
// on the admin streaming-policy mode.
//
// Live-mode specifics:
//   - HoldBack (admin-driven via response-pipeline probe at the caller)
//     buffers assistant deltas server-side until the first compliance
//     checkpoint approves.
//   - EmitOpenAIDone appends the `data: [DONE]\n\n` terminator for
//     OpenAI-shape ingress clients (Anthropic / Gemini SDKs choke on
//     stray [DONE] frames).
//   - PreHook callback fires per checkpoint with cumulative bytes,
//     same Registry pipeline as buffer + tlsbump paths.
func runLiveStream(ctx context.Context, d runStreamDeps) {
	// Production always wires SSEReader +
	// Tee; defensive nil-guard symmetric with runPassthroughStream /
	// runBufferStream so a malformed runStreamDeps doesn't nil-deref
	// into a 502.
	if d.SSEReader == nil || d.Tee == nil {
		return
	}
	lp := streaming.NewLivePipeline(streaming.LiveConfig{
		HoldBack:       d.HoldBack,
		EmitOpenAIDone: d.EmitDone,
		MaxBufferSize:  d.MaxBufferBytes,
	}, d.HookRunner, nil, d.Logger)

	// The PreHook re-normalizes the cumulative raw SSE through the Registry at
	// every checkpoint so a response hook sees the same structured payload the
	// non-stream path produces. When no response-stage rule is wired
	// (HasResponseHooks=false) nothing consumes that payload — installing it
	// would only pay the per-checkpoint normalize + the raw-accumulating
	// TeeReader for no behavioural effect. Skip it; the pipeline falls back to
	// the flat-text Normalized fill, which an absent hook never reads anyway.
	if d.HasResponseHooks {
		if cb := buildStreamPreHookCallback(ctx, d.Deps, d.AdapterType, d.Path, d.AcceptHeader); cb != nil {
			lp.WithPreHook(cb)
		}
	}

	// LivePipeline.Process returns a blocked bool, deliberately
	// discarded here. The discard is intentional: hookCtx.OnCheckpoint (closure built
	// at proxy_cache.go around line 704) already fires INSIDE Process
	// with the full pipeline result BEFORE the Decision switch, so
	// audit-row fields (ResponseHookDecision, Reason, ComplianceTags)
	// are stamped on RejectHard the same way they are on Approve. The
	// bool carries no information the audit path doesn't already have.
	_ = lp.Process(ctx, d.SSEReader, d.Tee, d.HookCtx)
}

package proxy

import (
	"context"
	"errors"
	"net"

	sharedstreaming "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming"
)

// runPassthroughStream wires the passthrough streaming mode — admin
// explicitly opted out of hook inspection, so the helper just relays
// upstream bytes to the client (the tee captures them along the way
// for response cache + audit). No hookRunner, no PreHook callback,
// no checkpoint logic.
//
// #115/R1 three-service alignment: this delegates to
// shared.Passthrough — the same relay used by tlsbump (agent +
// compliance-proxy). Previously this file carried a near-duplicate
// io.Copy + flushingWriter implementation; now there is one
// passthrough relay across all three services (#115/O8). The shared
// helper handles per-read flush against http.Flusher writers and
// respects context cancellation, matching what the original local
// impl did.
//
// What admins should expect from this mode:
//   - bytes flow client-bound as fast as upstream delivers them
//     (no per-checkpoint hold-back)
//   - response cache + audit row still populate via the surrounding
//     handler's pcStream.StoreResponseBody / emitAudit code path —
//     passthrough opts OUT of hook inspection, not OUT of audit
//   - Modify / RejectHard / Approve decisions are not applicable;
//     no hook runs to produce one
func runPassthroughStream(ctx context.Context, d runStreamDeps) {
	if d.SSEReader == nil || d.Tee == nil {
		return
	}
	err := sharedstreaming.Passthrough(ctx, d.SSEReader, d.Tee)
	if err == nil {
		return
	}
	// PR #24 follow-up S2-code: distinguish ctx-cancel (client gave
	// up — silent is fine) from a genuine upstream error (operators
	// need to see it). The prior `ctx.Err() == nil` guard collapsed
	// both cases together when an upstream Read error coincided with
	// an external cancel; the upstream signal was lost. Compare the
	// error itself against the context sentinels so we only silence
	// the cases we intend to silence.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	// PR #24 follow-up R-6: when CloseUpstreamOnExit fires on a sibling
	// pipeline (or the outer caller's defer Close races this passthrough
	// relay), io.Copy returns net.ErrClosed. That's not a ctx sentinel
	// but it IS the expected post-close path — silencing it prevents
	// per-teardown log noise that operators have to filter out.
	if errors.Is(err, net.ErrClosed) {
		return
	}
	requestID := ""
	if d.HookCtx != nil {
		requestID = d.HookCtx.RequestID
	}
	d.Logger.Warn("passthrough stream copy error",
		"requestID", requestID,
		"error", err,
	)
}

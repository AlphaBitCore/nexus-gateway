package tlsbump

import (
	"context"
	"log/slog"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming"
)

// sseFrameRedactor is the tlsbump implementation of streaming.FrameRedactor.
// It turns a buffered SSE timeline into a redacted one when a response-stage
// hook returns Modify, by splicing the masked text into the per-host text
// frames and passing every non-text frame byte-verbatim. The per-host decode
// is delegated to the matched interception adapter's ExtractStreamChunk, so
// non-OpenAI wires redact correctly.
//
// Fail posture is PER-CALLER, carried by strictFailClosed (sourced from
// bumpOptions.strictFailClosed at the build site). Whenever the masked wire
// cannot be soundly reconstructed, REDACT_INFLIGHT_UNSUPPORTED is always
// stamped on the result so the audit trail is honest, and then:
//
//   - strictFailClosed=false (agent NE host-packet path): forward the ORIGINAL
//     events with a nil error (FAIL-OPEN). The host's outbound packet path must
//     never fail closed (CLAUDE.md NE safety rule); a hung/blocked redaction
//     would take down the Mac's whole network.
//   - strictFailClosed=true (compliance-proxy appliance): return
//     streaming.ErrRewriteUnsupported (FAIL-CLOSED). The buffer pipeline then
//     emits the policy error frame and replays no original byte, so zero
//     unredacted content reaches the client — matching the ai-gateway sibling's
//     fail-closed posture on an unproducible redaction.
type sseFrameRedactor struct {
	codec            streaming.WireTextCodec
	adapter          traffic.Adapter
	logger           *slog.Logger
	strictFailClosed bool
}

// adapterWireCodec adapts a traffic.Adapter's ExtractStreamChunk to the
// provider-agnostic streaming.WireTextCodec the splice logic consumes.
type adapterWireCodec struct {
	ctx     context.Context
	adapter traffic.Adapter
	path    string
}

// ChunkText returns the visible text a single SSE frame carries on its native
// wire. A frame with no text segments (tool_use / ping / role / [DONE] /
// reasoning-only) or a decode error reports ok=false and is passed verbatim.
func (c adapterWireCodec) ChunkText(data string) (string, bool) {
	nc, err := c.adapter.ExtractStreamChunk(c.ctx, []byte(data), c.path)
	if err != nil {
		return "", false
	}
	txt := strings.Join(nc.Segments, "")
	return txt, txt != ""
}

// newSSEFrameRedactor builds the redactor for one SSE response. Returns nil
// when no adapter is available (the buffer pipeline then keeps its
// backward-compatible Modify-degrade behavior rather than redacting).
// strictFailClosed selects the unreconstructable-wire posture (see the type
// doc): false = fail-open forward-original (agent), true = fail-closed (CP).
func newSSEFrameRedactor(ctx context.Context, adapter traffic.Adapter, path string, strictFailClosed bool, logger *slog.Logger) *sseFrameRedactor {
	if adapter == nil {
		return nil
	}
	return &sseFrameRedactor{
		codec:            adapterWireCodec{ctx: ctx, adapter: adapter, path: path},
		adapter:          adapter,
		logger:           logger,
		strictFailClosed: strictFailClosed,
	}
}

// RedactReplay implements streaming.FrameRedactor.
func (r *sseFrameRedactor) RedactReplay(events []*streaming.SSEEvent, result *core.CompliancePipelineResult) ([]*streaming.SSEEvent, error) {
	if result == nil {
		return events, nil
	}

	// (1) Tool-call argument masking is undeliverable on the streaming wire:
	//     tool_use / tool_call frames are passed byte-verbatim (their arguments
	//     stream as fragmented JSON deltas; the ai-gateway canonical-buffer path
	//     owns tool-arg redaction). GuardToolArgMasking is the shared gate that
	//     the non-stream request path also uses — for a per-host adapter that is
	//     not a traffic.ToolArgMasker it reports ErrRewriteUnsupported. We
	//     additionally treat ANY tool-arg span as unsupported, so even a
	//     ToolArgMasker adapter never leaks an unmasked tool argument through a
	//     verbatim frame (posture handled by redactUnsupported per caller).
	guard := traffic.NormalizedContent{ToolCallArgs: toolArgSentinel(result.TransformSpans)}
	if traffic.GuardToolArgMasking(r.adapter, guard) != nil || len(guard.ToolCallArgs) > 0 {
		return r.redactUnsupported(events, result, "tool-call argument masking is undeliverable on the streaming wire", failReasonToolArg)
	}

	// (2) Splice the text frames; non-text frames pass byte-verbatim. A false
	//     return means the masked wire could not be soundly reconstructed —
	//     usually a divergence between the normalized text the spans were
	//     computed against and the per-frame ExtractStreamChunk wire transcript
	//     (the splice's divergence fence). The counter exposes how often this
	//     happens so operators can see real redaction could not be applied
	//     (then forwarded-original for the agent, or blocked for the appliance).
	redacted, ok := streaming.SpliceTextFrames(events, result, r.codec)
	if !ok {
		return r.redactUnsupported(events, result, "frame splice could not reconstruct the masked wire", failReasonSplice)
	}
	return redacted, nil
}

// fail reasons are bounded metric labels for nexus_streaming_modify_degraded_total,
// distinguishing the two root causes of an unreconstructable masked wire so
// operators can tell a structurally-undeliverable tool-arg mask apart from a
// wire/normalized divergence (the actionable signal — real text redaction could
// not be applied for some host). These root-cause labels are recorded in BOTH
// postures; in the fail-closed posture the buffer pipeline additionally bumps
// its coarse "redact_inflight_unsupported" counter on the returned error.
const (
	failReasonToolArg = "tlsbump_tool_arg_undeliverable"
	failReasonSplice  = "tlsbump_splice_divergence"
)

// redactUnsupported handles an unreconstructable masked wire. It always stamps
// the disclosed degraded reason on the result (audit honesty in both postures)
// and bumps the modify-degraded counter under the root-cause label, then
// branches on the per-caller fail posture (see the type doc):
//
//   - strictFailClosed=false (agent): forward the ORIGINAL events with a nil
//     error (fail-open). No original byte is dropped; the host packet path
//     stays open.
//   - strictFailClosed=true (compliance-proxy): return ErrRewriteUnsupported so
//     the buffer pipeline fails CLOSED — it emits the policy error frame and
//     replays no original byte, so zero unredacted content reaches the client.
func (r *sseFrameRedactor) redactUnsupported(events []*streaming.SSEEvent, result *core.CompliancePipelineResult, reason, metricReason string) ([]*streaming.SSEEvent, error) {
	result.ReasonCode = core.ReasonRedactInflightUnsupported
	streaming.RecordModifyDegraded(metricReason)
	if r.strictFailClosed {
		if r.logger != nil {
			r.logger.Warn("SSE buffer redaction unsupported; failing closed (strict appliance, no original forwarded)",
				"reason", reason,
				"reasonCode", core.ReasonRedactInflightUnsupported,
				"metricReason", metricReason,
			)
		}
		return nil, streaming.ErrRewriteUnsupported
	}
	if r.logger != nil {
		r.logger.Warn("SSE buffer redaction unsupported; forwarding original body (fail-open, agent host-packet path)",
			"reason", reason,
			"reasonCode", core.ReasonRedactInflightUnsupported,
			"metricReason", metricReason,
		)
	}
	return events, nil
}

// toolArgSentinel returns a non-empty ToolCallArgs marker iff any span masks a
// tool-call argument leaf (address shape ...toolUse.input...), so
// GuardToolArgMasking engages. nil when no tool argument was masked.
func toolArgSentinel(spans []normalize.TransformSpan) []string {
	for i := range spans {
		if strings.Contains(spans[i].ContentAddress, ".toolUse.input.") {
			return []string{""}
		}
	}
	return nil
}

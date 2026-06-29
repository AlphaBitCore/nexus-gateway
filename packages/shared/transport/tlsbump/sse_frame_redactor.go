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
// FAIL-OPEN posture (best-effort, matching the non-stream tlsbump request path
// and the NE host-packet fail-open safety rule): whenever the masked wire
// cannot be soundly reconstructed, RedactReplay forwards the ORIGINAL events
// with a nil error and stamps REDACT_INFLIGHT_UNSUPPORTED on the result so the
// audit trail is honest. It never returns streaming.ErrRewriteUnsupported —
// that signals the buffer pipeline to fail CLOSED, which is the wrong posture
// for tlsbump's best-effort packet path.
type sseFrameRedactor struct {
	codec   streaming.WireTextCodec
	adapter traffic.Adapter
	logger  *slog.Logger
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
func newSSEFrameRedactor(ctx context.Context, adapter traffic.Adapter, path string, logger *slog.Logger) *sseFrameRedactor {
	if adapter == nil {
		return nil
	}
	return &sseFrameRedactor{
		codec:   adapterWireCodec{ctx: ctx, adapter: adapter, path: path},
		adapter: adapter,
		logger:  logger,
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
	//     additionally fail open on ANY tool-arg span, so even a ToolArgMasker
	//     adapter never leaks an unmasked tool argument through a verbatim frame.
	guard := traffic.NormalizedContent{ToolCallArgs: toolArgSentinel(result.TransformSpans)}
	if traffic.GuardToolArgMasking(r.adapter, guard) != nil || len(guard.ToolCallArgs) > 0 {
		return r.failOpen(events, result, "tool-call argument masking is undeliverable on the streaming wire", failOpenReasonToolArg)
	}

	// (2) Splice the text frames; non-text frames pass byte-verbatim. A false
	//     return means the masked wire could not be soundly reconstructed —
	//     usually a divergence between the normalized text the spans were
	//     computed against and the per-frame ExtractStreamChunk wire transcript
	//     (the splice's divergence fence). The counter exposes how often this
	//     happens so operators can see real redaction degrading to forward-original.
	redacted, ok := streaming.SpliceTextFrames(events, result, r.codec)
	if !ok {
		return r.failOpen(events, result, "frame splice could not reconstruct the masked wire", failOpenReasonSplice)
	}
	return redacted, nil
}

// failOpen reasons are bounded metric labels for nexus_streaming_modify_degraded_total,
// distinguishing the two best-effort degradations of the tlsbump packet path so
// operators can tell a structurally-undeliverable tool-arg mask apart from a
// wire/normalized divergence (the actionable signal — it means real text
// redaction is silently degrading to forward-original for some host).
const (
	failOpenReasonToolArg = "tlsbump_tool_arg_undeliverable"
	failOpenReasonSplice  = "tlsbump_splice_divergence"
)

// failOpen forwards the original events unchanged, stamps the disclosed degraded
// reason on the result so the audit row reflects that the inflight redaction was
// not applied (no original is dropped, no error is surfaced), and bumps the
// modify-degraded counter so the fail-open rate is observable.
func (r *sseFrameRedactor) failOpen(events []*streaming.SSEEvent, result *core.CompliancePipelineResult, reason, metricReason string) ([]*streaming.SSEEvent, error) {
	result.ReasonCode = core.ReasonRedactInflightUnsupported
	streaming.RecordModifyDegraded(metricReason)
	if r.logger != nil {
		r.logger.Warn("SSE buffer redaction unsupported; forwarding original body (fail-open)",
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

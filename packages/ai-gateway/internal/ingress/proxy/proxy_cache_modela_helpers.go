package proxy

// proxy_cache_modela_helpers.go — standalone helpers for the Model-A streaming
// mode (split from proxy_cache_modela.go along the helper seam to keep the main
// file under the size ratchet): the content-byte sizer + audit stamper, the cheap
// prescan builder, and the single-encoder wire-frame writers.

import (
	"context"
	"io"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// modelAContentSize returns the redactable-content byte size of a chunk — the
// channels the prescan/confirm scan and the enforcer redacts (assistant delta +
// tool-call arguments + name + id). Used to size the tail-hold window in content
// bytes (not total canonical bytes) so reasoning/non-content chunks do not evict
// redactable content from the window early.
func modelAContentSize(c provcore.Chunk) int {
	n := len(c.Delta)
	for _, d := range c.ToolCallDeltas {
		n += len(d.Arguments) + len(d.Name) + len(d.ID)
	}
	return n
}

// stampModelAResponseHook records a response-stage hook outcome onto the audit
// record, mirroring the wire/buffer paths (redactCanonicalBuffer). Without it a
// Model-A stream that approved or hit a false positive would leave
// ResponseHookDecision empty — indistinguishable to a SIEM from "no response
// hook configured" — and would lose any tags the confirm carried.
func stampModelAResponseHook(rec *audit.Record, res *hookcore.CompliancePipelineResult) {
	if rec == nil || res == nil {
		return
	}
	rec.ResponseHookDecision = string(res.Decision)
	rec.ResponseHookReason = res.Reason
	rec.ResponseHookReasonCode = res.ReasonCode
	rec.ComplianceTags = mergeTagSets(rec.ComplianceTags, res.Tags)
	rec.HooksPipeline = appendHookTrace(rec.HooksPipeline, "response", res.HookResults)
	rec.ResponseAction = hookcore.ActionFromDecision(res.Decision)
}

// buildResponsePrescan resolves the cheap union prefilter for the Model-A gate.
// It builds the response-stage probe pipeline (the same shape the hooks stage
// already probed) and returns its MayMatchRawContent method — a sound prefilter
// that returns false ONLY when no response hook can match the bytes. When the
// probe cannot be built (no cache, build error, or no response rules), it returns
// an "always confirm" prescan so the gate fails safe to a full confirm on every
// checkpoint rather than silently skipping enforcement.
func (h *Handler) buildResponsePrescan(ctx context.Context, s *streamState) func([]byte) bool {
	alwaysConfirm := func([]byte) bool { return true }
	if h.deps == nil || h.deps.HookConfigCache == nil {
		return alwaysConfirm
	}
	var epType hookcore.EndpointType
	if ingress, ok := IngressFromContext(s.r.Context()); ok {
		epType = typology.KindFromWireShape(ingress.WireShape)
	}
	modalities := []hookcore.Modality{hookcore.ModalityText}
	probe, err := h.deps.HookConfigCache.Resolver(ctx).BuildPipeline(
		"response", "AI_GATEWAY", epType, modalities,
		5*time.Second, 15*time.Second, false, true, /* strictFailClosed */
		s.logger,
	)
	if err != nil || probe == nil {
		return alwaysConfirm
	}
	return probe.MayMatchRawContent
}

// writeEncodedChunk forward-encodes one canonical BODY chunk through the shared
// stream encoder and writes the frames to w. Done/Usage are stripped so the
// terminal framing is emitted exactly once by writeEncodedTerminal (mirrors
// emitCanonicalStream's body loop, but over a single long-lived encoder so the
// real-time → escalation switch preserves SSE frame continuity — role header,
// tool-call block indices, sequence — instead of restarting a fresh encoder).
func writeEncodedChunk(ctx context.Context, enc canonicalbridge.StreamTranscoder, w io.Writer, c provcore.Chunk) error {
	c.Done = false
	c.Usage = nil
	b, err := enc.Write(ctx, c)
	if err != nil {
		return err
	}
	if len(b) > 0 {
		if _, werr := w.Write(b); werr != nil {
			return werr
		}
	}
	return nil
}

// writeEncodedTerminal emits the one terminal frame (finish_reason + aggregated
// usage) through the shared encoder, then the OpenAI `data: [DONE]` sentinel when
// emitDone is set. Mirrors emitCanonicalStream's terminal handling.
func writeEncodedTerminal(ctx context.Context, enc canonicalbridge.StreamTranscoder, w io.Writer, emitDone bool, finalUsage *provcore.Usage, finishReason string) error {
	b, err := enc.Write(ctx, provcore.Chunk{Done: true, Usage: finalUsage, FinishReason: finishReason})
	if err != nil {
		return err
	}
	if len(b) > 0 {
		if _, werr := w.Write(b); werr != nil {
			return werr
		}
	}
	if emitDone {
		if _, werr := w.Write([]byte("data: [DONE]\n\n")); werr != nil {
			return werr
		}
	}
	return nil
}

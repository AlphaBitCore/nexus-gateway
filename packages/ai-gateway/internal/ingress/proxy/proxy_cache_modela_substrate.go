package proxy

// proxy_cache_modela_substrate.go — the canonical-subscription substrate that
// drives the shared streaming Model-A engine (shared/transport/streaming/modela).
// The engine owns the prescan-gated tail-hold algorithm; this adapter supplies the
// canonical seams: pulling provider chunks off the ChunkSubscription, extracting
// the redactable channels, forward-encoding through the single long-lived stream
// encoder, confirming via the relay's per-checkpoint hook runner, and escalating to
// the canonical-buffer redaction LOCUS. Its fail posture is fail-CLOSED (the
// gateway is not in the host packet path) — surfaced inside escalateModelA /
// redactCanonicalBuffer as an in-band SSE error frame, never the original body.

import (
	"context"
	"errors"
	"net/http"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/envelope"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	sharedstreaming "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming"
)

// modelACanonicalSubstrate adapts a canonical ChunkSubscription stream to the
// shared Model-A engine. It carries the per-stream delivery context (encoder, tee,
// usage accumulator, terminal-error carrier) and the cheap union prescan; the
// finishReason is observed as chunks arrive so the terminal frame and any
// escalation carry it.
type modelACanonicalSubstrate struct {
	h       *Handler
	s       *streamState
	tee     http.ResponseWriter
	enc     canonicalbridge.StreamTranscoder
	usage   *chunkUsageHolder
	prescan func([]byte) bool
	term    *chunkSSEReader

	// respAcc folds every false-positive confirm's per-hook latency into one record
	// per hook (set from the owning runModelAStream local); authoritativeAppended is
	// flipped true once escalation's redactCanonicalBuffer has written the
	// authoritative full-buffer scan, so the owner's defer does not double-append.
	respAcc               *responseHookAccumulator
	authoritativeAppended *bool

	finishReason string
}

// Next pulls the next canonical chunk, recording usage and the latest finish reason
// as it passes (the engine holds the chunk thereafter and does not surface it back
// except via Deliver / Escalate). io.EOF ends the stream; any other error is
// terminal and routed to OnError by the engine.
func (m *modelACanonicalSubstrate) Next(ctx context.Context) (provcore.Chunk, error) {
	chunk, err := m.s.sub.Next(ctx)
	if err != nil {
		return provcore.Chunk{}, err
	}
	if chunk.Usage != nil {
		m.usage.record(chunk.Usage)
	}
	if chunk.FinishReason != "" {
		m.finishReason = chunk.FinishReason
	}
	return chunk, nil
}

// AppendRedactableText appends the chunk's scannable channels — assistant delta plus
// tool-call arguments / name / id — onto dst, newline-separating the tool-call
// fields so a pattern cannot span two unrelated fields. Reasoning is omitted to
// match the canonical redaction's coverage. Appending onto the engine's buffer keeps
// the hot miss path allocation-free.
func (m *modelACanonicalSubstrate) AppendRedactableText(dst []byte, chunk provcore.Chunk) []byte {
	dst = append(dst, chunk.Delta...)
	for _, d := range chunk.ToolCallDeltas {
		if d.Arguments != "" {
			dst = append(dst, d.Arguments...)
			dst = append(dst, '\n')
		}
		if d.Name != "" {
			dst = append(dst, d.Name...)
			dst = append(dst, '\n')
		}
		if d.ID != "" {
			dst = append(dst, d.ID...)
			dst = append(dst, '\n')
		}
	}
	return dst
}

// UnitBytes is the chunk's total canonical size (the held-bytes ceiling budget).
func (m *modelACanonicalSubstrate) UnitBytes(chunk provcore.Chunk) int {
	return canonicalChunkSize(chunk)
}

// ContentBytes is the chunk's redactable-content size without field separators (the
// tail-window budget): assistant delta + tool-call arguments / name / id, never
// reasoning, so a non-content reasoning chunk does not evict content from the window.
func (m *modelACanonicalSubstrate) ContentBytes(chunk provcore.Chunk) int {
	return modelAContentSize(chunk)
}

// IsDone reports the canonical terminal chunk.
func (m *modelACanonicalSubstrate) IsDone(chunk provcore.Chunk) bool { return chunk.Done }

// Deliver forward-encodes one held chunk's body through the single long-lived
// encoder so the real-time → escalation switch preserves SSE frame continuity. A
// write error is terminal; it is recorded on the carrier and returned.
func (m *modelACanonicalSubstrate) Deliver(ctx context.Context, chunk provcore.Chunk) error {
	if err := writeEncodedChunk(ctx, m.enc, m.tee, chunk); err != nil {
		m.term.termErr.Store(&streamTerminalError{code: streamErrCodeUpstream, err: err})
		return err
	}
	return nil
}

// DeliverTerminal emits the one terminal frame (finish reason + aggregated usage)
// plus the OpenAI sentinel when the ingress expects it.
func (m *modelACanonicalSubstrate) DeliverTerminal(ctx context.Context) error {
	snap := m.usage.snapshot()
	var finalUsage *provcore.Usage
	if usageHasAny(snap) {
		finalUsage = &snap
	}
	if err := writeEncodedTerminal(ctx, m.enc, m.tee, m.s.emitDone, finalUsage, m.finishReason); err != nil {
		m.term.termErr.Store(&streamTerminalError{code: streamErrCodeUpstream, err: err})
		return err
	}
	return nil
}

// Prescan is the cheap union prefilter (closing over the response probe's
// MayMatchRawContent); a nil prescan was already replaced with "always confirm" at
// construction, so this is never nil.
func (m *modelACanonicalSubstrate) Prescan(content []byte) bool { return m.prescan(content) }

// Confirm runs ONE full response-stage confirm over the accumulated canonical
// content via the relay's per-checkpoint hook runner. modelAConfirm ALWAYS returns
// a non-nil result — RejectHard on a missing/failed runner, otherwise the pipeline's
// non-nil aggregate (Execute never returns nil) — so the engine's nil-as-approve
// branch and OnConfirmApproved(nil) are never exercised on the canonical path, and a
// missing enforcer never streams unredacted.
func (m *modelACanonicalSubstrate) Confirm(ctx context.Context, content string) *hookcore.CompliancePipelineResult {
	return m.h.modelAConfirm(ctx, m.s, content, m.usage)
}

// Escalate hands off to the canonical buffer-to-end redaction. trigger is the
// triggering decision on a confirmed hit, or nil on a memory-pressure escalation;
// escalateModelA RE-EVALUATES the buffered remainder on the canonical waist (it does
// not trust trigger's spans and stamps the authoritative response-hook outcome itself),
// so trigger is advisory only and the same hand-off serves both triggers. The
// confirmed-vs-pressure distinction is recorded as a metric dimension only (NOT a
// persisted field, and kept off ResponseHookReasonCode which redactCanonicalBuffer
// overwrites).
func (m *modelACanonicalSubstrate) Escalate(ctx context.Context, held []provcore.Chunk, trigger *hookcore.CompliancePipelineResult) error {
	cause := sharedstreaming.ModelAEscalationConfirmed
	if trigger == nil {
		cause = sharedstreaming.ModelAEscalationMemoryPressure
	}
	sharedstreaming.RecordModelAEscalation(cause)
	m.h.escalateModelA(ctx, m.s, m.tee, m.enc, m.usage, held, m.finishReason, m.term, m.authoritativeAppended)
	return nil
}

// OnConfirmApproved stamps a non-enforcing confirm outcome (false positive or
// nil-as-approve) onto the audit record. stampModelAResponseHook no-ops on a nil
// result.
func (m *modelACanonicalSubstrate) OnConfirmApproved(res *hookcore.CompliancePipelineResult) {
	stampModelAResponseHook(m.s.rec, m.respAcc, res)
}

// OnApproveEOF stamps the response as evaluated-approved when the stream reached EOF
// without any confirm — the sound prescan cleared the whole stream.
func (m *modelACanonicalSubstrate) OnApproveEOF() {
	m.s.rec.ResponseHookDecision = string(hookcore.Approve)
	m.s.rec.ResponseAction = hookcore.ActionApprove
}

// OnError emits the substrate's terminal error framing for a non-EOF Next error: a
// client abort is recorded without a wire frame; an upstream error synthesizes an
// in-band SSE error frame on the ingress wire. Fail-closed: never the original body.
func (m *modelACanonicalSubstrate) OnError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		m.term.termErr.Store(&streamTerminalError{code: streamErrCodeClientAbort, err: err})
		return err
	}
	var pe *provcore.ProviderError
	if !errors.As(err, &pe) {
		pe = &provcore.ProviderError{Status: http.StatusBadGateway, Code: provcore.CodeUpstreamError, Message: err.Error()}
	}
	_, _ = m.tee.Write(envelope.SynthesizeSSEErrorFrame(m.s.ingressFormat, pe))
	m.term.termErr.Store(&streamTerminalError{code: streamErrCodeUpstream, err: err})
	return err
}

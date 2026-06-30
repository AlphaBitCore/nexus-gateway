package proxy

// proxy_cache_modela.go — ai-gateway streaming Model-A mode (05-PLAN §B2).
//
// Model A is the prescan-gated REAL-TIME streaming path for a redact-scope
// chunked_async stream. Like the canonical BUFFER mode it consumes the canonical
// ChunkSubscription PRE-transcode (NOT the wire chunkSSEReader), so the relay
// stage dispatches it as a sibling of runCanonicalBufferStream — it must NOT be
// routed through runStreamDeps / dispatchStreamMode, which build the wire reader
// by draining the same subscription.
//
// Mechanics (05-PLAN §B2 routing table row "Model A"):
//   - chunks are forwarded to the client in real time through a SINGLE stream
//     encoder, EXCEPT the trailing bounded tail (modelATailWindowBytes, ~8KB of
//     canonical content) which is held undelivered;
//   - after every chunk a CHEAP union prescan (Pipeline.MayMatchRawContent over
//     the accumulated canonical content) gates the expensive confirm. The prescan
//     is SOUND: it returns false only when no response hook can possibly match, so
//     a MISS streak streams in real time with zero confirms; a HIT may be a false
//     positive;
//   - on a prescan HIT over content not yet confirmed, ONE full confirm runs via
//     the relay's per-checkpoint hookRunner. Approve (false positive) → release
//     the tail beyond the window and resume real-time streaming. Modify /
//     RejectHard (confirmed) → ESCALATE to buffer-to-END: stop delivering, drain
//     the retained tail + remaining canonical chunks into a fresh accumulator,
//     redact on the canonical waist via redactCanonicalBuffer, and deliver only
//     the redacted REMAINDER through the same encoder (already-delivered prefix is
//     never re-delivered; the held tail is delivered REDACTED, never raw).
//
// The bounded tail is the load-bearing safety guarantee: a PII value (< the tail
// window) is still HELD when its pattern-completing bytes arrive, so the prescan
// HIT + confirm fire while the value is undelivered and the escalation redacts it
// — the COMPLETE value is never delivered. Values larger than the window may leak
// a bounded fragment (the disclosed best-effort-wire risk; strong compliance is
// buffer / block). Because the ai-gateway is NOT in the host packet path, a
// redaction the policy demanded but could not produce fails closed inside
// redactCanonicalBuffer (an in-band SSE error frame, never the original body).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/envelope"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/middleware"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/modela"
)

// modelATailWindowBytes bounds the trailing canonical content held undelivered.
// A PII value shorter than this window is fully retained when its completing
// bytes arrive, so the prescan HIT + escalation redact it before delivery; longer
// values carry the disclosed bounded-fragment risk. ~8KB mirrors the live
// pipeline's scan-window default.
const modelATailWindowBytes = 8 * 1024

// runModelAStream is the prescan-gated real-time streaming handler. It mirrors
// runCanonicalBufferStream's structure (canonical subscription, usage accounting,
// capture tee, terminal-error carrier) but delivers in real time until a prescan
// HIT is confirmed, at which point it escalates to canonical-buffer redaction for
// the remainder. prescan is the cheap union prefilter (closing over the response
// probe's MayMatchRawContent); a nil prescan fails safe to "always confirm".
func (h *Handler) runModelAStream(ctx context.Context, s *streamState, tee http.ResponseWriter, usage *chunkUsageHolder, prescan func([]byte) bool, maxPattern int) *chunkSSEReader {
	term := &chunkSSEReader{ctx: ctx, ingressFormat: s.ingressFormat}

	// respAcc folds every false-positive confirm's per-hook latency into one record
	// per hook; appended once at stream end. authoritativeAppended is set true only
	// after escalation's redactCanonicalBuffer has written the authoritative
	// full-buffer scan — so on escalation success the accumulator is discarded (no
	// double count), while on a normal end OR an escalate-then-abort (overflow /
	// client-abort / upstream-error before the redact append) the defer appends the
	// accumulated confirms so the trace is never lost.
	var respAcc responseHookAccumulator
	authoritativeAppended := false
	defer func() {
		if s.rec != nil && !authoritativeAppended {
			s.rec.HooksPipeline = appendHookTrace(s.rec.HooksPipeline, "response", respAcc.finalize())
		}
	}()

	// Defensive nil-guard, symmetric with the canonical buffer handler.
	if s.sub == nil || tee == nil {
		return term
	}

	if prescan == nil {
		// Fail safe: a missing prefilter must never silently skip enforcement.
		// "Always confirm" is sound (it only ever pays the full confirm more
		// often, never fewer).
		prescan = func([]byte) bool { return true }
	}

	enc := s.transcoder
	if enc == nil {
		enc = fallbackStreamEncoder(s.ingressFormat, s.target.ModelCode)
	}

	maxBuf := s.streamMaxBufferBytes
	if maxBuf <= 0 {
		maxBuf = defaultCanonicalBufferMaxBytes
	}

	// The shared engine owns the prescan-gated tail-hold / confirm / escalate
	// algorithm; modelACanonicalSubstrate supplies the canonical seams (chunk
	// subscription, redactable-channel extraction, single-encoder delivery, the
	// per-checkpoint confirm, and the canonical-buffer escalation). The substrate
	// records all terminal state on `term`, so the engine's returned error — already
	// reflected there — is not consulted here.
	sub := &modelACanonicalSubstrate{
		h:                     h,
		s:                     s,
		tee:                   tee,
		enc:                   enc,
		usage:                 usage,
		prescan:               prescan,
		term:                  term,
		respAcc:               &respAcc,
		authoritativeAppended: &authoritativeAppended,
	}
	// Config-time operator signal (#16): warn once if the derived contiguous-pattern bound
	// meets/exceeds the tail window the engine clamps the lookahead below. Off the per-byte
	// path; maxPattern is already derived (buildResponsePrescan), not recomputed here.
	modela.WarnStreamingCoverageGap(s.logger, maxPattern, modelATailWindowBytes)
	_ = modela.Run(ctx, sub, modela.Config{TailWindowBytes: modelATailWindowBytes, MaxBufferBytes: maxBuf, MaxPatternBytes: maxPattern})
	return term
}

// modelACanonicalSubstrate satisfies the shared engine's Substrate over canonical
// provider chunks.
var _ modela.Substrate[provcore.Chunk] = (*modelACanonicalSubstrate)(nil)

// modelAConfirm runs ONE full response-stage confirm over the accumulated
// canonical content via the relay's per-checkpoint hookRunner (the same pipeline
// the live path runs). It returns the decision only; the actual redaction on a
// confirmed hit is produced later by redactCanonicalBuffer over the remainder.
// When the hookRunner is absent (defensive — the relay always sets it), the
// confirm fails closed to RejectHard so a missing enforcer never delivers.
func (h *Handler) modelAConfirm(ctx context.Context, s *streamState, content string, usage *chunkUsageHolder) *hookcore.CompliancePipelineResult {
	if s.hookRunner == nil {
		return &hookcore.CompliancePipelineResult{Decision: hookcore.RejectHard, Reason: "missing stream hook runner", ReasonCode: "hook_pipeline_error"}
	}
	snap := usage.snapshot()
	input := &hookcore.HookInput{
		RequestID:      s.requestID,
		Stage:          "response",
		Normalized:     hookcore.PayloadFromTextSegments([]string{content}),
		IngressType:    "AI_GATEWAY",
		Path:           "/v1/chat/completions",
		Model:          s.target.ModelCode,
		TokenCount:     usageInt(snap.TotalTokens),
		SourceIP:       middleware.ClientIP(s.r),
		ProviderRegion: s.target.Region,
	}
	return s.hookRunner(ctx, input)
}

// escalateModelA performs the buffer-to-END hand-off on a confirmed hit. It folds
// the retained tail PLUS the remaining canonical chunks into a fresh accumulator,
// redacts on the canonical waist via redactCanonicalBuffer, and delivers ONLY the
// redacted remainder through the shared encoder. Escalation invariants:
//   - no double delivery: the already-delivered prefix is NOT in `held`, so it is
//     never re-emitted;
//   - the held tail is delivered REDACTED, never raw-then-redacted;
//   - SSE frame integrity is preserved by reusing the SAME encoder (role header /
//     tool-block indices / sequence continue) instead of a fresh one;
//   - bounded accumulation: the remainder is capped by streamMaxBufferBytes and
//     fails closed (in-band error frame) on overflow;
//   - fail closed: a RejectHard / unproducible-rewrite is surfaced as an error
//     frame inside redactCanonicalBuffer — the original remainder is never sent.
func (h *Handler) escalateModelA(ctx context.Context, s *streamState, tee http.ResponseWriter, enc canonicalbridge.StreamTranscoder, usage *chunkUsageHolder, held []provcore.Chunk, finishReason string, term *chunkSSEReader, authoritativeAppended *bool) *chunkSSEReader {
	maxBuf := s.streamMaxBufferBytes
	if maxBuf <= 0 {
		maxBuf = defaultCanonicalBufferMaxBytes
	}

	acc := newCanonicalStreamAccumulator(s.target.ModelCode)
	var bufferedBytes int
	remainderDone := false
	for _, c := range held {
		bufferedBytes += canonicalChunkSize(c)
		acc.add(c)
		if c.Done {
			remainderDone = true
		}
	}

	// Drain the remaining canonical chunks into the accumulator (the held tail did
	// not already carry the terminal Done frame).
	for !remainderDone {
		chunk, err := s.sub.Next(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if ctx.Err() != nil {
				term.termErr.Store(&streamTerminalError{code: streamErrCodeClientAbort, err: err})
				return term
			}
			var pe *provcore.ProviderError
			if !errors.As(err, &pe) {
				pe = &provcore.ProviderError{Status: http.StatusBadGateway, Code: provcore.CodeUpstreamError, Message: err.Error()}
			}
			_, _ = tee.Write(envelope.SynthesizeSSEErrorFrame(s.ingressFormat, pe))
			term.termErr.Store(&streamTerminalError{code: streamErrCodeUpstream, err: err})
			return term
		}
		if chunk.Usage != nil {
			usage.record(chunk.Usage)
		}
		if chunk.FinishReason != "" {
			finishReason = chunk.FinishReason
		}
		bufferedBytes += canonicalChunkSize(chunk)
		if bufferedBytes > maxBuf {
			pe := &provcore.ProviderError{
				Status:  http.StatusBadGateway,
				Code:    provcore.CodeUpstreamError,
				Message: fmt.Sprintf("stream exceeded maximum buffer size of %d bytes", maxBuf),
			}
			_, _ = tee.Write(envelope.SynthesizeSSEErrorFrame(s.ingressFormat, pe))
			term.termErr.Store(&streamTerminalError{code: streamErrCodeUpstream, err: pe})
			return term
		}
		acc.add(chunk)
		if chunk.Done {
			break
		}
	}

	ingress, _ := IngressFromContext(s.r.Context())
	snap := usage.snapshot()
	var finalUsage *provcore.Usage
	if usageHasAny(snap) {
		finalUsage = &snap
	}
	tokenTotal := int64(usageInt(snap.TotalTokens))

	outcome := h.redactCanonicalBuffer(s.r, s.rec, ingress, s.target, acc.canonicalBody(), tokenTotal, s.requestID, s.logger)
	// Mark the authoritative append done ONLY when redactCanonicalBuffer actually
	// appended (outcome.appended). On its non-appending early returns — BypassHooks,
	// pipeline build error, nil pipeline — the flag stays false so the caller's defer
	// falls back to appending the accumulated confirm traces (no silent loss). On the
	// normal executed path it appended the authoritative full-buffer scan, so the
	// defer must NOT also append (which would duplicate the response hook rows). Every
	// early return ABOVE this line also leaves the flag false → fallback still covers
	// escalate-then-abort.
	if authoritativeAppended != nil {
		*authoritativeAppended = outcome.appended
	}
	if outcome.failClosed {
		// Hard block / unproducible rewrite: deliver ONLY an in-band SSE error
		// frame — zero remainder content. ResponseHookRewritten stays false so the
		// relay's redacted-copy guard keeps storage at NULL (no leak).
		pe := &provcore.ProviderError{Status: outcome.errStatus, Code: "blocked", Message: outcome.errMsg}
		_, _ = tee.Write(envelope.SynthesizeSSEErrorFrame(s.ingressFormat, pe))
		if ingress.BodyFormat.IsOpenAIFamily() {
			_, _ = tee.Write([]byte("data: [DONE]\n\n"))
		}
		return term
	}

	// Deliver the remainder: the REDACTED canonical body on a Modify, or the
	// accumulated remainder unchanged when the remainder itself carried no PII
	// (the confirm fired on content already in the delivered prefix).
	body := acc.canonicalBody()
	if outcome.rewritten {
		body = outcome.body
	}
	synth := syntheticChunkFromCanonical(body, acc.reasoning.String())
	// Preserve the ORIGINAL wire tool-call indices across the live→escalation
	// switch. canonicalBody renders tool_calls in acc.toolOrder order, so the
	// positional index syntheticChunkFromCanonical assigned (0,1,2…) maps back to
	// the wire index acc.toolOrder[i]. The client already received any pre-tail
	// tool-call prefix (id/name + leading args) under its WIRE index, so the
	// escalated continuation/redacted frame must carry that same wire index — a
	// positional index would split/orphan the tool_call. (Buffer mode delivers
	// nothing in real time, so positional indices are self-consistent there; only
	// Model A, which streamed a prefix, needs the remap.)
	for i := range synth.ToolCallDeltas {
		if i < len(acc.toolOrder) {
			synth.ToolCallDeltas[i].Index = acc.toolOrder[i]
		}
	}
	if werr := writeEncodedChunk(ctx, enc, tee, synth); werr != nil {
		term.termErr.Store(&streamTerminalError{code: streamErrCodeUpstream, err: werr})
		return term
	}
	if werr := writeEncodedTerminal(ctx, enc, tee, s.emitDone, finalUsage, finishReason); werr != nil {
		term.termErr.Store(&streamTerminalError{code: streamErrCodeUpstream, err: werr})
	}
	return term
}

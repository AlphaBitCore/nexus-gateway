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
func (h *Handler) runModelAStream(ctx context.Context, s *streamState, tee http.ResponseWriter, usage *chunkUsageHolder, prescan func([]byte) bool) *chunkSSEReader {
	term := &chunkSSEReader{ctx: ctx, ingressFormat: s.ingressFormat}
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
		enc = canonicalbridge.NewChatCompletionsStreamEncoder(s.target.ModelCode)
	}

	maxBuf := s.streamMaxBufferBytes
	if maxBuf <= 0 {
		maxBuf = defaultCanonicalBufferMaxBytes
	}

	// held holds the trailing bounded-tail chunks that have NOT been delivered to
	// the client. scanBuf accumulates the redactable canonical content the cheap
	// union prescan reads: assistant deltas + tool-call arguments AND tool-call
	// name/id. The enforcer (extractChatResponse) scans the whole tool_calls
	// payload (function name + id + arguments), so the gate must cover the same
	// channels — otherwise a value present only in a tool-call name/id would be
	// invisible to Model A and delivered raw (reasoning is omitted, matching the
	// canonical redaction's coverage). scannedLen high-waters the bytes already
	// prefilter-scanned so the prescan is WINDOWED (new content + a bounded
	// lookbehind) instead of re-scanning the whole buffer every chunk — O(N), not
	// O(N²). contentBytes mirrors heldBytes but counts only scanned-content bytes
	// so reasoning/non-content chunks don't evict content from the tail window
	// early. confirmedLen is the scan length already cleared by a confirm, so a
	// benign false-positive trigger is not re-confirmed until NEW content arrives.
	var (
		held         []provcore.Chunk
		heldBytes    int
		contentBytes int
		scanBuf      []byte
		scannedLen   int
		confirmedLen int
		finishReason string
		anyConfirm   bool
	)
	for {
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

		// Accumulate the redactable canonical content for the prescan, then HOLD
		// the chunk. Tool-call name/id are scanned alongside arguments — the
		// enforcer extracts all three; fields are newline-separated so a pattern
		// cannot span two fields (over-matching a prefilter only wastes a confirm,
		// never misses a match).
		contentStart := len(scanBuf)
		scanBuf = append(scanBuf, chunk.Delta...)
		for _, d := range chunk.ToolCallDeltas {
			if d.Arguments != "" {
				scanBuf = append(scanBuf, d.Arguments...)
				scanBuf = append(scanBuf, '\n')
			}
			if d.Name != "" {
				scanBuf = append(scanBuf, d.Name...)
				scanBuf = append(scanBuf, '\n')
			}
			if d.ID != "" {
				scanBuf = append(scanBuf, d.ID...)
				scanBuf = append(scanBuf, '\n')
			}
		}
		held = append(held, chunk)
		heldBytes += canonicalChunkSize(chunk)
		contentBytes += len(scanBuf) - contentStart

		// Prescan GATE: a HIT over content not yet confirmed pays for ONE full
		// confirm. The prescan is WINDOWED — it scans only the content not yet
		// prefilter-scanned plus a bounded lookbehind (the tail window), so a value
		// spanning chunk boundaries within the still-held tail is detected, without
		// re-scanning the whole accumulated buffer every chunk (O(N²)) or copying
		// it ([]byte(scan.String()) was a full per-chunk copy). A prefilter hit on
		// already-delivered content (scrolled past the lookbehind) is irrelevant —
		// that content can no longer be redacted. confirmedLen gates "new content
		// since the last confirm". The prescan is checked BEFORE the tail is
		// released below, so a chunk that completes a confirmed match is always
		// still held (undelivered) when the escalation redacts it.
		if len(scanBuf) > confirmedLen {
			winStart := scannedLen - modelATailWindowBytes
			if winStart < 0 {
				winStart = 0
			}
			scannedLen = len(scanBuf)
			if prescan(scanBuf[winStart:]) {
				confirmedLen = len(scanBuf)
				res := h.modelAConfirm(ctx, s, string(scanBuf), usage)
				if res != nil {
					anyConfirm = true
					if res.Decision == hookcore.RejectHard || res.Decision == hookcore.Modify {
						// Confirmed hit → escalate to buffer-to-END: deliver only the
						// redacted REMAINDER (held + drained rest) through the SAME
						// encoder. The already-delivered prefix is never re-delivered.
						return h.escalateModelA(ctx, s, tee, enc, usage, held, finishReason, term)
					}
					// False positive (Approve / non-enforcing): stamp the response
					// audit outcome so a SIEM sees "hooks evaluated" + any tags
					// (parity with the wire/buffer paths), then resume real-time.
					stampModelAResponseHook(s.rec, res)
				}
			}
		}

		// Release the tail: deliver any held chunk once the held CONTENT exceeds the
		// bounded window — measured in scanned-content bytes (Delta + tool args /
		// name / id) so reasoning and other non-content chunks don't evict
		// redactable content from the window early. A secondary cap on total held
		// bytes bounds memory if a long non-content (reasoning) phase holds many
		// chunks. Sound to deliver here — either the prescan MISSed (no hook can
		// match) or the latest confirm Approved the accumulated content.
		for (contentBytes > modelATailWindowBytes || heldBytes > maxBuf) && len(held) > 0 {
			front := held[0]
			if werr := writeEncodedChunk(ctx, enc, tee, front); werr != nil {
				term.termErr.Store(&streamTerminalError{code: streamErrCodeUpstream, err: werr})
				return term
			}
			heldBytes -= canonicalChunkSize(front)
			contentBytes -= modelAContentSize(front)
			held = held[1:]
		}

		if chunk.Done {
			break
		}
	}

	// EOF without escalation → every prescan was a MISS or a false-positive
	// Approve, so the held tail is provably benign. If NO confirm ever ran the
	// sound prescan cleared the whole stream, so the response is APPROVE — stamp
	// it so the audit row distinguishes "response hooks evaluated, approved" from
	// "no response hook configured" (a false-positive confirm already stamped its
	// own outcome above). Deliver the held tail raw, then the terminal frame.
	if !anyConfirm {
		s.rec.ResponseHookDecision = string(hookcore.Approve)
		s.rec.ResponseAction = hookcore.ActionApprove
	}
	for _, c := range held {
		if werr := writeEncodedChunk(ctx, enc, tee, c); werr != nil {
			term.termErr.Store(&streamTerminalError{code: streamErrCodeUpstream, err: werr})
			return term
		}
	}
	snap := usage.snapshot()
	var finalUsage *provcore.Usage
	if usageHasAny(snap) {
		finalUsage = &snap
	}
	if werr := writeEncodedTerminal(ctx, enc, tee, s.emitDone, finalUsage, finishReason); werr != nil {
		term.termErr.Store(&streamTerminalError{code: streamErrCodeUpstream, err: werr})
	}
	return term
}

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
func (h *Handler) escalateModelA(ctx context.Context, s *streamState, tee http.ResponseWriter, enc canonicalbridge.StreamTranscoder, usage *chunkUsageHolder, held []provcore.Chunk, finishReason string, term *chunkSSEReader) *chunkSSEReader {
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

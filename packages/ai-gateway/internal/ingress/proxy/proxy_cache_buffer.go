package proxy

// proxy_cache_buffer.go — ai-gateway streaming BUFFER mode.
//
// LOCKED design (05-PLAN §B3 ai-gateway bullet): the ai-gateway buffer path is
// the S-canon LOCUS. Unlike the shared wire-frame BufferPipeline + FrameRedactor
// seam (which tlsbump uses), ai-gateway does NOT redact wire frames. It consumes
// the CANONICAL ChunkSubscription PRE-transcode, accumulates the full canonical
// (OpenAI-shape) response body, runs the response pipeline on the canonical waist
// (reuse redactCanonicalBuffer / RewriteResponseBody), then forward-encodes the
// redacted canonical to the ingress wire via the existing stream transcoder. This
// makes buffer mode actually redact a Modify decision (instead of degrading and
// replaying the original — a leak) AND makes redaction correct for non-OpenAI
// ingress (Anthropic / Gemini): the scan/rewrite is wire-shape-agnostic because
// it runs on canonical, and the transcoder re-encodes the masked canonical back
// to the client's native wire.
//
// redactCanonicalBuffer (proxy_upstream.go) is the single redaction entry point,
// shared by this buffer path AND the future B2 Model-A escalation hand-off — one
// redaction impl, multiple delivery wrappers. B2 routing/escalation is NOT wired
// here (NON-GOAL): this file only provides the clean canonical-buffer substrate.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/envelope"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// emitCanonicalStream forward-encodes a canonical chunk sequence to the ingress
// wire and writes it to w. It re-uses the stream transcoder selected by the
// shape stage; when that is nil (OpenAI-family same-shape passthrough) it builds
// the chat-completions encoder so a Modify re-emit still rebuilds valid OpenAI
// frames (the wire reader's Delta-fallback drops tool_calls, so it cannot be
// used for a redacted re-emit).
//
// Terminal framing is emitted exactly once via a synthetic Done chunk carrying
// the final aggregated usage, so per-chunk Done/Usage are stripped from the body
// chunks. The OpenAI encoder does not emit `data: [DONE]` (the live pipeline
// normally appends it), so this helper appends it when emitDone is set.
func emitCanonicalStream(ctx context.Context, w io.Writer, transcoder canonicalbridge.StreamTranscoder, model string, emitDone bool, chunks []provcore.Chunk, finalUsage *provcore.Usage, finishReason string) error {
	enc := transcoder
	if enc == nil {
		enc = canonicalbridge.NewChatCompletionsStreamEncoder(model)
	}
	for _, c := range chunks {
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
	}
	// Terminal framing is emitted exactly once via a synthetic Done chunk.
	// FinishReason carries the real observed finish_reason (the body chunks'
	// terminal-reason frame re-encodes to nothing — its delta is empty) so the
	// encoder emits the true value instead of its hardcoded default.
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

// runCanonicalBufferStream is the streaming BUFFER-mode handler. It consumes the
// canonical ChunkSubscription pre-transcode, accumulates the full canonical
// response, redacts on the canonical waist via redactCanonicalBuffer, and
// forward-encodes the result to the ingress wire through the capture tee.
//
//   - RejectHard (hard block) → only an in-band SSE error frame is delivered;
//     ZERO content bytes reach the client. The 200 SSE preamble was already
//     flushed by the preamble stage, so a block can only be surfaced in-band.
//   - Modify → the masked canonical body is re-emitted; the original PII /
//     tool-arg PII is never delivered.
//   - Approve / no response hook → the buffered canonical chunks are re-encoded
//     to the wire unchanged.
//
// Usage accounting (usage) and the capture tee (the storage copy) are preserved:
// usage is recorded from the canonical chunks as they drain, and the redacted
// wire transcript flows through the same tee the relay stage stamps onto the
// audit record. The returned *chunkSSEReader is a terminal-error carrier only
// (the buffer owns the subscription, so it classifies client-abort / upstream
// faults itself); the accounting stage reads it via terminalError().
func (h *Handler) runCanonicalBufferStream(ctx context.Context, s *streamState, tee http.ResponseWriter, usage *chunkUsageHolder) *chunkSSEReader {
	term := &chunkSSEReader{ctx: ctx, ingressFormat: s.ingressFormat}
	// Defensive nil-guard, symmetric with the live/passthrough handlers: a wiring
	// path that forgot the subscription or tee no-ops rather than nil-derefs.
	if s.sub == nil || tee == nil {
		return term
	}

	acc := newCanonicalStreamAccumulator(s.target.ModelCode)
	// Bound the accumulation: the canonical buffer holds the whole response
	// before redacting, so an unbounded (or malicious) upstream would grow it
	// without limit → OOM. Tally the canonical content bytes and fail closed
	// once the admin-resolved cap (stream_shape.go) is crossed, mirroring the
	// shared BufferPipeline's MaxBufferSize bound. Zero → the 8MB default.
	maxBuf := s.streamMaxBufferBytes
	if maxBuf <= 0 {
		maxBuf = defaultCanonicalBufferMaxBytes
	}
	var bufferedBytes int
	var collected []provcore.Chunk
	for {
		chunk, err := s.sub.Next(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			// Client disconnect / deadline: nothing to deliver, classify as a
			// client abort (mirrors chunkSSEReader); no error frame to a gone peer.
			if ctx.Err() != nil {
				term.termErr.Store(&streamTerminalError{code: streamErrCodeClientAbort, err: err})
				return term
			}
			// Provider error: synthesize an ingress-shaped terminal SSE error frame
			// so the client receives a parseable payload, then classify (§9.5).
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
		bufferedBytes += canonicalChunkSize(chunk)
		if bufferedBytes > maxBuf {
			// Cap exceeded: the 200 SSE preamble was already flushed, so deliver
			// an in-band SSE error frame (zero further content) and classify the
			// stream as an upstream fault for the accounting stage. Accumulation
			// is bounded — no OOM. ResponseHookRewritten stays false so the
			// relay's redacted-copy guard keeps storage at NULL.
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
		collected = append(collected, chunk)
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
		// Hard block / fail-closed: deliver ONLY an in-band SSE error frame — zero
		// content bytes. The audit fields (ResponseHookDecision / ResponseAction)
		// were stamped by redactCanonicalBuffer; ResponseHookRewritten stays false
		// so the relay's redacted-copy guard keeps ResponseBodyRedacted nil →
		// storage stores NULL, never a leak. fail-closed posture: the ai-gateway
		// is NOT in the packet path, so a redaction the policy required but could
		// not produce is surfaced as the error frame, never as the original body.
		pe := &provcore.ProviderError{Status: outcome.errStatus, Code: "blocked", Message: outcome.errMsg}
		_, _ = tee.Write(envelope.SynthesizeSSEErrorFrame(s.ingressFormat, pe))
		if ingress.BodyFormat.IsOpenAIFamily() {
			_, _ = tee.Write([]byte("data: [DONE]\n\n"))
		}
		return term
	}

	var emit []provcore.Chunk
	if outcome.rewritten {
		// Modify: re-emit the REDACTED canonical body. Masked content + masked
		// tool-call arguments are read back from the rewritten canonical and
		// forward-encoded to the ingress wire; the original is never delivered.
		emit = []provcore.Chunk{syntheticChunkFromCanonical(outcome.body, acc.reasoning.String())}
	} else {
		// Approve / no response hook: re-encode the buffered canonical chunks
		// unchanged to the ingress wire.
		emit = collected
	}
	if err := emitCanonicalStream(ctx, tee, s.transcoder, s.target.ModelCode, s.emitDone, emit, finalUsage, acc.finishReason); err != nil {
		term.termErr.Store(&streamTerminalError{code: streamErrCodeUpstream, err: err})
	}
	return term
}

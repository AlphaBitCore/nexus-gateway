// stream_relay.go — the relay stage of the streaming stage chain:
// adapts the subscription into an SSE reader, wires the hook context +
// capture tee, dispatches the admin-selected streaming pipeline, and
// stamps the captured response onto the audit record. Owns
// streamState.sseReader / usageHolder.
package proxy

import (
	"net/http"

	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/streaming"
)

// streamRelayStage pumps the chunk timeline to the client.
type streamRelayStage struct{ s *streamState }

func (st streamRelayStage) run() bool {
	s := st.s
	h := s.h
	r := s.r
	w := s.w
	rec := s.rec
	target := s.target
	logger := s.logger
	requestID := s.requestID

	// usageHolder captures the final reported usage observed in the
	// chunk timeline. The wire reader / canonical buffer update it from
	// chunk.Usage; we read it after the pump to stamp rec/metrics.
	usageHolder := &chunkUsageHolder{}

	pcStream := h.payloadCaptureConfig()
	hardCap := h.streamCaptureHardCap()
	tee := newStreamCaptureTee(w, hardCap)

	switch {
	case s.streamMode == streampolicy.ModeBufferFullBlock:
		// Buffer mode is the locked S-canon LOCUS: it consumes the canonical
		// ChunkSubscription PRE-transcode (NOT the wire chunkSSEReader), so it
		// must NOT build the wire reader — the subscription is consumed once,
		// inside runCanonicalBufferStream, which accumulates the full canonical
		// body, redacts on the canonical waist, and forward-encodes the redacted
		// result to the ingress wire through the same capture tee. It owns the
		// terminal-error classification and returns a carrier the accounting
		// stage reads via terminalError().
		s.sseReader = h.runCanonicalBufferStream(r.Context(), s, tee, usageHolder)
	case s.modelAArmed:
		// Model A: a redact-scope chunked_async stream. Like buffer mode it
		// consumes the canonical ChunkSubscription PRE-transcode, so it is
		// dispatched here as a sibling — NOT through the wire dispatch below
		// (which would drain the subscription into the wire reader). It streams
		// in real time behind a bounded tail + cheap union prescan, escalating to
		// canonical-buffer redaction on a confirmed hit. The prescan closes over
		// the response probe's MayMatchRawContent (the cheap union prefilter).
		s.sseReader = h.runModelAStream(r.Context(), s, tee, usageHolder, h.buildResponsePrescan(r.Context(), s))
	default:
		// Drain the subscription (replay or live broker pump) into an
		// io.Reader of SSE-formatted lines so the wire-consuming pipelines
		// (live / passthrough) can consume it unchanged.
		sseReader := newChunkSSEReaderFromSubscription(r.Context(), s.sub, s.transcoder, s.ingressFormat)
		sseReader.usageSink = usageHolder

		hookCtx := &streaming.StreamHookContext{
			RequestID:      requestID,
			IngressType:    "AI_GATEWAY",
			Path:           r.URL.Path,
			Method:         r.Method,
			Model:          target.ModelCode,
			SourceIP:       middleware.ClientIP(r),
			ProviderRegion: target.Region,
			OnCheckpoint: func(res *hookcore.CompliancePipelineResult) {
				if res == nil {
					return
				}
				rec.ResponseHookDecision = string(res.Decision)
				rec.ResponseHookReason = res.Reason
				rec.ResponseHookReasonCode = res.ReasonCode
				rec.ComplianceTags = mergeTagSets(rec.ComplianceTags, res.Tags)
				if br := mapBlockingRule(res.BlockingRule); br != nil {
					rec.BlockingRule = br
				}
				// Carry the single hook action so the audit writer governs the
				// persisted copies of the streamed response — the captured SSE
				// tee obeys the same storage governance as the non-streaming path.
				// Derived from the Decision so a no-match approve still stamps
				// ActionApprove (persist) rather than dropping the captured stream.
				rec.ResponseAction = hookcore.ActionFromDecision(res.Decision)
			},
		}

		// #115/R1 dispatch — the wire-consuming streaming modes, one helper per
		// mode. Three-service alignment: tlsbump (agent + cp) honors the same
		// modes in shared/transport/tlsbump/sse.go's resolveStreamingMode switch.
		// Collapsing passthrough into live (the original #115 oversight) silently
		// kept hooks running on traffic the admin had explicitly opted out of —
		// fixed here so admin policy is honored consistently across all three
		// services. Buffer mode is dispatched above (canonical, not wire).
		streamDeps := runStreamDeps{
			Deps:             h.deps,
			AdapterType:      target.AdapterType,
			Path:             r.URL.Path,
			AcceptHeader:     r.Header.Get("Accept"),
			HookRunner:       s.hookRunner,
			HookCtx:          hookCtx,
			SSEReader:        sseReader,
			Tee:              tee,
			Logger:           logger,
			EmitDone:         s.emitDone,
			HasResponseHooks: s.responseHooksActive,
			MaxBufferBytes:   s.streamMaxBufferBytes,
		}
		dispatchStreamMode(r.Context(), s.streamMode, streamDeps)
		s.sseReader = sseReader
	}
	logger.Debug("stream response capture",
		"hardCap", hardCap,
		"capturedBytes", len(tee.captured()),
		"truncated", tee.truncatedBeyondCap(),
		"storeFlag", pcStream.StoreResponseBody,
	)
	if pcStream.StoreResponseBody {
		rec.ResponseBody = tee.captured()
		// The captured bytes ARE the pooled tee buffer; hand its handle to the
		// audit record so the writer returns it to the pool at the record's
		// terminal resolution (mirrors the request-body pool).
		rec.AttachPooledResponseBody(tee.handle())
		rec.ResponseTruncated = tee.truncatedBeyondCap()
		rec.ResponseContentType = "text/event-stream"
		// When a response-stage redact/block hook actually rewrote the stream,
		// the captured tee bytes ARE the redacted, client-consistent transcript;
		// hand them to the audit writer as the storage-safe redacted copy so
		// StorageRawBody persists the masked stream instead of dropping it to
		// NULL. Guard on ResponseHookRewritten: a post-release checkpoint that
		// detected but could not rewrite (bytes already flushed) leaves the
		// capture holding unredacted content — keep ResponseBodyRedacted nil so
		// the fail-safe stores NULL, never a leak.
		//
		// Model A (the only redact path that reaches here): escalation redacts the
		// canonical buffer over the still-HELD tail, so any PII value completing
		// within the tail window is fully masked in this capture before storage —
		// the delivered prefix never carries a complete un-redacted value (all
		// realistic PII is far shorter than the tail window). A redaction match
		// whose SPAN exceeds the window has leading bytes best-effort-delivered AND
		// captured here: the disclosed best-effort-wire limitation, consistent
		// across the wire and this stored copy.
		if rec.ResponseHookRewritten &&
			(rec.ResponseAction == hookcore.ActionRedact || rec.ResponseAction == hookcore.ActionBlock) {
			rec.ResponseBodyRedacted = rec.ResponseBody
		}
	} else {
		// Body not stored on the audit record → return the pooled capture buffer
		// directly; no terminal reclaim will run for it.
		tee.release()
	}
	rec.StatusCode = http.StatusOK

	// s.sseReader was set by whichever branch ran (the wire reader for
	// live/passthrough, the terminal-error carrier for the canonical buffer).
	s.usageHolder = usageHolder
	return true
}

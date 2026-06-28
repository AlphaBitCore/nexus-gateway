// stream_relay.go — the relay stage of the streaming stage chain:
// adapts the subscription into an SSE reader, wires the hook context +
// capture tee, dispatches the admin-selected streaming pipeline, and
// stamps the captured response onto the audit record. Owns
// streamState.sseReader / usageHolder.
package proxy

import (
	"net/http"

	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"

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

	// Drain the subscription (replay or live broker pump) into an
	// io.Reader of SSE-formatted lines so LivePipeline.Process can
	// consume it unchanged.
	sseReader := newChunkSSEReaderFromSubscription(r.Context(), s.sub, s.transcoder, s.ingressFormat)

	// usageHolder captures the final reported usage observed in the
	// chunk timeline. The reader updates it from chunk.Usage; we read
	// it after Process returns to stamp rec/metrics.
	usageHolder := &chunkUsageHolder{}
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
		OnStreamRewrite: func(n int) {
			rec.ResponseHookRewritten = true
			rec.ResponseHookRewriteCount += n
		},
	}

	pcStream := h.payloadCaptureConfig()
	hardCap := h.streamCaptureHardCap()
	tee := newStreamCaptureTee(w, hardCap)

	// Dispatch — three streaming modes, one helper per mode.
	// Three-service alignment: tlsbump (agent + cp) honors all three
	// modes in shared/transport/tlsbump/sse.go's resolveStreamingMode
	// switch; ai-gateway does the same. Collapsing passthrough
	// into live would silently keep hooks
	// running on traffic the admin had explicitly opted out of —
	// fixed here so admin policy is honored consistently across all
	// three services.
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
		HoldBack:         s.holdBack,
		EmitDone:         s.emitDone,
		HasResponseHooks: s.responseHooksActive,
		MaxBufferBytes:   s.streamMaxBufferBytes,
	}
	dispatchStreamMode(r.Context(), s.streamMode, streamDeps)
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
	} else {
		// Body not stored on the audit record → return the pooled capture buffer
		// directly; no terminal reclaim will run for it.
		tee.release()
	}
	rec.StatusCode = http.StatusOK

	s.sseReader = sseReader
	s.usageHolder = usageHolder
	return true
}

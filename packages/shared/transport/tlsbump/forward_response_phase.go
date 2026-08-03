package tlsbump

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// runResponseStage is the post-upstream compliance phase: SSE/streaming
// responses dispatch into the streaming pipeline; buffered responses run
// the response hook pipeline and/or LLM usage extraction; non-AI traffic
// with no response hooks streams through with a non_llm audit row. No-op
// when compliance is disabled for this request.
//
// Returns true when the response was fully handled here (SSE stream
// relayed, strict fail-closed refusal, or response hard-reject) and the
// caller must NOT run the buffered relay. Returns false when the caller
// should relay resp to the client.
func (x *bumpedExchange) runResponseStage(resp *http.Response) bool {
	bo, logger := x.flow.bo, x.flow.logger

	if !x.complianceEnabled {
		return false
	}
	audCtx, _ := x.r.Context().Value(requestAuditKey{}).(*requestAuditCtx)
	// For a streamed request the body could not be normalized before forwarding;
	// now that the upstream has drained it into the tee capture, normalize it so
	// the audit row carries a structured request (e.g. openai-responses) instead
	// of tier3 generic-http. No-op on the buffered path.
	x.refineStreamingRequestMeta(audCtx)
	contentType := resp.Header.Get("Content-Type")
	// Stamp the upstream response Content-Type onto the shared
	// audit info so every downstream Emit / EmitDual call in
	// this branch (SSE handler, buffered AI path, fast-path)
	// hands a truthful CT to spillstore.EmitBody. Without this,
	// Hub-side normalization must guess the body shape from raw
	// bytes.
	if audCtx != nil {
		audCtx.info.ResponseContentType = contentType
	}
	// Connect-RPC streaming (application/connect+proto|json) uses the same
	// streaming passthrough path as SSE: we must not buffer the full body
	// with io.ReadAll or the client times out waiting for the first byte.
	isSSE := isStreamingContentType(contentType)

	// If the request body was relayed as a live stream (unknown-length / bidi —
	// runStreamingRequestPhase set requestCapture), the response is likely the
	// other half of a full-duplex exchange whose server will not half-close its
	// response until it has read the whole request. Buffering it to EOF would
	// re-introduce the deadlock the request-side streaming fix removed, for any
	// response Content-Type not in the streaming allow-list. Force the streaming
	// relay for these flows; handleSSEResponse streams + captures arbitrary
	// bytes, so a non-streaming response just relays without buffering.
	if !isSSE && audCtx != nil && audCtx.requestCapture != nil {
		isSSE = true
	}

	// Debug, and guarded (finding C-9). This fired at INFO on EVERY response with
	// seven attributes, each of which slog boxes at the call site whether or not
	// the level is enabled — the same double cost C-9 removed from the two
	// runtimeNormalize lines and C-4 from the request-entry pair.
	//
	// It was kept at INFO on the argument that the SSE-vs-buffered fork must be
	// answerable from agent.log alone after an incident. That argument is exactly
	// the one the owner overruled when C-9 was reopened — "logs should carry only
	// important information, do not grow the volume" — and it is weaker here than
	// it looks: a line that fires on every single response is not something an
	// operator greps for a rare mis-route, it is what they grep past. The one
	// genuinely operational signal it carried is the buffered-stream smell, which
	// is now raised on its own below, only when it is true.
	if logger.Enabled(x.r.Context(), slog.LevelDebug) {
		logger.Debug("post-upstream response routing",
			"path", x.r.URL.Path,
			"status_code", resp.StatusCode,
			"content_type", contentType,
			"is_sse", isSSE,
			"route", responseRouteName(isSSE, audCtx),
			"audit_ctx_nil", audCtx == nil,
			"tx_id", x.txID,
		)
	}

	if isSSE {
		// Build a response HookInput for SSE processing.
		var respInput *core.HookInput
		if audCtx != nil {
			respInput = &core.HookInput{
				Stage:        "response",
				SourceIP:     audCtx.input.SourceIP,
				TargetHost:   audCtx.input.TargetHost,
				Method:       audCtx.input.Method,
				Path:         audCtx.input.Path,
				IngressType:  audCtx.input.IngressType,
				ContentType:  contentType,
				EndpointType: x.endpointType,
			}
		}
		// Route to streaming pipeline.
		var auditInfo *compliance.AuditInfo
		if audCtx != nil {
			ai := audCtx.info
			auditInfo = &ai
		}
		// Use r.Context() (not the connection-level ctx) so the SSE
		// handler's stampMarkers can read the CPMarker injected by
		// stampCPMarker — needed for request-id, mode, hook, and
		// domain-rule headers.
		handleSSEResponse(x.r.Context(), x.w, resp, audCtx, respInput, auditInfo, bo, logger, x.requestStart)
		return true
	}

	// Non-SSE response: run response pipeline if hooks exist.
	// Reuse the endpointType classified at request time so the
	// response pipeline applies the same endpoint-aware filtering.
	if audCtx == nil {
		// SILENT-DROP SUSPECT: with no audit context this returns to the
		// buffered relay WITHOUT emitting any audit row. If the relay then
		// fails (client cancels a streaming reply), the flow leaves NO trace
		// in audit_events — the exact "we lost the chat and can't reconcile
		// it" case. Log it loudly with the reconciliation fields so a
		// post-hoc grep can find every unaudited bumped relay.
		logger.Warn("response stage: no audit context — relaying UNAUDITED (no audit row will be emitted for this flow)",
			"target", x.flow.targetHost,
			"method", x.r.Method,
			"path", x.r.URL.Path,
			"status_code", resp.StatusCode,
			"content_type", contentType,
			"is_sse", isSSE,
			"tx_id", x.txID,
		)
		return false
	}
	respPipeline, pErr := bo.policyResolver.BuildPipeline(
		"response", "COMPLIANCE_PROXY",
		x.endpointType, nil,
		bo.perHookTimeout, bo.totalTimeout, bo.parallelHooks,
		bo.strictFailClosed, // per-caller: false for the agent NE host-packet path (fail-open); true for the compliance-proxy appliance (refuse on unbuildable fail-closed hook)
		logger,
	)
	providerDetected := audCtx.info.RequestMeta.Provider != ""
	// Buffer the response body when either a response hook is
	// configured OR the request was detected as AI traffic (we
	// need the body to extract usage tokens). Non-AI traffic
	// with no hooks stays on the stream-through fast path.
	needBuffer := respPipeline != nil || providerDetected

	// Debug, and guarded (finding C-9), for the same reason as the routing line
	// above: seven attributes boxed on every non-SSE response to record which of
	// three arms ran, which is a debugging aid rather than something an operator
	// acts on. The outcome reaches the audit row.
	if logger.Enabled(x.r.Context(), slog.LevelDebug) {
		logger.Debug("response stage: non-SSE arm",
			"path", x.r.URL.Path,
			"arm", responseArmName(pErr, needBuffer),
			"provider_detected", providerDetected,
			"has_response_pipeline", respPipeline != nil,
			"content_type", contentType,
			"tx_id", x.txID,
		)
	}

	//nolint:gocritic // ifElseChain: the three arms (pipeline-build error / buffered AI path / stream-through fast path) each carry distinct ~50-line bodies; flattening to switch hurts readability without removing nesting.
	if pErr != nil {
		if x.handlePipelineBuildFailure(pErr, resp, audCtx) {
			return true
		}
	} else if needBuffer {
		// The buffered-AI arm lives in forward_response_buffered.go: this file owns arm
		// SELECTION, that one owns the capture / normalize / emit contract.
		if x.runBufferedResponseArm(resp, audCtx, respPipeline, providerDetected, contentType) {
			return true
		}
	} else {
		// Non-AI traffic with no response hooks — stream through.
		// Emit audit with non_llm usage status. No response body
		// buffered on this fast path, so ResponseBody stays nil
		// regardless of the capture flag.
		//
		// The emission is DEFERRED to after relayResponse (finding C-34). Emitting
		// here — which is what this arm used to do — builds the row before a single
		// response byte has been read, and two of its columns are populated off the
		// body read: PhaseSink stamps upstream TTFB on the first Read returning
		// content and refreshes upstream-total on every Read, so both landed as NULL
		// for every stream-through row, forever. latency_ms was worse than NULL: it
		// was computed before the transfer, so a large download's row under-reported
		// its own duration by the whole transfer time.
		//
		// serveRequest invokes this with `defer`, not a straight-line call, so a
		// panic in the relay cannot lose the row — which is what made deferring
		// acceptable at all on a compliance product. The arms above keep emitting
		// inline because they have already read the body.
		//nolint:gocritic // elseif: the comment block above documents the entire branch ("non-AI fast path"), not just the inner auditEmitter check; flattening to `else if` would orphan that documentation.
		if bo.auditEmitter != nil {
			x.deferredAudit = func() {
				approveResult := uninspectedResponse()
				// EmitDual so the request-stage StorageAction governs the
				// persisted request body even on this non-AI fast path.
				bo.auditEmitter.EmitDual(audCtx.input, audCtx.info, audCtx.requestPipelineResult, approveResult, "BUMP_SUCCESS", resp.StatusCode, int(time.Since(x.requestStart).Milliseconds()), audCtx.requestBodyBytes(), nil, traffic.UsageMeta{Status: traffic.UsageStatusNonLLM})
			}
		}
	}
	return false
}

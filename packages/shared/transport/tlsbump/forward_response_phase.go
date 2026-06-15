package tlsbump

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
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
	x.normalizeStreamingRequest(audCtx)
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

	// INFO (not Debug): the SSE-vs-buffered routing decision is the load-
	// bearing fork for "did we stream the chat reply promptly or buffer it
	// and make the client give up". Logging it at default level for every
	// response makes a mis-routed streaming endpoint (a streaming Content-Type
	// not in isStreamingContentType → buffered → client cancels) verifiable
	// from agent.log alone, without re-instrumenting after the fact.
	logger.Info("post-upstream response routing",
		"path", x.r.URL.Path,
		"status_code", resp.StatusCode,
		"content_type", contentType,
		"is_sse", isSSE,
		"route", responseRouteName(isSSE, audCtx),
		"audit_ctx_nil", audCtx == nil,
		"tx_id", x.txID,
	)

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

	// Diagnostic at INFO: which non-SSE arm this response takes, and a
	// "buffering a streaming response" smell flag. The buffered AI arm below
	// does io.ReadAll on the body; if the upstream is actually a streaming
	// reply whose Content-Type wasn't recognized by isStreamingContentType,
	// buffering it here blocks until the whole stream ends before the client
	// sees a byte — the suspected mechanism behind clients canceling long
	// chat streams. maybe_buffered_stream surfaces that smell so it's
	// verifiable from agent.log without re-instrumenting.
	logger.Info("response stage: non-SSE arm",
		"path", x.r.URL.Path,
		"arm", responseArmName(pErr, needBuffer),
		"provider_detected", providerDetected,
		"has_response_pipeline", respPipeline != nil,
		"content_type", contentType,
		"maybe_buffered_stream", needBuffer && looksLikeStreamingResponse(resp),
		"tx_id", x.txID,
	)

	//nolint:gocritic // ifElseChain: the three arms (pipeline-build error / buffered AI path / stream-through fast path) each carry distinct ~50-line bodies; flattening to switch hurts readability without removing nesting.
	if pErr != nil {
		logger.Warn("failed to build response pipeline",
			"target", x.flow.targetHost,
			"transactionId", audCtx.info.TransactionID,
			"error", pErr,
		)
		if bo.strictFailClosed {
			// Refuse rather than relay an uninspected upstream
			// response body. The client headers have NOT been written yet at
			// this point (the buffered relay runs later), so a 502 is safe to
			// send. Close the upstream body here: the early return skips
			// the relay's deferred close, and leaking the connection
			// would add FD pressure to an already-degraded appliance.
			_ = resp.Body.Close()
			if bo.auditEmitter != nil {
				// EmitDual so the synthesized refusal lands in the RESPONSE
				// column (the build failure is response-stage), with the real
				// request-stage result alongside — same shape as the SSE
				// strict abort.
				bo.auditEmitter.EmitDual(audCtx.input, audCtx.info, audCtx.requestPipelineResult, &core.CompliancePipelineResult{Decision: compliance.RejectHard}, "BUMP_PIPELINE_BUILD_FAILED", http.StatusBadGateway, int(time.Since(x.requestStart).Milliseconds()), audCtx.requestBodyBytes(), nil, traffic.UsageMeta{})
			}
			WriteRejectResponse(x.w, x.r, bo.rejectConfig, audCtx.info.TransactionID, "compliance pipeline unavailable (fail-closed)", "PIPELINE_BUILD_FAILED", http.StatusBadGateway)
			return true
		}
		// Non-strict (agent host path): emit an approve audit and fall
		// through to relay — fail-open preserves host networking.
		if bo.auditEmitter != nil {
			approveResult := &core.CompliancePipelineResult{Decision: compliance.Approve}
			// EmitDual so the request-stage StorageAction governs the
			// persisted request body even on this approve fast path.
			bo.auditEmitter.EmitDual(audCtx.input, audCtx.info, audCtx.requestPipelineResult, approveResult, "BUMP_SUCCESS", resp.StatusCode, int(time.Since(x.requestStart).Milliseconds()), audCtx.requestBodyBytes(), nil, traffic.UsageMeta{})
		}
	} else if needBuffer {
		// Read response body so we can (a) run response hooks if
		// any, and/or (b) extract LLM usage via the adapter. Bounded
		// by MaxResponseBytes (mirrors the request-side readBody cap)
		// so a malicious upstream cannot OOM the proxy with an
		// unbounded buffered response.
		respBody, readErr := readResponseBodyBounded(resp.Body, x.pcCfg.MaxResponseBytes)
		if readErr != nil {
			logger.Error("failed to read response body for compliance",
				"target", x.flow.targetHost,
				"error", readErr,
			)
			// Restore an empty body so the relay doesn't read from a closed reader.
			resp.Body = io.NopCloser(bytes.NewReader(nil))
			// Emit audit with approve (best-effort: body unreadable, let through).
			if bo.auditEmitter != nil {
				approveResult := &core.CompliancePipelineResult{Decision: compliance.Approve}
				// EmitDual so the request-stage StorageAction governs the
				// persisted request body even when the response is unreadable.
				bo.auditEmitter.EmitDual(audCtx.input, audCtx.info, audCtx.requestPipelineResult, approveResult, "BUMP_SUCCESS", resp.StatusCode, int(time.Since(x.requestStart).Milliseconds()), audCtx.requestBodyBytes(), nil, traffic.UsageMeta{Status: traffic.UsageStatusNoBody})
			}
		} else {
			// Decompress once before normalize / usage / capture.
			// Go's http.Transport only auto-decompresses gzip;
			// some origins ship brotli (br) or zstd-encoded SSE,
			// so respBody after io.ReadAll may be compressed bytes.
			// decompressForCapture is idempotent — respBody stays
			// the original compressed bytes for the relay so the
			// client receives the encoding it requested.
			decompressedBody := decompressForCapture(respBody, resp, logger)
			// Extract usage signals on the AI path. Done once per
			// request on the already-buffered body.
			var usage traffic.UsageMeta
			var respContent *normalize.NormalizedPayload
			if adapter := audCtx.adapter; adapter != nil {
				if providerDetected {
					usage = adapter.DetectResponseUsage(resp, decompressedBody)
				}
				// Hot-path normalize (response side): the Registry's
				// Tier 1+2+3 chain produces structured Messages;
				// when no tier claims, the adapter's ExtractResponse
				// → Segments chain recovers hookable text.
				respContent = runtimeNormalize(x.r.Context(), bo.normalizeRegistry, adapter, decompressedBody, x.r.URL.Path, contentType, normalize.DirectionResponse, logger, audCtx.info.TransactionID)
			}

			var respResult *core.CompliancePipelineResult
			if respPipeline != nil {
				respInput := &core.HookInput{
					Stage:             "response",
					Normalized:        respContent,
					SourceIP:          audCtx.input.SourceIP,
					TargetHost:        audCtx.input.TargetHost,
					Method:            audCtx.input.Method,
					Path:              audCtx.input.Path,
					IngressType:       audCtx.input.IngressType,
					BodySize:          int64(len(respBody)),
					ContentType:       contentType,
					DetectedProvider:  audCtx.info.RequestMeta.Provider,
					DetectedModel:     audCtx.info.RequestMeta.Model,
					ApiKeyClass:       audCtx.info.RequestMeta.ApiKeyClass,
					ApiKeyFingerprint: audCtx.info.RequestMeta.ApiKeyFingerprint,
					EndpointType:      x.endpointType,
				}
				respPipeline.SetClearSoftOnApprove(true)
				respResult = respPipeline.Execute(x.flow.ctx, respInput)
			} else {
				respResult = &core.CompliancePipelineResult{Decision: compliance.Approve}
			}

			if bo.auditEmitter != nil {
				// Reuse the already-decompressed body; calling
				// decompressForCapture again would be redundant.
				captureBody := decompressedBody
				// Stamp response-side normalize result onto
				// audCtx.info before emit so agent SQLite + Hub
				// MQ wire both carry the pre-computed response
				// NormalizedPayload (mirrors the request-side stamp).
				if respContent != nil {
					if b, err := json.Marshal(respContent); err == nil {
						audCtx.info.ResponseNormalized = b
					}
				}
				// EmitDual: the response pipeline's decision belongs in the
				// RESPONSE-stage columns; the request-stage result rides
				// alongside. The single-stage Emit previously used here put a
				// response-hook reject into the request column, so the Traffic
				// page misattributed which stage blocked.
				bo.auditEmitter.EmitDual(audCtx.input, audCtx.info, audCtx.requestPipelineResult, respResult, "BUMP_SUCCESS", resp.StatusCode, int(time.Since(x.requestStart).Milliseconds()), audCtx.requestBodyBytes(), captureBodyIfEnabled(audCtx.storeResponseBody, captureBody), usage)
			}

			// If a response hook hard-rejects, return HTTP 451 to the client
			// instead of forwarding the upstream response. The response body has
			// already been buffered, so we can safely suppress it and write an
			// error response in its place.
			if respResult.Decision == compliance.RejectHard {
				logger.Info("response blocked by compliance (REJECT_HARD)",
					"target", x.flow.targetHost,
					"transactionId", audCtx.info.TransactionID,
					"reason", respResult.Reason,
				)
				stampRejectMarkers(x.w.Header(), bo.identity, audCtx.info.TransactionID, x.domainRuleID, cpHookOutcomeFromResult(respResult))
				WriteRejectResponse(x.w, x.r, bo.rejectConfig, audCtx.info.TransactionID,
					respResult.Reason, respResult.ReasonCode, http.StatusUnavailableForLegalReasons)
				resp.Body = io.NopCloser(bytes.NewReader(nil))
				return true
			}
			resp.Body = io.NopCloser(bytes.NewReader(respBody))
		}
	} else {
		// Non-AI traffic with no response hooks — stream through.
		// Emit audit with non_llm usage status. No response body
		// buffered on this fast path, so ResponseBody stays nil
		// regardless of the capture flag.
		//nolint:gocritic // elseif: the comment block above documents the entire branch ("non-AI fast path"), not just the inner auditEmitter check; flattening to `else if` would orphan that documentation.
		if bo.auditEmitter != nil {
			approveResult := &core.CompliancePipelineResult{Decision: compliance.Approve}
			// EmitDual so the request-stage StorageAction governs the
			// persisted request body even on this non-AI fast path.
			bo.auditEmitter.EmitDual(audCtx.input, audCtx.info, audCtx.requestPipelineResult, approveResult, "BUMP_SUCCESS", resp.StatusCode, int(time.Since(x.requestStart).Milliseconds()), audCtx.requestBodyBytes(), nil, traffic.UsageMeta{Status: traffic.UsageStatusNonLLM})
		}
	}
	return false
}

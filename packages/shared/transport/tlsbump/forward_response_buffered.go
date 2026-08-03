package tlsbump

import (
	"bytes"
	"io"
	"net/http"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// runBufferedResponseArm is the buffered-AI arm of the post-upstream response
// stage: read the body to a bound, decompress it once, extract usage, optionally
// run the response hook pipeline over the normalized payload, emit the audit row,
// and either relay the body or refuse with 451.
//
// Split out of forward_response_phase.go, which owns the arm SELECTION (SSE vs
// buffered vs stream-through) and the diagnostics around it. The two change for
// unrelated reasons — selection follows Content-Type and pipeline shape, this
// follows the capture/normalize/emit contract — and keeping them together put
// the file over the size ratchet.
//
// Returns true when the response was fully handled here (a hard reject wrote 451
// or 403) and the caller must NOT relay resp.
func (x *bumpedExchange) runBufferedResponseArm(
	resp *http.Response,
	audCtx *requestAuditCtx,
	respPipeline *compliance.Pipeline,
	providerDetected bool,
	contentType string,
) bool {
	bo, logger := x.flow.bo, x.flow.logger
	// Read response body so we can (a) run response hooks if
	// any, and/or (b) extract LLM usage via the adapter. Bounded
	// by MaxResponseBytes (mirrors the request-side readBody cap)
	// so a malicious upstream cannot OOM the proxy with an
	// unbounded buffered response.
	respBody, readErr := readResponseBodyBounded(resp, x.pcCfg.MaxResponseBytes)
	if readErr != nil {
		logger.Error("failed to read response body for compliance",
			"target", x.flow.targetHost,
			"error", readErr,
		)
		// Restore an empty body so the relay doesn't read from a closed reader.
		resp.Body = io.NopCloser(bytes.NewReader(nil))
		// Emit audit with approve (best-effort: body unreadable, let through).
		if bo.auditEmitter != nil {
			approveResult := uninspectedResponse()
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
		// The one condition on this path an operator should act on: we buffered a
		// body that is genuinely an event stream, so the client saw nothing until
		// the upstream finished — the suspected mechanism behind clients cancelling
		// long chat streams. Decided AFTER the read, on the bytes themselves, and
		// raised on its own line only when true.
		//
		// It used to ride as one attribute among seven on a line that fired for
		// every non-SSE response, computed from a chunked/no-Content-Length
		// heuristic that is true for ordinary JSON responses. So the flag was
		// always set and the line was always written: the condition worth alerting
		// on was indistinguishable from the routine case, and the diagnostic
		// carried no information at all. Surfacing it as its own signal is what
		// made that visible — it fired on 4 of 4 ordinary completions.
		if bodyLooksLikeEventStream(decompressedBody) {
			logger.Warn("buffered a response that is an event stream — the client saw nothing "+
				"until the upstream finished; its Content-Type is not recognised by "+
				"isStreamingContentType",
				"path", x.r.URL.Path,
				"content_type", contentType,
				"tx_id", x.txID,
			)
		}
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
			//
			// Gated on a bound pipeline (finding C-18): respContent's only
			// consumer is respInput.Normalized below, and the audit row does not
			// carry it — the emitter reads AuditInfo.ResponseNormalized, which
			// the bumped path never stamps. With no response hooks the entire
			// Tier 1+2+3 decode was computed and thrown away. usage detection
			// above stays unconditional because it DOES reach the audit row.
			//
			// The three multimodal entry points in the ai-gateway already gate
			// their normalize the same way (stt_prompt_scan.go, video_submit_scan.go,
			// guardrail_handler.go all return early on a nil pipeline); this made
			// the bumped path the inconsistent one.
			if respPipeline != nil {
				respContent = runtimeNormalize(x.r.Context(), bo.normalizeRegistry, adapter, decompressedBody, x.r.URL.Path, contentType, normalize.DirectionResponse, logger, audCtx.info.TransactionID)
			}
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
			respResult = uninspectedResponse()
		}

		if bo.auditEmitter != nil {
			// Reuse the already-decompressed body; calling
			// decompressForCapture again would be redundant.
			captureBody := decompressedBody
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
			if bo.richReject {
				// respResult.Reason carries the hook's rule-ID/label only —
				// never the upstream's original sensitive value — so the
				// attributed body cannot echo what was matched.
				WriteRejectResponse(x.w, x.r, bo.rejectConfig, audCtx.info.TransactionID,
					respResult.Reason, respResult.ReasonCode, http.StatusUnavailableForLegalReasons)
			} else {
				// Agent on-host interceptor: minimal 403 with no attribution body.
				http.Error(x.w, "Forbidden", http.StatusForbidden)
			}
			resp.Body = io.NopCloser(bytes.NewReader(nil))
			return true
		}
		resp.Body = io.NopCloser(bytes.NewReader(respBody))
	}
	return false
}

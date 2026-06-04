package tlsbump

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// stampSSEResponseNormalized runs the SSE response body through the
// Registry's Tier 1+2+3 normalize chain (same path the non-SSE response
// at forward_handler.go:692 uses) and stamps the result onto
// info.ResponseNormalized so the audit_event.normalized_response column
// lands populated. Pre-#89 this never happened for SSE — the bytes
// were captured, written to client, captured to body buffer for inline
// audit, but no Normalize call ran at end-of-stream. Result: every
// SSE audit row at agent + cp + ai-gateway shipped normalized_response
// = NULL. Cover all three streamingMode values: passthrough (when
// capture is on we have the body too), live (chunked_async), buffer
// (buffer_full_block) — each branch in handleSSEResponse calls this
// before emitAudit.
//
// Best-effort: nil body / nil reg / Normalize hard error are silently
// dropped (already debug-logged inside runtimeNormalize). Hot path —
// never abort the audit emit because normalize hit a snag.
func stampSSEResponseNormalized(
	ctx context.Context,
	bo *bumpOptions,
	audCtx *requestAuditCtx,
	info *compliance.AuditInfo,
	respInput *core.HookInput,
	body []byte,
	contentType string,
	logger *slog.Logger,
) {
	if info == nil || len(body) == 0 || bo.normalizeRegistry == nil {
		return
	}
	var adapter traffic.Adapter
	if audCtx != nil {
		adapter = audCtx.adapter
	}
	path := ""
	if respInput != nil {
		path = respInput.Path
	}
	txID := info.TransactionID
	payload := runtimeNormalize(ctx, bo.normalizeRegistry, adapter, body, path, contentType, normalize.DirectionResponse, logger, txID)
	if payload == nil {
		return
	}
	if b, err := json.Marshal(payload); err == nil {
		info.ResponseNormalized = b
	}
}

// emitAudit emits an audit event for the SSE response path. The historical
// signature dropped both the request body and the request-stage pipeline
// result on the floor — a bug that was fixed. We now thread both
// through audCtx so:
//
//   - audCtx.requestBody → traffic_event_payload.inline_request_body
//   - audCtx.requestPipelineResult → traffic_event.request_hooks_pipeline
//   - result (response pipeline)  → traffic_event.response_hooks_pipeline
//   - respBody (when capture is on) → traffic_event_payload.inline_response_body
//
// respBody comes from the streaming pipeline's WithBodyCapture buffer when
// audCtx.storeResponseBody is true — same shape as the non-stream path's
// captureBodyIfEnabled. Pass nil to skip response-body persistence.
//
// `result` may be nil when the response pipeline did not run (e.g. fast-path
// chunked_async with no response hooks); in that case the responseDecision
// stays empty in the audit row.
func emitAudit(logger *slog.Logger, audCtx *requestAuditCtx, respInput *core.HookInput, info *compliance.AuditInfo, bo *bumpOptions, result *core.CompliancePipelineResult, statusCode int, requestStart time.Time, usage traffic.UsageMeta, respBody []byte) {
	if respInput == nil || info == nil || bo.auditEmitter == nil {
		logger.Debug("emitAudit skipped",
			"respInputNil", respInput == nil,
			"infoNil", info == nil,
			"emitterNil", bo.auditEmitter == nil,
		)
		return
	}
	// reqResult is the request-stage pipeline result that forward_handler
	// stashed onto audCtx; nil means no request hook ran for this scope.
	var reqResult *core.CompliancePipelineResult
	var reqBody []byte
	if audCtx != nil {
		reqResult = audCtx.requestPipelineResult
		reqBody = audCtx.requestBody
	}
	if result == nil {
		// No response pipeline executed; default decision stays empty
		// rather than fabricating an Approve.
		result = &core.CompliancePipelineResult{Decision: compliance.Approve}
	}
	bo.auditEmitter.EmitDual(respInput, *info, reqResult, result, "BUMP_SUCCESS",
		statusCode, int(time.Since(requestStart).Milliseconds()),
		reqBody, respBody, usage)
}

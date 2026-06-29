package tlsbump

import (
	"log/slog"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming"
)

// stampSpliceRedactedBody records the masked SSE transcript as the audit's
// redacted copy on a SUCCESSFUL buffer-mode splice — a Modify the redactor
// honored: the redactor was wired AND the disclosed REDACT_INFLIGHT_UNSUPPORTED
// fail-open was NOT taken. The captured bytes are then the masked stream the
// client received, so StorageRawBody persists it instead of dropping to NULL.
// Excluded by design (left nil → storage NULL, never a leak): the fail-open
// path (original forwarded → capture holds unredacted PII) and the no-adapter
// degrade (no redactor wired).
func stampSpliceRedactedBody(info *compliance.AuditInfo, result *core.CompliancePipelineResult, spliceWired bool, bufPipeline *streaming.BufferPipeline) {
	if info == nil || !spliceWired || result == nil ||
		result.Decision != core.Modify ||
		result.ReasonCode == core.ReasonRedactInflightUnsupported {
		return
	}
	info.ResponseBodyRedacted = bufPipeline.CapturedBytes()
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
		reqBody = audCtx.requestBodyBytes()
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

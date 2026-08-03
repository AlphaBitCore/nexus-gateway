package tlsbump

import (
	"net/http"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// The response stage's first arm: BuildPipeline errored.
//
// Split out of runResponseStage along the seam its three arms already had. This one carries the
// whole strict-versus-fail-open asymmetry, which is the only part of the response stage that is a
// SAFETY decision rather than a cost one — the appliance refuses with 502 rather than relay a body
// it could not inspect, and the agent, whose path is the host's own network, emits an approve row
// and relays anyway (CLAUDE.md: the NE path must fail open, never closed). Keeping it in its own
// file makes that asymmetry hard to edit by accident.

// handlePipelineBuildFailure returns true when the request was fully handled and the caller must
// stop; false means "audit row emitted, fall through to the relay" (the fail-open agent path).
func (x *bumpedExchange) handlePipelineBuildFailure(pErr error, resp *http.Response, audCtx *requestAuditCtx) bool {
	bo, logger := x.flow.bo, x.flow.logger
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
		approveResult := uninspectedResponse()
		// EmitDual so the request-stage StorageAction governs the
		// persisted request body even on this approve fast path.
		bo.auditEmitter.EmitDual(audCtx.input, audCtx.info, audCtx.requestPipelineResult, approveResult, "BUMP_SUCCESS", resp.StatusCode, int(time.Since(x.requestStart).Milliseconds()), audCtx.requestBodyBytes(), nil, traffic.UsageMeta{})
	}
	return false
}

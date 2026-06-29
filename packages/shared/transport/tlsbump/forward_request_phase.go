package tlsbump

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// runRequestPhase is the pre-upstream compliance phase: read + normalize
// the request body, build and run the request-stage hook pipeline, apply
// its decision (refuse / inflight rewrite / approve), and
// stash the requestAuditCtx on the request context for the post-upstream
// emit sites. No-op when compliance is disabled for this request.
//
// x.reqHookResult is populated when a domain rule matches and the hook
// pipeline runs; stampCPMarker converts it into the CPMarker that
// downstream response writers (upstream.go relay, sse.go) read via
// CPMarkerFromContext.
//
// Returns true when the request was fully answered here (strict
// fail-closed refusal or REJECT_HARD) and must not be
// forwarded upstream.
func (x *bumpedExchange) runRequestPhase() bool {
	bo, logger := x.flow.bo, x.flow.logger

	if !x.complianceEnabled {
		return false
	}
	// A streaming (unknown-length) request body must not be buffered to EOF:
	// a connect-RPC / gRPC bidi call holds its request stream open waiting for
	// the response we will not forward until the request ends, so buffering
	// deadlocks it. Relay those live with a bounded tee capture (audit-only,
	// no in-flight hook blocking) instead.
	//
	// Only on the fail-open caller (the agent NE host-packet path, where a
	// buffering deadlock would hang the host's networking). The strict
	// fail-closed appliance (compliance-proxy) is NOT in a host packet path —
	// it keeps buffering so its request-stage RejectHard inspection is
	// never silently bypassed by an unknown-length body; its traffic is unary
	// LLM API calls that carry Content-Length, so this path is rarely hit there.
	if !bo.strictFailClosed && isStreamingRequestBody(x.r) {
		return x.runStreamingRequestPhase()
	}
	// Read request body for both compliance inspection and upstream forwarding.
	bodyBytes, err := readBody(x.r, x.pcCfg.MaxRequestBytes)
	if err != nil {
		logger.Error("failed to read request body for compliance",
			"target", x.flow.targetHost,
			"error", err,
		)
		// Continue without compliance rather than blocking the request.
		return false
	}
	// Collect sanitised headers (auth stripped) for audit User-Agent extraction.
	sanitisedHeaders := copyHeadersStrippingAuth(x.r.Header)

	// Extract and normalize content via domain-specific adapter.
	// Run the request-side LLM signal detector on the same adapter
	// so provider/model/api-key class are stamped onto the hook
	// input before any hook sees the request.
	var content *normalize.NormalizedPayload
	var reqMeta traffic.RequestMeta
	var resolvedAdapter traffic.Adapter
	if bo.adapterRegistry != nil && x.matchedDomain != nil && x.matchedDomain.AdapterID != "" {
		if factory := bo.adapterRegistry.Get(x.matchedDomain.AdapterID); factory != nil {
			resolvedAdapter = factory()
			// (domainRuleID was set during domain resolution on
			// matchedDomain != nil; no need to reassign here.)
			// Hot-path normalize: the Registry's Tier 1+2+3 chain
			// produces a structured NormalizedPayload with
			// role-aware Messages. When no tier claims the body,
			// the adapter's ExtractRequest → Segments →
			// PayloadFromTextSegments chain recovers hookable
			// text for the PII pipeline.
			content = runtimeNormalize(x.r.Context(), bo.normalizeRegistry, resolvedAdapter, bodyBytes, x.r.URL.Path, x.r.Header.Get("Content-Type"), normalize.DirectionRequest, logger, x.txID)
			reqMeta = resolvedAdapter.DetectRequestMeta(x.r, bodyBytes)
		}
	}

	reqInput := &core.HookInput{
		Stage:             "request",
		Normalized:        content,
		SourceIP:          bo.sourceIP,
		TargetHost:        x.flow.targetHost,
		Method:            x.r.Method,
		Path:              x.r.URL.Path,
		IngressType:       "COMPLIANCE_PROXY",
		BodySize:          int64(len(bodyBytes)),
		ContentType:       x.r.Header.Get("Content-Type"),
		DetectedProvider:  reqMeta.Provider,
		DetectedModel:     reqMeta.Model,
		ApiKeyClass:       reqMeta.ApiKeyClass,
		ApiKeyFingerprint: reqMeta.ApiKeyFingerprint,
		EndpointType:      x.endpointType,
	}

	// Finalize conn_setup_ms = elapsed from handler entry to the
	// audit info build (covers CONNECT parse + header sanitize +
	// reqMeta detection). tls_handshake_ms was set earlier in
	// phaseBreakdown via tlsHandshakeOnce.
	if connSetupMs := int(time.Since(x.connSetupStart).Milliseconds()); connSetupMs > 0 {
		x.phaseBreakdown["conn_setup_ms"] = connSetupMs
	}
	auditInfo := compliance.AuditInfo{
		TransactionID:    x.txID,
		ConnectionID:     bo.connectionID,
		TraceID:          x.traceID,
		Headers:          sanitisedHeaders,
		RequestMeta:      reqMeta,
		PhaseSink:        x.phaseSink,
		LatencyBreakdown: x.phaseBreakdown,
		// Stamp classification inputs so the agent audit row
		// carries enough context for classify() to distinguish
		// Inspect / Processed / Blocked / Bump failed /
		// Untracked at query time.
		DomainRuleID: x.domainRuleID,
		PathAction:   string(x.resolvedPathAction),
		// Stamp originating process attribution so admin Traffic
		// UI's App column populates for inspect rows. cp callers
		// leave the procName/Bundle/User strings empty so the
		// audit row stays unchanged for cp-originated traffic.
		SourceProcess:       bo.procName,
		SourceProcessBundle: bo.procBundle,
		SourceUser:          bo.procUser,
	}
	// Persist the domain-matched adapter id as ingress_format so the
	// view-time normalize recompute (Control Plane + Agent UI) keys on the
	// authoritative adapter instead of resolving by path + content sniff.
	// Guarded: empty when no domain rule matched an adapter → recompute
	// falls back to path/sniff (preserves old-row behaviour).
	if x.matchedDomain != nil && x.matchedDomain.AdapterID != "" {
		auditInfo.IngressFormat = x.matchedDomain.AdapterID
	}

	// Build and run request pipeline. Pass endpointType so
	// endpoint-aware hooks (e.g. embedding-specific or chat-only
	// hooks) apply correctly. Empty string when unclassified —
	// all hooks that SupportsEndpoint("") are included.
	reqPipeline, pErr := bo.policyResolver.BuildPipeline(
		"request", "COMPLIANCE_PROXY",
		x.endpointType, nil,
		bo.perHookTimeout, bo.totalTimeout, bo.parallelHooks,
		bo.strictFailClosed, // per-caller: false for the agent NE host-packet path (fail-open); true for the compliance-proxy appliance (refuse on unbuildable fail-closed hook)
		logger,
	)
	if pErr != nil {
		logger.Error("failed to build request pipeline",
			"target", x.flow.targetHost,
			"transactionId", x.txID,
			"error", pErr,
		)
		if bo.strictFailClosed {
			// On the dedicated forward-proxy appliance an unbuildable
			// fail-closed hook MUST refuse the request, never forward
			// it uninspected. (The agent NE host-packet path leaves
			// strictFailClosed unset and falls through to forward, preserving
			// fail-open so a hook-config error never takes down host networking.)
			if bo.auditEmitter != nil {
				bo.auditEmitter.Emit(reqInput, auditInfo, &core.CompliancePipelineResult{Decision: compliance.RejectHard}, "BUMP_PIPELINE_BUILD_FAILED", http.StatusBadGateway, int(time.Since(x.requestStart).Milliseconds()), captureBodyIfEnabled(x.pcCfg.StoreRequestBody, bodyBytes), nil, traffic.UsageMeta{})
			}
			WriteRejectResponse(x.w, x.r, bo.rejectConfig, x.txID, "compliance pipeline unavailable (fail-closed)", "PIPELINE_BUILD_FAILED", http.StatusBadGateway)
			return true
		}
	} else if reqPipeline != nil {
		reqPipeline.SetClearSoftOnApprove(true)
		result := reqPipeline.Execute(x.flow.ctx, reqInput)
		// Capture the result so the CPMarker built before upstream can
		// convert it into a HookOutcomeInput for downstream writers.
		x.reqHookResult = result

		switch result.Decision {
		case compliance.RejectHard:
			logger.Info("request blocked by compliance (REJECT_HARD)",
				"target", x.flow.targetHost,
				"transactionId", x.txID,
				"reason", result.Reason,
			)
			if bo.auditEmitter != nil {
				bo.auditEmitter.Emit(reqInput, auditInfo, result, "BUMP_SUCCESS", http.StatusForbidden, int(time.Since(x.requestStart).Milliseconds()), captureBodyIfEnabled(x.pcCfg.StoreRequestBody, bodyBytes), nil, traffic.UsageMeta{})
			}
			stampRejectMarkers(x.w.Header(), bo.identity, x.txID, x.domainRuleID, cpHookOutcomeFromResult(result))
			if bo.richReject {
				WriteRejectResponse(x.w, x.r, bo.rejectConfig, x.txID, result.Reason, result.ReasonCode, http.StatusForbidden)
			} else {
				// Agent on-host interceptor: minimal 403 with no attribution
				// body (it does not synthesize a rich error page in the host
				// outbound path).
				http.Error(x.w, "Forbidden", http.StatusForbidden)
			}
			return true

		case compliance.Modify:
			// Hook requested inflight redact. Try to rewrite the
			// upstream body via the resolved adapter. If the adapter
			// declares ErrRewriteUnsupported, fall back to
			// "upstream sees original, audit log stores spans" and
			// stamp REDACT_INFLIGHT_UNSUPPORTED on the result so the
			// audit trail reflects the degraded path.
			if resolvedAdapter != nil && len(result.ModifiedContent) > 0 {
				rewriteContent := rewriteContentWithToolArgs(result.ModifiedContent, content, result.TransformSpans)
				rewritten, _, rErr := resolvedAdapter.RewriteRequestBody(x.flow.ctx, bodyBytes, x.r.URL.Path, rewriteContent)
				if rErr == nil {
					// Fail closed when tool-call args were masked but this
					// per-host adapter cannot put them back on its native wire
					// (only the OpenAI canonical adapter implements
					// traffic.ToolArgMasker). Without this guard a non-OpenAI
					// upstream would receive UNMASKED tool-arg PII while the audit
					// recorded it redacted — a silent divergence. Routing to the
					// disclosed REDACT_INFLIGHT_UNSUPPORTED path makes the gap
					// honest instead of silent.
					rErr = traffic.GuardToolArgMasking(resolvedAdapter, rewriteContent)
				}
				switch {
				case errors.Is(rErr, traffic.ErrRewriteUnsupported):
					logger.Warn("inflight rewrite unsupported; forwarding original body",
						"target", x.flow.targetHost,
						"transactionId", x.txID,
						"adapter", resolvedAdapter.ID(),
					)
					result.ReasonCode = core.ReasonRedactInflightUnsupported
				case rErr != nil:
					logger.Error("inflight rewrite failed",
						"target", x.flow.targetHost,
						"transactionId", x.txID,
						"error", rErr,
					)
					result.ReasonCode = core.ReasonRedactInflightUnsupported
				default:
					bodyBytes = rewritten
					// The rewritten copy is the only raw bytes allowed into
					// the audit store under the redact action — the emitter
					// selects it via StorageRawBody at build time.
					auditInfo.RequestBodyRedacted = rewritten
					logger.Info("request body redacted by compliance hook",
						"target", x.flow.targetHost,
						"transactionId", x.txID,
					)
				}
			}
			// MODIFY does NOT short-circuit upstream; fall through.
		}

		// APPROVE / ABSTAIN / MODIFY-handled — continue to upstream.
	}

	// Restore the request body for upstream forwarding since we consumed it.
	x.r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	x.r.ContentLength = int64(len(bodyBytes))

	// Store the request audit context for post-upstream use.
	// requestPipelineResult is reqHookResult so the SSE / non-SSE
	// post-upstream emit can record request-stage executions on
	// traffic_event.request_hooks_pipeline.
	x.r = x.r.WithContext(context.WithValue(x.r.Context(), requestAuditKey{}, &requestAuditCtx{
		input:                 reqInput,
		info:                  auditInfo,
		requestBody:           captureBodyIfEnabled(x.pcCfg.StoreRequestBody, bodyBytes),
		storeResponseBody:     x.pcCfg.StoreResponseBody,
		requestPipelineResult: x.reqHookResult,
		matchedDomain:         x.matchedDomain,
		adapter:               resolvedAdapter,
	}))
	return false
}

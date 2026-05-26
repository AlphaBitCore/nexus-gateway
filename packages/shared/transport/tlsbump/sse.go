package tlsbump

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/domain"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/responseprehook"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// resolveStreamingMode returns the effective streaming mode string for
// this request. Reads the live policy snapshot from
// bo.streamingPolicyStore (admin-driven, hot-swapped via shadow push),
// merges with per-host override columns on the matched
// interception_domain via shared/streaming/policy.Resolve, and maps
// the resulting Mode to the live/buffer/passthrough strings the
// underlying streaming pipelines consume:
//
//	policy.ModePassThrough     → "passthrough"
//	policy.ModeChunkedAsync    → "live"   (real-time relay + per-chunk hook)
//	policy.ModeBufferFullBlock → "buffer" (full-buffer hook with reject)
//
// Nil Store = passthrough (#115: deleted the legacy YAML
// `streamingMode` fallback path — admin policy is now the single
// source of truth, three-service aligned).
func resolveStreamingMode(bo *bumpOptions, matched *domain.InterceptionDomain) string {
	if bo.streamingPolicyStore == nil {
		// No Store wired by the caller — fail-safe to passthrough so
		// we don't silently engage live/buffer compliance for traffic
		// the caller hasn't opted into.
		return "passthrough"
	}
	g := bo.streamingPolicyStore.Get()
	var override *streampolicy.Override
	if matched != nil {
		override = streampolicy.OverrideFromColumns(
			matched.StreamingMode,
			matched.StreamingChunkBytes,
			matched.StreamingHookTimeoutMs,
			matched.StreamingMaxBufferBytes,
			matched.StreamingFailBehavior,
			matched.CaptureRequestBody,
			matched.CaptureResponseBody,
			matched.RawBodySpillEnabled,
		)
	}
	merged := streampolicy.Resolve(g, override)
	switch merged.Mode {
	case streampolicy.ModePassThrough:
		return "passthrough"
	case streampolicy.ModeChunkedAsync:
		return "live"
	case streampolicy.ModeBufferFullBlock:
		return "buffer"
	default:
		// Unknown mode (validation failed somewhere upstream); be safe
		// and fall back to passthrough.
		return "passthrough"
	}
}

// stripDynamicHopByHop removes headers that are listed in the Connection header
// per RFC 7230 §6.1. It must be called BEFORE any loop that deletes the
// Connection header itself (e.g. the static isHopByHopHeader sweep), because
// once Connection is gone Values("Connection") returns nothing.
//
// This matches the same logic used by responseio.Copy and copyResponse on the
// non-SSE path (Task 0.3), closing the asymmetry noted in that review.
func stripDynamicHopByHop(h http.Header) {
	for _, line := range h.Values("Connection") {
		for _, name := range strings.Split(line, ",") {
			if n := strings.TrimSpace(name); n != "" {
				h.Del(n)
			}
		}
	}
}

// buildSSEPreHookCallback returns a streaming.PreHookCallback that
// runs the supplied raw SSE body through the Registry's Tier 1+2+3
// normalize chain, then stamps both:
//   (1) checkpointInput.Normalized — so the hook executor sees rich
//       structured chat content (model name, tool_calls, reasoning
//       segments) instead of buildCheckpointInput's flat-text fallback
//   (2) auditInfo.ResponseNormalized — so the audit row's
//       normalized_response column lands populated even on the
//       streaming path
//
// #93 — implementation delegates to shared
// transport/normalize/responseprehook.Build so all three ingress
// services (agent / compliance-proxy / ai-gateway) wire the same
// PreHookCallback shape. The audit-row stamp (#2) is service-specific
// and rides through the OnPayload option.
//
// Used by BOTH BufferPipeline (fires once between read + hooks) and
// LivePipeline (fires at every checkpoint with cumulative body bytes).
// Best-effort: nil body / nil registry / Normalize hard error are
// silently dropped — never abort hook execution because normalize
// stumbled.
func buildSSEPreHookCallback(
	ctx context.Context,
	bo *bumpOptions,
	audCtx *requestAuditCtx,
	auditInfo *compliance.AuditInfo,
	respInput *core.HookInput,
	respContentType string,
) streaming.PreHookCallback {
	if bo.normalizeRegistry == nil {
		return nil
	}
	adapterID := ""
	if audCtx != nil && audCtx.adapter != nil {
		adapterID = audCtx.adapter.ID()
	}
	endpointPath := ""
	if respInput != nil {
		endpointPath = respInput.Path
	}
	return responseprehook.Build(responseprehook.Options{
		Ctx:          ctx,
		Registry:     bo.normalizeRegistry,
		AdapterID:    adapterID,
		EndpointPath: endpointPath,
		ContentType:  respContentType,
		Direction:    normalize.DirectionResponse,
		OnPayload: func(payload *normalize.NormalizedPayload, _ []byte) {
			if auditInfo == nil || payload == nil {
				return
			}
			if b, err := json.Marshal(payload); err == nil {
				auditInfo.ResponseNormalized = b
			}
		},
	})
}

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

// finalizeUsage drains the UsageAccumulator (if any) using a short deadline
// so a stuck tokenizer cannot block the SSE hot path. Returns a zero
// UsageMeta when acc is nil — the emitter's defaulting will stamp non_llm.
func finalizeUsage(ctx context.Context, acc streaming.UsageAccumulator) traffic.UsageMeta {
	if acc == nil {
		return traffic.UsageMeta{}
	}
	deadline, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return acc.Finalize(deadline)
}

// handleSSEResponse routes an SSE response through the appropriate streaming
// compliance pipeline based on the configured streaming mode.
//
// `audCtx` carries the request-stage outcome — request body bytes already
// captured for payload audit, plus the request hook pipeline result — so the
// per-mode emit sites can record both stages on traffic_event. Previously
// this data was thrown away on the SSE path.
func handleSSEResponse(
	ctx context.Context,
	w http.ResponseWriter,
	resp *http.Response,
	audCtx *requestAuditCtx,
	respInput *core.HookInput,
	auditInfo *compliance.AuditInfo,
	bo *bumpOptions,
	logger *slog.Logger,
	requestStart time.Time,
) {
	defer func() {
		_ = resp.Body.Close()
	}()

	// Strip dynamically-listed hop-by-hop headers per RFC 7230 §6.1, then
	// strip the static set. Both must happen before copying to the client.
	stripDynamicHopByHop(resp.Header)

	// Capture content-type ONCE up front — #89 stampSSEResponseNormalized
	// needs it on every branch, and it must be read before the strip+copy
	// loop modifies the header map.
	respContentType := resp.Header.Get("Content-Type")

	// Copy response headers to client, stripping hop-by-hop per RFC 7230 §6.1.
	for key, values := range resp.Header {
		if isHopByHopHeader(key) {
			continue
		}
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}

	// Inject x-nexus-via + x-nexus-cp-* markers BEFORE the first flush so
	// the client receives them as HTTP response headers (not trailers).
	// This is the SSE equivalent of markerHook on the non-streaming path.
	stampMarkers(ctx, w.Header(), bo.identity)

	w.WriteHeader(resp.StatusCode)

	var matched *domain.InterceptionDomain
	if audCtx != nil {
		matched = audCtx.matchedDomain
	}
	mode := resolveStreamingMode(bo, matched)
	if mode == "" {
		mode = "passthrough"
	}

	logger.Debug("SSE handler entry",
		"mode", mode,
		"audCtxNil", audCtx == nil,
		"respInputNil", respInput == nil,
		"auditInfoNil", auditInfo == nil,
		"statusCode", resp.StatusCode,
		"contentType", resp.Header.Get("Content-Type"),
	)

	// Build a UsageAccumulator when the request was detected as AI
	// traffic. The accumulator sees every parsed SSE frame and produces
	// tier-1 (provider-reported) or tier-2 (tokenizer-estimated) counts
	// at finalize time. Non-AI SSE streams leave acc == nil and the
	// emitter stamps non_llm in place of numeric tokens.
	var acc streaming.UsageAccumulator
	if auditInfo != nil && auditInfo.RequestMeta.Provider != "" {
		acc = streaming.NewUsageAccumulator(auditInfo.RequestMeta.Provider, auditInfo.RequestMeta.Model)
	}

	// captureMax is the upper bound on bytes we'll keep for audit when
	// audCtx.storeResponseBody is true. Mirrors the streamingConfig's
	// MaxBufferSize so a single tunable governs both the live-pipeline
	// hold-back budget and the audit-capture budget.
	captureMax := 0
	if audCtx != nil && audCtx.storeResponseBody {
		captureMax = bo.streamingConfig.MaxBufferSize
		if captureMax <= 0 {
			captureMax = 8 * 1024 * 1024 // mirror streaming.defaultBufferMaxSize
		}
	}

	// Connect-RPC server-streaming (e.g. Cursor chat via api2.cursor.sh).
	// SSE-based live/buffer pipelines are incompatible with the binary
	// 5-byte frame envelope; use frame-aware passthrough that tees payloads
	// through the adapter's ExtractStreamChunk for audit regardless of mode.
	if strings.Contains(resp.Header.Get("Content-Type"), "application/connect+proto") {
		payloadGzip := strings.Contains(resp.Header.Get("Connect-Content-Encoding"), "gzip")
		var extractor streaming.ConnectRPCFrameExtractor
		if audCtx != nil && audCtx.adapter != nil {
			adap := audCtx.adapter
			path := ""
			if respInput != nil {
				path = respInput.Path
			}
			extractor = func(payload []byte) string {
				nc, _ := adap.ExtractStreamChunk(ctx, payload, path)
				return strings.Join(nc.Segments, "")
			}
		}
		var captureBuf *streaming.CappedBuffer
		if captureMax > 0 {
			captureBuf = streaming.NewCappedBuffer(captureMax)
		}
		accumulated, relayErr := streaming.PassthroughWithConnectRPCExtract(ctx, resp.Body, w, captureBuf, extractor, payloadGzip)
		if relayErr != nil {
			target := ""
			if respInput != nil {
				target = respInput.TargetHost
			}
			logger.Error("ConnectRPC passthrough failed", "target", target, "error", relayErr)
		}
		if respInput != nil && accumulated != "" {
			respInput.Normalized = core.PayloadFromTextSegments([]string{accumulated})
		}
		var capturedBytes []byte
		if captureBuf != nil {
			capturedBytes = captureBuf.Bytes()
		}
		stampSSEResponseNormalized(ctx, bo, audCtx, auditInfo, respInput, capturedBytes, respContentType, logger)
		emitAudit(logger, audCtx, respInput, auditInfo, bo, nil, resp.StatusCode, requestStart, traffic.UsageMeta{}, capturedBytes)
		return
	}

	switch mode {
	case "passthrough":
		// No compliance inspection — just relay. Passthrough doesn't parse
		// frames so we can't feed the accumulator; report no_body when
		// provider is set, non_llm otherwise. Capture the streamed bytes
		// when audCtx.storeResponseBody is on so the audit row carries
		// inline_response_body for SSE flows the same way it does for
		// non-stream flows (review finding compliance-proxy/sse.go:96-116).
		var captureBuf *streaming.CappedBuffer
		var dest io.Writer = w
		if captureMax > 0 {
			captureBuf = streaming.NewCappedBuffer(captureMax)
			dest = io.MultiWriter(w, captureBuf)
		}
		if err := streaming.Passthrough(ctx, resp.Body, dest); err != nil {
			target := ""
			if respInput != nil {
				target = respInput.TargetHost
			}
			logger.Error("SSE passthrough failed",
				"target", target,
				"error", err,
			)
		}
		stampSSEResponseNormalized(ctx, bo, audCtx, auditInfo, respInput, captureBuf.Bytes(), respContentType, logger)
		emitAudit(logger, audCtx, respInput, auditInfo, bo, nil, resp.StatusCode, requestStart, traffic.UsageMeta{}, captureBuf.Bytes())

	case "live":
		// Build a response pipeline for checkpoint evaluation.
		var pipelineExec streaming.PipelineExecutor
		if respInput != nil {
			// Endpoint type not classified at SSE stage — pass empty string so all hooks run.
			respPipeline, pErr := bo.policyResolver.BuildPipeline(
				"response", "COMPLIANCE_PROXY",
				"", nil,
				bo.perHookTimeout, bo.totalTimeout, bo.parallelHooks, logger,
			)
			if pErr != nil {
				logger.Warn("failed to build SSE live pipeline, falling back to passthrough",
					"error", pErr,
				)
				var captureBuf *streaming.CappedBuffer
				var dest io.Writer = w
				if captureMax > 0 {
					captureBuf = streaming.NewCappedBuffer(captureMax)
					dest = io.MultiWriter(w, captureBuf)
				}
				_ = streaming.Passthrough(ctx, resp.Body, dest)
				stampSSEResponseNormalized(ctx, bo, audCtx, auditInfo, respInput, captureBuf.Bytes(), respContentType, logger)
				emitAudit(logger, audCtx, respInput, auditInfo, bo, nil, resp.StatusCode, requestStart, traffic.UsageMeta{}, captureBuf.Bytes())
				return
			}
			if respPipeline != nil {
				pipelineExec = respPipeline
			}
		}

		if pipelineExec == nil && acc == nil {
			// No response hooks and non-AI traffic — passthrough.
			var captureBuf *streaming.CappedBuffer
			var dest io.Writer = w
			if captureMax > 0 {
				captureBuf = streaming.NewCappedBuffer(captureMax)
				dest = io.MultiWriter(w, captureBuf)
			}
			_ = streaming.Passthrough(ctx, resp.Body, dest)
			stampSSEResponseNormalized(ctx, bo, audCtx, auditInfo, respInput, captureBuf.Bytes(), respContentType, logger)
			emitAudit(logger, audCtx, respInput, auditInfo, bo, nil, resp.StatusCode, requestStart, traffic.UsageMeta{}, captureBuf.Bytes())
			return
		}

		livePipeline := streaming.NewLivePipeline(bo.streamingConfig, pipelineExec, logger)
		if acc != nil {
			livePipeline.WithUsageAccumulator(acc)
		}
		if captureMax > 0 {
			livePipeline.WithBodyCapture(captureMax)
		}
		// #90 — fire normalize before every checkpoint so hooks see
		// the Registry-produced rich Normalized (model/tools/reasoning),
		// not just flat-text. The callback ALSO stamps
		// auditInfo.ResponseNormalized on each fire, so by end-of-stream
		// the audit row carries the latest cumulative-normalized payload
		// — no post-Process stamp needed.
		if cb := buildSSEPreHookCallback(ctx, bo, audCtx, auditInfo, respInput, respContentType); cb != nil {
			livePipeline.WithPreHook(cb)
		}
		result, err := livePipeline.Process(ctx, resp.Body, w, respInput)
		if err != nil {
			target := ""
			if respInput != nil {
				target = respInput.TargetHost
			}
			logger.Error("SSE live pipeline error",
				"target", target,
				"error", err,
			)
		}
		emitAudit(logger, audCtx, respInput, auditInfo, bo, result, resp.StatusCode, requestStart, finalizeUsage(ctx, acc), livePipeline.CapturedBytes())

	case "buffer":
		var pipelineExec streaming.PipelineExecutor
		if respInput != nil {
			// Endpoint type not classified at SSE buffer stage — pass empty string so all hooks run.
			respPipeline, pErr := bo.policyResolver.BuildPipeline(
				"response", "COMPLIANCE_PROXY",
				"", nil,
				bo.perHookTimeout, bo.totalTimeout, bo.parallelHooks, logger,
			)
			if pErr != nil {
				logger.Warn("failed to build SSE buffer pipeline, falling back to passthrough",
					"error", pErr,
				)
				var captureBuf *streaming.CappedBuffer
				var dest io.Writer = w
				if captureMax > 0 {
					captureBuf = streaming.NewCappedBuffer(captureMax)
					dest = io.MultiWriter(w, captureBuf)
				}
				_ = streaming.Passthrough(ctx, resp.Body, dest)
				stampSSEResponseNormalized(ctx, bo, audCtx, auditInfo, respInput, captureBuf.Bytes(), respContentType, logger)
				emitAudit(logger, audCtx, respInput, auditInfo, bo, nil, resp.StatusCode, requestStart, traffic.UsageMeta{}, captureBuf.Bytes())
				return
			}
			if respPipeline != nil {
				pipelineExec = respPipeline
			}
		}

		if pipelineExec == nil && acc == nil {
			var captureBuf *streaming.CappedBuffer
			var dest io.Writer = w
			if captureMax > 0 {
				captureBuf = streaming.NewCappedBuffer(captureMax)
				dest = io.MultiWriter(w, captureBuf)
			}
			_ = streaming.Passthrough(ctx, resp.Body, dest)
			stampSSEResponseNormalized(ctx, bo, audCtx, auditInfo, respInput, captureBuf.Bytes(), respContentType, logger)
			emitAudit(logger, audCtx, respInput, auditInfo, bo, nil, resp.StatusCode, requestStart, traffic.UsageMeta{}, captureBuf.Bytes())
			return
		}

		bufConfig := streaming.BufferConfig{MaxBufferSize: bo.streamingConfig.MaxBufferSize}
		bufPipeline := streaming.NewBufferPipeline(bufConfig, pipelineExec, logger)
		if acc != nil {
			bufPipeline.WithUsageAccumulator(acc)
		}
		if captureMax > 0 {
			bufPipeline.WithBodyCapture(captureMax)
		}
		// #90 — fire normalize between Phase 1 (read full body) and
		// Phase 2 (run hooks) so hooks see the Registry-produced rich
		// Normalized (model/tools/reasoning), not just buildCheckpointInput's
		// flat-text fallback. The same callback ALSO stamps
		// auditInfo.ResponseNormalized so the audit row carries it — no
		// post-Process stamp needed.
		if cb := buildSSEPreHookCallback(ctx, bo, audCtx, auditInfo, respInput, respContentType); cb != nil {
			bufPipeline.WithPreHook(cb)
		}
		result, err := bufPipeline.Process(ctx, resp.Body, w, respInput)
		if err != nil {
			target := ""
			if respInput != nil {
				target = respInput.TargetHost
			}
			logger.Error("SSE buffer pipeline error",
				"target", target,
				"error", err,
			)
		}
		emitAudit(logger, audCtx, respInput, auditInfo, bo, result, resp.StatusCode, requestStart, finalizeUsage(ctx, acc), bufPipeline.CapturedBytes())

	default:
		logger.Warn("unknown streaming mode, using passthrough", "mode", mode)
		var captureBuf *streaming.CappedBuffer
		var dest io.Writer = w
		if captureMax > 0 {
			captureBuf = streaming.NewCappedBuffer(captureMax)
			dest = io.MultiWriter(w, captureBuf)
		}
		_ = streaming.Passthrough(ctx, resp.Body, dest)
		stampSSEResponseNormalized(ctx, bo, audCtx, auditInfo, respInput, captureBuf.Bytes(), respContentType, logger)
		emitAudit(logger, audCtx, respInput, auditInfo, bo, nil, resp.StatusCode, requestStart, traffic.UsageMeta{}, captureBuf.Bytes())
	}
}

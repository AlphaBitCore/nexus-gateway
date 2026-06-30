package tlsbump

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/domain"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
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
// Nil Store = passthrough (there is no legacy YAML `streamingMode`
// fallback path — admin policy is the single source of truth,
// three-service aligned).
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
// non-SSE path, closing the SSE/non-SSE asymmetry.
func stripDynamicHopByHop(h http.Header) {
	for _, line := range h.Values("Connection") {
		for _, name := range strings.Split(line, ",") {
			if n := strings.TrimSpace(name); n != "" {
				h.Del(n)
			}
		}
	}
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

	// Strict (appliance) fail-closed guard, evaluated
	// BEFORE any response header is written to the client. If a mandatory
	// fail-closed response hook cannot be built under strict policy, the
	// streamed body CANNOT be inspected — refuse with 451 rather than relaying
	// uninspected SSE chunks. Once w.WriteHeader fires below we can no longer
	// send a clean status, so this check must precede the header copy. The
	// agent NE host-packet path passes strictFailClosed=false, skips this
	// guard, and stays fail-open by design (CLAUDE.md NE safety rule).
	if bo.strictFailClosed && respInput != nil {
		// Endpoint type is not classified at SSE stage — pass "" so all hooks
		// are considered, mirroring the per-mode BuildPipeline calls below.
		if _, pErr := bo.policyResolver.BuildPipeline(
			"response", "COMPLIANCE_PROXY",
			"", nil,
			bo.perHookTimeout, bo.totalTimeout, bo.parallelHooks,
			true, // strict
			logger,
		); pErr != nil {
			logger.Error("SSE response blocked: mandatory fail-closed hook unbuildable under strict policy",
				"target", respInput.TargetHost,
				"error", pErr,
			)
			blockResult := &core.CompliancePipelineResult{
				Decision:   compliance.RejectHard,
				Reason:     "compliance pipeline unavailable (fail-closed)",
				ReasonCode: core.ReasonFailClosed,
			}
			if bo.auditEmitter != nil && audCtx != nil {
				emitAudit(logger, audCtx, respInput, auditInfo, bo, blockResult, http.StatusUnavailableForLegalReasons, requestStart, traffic.UsageMeta{}, nil)
			}
			stampRejectMarkers(w.Header(), bo.identity, "", "", cpHookOutcomeFromResult(blockResult))
			http.Error(w, "Forbidden", http.StatusUnavailableForLegalReasons)
			return
		}
	}

	// Strip dynamically-listed hop-by-hop headers per RFC 7230 §6.1, then
	// strip the static set. Both must happen before copying to the client.
	stripDynamicHopByHop(resp.Header)

	// Capture content-type ONCE up front — buildSSEPreHookCallback needs it
	// on every branch, and it must be read before the strip+copy loop
	// modifies the header map.
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

	// Diagnostic (Info so it shows in prod agent.log): which streaming mode the
	// flow resolved to, per host. chunked_async = real-time relay; buffer_full_block
	// = hold the whole response (breaks SSE realtime); passthrough = relay only.
	// Grep "SSE streaming relay" + the host to see if e.g. claude.ai is being
	// buffered (a per-host override) vs streamed.
	sseHost, ssePath := "", ""
	if respInput != nil {
		sseHost = respInput.TargetHost
		ssePath = respInput.Path
	}
	logger.Info("SSE streaming relay: resolved mode",
		"host", sseHost,
		"path", ssePath,
		"mode", mode,
		"statusCode", resp.StatusCode,
		"contentType", resp.Header.Get("Content-Type"),
	)
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
	// audit-accumulation cap and the audit-capture budget.
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
		accumulated, relayErr := streaming.PassthroughWithConnectRPCExtract(ctx, resp.Body, w, captureBuf, extractor)
		if relayErr != nil {
			target := ""
			if respInput != nil {
				target = respInput.TargetHost
			}
			logger.Error("ConnectRPC passthrough failed", "target", target, "error", relayErr,
				"cancel_cause", cancelCause(ctx),
				"duration_ms", int(time.Since(requestStart).Milliseconds()))
		}
		if respInput != nil && accumulated != "" {
			respInput.Normalized = core.PayloadFromTextSegments([]string{accumulated})
		}
		var capturedBytes []byte
		if captureBuf != nil {
			capturedBytes = captureBuf.Bytes()
		}
		emitAudit(logger, audCtx, respInput, auditInfo, bo, nil, resp.StatusCode, requestStart, traffic.UsageMeta{}, capturedBytes)
		return
	}

	// Scope-derived routing (mirror ai-gateway stream_shape.go §B2): an enforcing
	// response scope OVERRIDES the admin streaming mode — the audit-only live path
	// cannot enforce, so enforcing traffic must route to buffer or Model A. The probe
	// is reused as the Model A prescan/confirm pipeline.
	var responseProbe *compliance.Pipeline
	mode, responseProbe = scopeRouteSSEMode(bo, mode, respInput, audCtx, logger)

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
				"cancel_cause", cancelCause(ctx),
				"duration_ms", int(time.Since(requestStart).Milliseconds()),
			)
		}
		emitAudit(logger, audCtx, respInput, auditInfo, bo, nil, resp.StatusCode, requestStart, traffic.UsageMeta{}, captureBuf.Bytes())

	case "live":
		// Build a response pipeline for checkpoint evaluation.
		var pipelineExec streaming.PipelineExecutor
		if respInput != nil {
			// Endpoint type not classified at SSE stage — pass empty string so all hooks run.
			respPipeline, pErr := bo.policyResolver.BuildPipeline(
				"response", "COMPLIANCE_PROXY",
				"", nil,
				bo.perHookTimeout, bo.totalTimeout, bo.parallelHooks,
				bo.strictFailClosed, // per-caller: false for the agent NE host-packet path (fail-open); true for the compliance-proxy appliance (refuse on unbuildable fail-closed hook)
				logger,
			)
			if pErr != nil {
				// Strict (appliance) fail-closed is refused up front by the
				// guard at SSE entry (clean 451 before headers); reaching here
				// means a non-strict caller, so fall back to passthrough.
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
		// Fire normalize before every checkpoint so hooks see
		// the Registry-produced rich Normalized (model/tools/reasoning),
		// not just flat-text.
		if cb := buildSSEPreHookCallback(ctx, bo, audCtx, respInput, respContentType); cb != nil {
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
				"cancel_cause", cancelCause(ctx),
				"bytes_captured", len(livePipeline.CapturedBytes()),
				"duration_ms", int(time.Since(requestStart).Milliseconds()),
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
				bo.perHookTimeout, bo.totalTimeout, bo.parallelHooks,
				bo.strictFailClosed, // per-caller: false for the agent NE host-packet path (fail-open); true for the compliance-proxy appliance (refuse on unbuildable fail-closed hook)
				logger,
			)
			if pErr != nil {
				// Strict fail-closed is refused up front by the SSE-entry guard;
				// reaching here means a non-strict caller → passthrough fallback.
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
			emitAudit(logger, audCtx, respInput, auditInfo, bo, nil, resp.StatusCode, requestStart, traffic.UsageMeta{}, captureBuf.Bytes())
			return
		}

		bufConfig := streaming.BufferConfig{MaxBufferSize: bo.streamingConfig.MaxBufferSize}
		bufPipeline := streaming.NewBufferPipeline(bufConfig, pipelineExec, logger)
		bufPipeline.WithStrictFailClosed(bo.strictFailClosed) // no-redactor posture (GAP B)
		if acc != nil {
			bufPipeline.WithUsageAccumulator(acc)
		}
		if captureMax > 0 {
			bufPipeline.WithBodyCapture(captureMax)
		}
		// Fire normalize between Phase 1 (read full body) and
		// Phase 2 (run hooks) so hooks see the Registry-produced rich
		// Normalized (model/tools/reasoning), not just buildCheckpointInput's
		// flat-text fallback.
		if cb := buildSSEPreHookCallback(ctx, bo, audCtx, respInput, respContentType); cb != nil {
			bufPipeline.WithPreHook(cb)
		}
		// Wire the per-host frame redactor so a redact decision rewrites the buffered
		// timeline (masked text spliced in, non-text frames byte-verbatim) instead of a
		// verbatim replay (a PII leak). nil adapter → no redactor → posture-aware degrade
		// (WithStrictFailClosed). Redactor carries the per-caller posture (open/closed).
		var sseAdapter traffic.Adapter
		if audCtx != nil {
			sseAdapter = audCtx.adapter
		}
		ssePath := ""
		if respInput != nil {
			ssePath = respInput.Path
		}
		spliceWired := false
		if fr := newSSEFrameRedactor(ctx, sseAdapter, ssePath, bo.strictFailClosed, logger); fr != nil {
			bufPipeline.WithFrameRedactor(fr)
			spliceWired = true
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
		stampSpliceRedactedBody(auditInfo, result, spliceWired, bufPipeline)
		emitAudit(logger, audCtx, respInput, auditInfo, bo, result, resp.StatusCode, requestStart, finalizeUsage(ctx, acc), bufPipeline.CapturedBytes())

	case "modela":
		// redact-scope under chunked_async with a per-host adapter: prescan-gated
		// real-time streaming with a bounded tail + escalate-to-buffer redaction via
		// the shared Model-A engine (fail-OPEN wire substrate). responseProbe + the
		// adapter are non-nil here (the routing reached "modela" only on MayRedact +
		// an available adapter).
		runSSEModelA(ctx, w, resp, audCtx, respInput, auditInfo, bo, logger, requestStart, responseProbe, acc, captureMax)

	default:
		logger.Warn("unknown streaming mode, using passthrough", "mode", mode)
		var captureBuf *streaming.CappedBuffer
		var dest io.Writer = w
		if captureMax > 0 {
			captureBuf = streaming.NewCappedBuffer(captureMax)
			dest = io.MultiWriter(w, captureBuf)
		}
		_ = streaming.Passthrough(ctx, resp.Body, dest)
		emitAudit(logger, audCtx, respInput, auditInfo, bo, nil, resp.StatusCode, requestStart, traffic.UsageMeta{}, captureBuf.Bytes())
	}
}

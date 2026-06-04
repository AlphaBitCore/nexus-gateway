// Package handler — proxy_responses.go hosts the subscription-driven
// downstream pipelines shared by the MISS broker leader, the HIT_LIVE
// broker joiner, the cache-HIT replay, and the direct-no-broker path:
// handleStreamWithSubscription (SSE) and handleNonStreamWithSubscription
// (single terminal chunk). It also carries the SSE reader that adapts a
// [streamcache.ChunkSubscription] into the LivePipeline's io.Reader
// contract, plus the chunkUsageHolder that captures the final reported
// usage observed in the chunk timeline.
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/stream"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/estimator"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/envelope"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/streaming"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/quota"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// handleStreamWithSubscription is the unified streaming pipeline used
// by every Phase 5.5 outcome (HIT replay, MISS broker leader, HIT_LIVE
// broker joiner, and the direct-no-broker path). It consumes a
// [streamcache.ChunkSubscription] regardless of the chunk source.
//
// Headers (Content-Type, Cache-Control, Connection, X-Cache,
// X-Nexus-Cache, X-Nexus-Attempts, x-nexus-aigw-stream,
// X-Nexus-Hook, X-Nexus-Coerced) MUST be set by the caller
// before this function flushes the response.
func (h *Handler) handleStreamWithSubscription(
	r *http.Request,
	w http.ResponseWriter,
	rec *audit.Record,
	sub streamcache.ChunkSubscription,
	target routingcore.RoutingTarget,
	coerced []string,
	quotaInPrice, quotaOutPrice float64,
	quotaDecision *quota.Decision,
	endpointType, requestID string,
	start time.Time,
	logger *slog.Logger,
) {
	defer func() {
		if err := sub.Close(); err != nil {
			logger.Debug("subscription close error", "error", err)
		}
	}()

	// Match the upstream Anthropic / OpenAI Content-Type byte-for-byte
	// — both append `; charset=utf-8`.
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	if len(coerced) > 0 {
		w.Header().Set("X-Nexus-Coerced", joinCSV(coerced))
	}

	// Extend server write deadline for streaming. Pinned to the live
	// upstream budget + 30s slack so a slow provider that hits its own
	// timeout still has time to surface the error before the server-side
	// write deadline kills the connection.
	if rc := http.NewResponseController(w); rc != nil {
		_ = rc.SetWriteDeadline(time.Now().Add(specutil.ActiveConfig().Timeout + 30*time.Second))
	}
	w.WriteHeader(http.StatusOK)

	// Derive endpoint type for hook filtering. The ingress descriptor is
	// stored on the request context by ServeProxy before any cache path
	// is entered; fall back to an empty type when not present.
	var streamEpType hookcore.EndpointType
	if streamIngress, ok := IngressFromContext(r.Context()); ok {
		streamEpType = typology.KindFromWireShape(streamIngress.WireShape)
	}
	streamModalities := []hookcore.Modality{hookcore.ModalityText}

	hookRunner := func(ctx context.Context, input *hookcore.HookInput) *hookcore.CompliancePipelineResult {
		input.EndpointType = streamEpType
		input.OutputModality = streamModalities
		pipeline, err := h.deps.HookConfigCache.Resolver(ctx).BuildPipeline(
			"response", "AI_GATEWAY",
			streamEpType,
			streamModalities,
			5*time.Second, 15*time.Second, false, logger,
		)
		if err != nil {
			logger.Error("failed to build response hook pipeline for stream", "error", err)
			return &hookcore.CompliancePipelineResult{Decision: hookcore.Approve}
		}
		if pipeline == nil {
			return &hookcore.CompliancePipelineResult{Decision: hookcore.Approve}
		}
		pipeline.SetAllowModify(true)
		pipeline.SetClearSoftOnApprove(true)
		return pipeline.Execute(ctx, input)
	}

	// HoldBack accumulates assistant deltas server-side until the first
	// compliance checkpoint approves. With FirstInspectChars=400 a
	// short response (e.g. Claude Code's "say hi" → ~5 tokens) never
	// hits the checkpoint mid-stream, so every chunk waits for the
	// final flush at end-of-stream — and the client sees a buffered
	// (Content-Length-bounded) body instead of a real SSE stream,
	// breaking Anthropic SDK / Claude Code's streaming UI rendering.
	//
	// Trade-off: HoldBack is ONLY useful when a response-stage hook
	// pipeline can actually reject content. If the response stage has
	// no rules wired (BuildPipeline returns nil), there is nothing to
	// gate on — we should pass chunks through live. Probe the resolver
	// once at stream entry; if the pipeline is nil we drop HoldBack so
	// the client sees real-time deltas. If a rule pack is configured
	// later, the next request rebuilds and re-enters HoldBack.
	holdBack := true
	if h.deps != nil && h.deps.HookConfigCache != nil {
		probe, probeErr := h.deps.HookConfigCache.Resolver(r.Context()).BuildPipeline(
			"response", "AI_GATEWAY",
			streamEpType,
			streamModalities,
			5*time.Second, 15*time.Second, false, logger,
		)
		if probeErr == nil && probe == nil {
			holdBack = false
		}
	}

	// The OpenAI `[DONE]` terminator is conditional on the ingress
	// format: OpenAI-shape clients expect it as the stream sentinel,
	// Anthropic / Gemini clients do NOT (their typed terminal event
	// closes the stream and a stray `data: [DONE]` line confuses
	// strict SDK parsers — Claude Code's symptom of "blank assistant
	// message even though all deltas arrived" was this exact bug).
	emitDone := false
	if ingress, ok := IngressFromContext(r.Context()); ok {
		emitDone = ingress.BodyFormat.IsOpenAIFamily()
	}
	// #115 — resolve admin streaming mode + buffer cap from the Store.
	// ai-gateway honors buffer_full_block (architect parity fix;
	// replaces the prior "chunked_async only" hardcode). Three-service
	// alignment: agent / compliance-proxy / ai-gateway all dispatch on
	// the same streampolicy.Store snapshot. Nil Store falls through to
	// chunked_async — the default for traffic that has already opted
	// into the gateway (unlike tlsbump's transparent-forwarder posture
	// where nil Store means "no opt-in, transparent passthrough").
	//
	// #115/O6 follow-up: read MaxBufferBytes from the same snapshot so
	// admin-configured caps (64MB default, larger for high-volume
	// deployments) propagate into both buffer and live pipelines. Zero
	// means "use the pipeline's built-in default" (8MB) — same shape as
	// the underlying BufferConfig / LiveConfig.
	streamMode := streampolicy.ModeChunkedAsync
	streamMaxBufferBytes := 0
	if h.deps.StreamingPolicy != nil {
		snapshot := h.deps.StreamingPolicy.Get()
		streamMode = snapshot.Mode
		streamMaxBufferBytes = snapshot.MaxBufferBytes
	}

	// Build a cross-format stream transcoder when the ingress and target wire
	// shapes differ. The transcoder converts canonical provider.Chunk fields
	// into ingress-native SSE frames so the client always receives the format
	// it expects. Returns nil for same-format pairs (passthrough).
	//
	// B2 cross-ingress override: when the cache HIT entry was written
	// under a different ingress wire shape (StreamHitOriginFromContext
	// returns ok=true with a non-matching BodyFormat), pick the
	// transcoder as if the "target" were the entry's origin wire shape.
	// That forces the chunkSSEReader to re-encode the cached canonical
	// chunks into the current ingress's SSE frames instead of forwarding
	// the cached RawBytes (which carry the writer's wire shape) verbatim.
	var transcoder canonicalbridge.StreamTranscoder
	var ingressFormat provcore.Format
	if ingress, ok := IngressFromContext(r.Context()); ok {
		ingressFormat = ingress.BodyFormat
		if h.deps.CanonicalBridge != nil {
			targetFormat := provcore.Format(target.AdapterType)
			origin, originOK := StreamHitOriginFromContext(r.Context())
			var originBodyFormat provcore.Format
			if originOK {
				var mapped bool
				originBodyFormat, mapped = WireShapeToBodyFormat(origin.WireShape)
				if !mapped {
					// Origin wire shape has no Format mapping (e.g. a future
					// Gemini/Vertex cache lane); skip the cross-ingress
					// transcoder override and let NewStreamTranscoder pick
					// the default for the current ingress + target pair.
					originOK = false
				} else {
					targetFormat = originBodyFormat
				}
			}
			transcoder = h.deps.CanonicalBridge.NewStreamTranscoder(ingress.BodyFormat, targetFormat, target.ModelCode)
			// Override edge case: the standard NewStreamTranscoder returns
			// nil for "ingress=FormatOpenAIResponses && target natively
			// serves Responses" (passthrough). On a cross-ingress cache
			// HIT where the cached chunks were written by a chat-completions
			// ingress, that passthrough would forward chat.completion SSE
			// frames to a /v1/responses client. Force the explicit ingress
			// encoder so the cached canonical chunks are re-encoded into
			// the request's wire SSE grammar.
			if originOK && transcoder == nil && originBodyFormat != ingress.BodyFormat {
				switch ingress.BodyFormat {
				case provcore.FormatOpenAIResponses:
					transcoder = canonicalbridge.NewResponsesStreamEncoder(target.ModelCode)
				default:
					if ingress.BodyFormat.IsOpenAIFamily() {
						transcoder = canonicalbridge.NewChatCompletionsStreamEncoder(target.ModelCode)
					}
				}
			}
		}
	}
	// Auto-upgrade: the client sent /v1/chat/completions but the upstream
	// actually got /v1/responses (its SSE is Responses-grammar chunks).
	// The (ingress=OpenAI, target=OpenAI) pair above resolved to nil
	// (same-format passthrough) — but we need to RE-ENCODE the chunks
	// back to chat-completions SSE so the chat-completions SDK can parse
	// them. Override with the chat-completions encoder.
	if ResponsesUpgradeFromContext(r.Context()) {
		transcoder = canonicalbridge.NewChatCompletionsStreamEncoder(target.ModelCode)
	}

	// Drain the subscription (replay or live broker pump) into an
	// io.Reader of SSE-formatted lines so LivePipeline.Process can
	// consume it unchanged.
	sseReader := newChunkSSEReaderFromSubscription(r.Context(), sub, transcoder, ingressFormat)

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
		},
		OnStreamRewrite: func(n int) {
			rec.ResponseHookRewritten = true
			rec.ResponseHookRewriteCount += n
		},
	}

	pcStream := h.payloadCaptureConfig()
	hardCap := h.streamCaptureHardCap()
	tee := newStreamCaptureTee(w, hardCap)

	// #115/R1 dispatch — three streaming modes, one helper per mode.
	// Three-service alignment: tlsbump (agent + cp) honors all three
	// modes in shared/transport/tlsbump/sse.go's resolveStreamingMode
	// switch; ai-gateway now does the same. Collapsing passthrough
	// into live (the original #115 oversight) silently kept hooks
	// running on traffic the admin had explicitly opted out of —
	// fixed here so admin policy is honored consistently across all
	// three services.
	streamDeps := runStreamDeps{
		Deps:           h.deps,
		AdapterType:    target.AdapterType,
		Path:           r.URL.Path,
		AcceptHeader:   r.Header.Get("Accept"),
		HookRunner:     hookRunner,
		HookCtx:        hookCtx,
		SSEReader:      sseReader,
		Tee:            tee,
		Logger:         logger,
		HoldBack:       holdBack,
		EmitDone:       emitDone,
		MaxBufferBytes: streamMaxBufferBytes,
	}
	dispatchStreamMode(r.Context(), streamMode, streamDeps)
	logger.Debug("stream response capture",
		"hardCap", hardCap,
		"capturedBytes", len(tee.captured()),
		"truncated", tee.truncatedBeyondCap(),
		"storeFlag", pcStream.StoreResponseBody,
	)
	if pcStream.StoreResponseBody {
		rec.ResponseBody = tee.captured()
		rec.ResponseTruncated = tee.truncatedBeyondCap()
		rec.ResponseContentType = "text/event-stream"
	}
	rec.StatusCode = http.StatusOK

	// Extract usage accumulated during streaming. Prefer rec values
	// already set by handleStreamHit (they came from the cache entry);
	// otherwise read what the SSE reader observed live.
	usage := metrics.Usage{
		PromptTokens:     rec.PromptTokens,
		CompletionTokens: rec.CompletionTokens,
		TotalTokens:      rec.TotalTokens,
	}
	if usage.PromptTokens == 0 && usage.CompletionTokens == 0 && usage.TotalTokens == 0 {
		live := usageHolder.snapshot()
		promptTok := usageInt(live.PromptTokens)
		complTok := usageInt(live.CompletionTokens)
		totalTok := usageInt(live.TotalTokens)
		if promptTok > 0 || complTok > 0 || totalTok > 0 {
			usage = metrics.Usage{
				PromptTokens:     int64(promptTok),
				CompletionTokens: int64(complTok),
				TotalTokens:      int64(totalTok),
			}
			rec.PromptTokens = usage.PromptTokens
			rec.CompletionTokens = usage.CompletionTokens
			rec.TotalTokens = usage.TotalTokens
			// Use per-endpoint formula so embeddings are priced correctly.
			streamCostUnits := estimator.BillableUnits{
				PromptTokens:     int(rec.PromptTokens),
				CompletionTokens: int(rec.CompletionTokens),
			}
			fullCost := estimator.Lookup(rec.EndpointType)(streamCostUnits, metrics.ModelPrices{
				InputUsdPerM:  &quotaInPrice,
				OutputUsdPerM: &quotaOutPrice,
			}).Total
			rec.EstimatedCostUsd = fullCost
			// Stamp ProviderCacheStatus from upstream usage cache fields.
			// Skip if already set (the broker joiner path stamps NA earlier).
			if rec.ProviderCacheStatus == "" {
				rec.ProviderCacheStatus = audit.ClassifyProviderCache(live.CacheReadTokens, live.CacheCreationTokens)
			}
			if live.CacheReadTokens != nil {
				rec.CacheReadTokens = int64(*live.CacheReadTokens)
			}
			if live.CacheCreationTokens != nil {
				rec.CacheCreationTokens = int64(*live.CacheCreationTokens)
			}
			// reasoning_tokens from the broker-leader live stream.
			if live.ReasoningTokens != nil {
				rec.ReasoningTokens = int64(*live.ReasoningTokens)
			}
			h.computeCacheCosts(rec, target)
			// HIT_LIVE: this joiner did not call the provider; actual cost is 0.
			// The leader (MISS) already accounts for the upstream spend and any
			// Provider prompt-cache savings, so clear those here to avoid double-counting.
			if rec.GatewayCacheStatus == audit.GatewayCacheHitInflight {
				rec.GatewayCacheSavingsUsd = fullCost
				rec.EstimatedCostUsd = 0
				rec.CacheCreationTokens = 0
				rec.CacheReadTokens = 0
				rec.CacheWriteCostUsd = 0
				rec.CacheReadSavingsUsd = 0
				rec.CacheNetSavingsUsd = 0
			}
			rec.UsageExtractionStatus = "streaming_reported"
		} else {
			// Stream completed but provider emitted no usage frame.
			// Tier-2 tokenizer estimation is not enabled for AI Gateway
			// (the upstream providers we support emit usage at near-100%).
			rec.UsageExtractionStatus = "streaming_unavailable"
		}
	}

	if h.deps.Metrics != nil {
		h.deps.Metrics.RecordRequest(target.ProviderName, target.ModelID, endpointType, rec.StatusCode, time.Since(start), usage)
	}

	// Quota reconcile. Streaming branch matches the non-streaming path's
	// status_code < 400 filter so streams that errored mid-flight do not
	// increment the runtime quota counter.
	//
	// Also skip Reconcile when the gateway cache served the response —
	// HIT (replay from L1) and HIT_INFLIGHT (joiner waiting on a leader)
	// both mean this caller did not pay for an upstream call. The leader
	// reconciles its own cost; charging joiners or HIT replays a second
	// time inflates the quota counter against $0 of actual spend.
	gatewayServed := rec.GatewayCacheStatus == audit.GatewayCacheHit || rec.GatewayCacheStatus == audit.GatewayCacheHitInflight
	if h.deps.QuotaEngine != nil && quotaDecision != nil && quotaDecision.Allowed && rec.StatusCode < 400 && !gatewayServed {
		go func() {
			defer func() {
				if rcv := recover(); rcv != nil {
					h.deps.Logger.Error("quota engine reconcile panic", "panic", rcv)
				}
			}()
			rcCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			h.deps.QuotaEngine.Reconcile(rcCtx, quotaDecision, quota.ActualUsage{
				PromptTokens:     usage.PromptTokens,
				CompletionTokens: usage.CompletionTokens,
				TotalTokens:      usage.TotalTokens,
				InputPricePM:     quotaInPrice,
				OutputPricePM:    quotaOutPrice,
			})
		}()
	}
}

// handleNonStreamWithSubscription drains the broker subscription's
// single terminal chunk (whose Delta carries the canonical response
// JSON), runs response-stage hooks (D2), and writes JSON to the
// client. Used on the non-stream MISS / HIT_LIVE paths via the
// streamcache broker; the cache HIT path goes through
// handleNonStreamHit (no broker, direct from Redis).
func (h *Handler) handleNonStreamWithSubscription(
	r *http.Request,
	w http.ResponseWriter,
	rec *audit.Record,
	sub streamcache.ChunkSubscription,
	target routingcore.RoutingTarget,
	coerced []string,
	quotaInPrice, quotaOutPrice float64,
	quotaDecision *quota.Decision,
	endpointType, requestID string,
	start time.Time,
	logger *slog.Logger,
	routeResult *routingcore.RouteResult,
	canonicalMsgs []normcore.Message,
) {
	defer func() {
		if err := sub.Close(); err != nil {
			logger.Debug("subscription close error", "error", err)
		}
	}()
	ctx := r.Context()

	// Drain the single terminal chunk.
	var (
		canonicalBody []byte
		usage         provcore.Usage
	)
	for {
		chunk, err := sub.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var pe *provcore.ProviderError
			if errors.As(err, &pe) {
				h.writeDetailedErr(w, rec, pe.Status, pe.Code, pe.Message, "")
				return
			}
			h.writeError(w, rec, http.StatusBadGateway, err.Error())
			return
		}
		if chunk.Delta != "" {
			canonicalBody = []byte(chunk.Delta)
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
		if chunk.Done {
			break
		}
	}

	// canonicalBody is the canonical (OpenAI) response: the leader's
	// upstream call decoded the target wire shape to canonical via
	// SchemaCodec.DecodeResponse (specAdapter.Execute returns CanonicalBody),
	// and the broker fans out those canonical bytes. egressReshapeNonStream
	// below re-encodes it to the caller's ingress shape ("B→canonical→A").
	respBody := canonicalBody

	ingress, _ := IngressFromContext(ctx)

	// L2 semantic write-back — only on a leader MISS, not on HIT_INFLIGHT
	// joiners (joiners just replay broker frames). Direct (non-broker)
	// path fires scheduleL2Write inside proxy.go::ServeProxy.
	if rec.GatewayCacheStatus == audit.GatewayCacheMiss && len(canonicalBody) > 0 {
		h.scheduleL2Write(
			routeResult,
			target,
			canonicalMsgs,
			canonicalBody,
			provcoreUsageToMap(&usage),
			resolveL2VKScope(rec, ""),
			false,
			ingress,
			logger,
		)
	}
	// Egress reshape — broker MISS / HIT_LIVE non-stream. respBody is the
	// canonical body; funnel through the single egress helper so the broker
	// path obeys "B→canonical→A" for EVERY ingress (the prior
	// WireShape==OpenAIChat-only guard silently returned canonical OpenAI for
	// anthropic /v1/messages + gemini /v1beta — this is the prod path that
	// produced the wrong-envelope responses).
	if shaped, err := h.egressReshapeNonStream(ingress, target, respBody); err != nil {
		logger.Error("response hub reshape failed (broker non-stream)", "error", err)
		h.writeError(w, rec, http.StatusBadGateway, "upstream response could not be reshaped for ingress format")
		return
	} else {
		respBody = shaped
	}

	usageMet := metrics.Usage{
		PromptTokens:     int64(usageInt(usage.PromptTokens)),
		CompletionTokens: int64(usageInt(usage.CompletionTokens)),
		TotalTokens:      int64(usageInt(usage.TotalTokens)),
	}
	rec.PromptTokens = usageMet.PromptTokens
	rec.CompletionTokens = usageMet.CompletionTokens
	rec.TotalTokens = usageMet.TotalTokens
	// Embeddings cost/usage fallback (same as handleNonStream): providers
	// that report no token usage (e.g. Gemini embedContent) get prompt_tokens
	// back-filled from the request-side local estimate so the cost formula
	// yields a non-zero embedding cost.
	if pt := embeddingTokenFallback(rec.EndpointType, rec.PromptTokens, rec.Metadata); pt != rec.PromptTokens {
		rec.PromptTokens = pt
		rec.TotalTokens = pt
		usageMet.PromptTokens = pt
		usageMet.TotalTokens = pt
	}
	// Use per-endpoint formula so embeddings are priced correctly.
	brokerCostUnits := estimator.BillableUnits{
		PromptTokens:     int(rec.PromptTokens),
		CompletionTokens: int(rec.CompletionTokens),
	}
	fullCost := estimator.Lookup(rec.EndpointType)(brokerCostUnits, metrics.ModelPrices{
		InputUsdPerM:  &quotaInPrice,
		OutputUsdPerM: &quotaOutPrice,
	}).Total
	rec.EstimatedCostUsd = fullCost
	// Stamp ProviderCacheStatus from upstream usage cache fields. Skip if
	// already set (joiners stamp NA earlier in this function).
	if rec.ProviderCacheStatus == "" {
		rec.ProviderCacheStatus = audit.ClassifyProviderCache(usage.CacheReadTokens, usage.CacheCreationTokens)
	}
	if usage.CacheReadTokens != nil {
		rec.CacheReadTokens = int64(*usage.CacheReadTokens)
	}
	if usage.CacheCreationTokens != nil {
		rec.CacheCreationTokens = int64(*usage.CacheCreationTokens)
	}
	// reasoning_tokens from the broker non-stream MISS path.
	if usage.ReasoningTokens != nil {
		rec.ReasoningTokens = int64(*usage.ReasoningTokens)
	}
	h.computeCacheCosts(rec, target)
	// HIT_LIVE: this joiner did not call the provider; actual cost is 0.
	// The leader (MISS) already accounts for the upstream spend and any
	// Provider prompt-cache savings, so clear those here to avoid double-counting.
	if rec.GatewayCacheStatus == audit.GatewayCacheHitInflight {
		rec.GatewayCacheSavingsUsd = fullCost
		rec.EstimatedCostUsd = 0
		rec.CacheCreationTokens = 0
		rec.CacheReadTokens = 0
		rec.CacheWriteCostUsd = 0
		rec.CacheReadSavingsUsd = 0
		rec.CacheNetSavingsUsd = 0
	}
	if usageMet.PromptTokens > 0 || usageMet.CompletionTokens > 0 || usageMet.TotalTokens > 0 {
		rec.UsageExtractionStatus = "ok"
	} else {
		rec.UsageExtractionStatus = "parse_failed"
	}
	// Update embedding dimension from the canonical response body.
	if rec.EndpointType == "embeddings" {
		rec.Metadata = updateEmbeddingDimension(rec.Metadata, respBody)
	}

	// Response-stage hooks — same code as handleNonStream.
	{
		extractor := h.trafficAdapterFor(ingress.BodyFormat)
		ingressFormat := string(ingress.BodyFormat)
		respContent, respModel, respFinish := h.extractResponseForHooks(ctx, extractor, ingressFormat, respBody, r.URL.Path, logger)
		brokerEpType := typology.KindFromWireShape(ingress.WireShape)
		respInput := &hookcore.HookInput{
			RequestID:      requestID,
			Stage:          "response",
			Normalized:     respContent,
			IngressType:    "AI_GATEWAY",
			Path:           r.URL.Path,
			Model:          respModel,
			FinishReason:   respFinish,
			TokenCount:     int(usageMet.TotalTokens),
			SourceIP:       middleware.ClientIP(r),
			ProviderRegion: target.Region,
			EndpointType:   brokerEpType,
			OutputModality: []hookcore.Modality{hookcore.ModalityText},
		}
		pipeline, pErr := h.deps.HookConfigCache.Resolver(ctx).BuildPipeline(
			"response", "AI_GATEWAY",
			brokerEpType,
			respInput.OutputModality,
			5*time.Second, 15*time.Second, false, logger,
		)
		if pErr != nil {
			logger.Error("failed to build response hook pipeline (broker non-stream)", "error", pErr)
			h.writeError(w, rec, http.StatusInternalServerError, "hook pipeline error")
			return
		}
		if pipeline != nil {
			pipeline.SetAllowModify(true)
			pipeline.SetClearSoftOnApprove(true)
			hookResult := pipeline.Execute(ctx, respInput)
			rec.ResponseHookDecision = string(hookResult.Decision)
			rec.ResponseHookReason = hookResult.Reason
			rec.ResponseHookReasonCode = hookResult.ReasonCode
			rec.ComplianceTags = mergeTagSets(rec.ComplianceTags, hookResult.Tags)
			rec.HooksPipeline = appendHookTrace(rec.HooksPipeline, "response", hookResult.HookResults)
			if br := mapBlockingRule(hookResult.BlockingRule); br != nil {
				rec.BlockingRule = br
			}
			if h.deps.Metrics != nil {
				h.deps.Metrics.RecordHookRequest(ingressFormat, "response", string(hookResult.Decision))
			}
			switch hookResult.Decision {
			case hookcore.RejectHard:
				h.writeError(w, rec, http.StatusForbidden, hookResult.Reason)
				return
			case hookcore.BlockSoft:
				h.writeError(w, rec, 246, hookResult.Reason)
				return
			case hookcore.Modify:
				if len(hookResult.ModifiedContent) > 0 {
					rewritten, n, rErr := extractor.RewriteResponseBody(ctx, respBody, r.URL.Path, contentBlocksToNormalized(hookResult.ModifiedContent))
					switch {
					case errors.Is(rErr, traffic.ErrRewriteUnsupported):
						logger.Warn("hook produced Modify on response but adapter does not support rewrite; returning original body",
							slog.String("adapter", extractor.ID()),
							slog.String("path", r.URL.Path),
						)
					case rErr != nil:
						logger.Error("hook response rewrite failed (broker non-stream)",
							slog.String("adapter", extractor.ID()),
							slog.String("path", r.URL.Path),
							slog.String("error", rErr.Error()),
						)
						h.writeError(w, rec, http.StatusInternalServerError, "response rewrite failed")
						return
					default:
						respBody = rewritten
						rec.ResponseHookRewriteCount = n
						rec.ResponseHookRewritten = true
					}
				}
			}
		}
	}

	pcCfg := h.payloadCaptureConfig()
	if pcCfg.StoreResponseBody && len(respBody) > 0 {
		rec.ResponseBody = respBody
		rec.ResponseContentType = "application/json"
	}
	rec.StatusCode = http.StatusOK

	if h.deps.Metrics != nil {
		h.deps.Metrics.RecordRequest(target.ProviderName, target.ModelID, endpointType, rec.StatusCode, time.Since(start), usageMet)
	}
	// Skip reconcile if the gateway cache served the response (HIT or
	// HIT_INFLIGHT) — see the streaming branch above for rationale.
	gatewayServed := rec.GatewayCacheStatus == audit.GatewayCacheHit || rec.GatewayCacheStatus == audit.GatewayCacheHitInflight
	if h.deps.QuotaEngine != nil && quotaDecision != nil && quotaDecision.Allowed && rec.StatusCode < 400 && !gatewayServed {
		go func() {
			defer func() {
				if rcv := recover(); rcv != nil {
					h.deps.Logger.Error("quota engine reconcile panic (broker non-stream)", "panic", rcv)
				}
			}()
			rcCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			h.deps.QuotaEngine.Reconcile(rcCtx, quotaDecision, quota.ActualUsage{
				PromptTokens:     usageMet.PromptTokens,
				CompletionTokens: usageMet.CompletionTokens,
				TotalTokens:      usageMet.TotalTokens,
				InputPricePM:     quotaInPrice,
				OutputPricePM:    quotaOutPrice,
			})
		}()
	}

	if len(coerced) > 0 {
		w.Header().Set("X-Nexus-Coerced", joinCSV(coerced))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(respBody)
}

// joinCSV joins parts with ',' separators. Local helper to avoid
// pulling in strings just for this one site (the rest of the file
// keeps its existing import surface).
func joinCSV(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	if len(parts) == 1 {
		return parts[0]
	}
	n := len(parts) - 1
	for _, p := range parts {
		n += len(p)
	}
	b := make([]byte, 0, n)
	for i, p := range parts {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, p...)
	}
	return string(b)
}

// chunkUsageHolder collects the most recent non-nil Usage observed by
// the chunkSSEReader. It is updated from the reader's hot path and read
// once after LivePipeline.Process returns. Concurrent access is bounded
// to one writer + one reader after the pump terminates, but we use an
// atomic.Pointer for safety since LivePipeline runs the reader in a
// goroutine.
type chunkUsageHolder struct {
	usage atomic.Pointer[provcore.Usage]
}

// record merges u into the accumulated usage snapshot. Non-nil fields in u
// overwrite the corresponding field in the current snapshot; nil fields leave
// the existing value untouched. This lets multi-event providers (Anthropic
// message_start + message_delta) accumulate a complete picture without losing
// fields that arrived on an earlier event.
//
// After merge, TotalTokens is recomputed as the sum of all non-nil token
// components so that the stored total reflects the true aggregate even when
// the provider spreads usage across multiple SSE events.
func (h *chunkUsageHolder) record(u *provcore.Usage) {
	if h == nil || u == nil {
		return
	}
	for {
		prev := h.usage.Load()
		var merged provcore.Usage
		if prev != nil {
			merged = *prev
		}
		if u.PromptTokens != nil {
			merged.PromptTokens = u.PromptTokens
		}
		if u.CompletionTokens != nil {
			merged.CompletionTokens = u.CompletionTokens
		}
		if u.CacheReadTokens != nil {
			merged.CacheReadTokens = u.CacheReadTokens
		}
		if u.CacheCreationTokens != nil {
			merged.CacheCreationTokens = u.CacheCreationTokens
		}
		if u.ReasoningTokens != nil {
			merged.ReasoningTokens = u.ReasoningTokens
		}
		// Prefer the provider-supplied total when present; otherwise
		// compute from parts so Anthropic's split events yield a correct sum.
		if u.TotalTokens != nil {
			merged.TotalTokens = u.TotalTokens
		} else if merged.PromptTokens != nil || merged.CompletionTokens != nil {
			total := 0
			if merged.PromptTokens != nil {
				total += *merged.PromptTokens
			}
			if merged.CacheReadTokens != nil {
				total += *merged.CacheReadTokens
			}
			if merged.CacheCreationTokens != nil {
				total += *merged.CacheCreationTokens
			}
			if merged.CompletionTokens != nil {
				total += *merged.CompletionTokens
			}
			merged.TotalTokens = &total
		}
		if h.usage.CompareAndSwap(prev, &merged) {
			break
		}
	}
}

func (h *chunkUsageHolder) snapshot() provcore.Usage {
	if h == nil {
		return provcore.Usage{}
	}
	if u := h.usage.Load(); u != nil {
		return *u
	}
	return provcore.Usage{}
}

// chunkSSEReader adapts a [streamcache.ChunkSubscription] into an
// io.Reader that emits SSE-formatted lines ("data: ...\n\n" or the
// upstream's typed terminator on Done). The OpenAI `data: [DONE]\n\n`
// sentinel is appended further downstream by streaming.LivePipeline,
// gated on LiveConfig.EmitOpenAIDone (only for OpenAI-shape ingress).
//
// Frame encoding prefers chunk.RawBytes when the upstream preserved
// the native frame; otherwise it falls back to a minimal OpenAI-compat
// envelope around chunk.Delta so that the LivePipeline has something
// coherent to parse.
//
// On replay (cache HIT) the underlying ChunkSubscription returns
// canonical chunks WITHOUT RawBytes; the Delta fallback path is what
// regenerates an SSE frame in those cases. The transcoder upstream of
// this reader (streaming.NewLivePipeline + downstream encoders) will
// tune the regenerated frame per ingress format.
type chunkSSEReader struct {
	ctx           context.Context
	sub           streamcache.ChunkSubscription
	usageSink     *chunkUsageHolder
	buf           []byte
	closed        bool
	err           error
	transcoder    canonicalbridge.StreamTranscoder // non-nil for cross-format; nil for passthrough
	ingressFormat provcore.Format                  // ingress wire shape; drives SSE error-frame envelope (G4)
}

func newChunkSSEReaderFromSubscription(ctx context.Context, sub streamcache.ChunkSubscription, transcoder canonicalbridge.StreamTranscoder, ingressFormat provcore.Format) *chunkSSEReader {
	return &chunkSSEReader{ctx: ctx, sub: sub, transcoder: transcoder, ingressFormat: ingressFormat}
}

func (r *chunkSSEReader) Read(p []byte) (int, error) {
	if len(r.buf) > 0 {
		n := copy(p, r.buf)
		r.buf = r.buf[n:]
		return n, nil
	}
	if r.closed {
		if r.err != nil {
			return 0, r.err
		}
		return 0, io.EOF
	}
	if r.sub == nil {
		r.closed = true
		return 0, io.EOF
	}

	chunk, err := r.sub.Next(r.ctx)
	if err != nil {
		if errors.Is(err, io.EOF) {
			r.closed = true
			return 0, io.EOF
		}
		r.closed = true
		r.err = err
		// Context cancellation (client disconnect / timeout) — let the
		// caller's read loop exit cleanly; no error event to the client.
		if r.ctx.Err() != nil {
			return 0, err
		}
		// Provider error (e.g. empty upstream SSE body): synthesise a
		// terminal SSE error frame in the ingress format so the client
		// receives a parseable error payload rather than an abrupt
		// connection close. G4: the envelope must follow the ingress
		// SDK contract (OpenAI vs Anthropic vs Gemini) — see
		// provider-adapter-architecture.md §9.5.
		var pe *provcore.ProviderError
		if !errors.As(err, &pe) {
			pe = &provcore.ProviderError{
				Status:  http.StatusBadGateway,
				Code:    provcore.CodeUpstreamError,
				Message: err.Error(),
			}
		}
		r.buf = envelope.SynthesizeSSEErrorFrame(r.ingressFormat, pe)
		n := copy(p, r.buf)
		r.buf = r.buf[n:]
		return n, nil
	}

	if chunk.Usage != nil {
		r.usageSink.record(chunk.Usage)
	}

	switch {
	case chunk.Done:
		// Cross-format: transcoder synthesises the ingress-format terminal
		// events (e.g. Anthropic message_stop, Gemini finishReason frame).
		// Passthrough: forward the provider's raw terminal frame so that
		// native ingress clients (Anthropic SDK, Gemini SDK, etc.) receive
		// the typed terminator they expect.
		if r.transcoder != nil {
			b, _ := r.transcoder.Write(r.ctx, chunk)
			if len(b) > 0 {
				r.buf = b
			}
		} else if len(chunk.RawBytes) > 0 {
			r.buf = append([]byte(nil), chunk.RawBytes...)
		}
		r.closed = true
	case r.transcoder != nil:
		// Cross-format: delegate all non-Done chunks to the transcoder so
		// provider-native RawBytes are never forwarded to the client.
		b, err := r.transcoder.Write(r.ctx, chunk)
		if err != nil {
			r.closed = true
			r.err = err
			return 0, err
		}
		if len(b) == 0 {
			return 0, nil // transcoder skipped this chunk (e.g. Anthropic ping)
		}
		r.buf = b
	case len(chunk.RawBytes) > 0:
		// Passthrough: stream decoders set RawBytes to a complete SSE frame.
		r.buf = append([]byte(nil), chunk.RawBytes...)
	case chunk.Delta != "":
		// Passthrough fallback: synthesise a minimal OpenAI-compat SSE
		// frame from the canonical Delta when RawBytes are absent
		// (e.g. cache replay). This branch fires ONLY when transcoder ==
		// nil, which means ingress == target wire shape; and same-shape
		// passthrough today is exclusively OpenAI-shape (Anthropic /
		// Gemini same-shape goes through their respective transcoder),
		// so the hardcoded OpenAI envelope here is correct — NOT a
		// §9.5 violation. If a future non-OpenAI-shape ingress acquires
		// a same-shape passthrough path, this case must branch on
		// r.ingressFormat the way synthesizeSSEErrorFrame does.
		envelope, _ := json.Marshal(map[string]any{
			"choices": []map[string]any{
				{"delta": map[string]string{"content": chunk.Delta}},
			},
		})
		r.buf = fmt.Appendf(nil, "data: %s\n\n", envelope)
	default:
		return 0, nil
	}

	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

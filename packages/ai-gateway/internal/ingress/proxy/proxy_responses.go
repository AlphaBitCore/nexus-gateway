// Package handler — proxy_responses.go hosts the subscription-driven
// downstream pipelines shared by the MISS broker leader, the HIT_LIVE
// broker joiner, the cache-HIT replay, and the direct-no-broker path:
// handleStreamWithSubscription (SSE; the driver over the streaming
// stage chain in stream_*.go) and handleNonStreamWithSubscription
// (single terminal chunk). It also carries the SSE reader that adapts a
// [streamcache.ChunkSubscription] into the LivePipeline's io.Reader
// contract, plus the chunkUsageHolder that captures the final reported
// usage observed in the chunk timeline.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"github.com/goccy/go-json"
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
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/quota"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
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
//
// The handler drives the stream through an explicit stage chain — one
// type per stage, each in its stream_<name>.go file: preamble (SSE
// headers, write deadline, 200 flush) → response hooks (per-checkpoint
// pipeline runner, hold-back decision) → wire shape ([DONE] sentinel,
// admin streaming mode, transcoder selection) → relay (SSE reader,
// capture tee, mode dispatch) → accounting (usage, cost, terminal-error
// classification, metrics, quota reconcile). Shared per-stream state
// travels in [streamState]; the subscription close runs as the
// function-scoped defer so every exit path releases it.
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
	s := h.newStreamState(r, w, rec, sub, target, coerced,
		quotaInPrice, quotaOutPrice, quotaDecision,
		endpointType, requestID, start, logger)
	for _, stage := range []proxyStage{
		streamPreambleStage{s},
		streamHooksStage{s},
		streamShapeStage{s},
		streamRelayStage{s},
		streamAccountingStage{s},
	} {
		if !stage.run() {
			return
		}
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
		truncated     bool
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
		// The leader synthesises this terminal chunk from a buffered
		// ExecutionResult; a clamped read cap on the leader is fanned out to
		// every joiner here so the usage status reflects the truncation.
		if chunk.Truncated {
			truncated = true
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
			rec,
			target,
			canonicalMsgs,
			canonicalBody,
			provcoreUsageToMap(&usage),
			false,
			ingress,
			logger,
		)
	}
	// Response compliance runs on the CANONICAL body BEFORE egress reshape (B0):
	// redaction rewrites canonical, then the egress codec forward-encodes it —
	// always supported, so the reverse-encode fail-closed / leak path is gone.
	redacted, _, blocked := h.runResponseHooksOnCanonical(w, r, rec, ingress, target, respBody, int64(usageInt(usage.TotalTokens)), requestID, logger)
	if blocked {
		return
	}
	respBody = redacted

	// Egress reshape — broker MISS / HIT_LIVE non-stream. respBody is the
	// (possibly redacted) canonical body; funnel through the single egress helper
	// so the broker path obeys "B→canonical→A" for EVERY ingress (the prior
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
	// The reshaped wire body is the redacted, client-consistent copy under a
	// rewrite; persist it as ResponseBodyRedacted (wire-shaped) for the audit copy.
	if rec.ResponseHookRewritten {
		rec.ResponseBodyRedacted = respBody
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
	embeddingEstimated := false
	if pt := embeddingTokenFallback(rec.EndpointType, rec.PromptTokens, rec.Metadata); pt != rec.PromptTokens {
		rec.PromptTokens = pt
		rec.TotalTokens = pt
		usageMet.PromptTokens = pt
		usageMet.TotalTokens = pt
		// Estimated, not provider-reported.
		embeddingEstimated = true
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
	// reasoning_cost_usd breakdown — consistent with the direct path and
	// cache-HIT paths.
	stampReasoningCost(rec, quotaOutPrice)
	h.computeCacheCosts(rec, target)
	// HIT_LIVE: this joiner did not call the provider; actual cost is 0.
	// The leader (MISS) already accounts for the upstream spend and any
	// Provider prompt-cache savings, so clear those here to avoid double-counting.
	if rec.GatewayCacheStatus == audit.GatewayCacheHitInflight {
		rec.GatewayCacheSavingsUsd = fullCost
		rec.EstimatedCostUsd = 0
		rec.ReasoningCostUsd = 0
		rec.CacheCreationTokens = 0
		rec.CacheReadTokens = 0
		rec.CacheWriteCostUsd = 0
		rec.CacheReadSavingsUsd = 0
		rec.CacheNetSavingsUsd = 0
	}
	switch {
	case embeddingEstimated:
		// Estimated embedding token count, not provider-reported
		// (request-side estimate, honest even if the body was truncated).
		rec.UsageExtractionStatus = "estimated"
	case truncated:
		// The leader's buffered response body was clamped at the read
		// cap before usage extraction; the token counts replayed here are
		// incomplete, so flag them instead of claiming "ok".
		rec.ResponseTruncated = true
		rec.UsageExtractionStatus = "truncated"
	case usageMet.PromptTokens > 0 || usageMet.CompletionTokens > 0 || usageMet.TotalTokens > 0:
		rec.UsageExtractionStatus = "ok"
	default:
		rec.UsageExtractionStatus = "parse_failed"
	}
	// Update embedding dimension from the canonical response body.
	if rec.EndpointType == "embeddings" {
		rec.Metadata = updateEmbeddingDimension(rec.Metadata, respBody)
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
	// Charge the single canonical cache-aware cost (rec.EstimatedCostUsd) so the
	// live counter matches the rollup billed_cost_usd and the Backfill seed.
	// Captured before the goroutine to avoid racing rec.
	reconcileCost := rec.EstimatedCostUsd
	if h.deps.QuotaEngine != nil && quotaDecision != nil && quotaDecision.Allowed && rec.StatusCode < 400 && !gatewayServed {
		go func() {
			defer func() {
				if rcv := recover(); rcv != nil {
					h.deps.Logger.Error("quota engine reconcile panic (broker non-stream)", "panic", rcv)
				}
			}()
			rcCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			h.deps.QuotaEngine.Reconcile(rcCtx, quotaDecision, quota.ActualUsage{CostUSD: reconcileCost})
		}()
	}

	if len(coerced) > 0 {
		w.Header().Set("X-Nexus-Coerced", joinCSV(coerced))
	}
	w.Header().Set("Content-Type", "application/json")
	// Extend the write deadline to the upstream request budget so a long
	// non-streaming inference (a reasoning model that returns a big body
	// after minutes of work) is governed by upstream.timeoutSec, not the
	// shorter flat server.writeTimeout that bounds ordinary responses.
	if rc := http.NewResponseController(w); rc != nil {
		_ = rc.SetWriteDeadline(time.Now().Add(specutil.ActiveConfig().Timeout))
	}
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
	ctx       context.Context
	sub       streamcache.ChunkSubscription
	usageSink *chunkUsageHolder
	buf       []byte
	// scratch is the reusable backing array for the passthrough RawBytes
	// copy-out. The caller copies r.buf into its own buffer on every Read, so a
	// chunk's bytes are never retained past the next chunk fetch — the array is
	// safe to reuse across chunks of this stream, turning a per-chunk frame
	// allocation (the dominant SSE relay allocator) into one per stream.
	scratch       []byte
	closed        bool
	err           error
	transcoder    canonicalbridge.StreamTranscoder // non-nil for cross-format; nil for passthrough
	ingressFormat provcore.Format                  // ingress wire shape; drives SSE error-frame envelope (G4)
	// allowVerbatim forwards a chunk's RawBytes byte-for-byte when the decode
	// session marked it Verbatim (genuine Responses upstream on a non-enforced
	// /v1/responses ingress), bypassing the transcoder so built-in-tool / audio
	// events survive. Off everywhere else, so a Verbatim flag is ignored.
	allowVerbatim bool
	// termErr publishes the reader's terminal failure (if any) for the
	// post-pump audit stamp. nil = the stream reached a clean EOF. It is
	// written once from the reader goroutine on the terminal Read and read
	// once by the accounting stage after the pump finishes; the atomic.Pointer
	// supplies the cross-goroutine happens-before (the pipeline drives Read
	// in a separate goroutine — see chunkUsageHolder).
	termErr atomic.Pointer[streamTerminalError]
}

// streamTerminalError records why an SSE stream ended abnormally so the
// audit row can carry a queryable error_code despite the HTTP-200 status
// (the response headers were already flushed before the failure).
type streamTerminalError struct {
	// code is the audit ErrorCode: streamErrCodeUpstream when the upstream
	// stream faulted, streamErrCodeClientAbort on ctx cancel (client
	// disconnect / deadline).
	code string
	err  error
}

const (
	streamErrCodeUpstream    = "UPSTREAM_STREAM_ERROR"
	streamErrCodeClientAbort = "CLIENT_ABORT"
)

// terminalError returns the reader's terminal failure, or nil if the
// stream completed cleanly. Safe to call after the streaming pump has
// returned.
func (r *chunkSSEReader) terminalError() *streamTerminalError {
	return r.termErr.Load()
}

func newChunkSSEReaderFromSubscription(ctx context.Context, sub streamcache.ChunkSubscription, transcoder canonicalbridge.StreamTranscoder, ingressFormat provcore.Format, allowVerbatim bool) *chunkSSEReader {
	return &chunkSSEReader{ctx: ctx, sub: sub, transcoder: transcoder, ingressFormat: ingressFormat, allowVerbatim: allowVerbatim}
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
			r.termErr.Store(&streamTerminalError{code: streamErrCodeClientAbort, err: err})
			return 0, err
		}
		// Provider error (e.g. empty upstream SSE body): synthesise a
		// terminal SSE error frame in the ingress format so the client
		// receives a parseable error payload rather than an abrupt
		// connection close. G4: the envelope must follow the ingress
		// SDK contract (OpenAI vs Anthropic vs Gemini) — see
		// provider-adapter-architecture.md §9.5.
		r.termErr.Store(&streamTerminalError{code: streamErrCodeUpstream, err: err})
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

	// verbatim forwards the upstream frame byte-for-byte, bypassing the
	// transcoder, when the decode session marked the chunk Verbatim and this
	// lane allows it (non-enforced /v1/responses passthrough). It preserves
	// built-in-tool / audio events the canonical waist cannot represent.
	verbatim := r.allowVerbatim && chunk.Verbatim && len(chunk.RawBytes) > 0

	switch {
	case chunk.Done:
		// Verbatim: forward the provider's real terminal frame (response.completed
		// with its full payload). Cross-format: transcoder synthesises the
		// ingress-format terminal events (e.g. Anthropic message_stop, Gemini
		// finishReason frame). Passthrough: forward the provider's raw terminal
		// frame so native ingress clients receive the typed terminator they expect.
		switch {
		case verbatim:
			r.scratch = append(r.scratch[:0], chunk.RawBytes...)
			r.buf = r.scratch
		case r.transcoder != nil:
			b, _ := r.transcoder.Write(r.ctx, chunk)
			if len(b) > 0 {
				r.buf = b
			}
		case len(chunk.RawBytes) > 0:
			r.scratch = append(r.scratch[:0], chunk.RawBytes...)
			r.buf = r.scratch
		}
		r.closed = true
	case verbatim:
		// Genuine Responses frame on the non-enforced passthrough lane: forward
		// the original bytes (built-in-tool / audio events included) instead of
		// re-encoding the decoded canonical fields.
		r.scratch = append(r.scratch[:0], chunk.RawBytes...)
		r.buf = r.scratch
	case r.transcoder != nil:
		// Cross-format: delegate all non-Done chunks to the transcoder so
		// provider-native RawBytes are never forwarded to the client.
		b, err := r.transcoder.Write(r.ctx, chunk)
		if err != nil {
			r.closed = true
			r.err = err
			r.termErr.Store(&streamTerminalError{code: streamErrCodeUpstream, err: err})
			return 0, err
		}
		if len(b) == 0 {
			return 0, nil // transcoder skipped this chunk (e.g. Anthropic ping)
		}
		r.buf = b
	case len(chunk.RawBytes) > 0:
		// Passthrough: stream decoders set RawBytes to a complete SSE frame.
		r.scratch = append(r.scratch[:0], chunk.RawBytes...)
		r.buf = r.scratch
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

// streamIdleWriter extends the connection write deadline on every chunk write
// so a streaming response is governed by an idle (silence) budget rather than
// a flat total cap: an actively-producing stream of any length is never cut,
// while a stalled upstream trips the deadline after `idle` of quiet. The
// caller sets the INITIAL deadline (to cover think-time before the first
// token); each Write then resets it to now+idle. Flush and Unwrap are
// forwarded so the stream-capture tee's flusher and http.NewResponseController
// still reach the underlying writer.
type streamIdleWriter struct {
	http.ResponseWriter
	rc   *http.ResponseController
	idle time.Duration
}

func (w *streamIdleWriter) Write(p []byte) (int, error) {
	if w.idle > 0 {
		_ = w.rc.SetWriteDeadline(time.Now().Add(w.idle))
	}
	return w.ResponseWriter.Write(p)
}

func (w *streamIdleWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *streamIdleWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

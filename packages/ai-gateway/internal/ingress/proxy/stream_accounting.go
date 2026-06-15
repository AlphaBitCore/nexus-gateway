// stream_accounting.go — the accounting stage of the streaming stage
// chain: usage extraction from the chunk timeline, per-endpoint cost
// stamping, terminal-error audit classification, the Prometheus request
// sample, and the async quota reconcile. Runs after the relay stage has
// finished pumping; it only reads relay outputs.
package proxy

import (
	"context"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/estimator"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/quota"
)

// streamAccountingStage settles usage, cost, metrics, and quota.
type streamAccountingStage struct{ s *streamState }

func (st streamAccountingStage) run() bool {
	s := st.s
	h := s.h
	rec := s.rec
	target := s.target
	logger := s.logger
	quotaInPrice := s.quotaInPrice
	quotaOutPrice := s.quotaOutPrice
	quotaDecision := s.quotaDecision
	usageHolder := s.usageHolder
	sseReader := s.sseReader

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
			// reasoning_cost_usd breakdown — consistent with the direct path
			// and cache-HIT paths.
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
			rec.UsageExtractionStatus = "streaming_reported"
		} else {
			// Stream completed but provider emitted no usage frame.
			// Tier-2 tokenizer estimation is not enabled for AI Gateway
			// (the upstream providers we support emit usage at near-100%).
			rec.UsageExtractionStatus = "streaming_unavailable"
		}
	}

	// An SSE stream that faulted mid-flight cannot change its
	// HTTP status (headers were flushed with 200 before the first chunk),
	// so the failure was previously indistinguishable from a clean
	// no-usage stream. Surface the reader's terminal error as a queryable
	// usage_extraction_status + error_code. A failed stream carries no
	// usage so $0 cost stays correct; only the audit classification changes.
	if term := sseReader.terminalError(); term != nil {
		rec.UsageExtractionStatus = "streaming_error"
		rec.ErrorCode = term.code
		if term.err != nil {
			rec.ErrorReason = term.err.Error()
		}
		logger.Warn("sse stream terminated abnormally",
			"errorCode", term.code,
			"provider", target.ProviderName,
			"model", target.ModelCode,
		)
	}

	if h.deps.Metrics != nil {
		h.deps.Metrics.RecordRequest(target.ProviderName, target.ModelID, s.endpointType, rec.StatusCode, time.Since(s.start), usage)
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
	// Charge the single canonical cache-aware cost (rec.EstimatedCostUsd) so the
	// live counter matches the rollup billed_cost_usd and the Backfill seed.
	// Captured before the goroutine to avoid racing rec.
	reconcileCost := rec.EstimatedCostUsd
	if h.deps.QuotaEngine != nil && quotaDecision != nil && quotaDecision.Allowed && rec.StatusCode < 400 && !gatewayServed {
		go func() {
			defer func() {
				if rcv := recover(); rcv != nil {
					h.deps.Logger.Error("quota engine reconcile panic", "panic", rcv)
				}
			}()
			rcCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			h.deps.QuotaEngine.Reconcile(rcCtx, quotaDecision, quota.ActualUsage{CostUSD: reconcileCost})
		}()
	}
	return true
}

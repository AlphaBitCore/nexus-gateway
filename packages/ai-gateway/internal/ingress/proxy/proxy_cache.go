// Package handler — proxy_cache.go hosts the streamcache wiring:
// the broker dispatch, the cache HIT short-circuits, the
// subscription-driven streaming/non-streaming downstream pipelines, and
// the SSE reader that adapts a [streamcache.ChunkSubscription] (or a
// raw [provcore.StreamSession] on the direct path) into the
// LivePipeline's io.Reader contract.
//
// All five Phase 5.5 outcomes (DISABLED, SKIP_NO_CACHE, HIT, MISS,
// HIT_LIVE) flow through the same downstream pipeline so the
// transcoder + LivePipeline + response-stage hook + writer chain runs
// identically on every code path. The only thing that changes per
// outcome is the chunk source.
package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/stream"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/executor"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/quota"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// copyUpstreamHeaders returns a defensive copy of src so the broker
// can persist it for the entry's TTL without sharing memory with the
// short-lived upstream response. Returns nil for an empty/nil input.
func copyUpstreamHeaders(src http.Header) map[string][]string {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string][]string, len(src))
	for k, vs := range src {
		copyVs := make([]string, len(vs))
		copy(copyVs, vs)
		out[k] = copyVs
	}
	return out
}

// runViaBroker is the MISS-path entry point. It builds a leaderFn that
// invokes the live executor, subscribes through the broker registry,
// stamps MISS / HIT_LIVE depending on whether this caller triggered
// the upstream, and forwards into the appropriate stream / non-stream
// downstream pipeline. Concurrent requests with the same cache key
// share one upstream call and one cache write.
//
// `body` is the ingress-format request body (post-request-hook
// rewrite). `preparedBody` is the corresponding Adapter.PrepareBody
// output for routeResult.Targets[0]; when non-nil the executor uses
// it for the primary target's first attempt and skips a redundant
// PrepareBody call. preparedRewrites is the matching rewrites slice
// for the X-Nexus-Coerced header. Pass nil/nil to fall back to
// the regular Execute path (PrepareBody runs once inside Execute).
func (h *Handler) runViaBroker(
	r *http.Request,
	w http.ResponseWriter,
	rec *audit.Record,
	routeResult *routingcore.RouteResult,
	body []byte,
	isStream bool,
	in Ingress,
	reqHookResult *hookcore.CompliancePipelineResult,
	cacheKey string,
	preparedBody []byte,
	preparedRewrites []string,
	quotaInPrice, quotaOutPrice float64,
	quotaDecision *quota.Decision,
	endpointType, requestID string,
	start time.Time,
	logger *slog.Logger,
	canonicalMsgs []normcore.Message,
) {
	// Captured by leaderFn so the caller can read the resolved target
	// + attempt count after Subscribe returns. The broker invokes
	// leaderFn synchronously while holding the registry mutex, so by
	// the time Subscribe returns these values are visible without
	// synchronisation. (HIT_LIVE joiners do not run leaderFn; they
	// re-derive target from routeResult.Targets[0] which is the same
	// primary the leader picked since the cache key folds in
	// provider+model.)
	var (
		resolvedTarget  routingcore.RoutingTarget
		attempts        int
		coerced         []string
		upstreamHeaders http.Header // populated by leaderFn; nil for joiners
	)
	leaderFn := func(_ context.Context) (provcore.StreamSession, *streamcache.CacheMeta, error) {
		// fetchUpstream wires its own error responses on failure; the
		// broker registry treats a returned error as "no broker for
		// this key" and surfaces it to the first subscriber. We must
		// NOT have written to w by the time we return non-nil error,
		// because subsequent joiners may also be waiting; in practice
		// fetchUpstream writes the error to w before returning, which
		// is fine because there are no joiners on a leaderFn-error
		// path (Subscribe returned an error and never published the
		// broker to the registry).
		result, target, n, err := h.fetchUpstreamWithPreparedBody(r, w, rec, routeResult, body, isStream, in, preparedBody, preparedRewrites, start, logger)
		if err != nil {
			return nil, nil, err
		}
		resolvedTarget = target
		attempts = n
		coerced = result.Coerced
		upstreamHeaders = result.Headers
		meta := &streamcache.CacheMeta{
			Provider:        target.ProviderName,
			Model:           target.ProviderModelID,
			IsStream:        isStream,
			UpstreamHeaders: copyUpstreamHeaders(result.Headers),
			OriginWireShape: in.WireShape,
		}
		if isStream {
			return result.Stream, meta, nil
		}
		// Non-streaming: wrap result into a single-chunk session so
		// stream and non-stream paths share one broker abstraction.
		// The broker's writeCache takes the terminal chunk's Delta as
		// the canonical response JSON.
		return newSingleChunkSession(result), meta, nil
	}

	sub, isFirst, err := h.deps.BrokerRegistry.Subscribe(r.Context(), cacheKey, leaderFn)
	if err != nil {
		// Leader path: fetchUpstream already wrote an error response;
		// nothing to do here. Joiner path cannot reach this branch
		// since Subscribe only returns leader errors.
		return
	}

	if isFirst {
		// rec.GatewayCacheStatus is already stamped as Miss in proxy.go
		// before runViaBroker was called; no need to re-stamp. Header
		// reflects unified MISS (x-nexus-cache is the single source of
		// truth).
		w.Header().Set("X-Nexus-Cache", string(audit.CacheStatusMiss))
	} else {
		// hit_inflight: joiner. resolvedTarget was not populated by
		// leaderFn for this caller; fall back to the routed primary.
		resolvedTarget = routeResult.Targets[0]
		attempts = 1
		// Overwrite gateway-side status from miss → hit_inflight.
		// This joiner did not call upstream so provider-cache is "na".
		rec.GatewayCacheStatus = audit.GatewayCacheHitInflight
		rec.GatewayCacheKind = audit.GatewayCacheKindExtract
		rec.ProviderCacheStatus = audit.ProviderCacheNA
		w.Header().Set("X-Nexus-Cache", string(audit.CacheStatusHit))
		h.deps.CacheMetrics.RecordLookup("hit_inflight")
	}
	rec.RoutedProviderID = resolvedTarget.ProviderID
	rec.RoutedProviderName = resolvedTarget.ProviderName
	rec.RoutedModelID = resolvedTarget.ModelID
	rec.RoutedModelName = resolvedTarget.ModelCode
	rec.TargetHost = upstreamHost(resolvedTarget)

	// Forward allowlisted upstream response headers BEFORE the Nexus
	// stamps. Leader has the live upstream headers in upstreamHeaders;
	// joiners replay frames from the broker and do not see the
	// upstream HTTP envelope, so they fall back to Nexus stamps only.
	// Either way, isCacheHit=false because this is a live (or shared-
	// live) call.
	writeForwardedResponseHeaders(w, h.deps.Allowlist, provcore.Format(resolvedTarget.AdapterType), upstreamHeaders, false)

	if isStream {
		h.setResponseHeadersStream(w, rec, resolvedTarget, routeResult, attempts)
		w.Header().Set("X-Nexus-Hook", traffic.FormatHookOutcome(aigwHookOutcomeFromResult(reqHookResult)))
		h.handleStreamWithSubscription(r, w, rec, sub, resolvedTarget, coerced, quotaInPrice, quotaOutPrice, quotaDecision, endpointType, requestID, start, logger)
		return
	}
	h.setResponseHeaders(w, rec, resolvedTarget, routeResult, start, attempts)
	w.Header().Set("X-Nexus-Hook", traffic.FormatHookOutcome(aigwHookOutcomeFromResult(reqHookResult)))
	h.handleNonStreamWithSubscription(r, w, rec, sub, resolvedTarget, coerced, quotaInPrice, quotaOutPrice, quotaDecision, endpointType, requestID, start, logger, routeResult, canonicalMsgs)
}

// directStreamSubscription wraps a [provcore.StreamSession] in the
// [streamcache.ChunkSubscription] contract so the direct (non-broker)
// path can share one downstream pipeline with the cache HIT and broker
// MISS paths. There is no cache write, no fan-out, no replay; this is
// purely a shape adapter.
type directStreamSubscription struct {
	session provcore.StreamSession
	closed  atomic.Bool
}

func newDirectStreamSubscription(session provcore.StreamSession) streamcache.ChunkSubscription {
	return &directStreamSubscription{session: session}
}

func (s *directStreamSubscription) Next(ctx context.Context) (provcore.Chunk, error) {
	if s.closed.Load() {
		return provcore.Chunk{}, io.EOF
	}
	if s.session == nil {
		return provcore.Chunk{}, io.EOF
	}
	return s.session.Next(ctx)
}

func (s *directStreamSubscription) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	if s.session == nil {
		return nil
	}
	return s.session.Close()
}

// singleChunkSession adapts an [executor.ExecutionResult] from a
// non-streaming upstream call into a [provcore.StreamSession] that
// emits exactly one terminal chunk. The chunk's Delta carries the
// canonical (un-reshaped) response JSON, and Done=true marks
// termination. This lets the broker pump treat stream and non-stream
// requests uniformly: the pump observes Done=true and writes a
// ResponseEntry whose CanonicalResponse is the chunk's Delta.
type singleChunkSession struct {
	response *executor.ExecutionResult
	consumed bool
	closed   bool
}

func newSingleChunkSession(res *executor.ExecutionResult) provcore.StreamSession {
	return &singleChunkSession{response: res}
}

func (s *singleChunkSession) Next(_ context.Context) (provcore.Chunk, error) {
	if s.closed || s.consumed {
		return provcore.Chunk{}, io.EOF
	}
	s.consumed = true
	usage := s.response.Usage
	return provcore.Chunk{
		Delta: string(s.response.Body),
		Usage: &usage,
		Done:  true,
	}, nil
}

func (s *singleChunkSession) Close() error {
	s.closed = true
	return nil
}

package proxy

// realtime_metering.go — per-response cost metering, audit rows, and quota
// settlement for the realtime relay.
//
// Row model: ONE traffic_event row per response.done (the protocol's natural
// exchange boundary — context is re-billed per response, so one row carries
// exactly one exchange's usage and cost, which is what a row means to the
// shipped cost aggregators), plus one $0 session row at close (stamped by
// the handler). Response rows enqueue AS THEY HAPPEN so a crash loses at
// most the in-flight response; settled cost is never lost.

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/estimator"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/realtimeproxy"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/quota"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// realtimePricing loads the routed model's pricing snapshot and reports
// whether it is REALTIME-priced: text input, text output, audio input, and
// audio output rates all non-nil AND > 0. A literal-zero row must not slip
// past — a $0-priced row would silently relay real audio spend for free
// under an enforced quota.
func (h *Handler) realtimePricing(ctx context.Context, modelID string) (*store.Model, bool) {
	if h.deps.Models == nil {
		return nil, false
	}
	m, err := h.deps.Models.GetModel(ctx, modelID)
	if err != nil || m == nil {
		return nil, false
	}
	priced := positiveRate(m.InputPricePM) && positiveRate(m.OutputPricePM) &&
		positiveRate(m.AudioInputPricePM) && positiveRate(m.AudioOutputPricePM)
	return m, priced
}

func positiveRate(p *float64) bool { return p != nil && *p > 0 }

// realtimeModelPrices maps the catalog snapshot into the estimator's price
// view. Nil model → all-nil prices (every component $0; reachable only for
// quota-un-enforced VKs — the admission gate refused enforced ones).
func realtimeModelPrices(m *store.Model) metrics.ModelPrices {
	if m == nil {
		return metrics.ModelPrices{}
	}
	return metrics.ModelPrices{
		InputUsdPerM:                m.InputPricePM,
		OutputUsdPerM:               m.OutputPricePM,
		CachedInputReadUsdPerM:      m.CachedInputReadPricePM,
		AudioInputUsdPerM:           m.AudioInputPricePM,
		AudioOutputUsdPerM:          m.AudioOutputPricePM,
		CachedAudioInputReadUsdPerM: m.CachedAudioInputReadPricePM,
	}
}

// realtimeMeterWarned dedupes the two metering WARNs (missing usage on a
// response.done; missing primary audio rate with nonzero audio tokens) to
// one line per model+kind for the process lifetime.
var realtimeMeterWarned sync.Map // key string -> struct{}

func (s *realtimeSession) warnMeterOnce(key, msg string) {
	if s.h.deps.Logger == nil {
		return
	}
	if _, loaded := realtimeMeterWarned.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	s.h.deps.Logger.Warn(msg, "model", s.target.ModelID)
}

// meterResponseDone builds and enqueues one response row from a
// response.done frame, records Prometheus metrics, and settles quota.
// Returns true when the post-settle evaluation crossed a reject-mode cap and
// the session must be severed. Metering fails OPEN (absent usage → deduped
// WARN, no row) — the frame itself was already forwarded.
func (s *realtimeSession) meterResponseDone(frame []byte, now time.Time) (sever bool) {
	latencyMs := s.state.OnResponseDone(now)

	usage, ok := realtimeproxy.ExtractUsage(frame)
	if !ok {
		s.warnMeterOnce("usage:"+s.target.ModelID,
			"realtime response.done carried no usage object; response not metered")
		return false
	}
	if s.priceModel != nil &&
		(!positiveRate(s.priceModel.AudioInputPricePM) && usage.AudioInput+usage.CachedAudioRead > 0 ||
			!positiveRate(s.priceModel.AudioOutputPricePM) && usage.AudioOutput > 0) {
		// Reachable only for quota-un-enforced VKs (admission refuses
		// enforced ones): the missing PRIMARY audio rate prices that
		// component $0 — a spend-visibility gap, never a quota bypass.
		s.warnMeterOnce("audiorate:"+s.target.ModelID,
			"realtime pricing incomplete: audio component priced $0 (missing primary audio rate)")
	}

	units := estimator.BillableUnits{
		PromptTokens:          usage.TextInput,
		CompletionTokens:      usage.TextOutput,
		AudioInputTokens:      usage.AudioInput,
		AudioOutputTokens:     usage.AudioOutput,
		CachedTextReadTokens:  usage.CachedTextRead,
		CachedAudioReadTokens: usage.CachedAudioRead,
	}
	cost := estimator.Lookup(string(typology.EndpointKindRealtime))(units, s.prices)

	rec := s.buildResponseRecord(usage, cost.Total, latencyMs, now)
	if s.h.deps.AuditWriter != nil {
		s.h.deps.AuditWriter.Enqueue(rec)
	}
	if s.h.deps.Metrics != nil {
		s.h.deps.Metrics.RecordRequest(s.target.ProviderName, s.target.ModelID,
			string(typology.EndpointKindRealtime), rec.StatusCode,
			time.Duration(latencyMs)*time.Millisecond, metrics.Usage{
				PromptTokens:     rec.PromptTokens,
				CompletionTokens: rec.CompletionTokens,
				TotalTokens:      rec.TotalTokens,
			})
	}
	return s.settleQuota(cost.Total)
}

// buildResponseRecord assembles one response row. Each row gets its own
// traffic_event id from the audit writer, so RequestID carries the upgrade
// request's real correlation value rather than a synthetic one; grouping
// rides the server-minted TraceID.
func (s *realtimeSession) buildResponseRecord(usage realtimeproxy.Usage, costUsd float64, latencyMs int, now time.Time) *audit.Record {
	rec := &audit.Record{
		// Carried from the session record rather than re-read from a header
		// this function does not have. Every per-exchange row belongs to the
		// same caller as the upgrade that opened the session.
		EndUserID:  s.rec.EndUserID,
		SessionID:  s.rec.SessionID,
		ClientTags: s.rec.ClientTags,
		RequestID:  s.rec.RequestID,
		TraceID:    s.sessionID,
		Timestamp:  now.UTC(),
		Method:     s.rec.Method,
		Path:       s.rec.Path,
		SourceIP:   s.rec.SourceIP,
		// The exchange completed on a live provider stream.
		StatusCode:    200,
		IngressFormat: string(s.in.BodyFormat),
		EndpointType:  string(typology.EndpointKindRealtime),
		// Wall clock from response.created to response.done — a REAL
		// per-exchange latency. UpstreamTotalMs mirrors it because the
		// exchange ran entirely upstream: without the stamp, the shipped
		// "gateway overhead" percentile (latency_ms − upstream_total_ms)
		// would classify whole responses as gateway-induced latency.
		// UpstreamTtfbMs stays nil (TTFB has no meaning mid-session).
		LatencyMs:          latencyMs,
		ComplianceCoverage: "none",
		// Aggregate token columns keep their everywhere-else meaning: total
		// input including cached, total output. The audio/cached splits live
		// in the cost computation, not in new columns.
		PromptTokens:     int64(usage.TotalInput()),
		CompletionTokens: int64(usage.TotalOutput()),
		EstimatedCostUsd: costUsd,

		ProviderID:     s.callTarget.ProviderID,
		ProviderName:   s.callTarget.ProviderName,
		CredentialID:   s.callTarget.CredentialID,
		CredentialName: s.callTarget.CredentialName,

		ModelID:            s.target.ModelID,
		ModelName:          s.target.ModelName,
		RoutedModelID:      s.target.ModelID,
		RoutedModelName:    s.target.ModelName,
		RoutedProviderID:   s.target.ProviderID,
		RoutedProviderName: s.target.ProviderName,

		TargetMethod: s.rec.TargetMethod,
		TargetPath:   s.rec.TargetPath,
		TargetHost:   s.rec.TargetHost,
	}
	rec.TotalTokens = rec.PromptTokens + rec.CompletionTokens
	upstreamMs := latencyMs
	rec.UpstreamTotalMs = &upstreamMs
	rec.ApplyVKMeta(s.vkMeta)
	rec.APIKeyClass = s.vkMeta.Class
	rec.APIKeyFingerprint = s.vkMeta.Fingerprint
	return rec
}

// settleQuota runs the per-response quota sequence in this exact order:
//
//  1. FRESH zero-estimate Check — NOT the upgrade-time decision. Reconcile
//     books into the decision's captured period keys, so a long session
//     crossing an hourly/daily rollover would otherwise credit spend to the
//     closed period; a fresh Check computes current period keys and current
//     counters.
//  2. Reconcile the row cost against that fresh decision (additive; the
//     counters track the session in near-real-time).
//  3. LOCAL post-settle evaluation, no second Redis read: an enforcing level
//     whose pre-settle counter plus this row's cost strictly exceeds its
//     limit severs the session — the sever fires on the response that
//     actually crossed the line, bounding overshoot to ≤ one response. Both
//     reject AND downgrade modes sever here: a running voice session cannot
//     be downgraded to a cheaper model, so a downgrade-mode cost cap has no
//     mid-session remedy other than to stop — treating it as reject is the
//     only enforcement realtime can offer (track-only/notify levels never
//     sever).
//
// A quota-backend failure fails OPEN (the engine's own posture): the session
// continues and metering rows still emit.
func (s *realtimeSession) settleQuota(rowCostUsd float64) bool {
	if s.quota == nil || s.vkMeta == nil {
		return false
	}
	chain := quota.BuildCheckChain(s.vkMeta, s.quota.OrgParents())
	fresh := s.quota.Check(s.ctx, chain, quota.CostEstimate{}, s.vkMeta)
	if fresh == nil {
		return false
	}
	s.quota.Reconcile(s.ctx, fresh, quota.ActualUsage{CostUSD: rowCostUsd})

	rowCents := int64(math.Round(rowCostUsd * 100))
	for _, lvl := range fresh.Levels {
		if lvl.HasLimit && realtimeSevereMode(lvl.EnforcementMode) &&
			lvl.CurrentCents+rowCents > lvl.LimitCents {
			return true
		}
	}
	return false
}

// realtimeSevereMode reports whether an over-limit level in this enforcement
// mode must sever a live realtime session. Reject and downgrade both do
// (downgrade has no live-session remedy); track-only and notify do not.
func realtimeSevereMode(mode string) bool {
	return mode == "reject" || mode == "downgrade"
}

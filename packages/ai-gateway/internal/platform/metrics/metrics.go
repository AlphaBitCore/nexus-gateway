// Package metrics implements opsmetrics-backed metrics recording and
// token/cost extraction for the AI gateway.
//
// The Recorder owns the AI-Gateway-specific business counters and
// histograms. They are registered against a shared *opsmetrics.Registry,
// which both:
//
//  1. registers the underlying Prometheus instruments on
//     prometheus.DefaultRegisterer (so /metrics scrapes keep working), and
//  2. keeps a binding so the per-tick Sampler.Collect() can include them
//     in metrics_sample messages pushed to Hub via thingclient.
//
// Names follow the dotted opsmetrics convention. Only the names that map
// to the spec catalog (§6.3) are catalog-aligned; AG-specific counters
// (proxy-level requests/errors/tokens) keep the same names they had under
// the old promauto namespace, minus the `nexus_ai_gateway_` prefix.
package metrics

import (
	"sync/atomic"
	"time"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/tidwall/gjson"
)

// Usage holds extracted token counts from a provider response.
type Usage struct {
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
}

// Recorder holds opsmetrics instruments for the AI gateway.
type Recorder struct {
	requestsTotal       *opsmetrics.Counter
	requestDurationMs   *opsmetrics.Histogram
	tokensTotal         *opsmetrics.Counter
	errorsTotal         *opsmetrics.Counter
	schemaMismatchTotal *opsmetrics.Counter
	hookRequestTotal    *opsmetrics.Counter
	trafficExtractTotal *opsmetrics.Counter
	// routerRetryTotal counts L2 retries / L3 failovers per target,
	// labelled by the matched provider, the configtypes.ErrorClass that
	// caused the decision, and the outcome bucket (one of:
	// "retried_succeeded", "exhausted", "failover_class_excluded").
	// Wired through Recorder.RecordRouterRetry; the executor calls it via
	// the metricsRecord package var so tests can swap a stub recorder.
	routerRetryTotal *opsmetrics.Counter
	// forwardHeaderDroppedTotal counts inbound or outbound HTTP headers
	// that the forward-header allowlist filtered out. Direction is
	// "request" (client → upstream) or "response" (upstream → client).
	// Header label cardinality is bounded by the closed denylist + an
	// "other" bucket (forwardheader.BucketDroppedHeader). Wired via
	// provdispatch.SetForwardHeaderDropFn at startup.
	forwardHeaderDroppedTotal *opsmetrics.Counter
	// reasoningPassthroughTotal counts nexus.ext.<provider>.<key>
	// reasoning-passthrough events. provider ∈ {anthropic, gemini};
	// action ∈ {injected, skipped_malformed}. "absent" is intentionally
	// not labelled — operators can compute it as total_requests minus
	// the labelled actions. Wired via provdispatch.SetReasoningPassthroughFn
	// at startup.
	reasoningPassthroughTotal *opsmetrics.Counter
	// estimateRequestsTotal counts dry-run dispatches + /v1/estimate
	// per-target invocations; the duration histogram observes estimator
	// latency. estimateCompareRequestsTotal counts top-level /v1/estimate
	// POSTs (1 per request); estimateCompareTargetsTotal counts targets
	// dispatched (N per request) so dashboards can plot fan-out distribution.
	estimateRequestsTotal        *opsmetrics.Counter
	estimateDurationSeconds      *opsmetrics.Histogram
	estimateCompareRequestsTotal *opsmetrics.Counter
	estimateCompareTargetsTotal  *opsmetrics.Counter
	estimateCompareDurationSec   *opsmetrics.Histogram

	// alertTotal and alertFiveXX are snapshot-and-reset counters used by the
	// alerting evaluator's sliding-window checks. They are never read for
	// metric reporting — opsmetrics counters above are the source of truth
	// for dashboards. These two exist only to feed per-tick buckets.
	alertTotal  atomic.Int64
	alertFiveXX atomic.Int64
}

// NewRecorder creates a Recorder and registers its instruments on the
// supplied opsmetrics registry. Re-registration of the same name is
// idempotent on the registry side; in practice this is called once per
// process at startup.
//
// reg must be non-nil. Tests that don't care about metrics can pass a
// freshly constructed registry built from prometheus.NewRegistry() to
// keep the per-test process clean.
func NewRecorder(reg *opsmetrics.Registry) *Recorder {
	return &Recorder{
		requestsTotal:       reg.NewCounter("requests_total", []string{"provider", "model", "endpoint", "status"}),
		requestDurationMs:   reg.NewHistogram("request_duration_ms", []string{"provider", "model", "endpoint"}),
		tokensTotal:         reg.NewCounter("tokens_total", []string{"provider", "model", "direction"}),
		errorsTotal:         reg.NewCounter("errors_total", []string{"provider", "error_type"}),
		schemaMismatchTotal: reg.NewCounter("schema_mismatch_total", []string{"ingress", "provider"}),
		hookRequestTotal:    reg.NewCounter("hook.pipeline_total", []string{"ingress_format", "stage", "decision"}),
		trafficExtractTotal: reg.NewCounter("traffic.extract_total", []string{"ingress_format", "direction", "outcome"}),
		routerRetryTotal:    reg.NewCounter("router.retry_total", []string{"provider", "class", "outcome"}),
		forwardHeaderDroppedTotal: reg.NewCounter(
			"forward_header_dropped_total",
			[]string{"direction", "adapter_type", "header"},
		),
		reasoningPassthroughTotal: reg.NewCounter(
			"reasoning_passthrough_total",
			[]string{"provider", "action"},
		),
		estimateRequestsTotal: reg.NewCounter(
			"estimate_requests_total",
			[]string{"ingress", "resolved_model", "resolved_provider"},
		),
		estimateDurationSeconds: reg.NewHistogram(
			"estimate_duration_seconds",
			[]string{"ingress"},
		),
		estimateCompareRequestsTotal: reg.NewCounter(
			"estimate_compare_requests_total",
			[]string{"ingress"},
		),
		estimateCompareTargetsTotal: reg.NewCounter(
			"estimate_compare_targets_total",
			[]string{"ingress"},
		),
		estimateCompareDurationSec: reg.NewHistogram(
			"estimate_compare_duration_seconds",
			[]string{"ingress"},
		),
	}
}

// RecordEstimate is the per-target estimator telemetry: increments the
// requests counter (one label tuple per target) and observes the
// duration histogram. Safe on nil receiver.
func (r *Recorder) RecordEstimate(ingress, model, provider string, duration time.Duration) {
	if r == nil {
		return
	}
	if ingress == "" {
		ingress = "unknown"
	}
	if model == "" {
		model = "unknown"
	}
	if provider == "" {
		provider = "unknown"
	}
	if r.estimateRequestsTotal != nil {
		r.estimateRequestsTotal.With(ingress, model, provider).Inc()
	}
	if r.estimateDurationSeconds != nil {
		r.estimateDurationSeconds.With(ingress).Observe(duration.Seconds())
	}
}

// RecordEstimateCompare is the top-level /v1/estimate telemetry:
// 1 request, N targets, total wall-clock duration. Safe on nil receiver.
func (r *Recorder) RecordEstimateCompare(ingress string, targetCount int, duration time.Duration) {
	if r == nil {
		return
	}
	if ingress == "" {
		ingress = "unknown"
	}
	if r.estimateCompareRequestsTotal != nil {
		r.estimateCompareRequestsTotal.With(ingress).Inc()
	}
	if r.estimateCompareTargetsTotal != nil {
		// AddBy avoids the per-target lock contention of N Inc calls.
		r.estimateCompareTargetsTotal.With(ingress).Add(float64(targetCount))
	}
	if r.estimateCompareDurationSec != nil {
		r.estimateCompareDurationSec.With(ingress).Observe(duration.Seconds())
	}
}

// RecordReasoningPassthrough increments the
// nexus_aigw_reasoning_passthrough_total counter. provider is the
// adapter slug ("anthropic" / "gemini" today); action is one of
// "injected" or "skipped_malformed". Safe to call on a nil recorder.
func (r *Recorder) RecordReasoningPassthrough(provider, action string) {
	if r == nil || r.reasoningPassthroughTotal == nil {
		return
	}
	if provider == "" {
		provider = "unknown"
	}
	if action == "" {
		action = "unknown"
	}
	r.reasoningPassthroughTotal.With(provider, action).Inc()
}

// RecordForwardHeaderDropped increments the forward-header-dropped
// counter. direction is "request" or "response"; adapterType is the
// provcore.Format slug; header is the bucketed label produced by
// forwardheader.BucketDroppedHeader (so cardinality is bounded).
// Safe to call on a nil recorder.
func (r *Recorder) RecordForwardHeaderDropped(direction, adapterType, header string) {
	if r == nil || r.forwardHeaderDroppedTotal == nil {
		return
	}
	if direction == "" {
		direction = "unknown"
	}
	if adapterType == "" {
		adapterType = "unknown"
	}
	if header == "" {
		header = "other"
	}
	r.forwardHeaderDroppedTotal.With(direction, adapterType, header).Inc()
}

// RecordRouterRetry increments the router retry/failover counter. Outcome
// is one of "retried_succeeded" (an L2 retry on this target eventually
// succeeded), "exhausted" (the L2 budget on this target was used up
// without success), or "failover_class_excluded" (the failure class was
// retryable but the rule's RetryOn excluded it, forcing immediate L3
// failover). class is the configtypes.ErrorClass string ("network",
// "timeout", "429", "5xx"); empty becomes "unknown" so the label always
// has a value. Safe to call on a nil recorder.
func (r *Recorder) RecordRouterRetry(provider, class, outcome string) {
	if r == nil || r.routerRetryTotal == nil {
		return
	}
	if provider == "" {
		provider = "unknown"
	}
	if class == "" {
		class = "unknown"
	}
	if outcome == "" {
		outcome = "unknown"
	}
	r.routerRetryTotal.With(provider, class, outcome).Inc()
}

// RecordSchemaMismatch increments the schema-mismatch counter when a
// routing target is rejected because its provider wire format is not
// compatible with the request's ingress format. Satisfies the
// handler.SchemaMismatchRecorder interface.
func (r *Recorder) RecordSchemaMismatch(ingress, provider string) {
	if r == nil || r.schemaMismatchTotal == nil {
		return
	}
	r.schemaMismatchTotal.With(ingress, provider).Inc()
}

// RecordRequest records metrics for a completed proxy request.
func (r *Recorder) RecordRequest(provider, model, endpoint string, status int, duration time.Duration, usage Usage) {
	statusStr := statusBucket(status)
	r.requestsTotal.With(provider, model, endpoint, statusStr).Inc()
	r.requestDurationMs.With(provider, model, endpoint).Observe(float64(duration.Milliseconds()))
	// Update alerting sliding-window counters.
	r.alertTotal.Add(1)
	if statusStr == "5xx" {
		r.alertFiveXX.Add(1)
	}

	if usage.PromptTokens > 0 {
		r.tokensTotal.With(provider, model, "prompt").Add(float64(usage.PromptTokens))
	}
	if usage.CompletionTokens > 0 {
		r.tokensTotal.With(provider, model, "completion").Add(float64(usage.CompletionTokens))
	}
}

// AlertingSnapshot reads and resets the sliding-window counters used by the
// alerting evaluator. Safe for concurrent callers but intended to be called by
// a single evaluator goroutine on a ticker. Each call returns the counts
// accumulated since the previous call and resets them to zero.
func (r *Recorder) AlertingSnapshot() (total, fiveXX int64) {
	return r.alertTotal.Swap(0), r.alertFiveXX.Swap(0)
}

// RecordError records an error metric.
func (r *Recorder) RecordError(provider, errorType string) {
	r.errorsTotal.With(provider, errorType).Inc()
}

// RecordHookRequest increments the hook-pipeline counter for a given
// ingress wire format, hook stage ("request"/"response"/"stream"), and
// terminal decision ("approve"/"modify"/"block_soft"/"reject_hard"/
// "error"/"skipped"). Safe to call on a nil recorder.
func (r *Recorder) RecordHookRequest(ingressFormat, stage, decision string) {
	if r == nil || r.hookRequestTotal == nil {
		return
	}
	if ingressFormat == "" {
		ingressFormat = "unknown"
	}
	if stage == "" {
		stage = "unknown"
	}
	if decision == "" {
		decision = "unknown"
	}
	r.hookRequestTotal.With(ingressFormat, stage, decision).Inc()
}

// RecordTrafficExtract increments the traffic-adapter extract counter
// for a given ingress format, direction ("request"/"response"/"stream"),
// and outcome ("success"/"error"/"skipped"). Safe to call on a nil
// recorder.
func (r *Recorder) RecordTrafficExtract(ingressFormat, direction, outcome string) {
	if r == nil || r.trafficExtractTotal == nil {
		return
	}
	if ingressFormat == "" {
		ingressFormat = "unknown"
	}
	if direction == "" {
		direction = "unknown"
	}
	if outcome == "" {
		outcome = "unknown"
	}
	r.trafficExtractTotal.With(ingressFormat, direction, outcome).Inc()
}

func statusBucket(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "2xx"
	case status >= 400 && status < 500:
		return "4xx"
	case status >= 500:
		return "5xx"
	default:
		return "other"
	}
}

// ExtractUsage extracts token usage from an OpenAI-format response body.
func ExtractUsage(body []byte) Usage {
	parsed := gjson.ParseBytes(body)
	return Usage{
		PromptTokens:     parsed.Get("usage.prompt_tokens").Int(),
		CompletionTokens: parsed.Get("usage.completion_tokens").Int(),
		TotalTokens:      parsed.Get("usage.total_tokens").Int(),
	}
}

// ModelPrices is the per-(Provider, Model) pricing snapshot consumed by
// CalculateCost. All fields are per million tokens; pointer types distinguish
// "not configured" (nil) from "configured zero" (rare but legal for
// free-tier upstreams).
//
// When a cache price is nil, CalculateCost falls back to the standard
// input price for that bucket — preserves the "no discount" semantics
// for providers that have no cache feature wired.
type ModelPrices struct {
	InputUsdPerM            *float64
	OutputUsdPerM           *float64
	CachedInputReadUsdPerM  *float64
	CachedInputWriteUsdPerM *float64
}

// Cost is the four-component cost breakdown. Fields are 0 when the
// relevant token count is 0/nil. Total is the sum. Reasoning cost is
// derived (ReasoningTokens × OutputUsdPerM) but already included in
// Output — surfaced as the bottom field for analytics.
type Cost struct {
	UncachedInput  float64 `json:"uncachedInput"`
	CacheRead      float64 `json:"cacheRead"`
	CacheWrite     float64 `json:"cacheWrite"`
	Output         float64 `json:"output"`
	Total          float64 `json:"total"`
	ReasoningSplit float64 `json:"reasoningSplit,omitempty"` // subset of Output attributable to reasoning_tokens (advisory).
}

// CalculateCost computes the four-component cost from usage + price data.
//
// Token bucket semantics:
//   - PromptTokens is the TOTAL input including cached subset (OpenAI
//     convention; Anthropic-shape input_tokens is normalized to this in
//     shared/normalize/anthropic_messages.go).
//   - CachedTokens (read-side) and CacheCreationTokens (write-side) are
//     subsets of PromptTokens.
//   - CompletionTokens INCLUDES reasoning tokens (all three frontier
//     providers bill reasoning at the output rate).
//
// UncachedInput = PromptTokens − CachedTokens − CacheCreationTokens
// (clamped to ≥ 0 for defense against inconsistent upstream reports).
//
// Cache price fallback chain: nil cache prices fall back to inputPrice
// → meaning "no discount available for this provider".
func CalculateCost(p provcore.Usage, prices ModelPrices) Cost {
	inP := derefPrice(prices.InputUsdPerM, 0)
	outP := derefPrice(prices.OutputUsdPerM, 0)
	cReadP := derefPrice(prices.CachedInputReadUsdPerM, inP)
	cWriteP := derefPrice(prices.CachedInputWriteUsdPerM, inP)

	prompt := derefInt(p.PromptTokens, 0)
	cacheRead := derefInt(p.CacheReadTokens, 0)
	cacheWrite := derefInt(p.CacheCreationTokens, 0)
	completion := derefInt(p.CompletionTokens, 0)
	reasoning := derefInt(p.ReasoningTokens, 0)

	uncached := prompt - cacheRead - cacheWrite
	if uncached < 0 {
		uncached = 0
	}

	const million = 1_000_000.0
	out := Cost{
		UncachedInput:  float64(uncached) * inP / million,
		CacheRead:      float64(cacheRead) * cReadP / million,
		CacheWrite:     float64(cacheWrite) * cWriteP / million,
		Output:         float64(completion) * outP / million,
		ReasoningSplit: float64(reasoning) * outP / million,
	}
	out.Total = out.UncachedInput + out.CacheRead + out.CacheWrite + out.Output
	return out
}

func derefPrice(p *float64, fallback float64) float64 {
	if p == nil {
		return fallback
	}
	return *p
}

func derefInt(p *int, fallback int) int {
	if p == nil {
		return fallback
	}
	return *p
}

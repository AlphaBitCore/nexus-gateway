package metrics

import (
	"strings"
	"testing"
	"time"

	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/prometheus/client_golang/prometheus"
)

// findCounter scans Registry.Collect() for the (metricName, dimensionKey) pair
// and returns its observed value. Returns -1 if the (name, dim) tuple has
// never been incremented (Prometheus does not emit zero series before first
// observation). Asserting via Collect() pins the externally-observable
// surface — same shape Hub consumes via metrics_sample.
func findCounter(t *testing.T, reg *opsmetrics.Registry, name, dim string) float64 {
	t.Helper()
	for _, s := range reg.Collect() {
		if s.Name == name && s.Kind == opsmetrics.KindCounter && s.DimensionKey == dim {
			return s.Value
		}
	}
	return -1
}

// findHistogramBuckets returns the 6-bucket array for a (name, dim) pair from
// Registry.Collect(); returns nil if absent.
func findHistogramBuckets(t *testing.T, reg *opsmetrics.Registry, name, dim string) []int {
	t.Helper()
	for _, s := range reg.Collect() {
		if s.Name == name && s.Kind == opsmetrics.KindHistogram && s.DimensionKey == dim {
			if buckets, ok := s.Metadata["buckets"].([]int); ok {
				return buckets
			}
		}
	}
	return nil
}

// histogramTotal sums the 6-bucket array — the observation count for this dim.
func histogramTotal(buckets []int) int {
	total := 0
	for _, b := range buckets {
		total += b
	}
	return total
}

// newRecorderAndReg returns a fresh (Recorder, Registry) pair so each test
// observes its own counter values without bleed-through from other tests.
func newRecorderAndReg() (*Recorder, *opsmetrics.Registry) {
	reg := opsmetrics.NewRegistry(prometheus.NewRegistry())
	return NewRecorder(reg), reg
}


// TestRecordEstimate_HappyPath_IncrementsAndObserves pins the dual emission:
// requests counter +1 for the (ingress, model, provider) tuple, AND the
// duration histogram receives the observation in the correct bucket.
func TestRecordEstimate_HappyPath_IncrementsAndObserves(t *testing.T) {
	r, reg := newRecorderAndReg()

	r.RecordEstimate("chat", "gpt-4o", "openai", 73*time.Millisecond)

	dim := "ingress=chat;model=gpt-4o;resolved_provider=openai;resolved_model=gpt-4o"
	// Note: opsmetrics joinDimension sorts label=value pairs alphabetically.
	// estimate_requests_total has labels [ingress, resolved_model, resolved_provider].
	want := "ingress=chat;resolved_model=gpt-4o;resolved_provider=openai"
	if got := findCounter(t, reg, "estimate_requests_total", want); got != 1 {
		t.Fatalf("estimate_requests_total[%s]: want 1, got %v (dim probe: %s)", want, got, dim)
	}

	bs := findHistogramBuckets(t, reg, "estimate_duration_seconds", "ingress=chat")
	if bs == nil {
		t.Fatalf("estimate_duration_seconds histogram missing")
	}
	if histogramTotal(bs) != 1 {
		t.Errorf("estimate_duration_seconds total: want 1, got %d (buckets=%v)", histogramTotal(bs), bs)
	}
}

// TestRecordEstimate_EmptyLabelsBecomeUnknown verifies that empty ingress /
// model / provider are coerced to "unknown" so the Prometheus label set is
// never broken (Prom rejects empty label values inconsistently across vec
// instantiations — coercion makes the behavior deterministic).
func TestRecordEstimate_EmptyLabelsBecomeUnknown(t *testing.T) {
	r, reg := newRecorderAndReg()

	r.RecordEstimate("", "", "", 10*time.Millisecond)

	wantDim := "ingress=unknown;resolved_model=unknown;resolved_provider=unknown"
	if got := findCounter(t, reg, "estimate_requests_total", wantDim); got != 1 {
		t.Errorf("estimate_requests_total[unknown,unknown,unknown]: want 1, got %v", got)
	}
}

// TestRecordEstimate_NilReceiver_NoPanic verifies the nil-recorder guard:
// the executor may call this through a package-level recorder var that the
// process never wired (test binary, dry-run).
func TestRecordEstimate_NilReceiver_NoPanic(t *testing.T) {
	var r *Recorder
	r.RecordEstimate("chat", "m", "p", time.Second) // must not panic
}


// TestRecordEstimateCompare_HappyPath_IncrementsAllThree pins:
// (1) compare requests +1, (2) targets +N (AddBy, not N Inc), (3) duration
// observed exactly once.
func TestRecordEstimateCompare_HappyPath_IncrementsAllThree(t *testing.T) {
	r, reg := newRecorderAndReg()

	r.RecordEstimateCompare("responses", 5, 250*time.Millisecond)

	if got := findCounter(t, reg, "estimate_compare_requests_total", "ingress=responses"); got != 1 {
		t.Errorf("estimate_compare_requests_total: want 1, got %v", got)
	}
	if got := findCounter(t, reg, "estimate_compare_targets_total", "ingress=responses"); got != 5 {
		t.Errorf("estimate_compare_targets_total: want 5 (Add not Inc), got %v", got)
	}
	bs := findHistogramBuckets(t, reg, "estimate_compare_duration_seconds", "ingress=responses")
	if histogramTotal(bs) != 1 {
		t.Errorf("compare duration: want 1 observation, got %d (buckets=%v)", histogramTotal(bs), bs)
	}
}

// TestRecordEstimateCompare_EmptyIngressUnknown verifies the "" → "unknown"
// coercion fires.
func TestRecordEstimateCompare_EmptyIngressUnknown(t *testing.T) {
	r, reg := newRecorderAndReg()
	r.RecordEstimateCompare("", 2, time.Millisecond)
	if got := findCounter(t, reg, "estimate_compare_requests_total", "ingress=unknown"); got != 1 {
		t.Errorf("compare requests[unknown]: want 1, got %v", got)
	}
	if got := findCounter(t, reg, "estimate_compare_targets_total", "ingress=unknown"); got != 2 {
		t.Errorf("compare targets[unknown]: want 2, got %v", got)
	}
}

// TestRecordEstimateCompare_ZeroTargets_StillObservesDuration verifies the
// /v1/estimate "no valid targets" arm: requests counter +1, targets counter
// +0 (no series emitted until a non-zero Add), duration still observed.
func TestRecordEstimateCompare_ZeroTargets_StillObservesDuration(t *testing.T) {
	r, reg := newRecorderAndReg()
	r.RecordEstimateCompare("chat", 0, 5*time.Millisecond)

	if got := findCounter(t, reg, "estimate_compare_requests_total", "ingress=chat"); got != 1 {
		t.Errorf("compare requests: want 1, got %v", got)
	}
	// Add(0) on a brand-new label tuple still instantiates the series in the
	// Prometheus CounterVec, so it shows up as 0 in Collect().
	if got := findCounter(t, reg, "estimate_compare_targets_total", "ingress=chat"); got != 0 {
		t.Errorf("compare targets: want 0, got %v", got)
	}
	bs := findHistogramBuckets(t, reg, "estimate_compare_duration_seconds", "ingress=chat")
	if histogramTotal(bs) != 1 {
		t.Errorf("compare duration: want 1 observation, got %d (buckets=%v)", histogramTotal(bs), bs)
	}
}

// TestRecordEstimateCompare_NilReceiver_NoPanic verifies the nil guard.
func TestRecordEstimateCompare_NilReceiver_NoPanic(t *testing.T) {
	var r *Recorder
	r.RecordEstimateCompare("chat", 3, time.Second) // must not panic
}


// TestRecordReasoningPassthrough_BothActions verifies the per-(provider,
// action) counter increments for both observed actions ("injected" and
// "skipped_malformed"), each with its own series.
func TestRecordReasoningPassthrough_BothActions(t *testing.T) {
	r, reg := newRecorderAndReg()

	r.RecordReasoningPassthrough("anthropic", "injected")
	r.RecordReasoningPassthrough("anthropic", "injected")
	r.RecordReasoningPassthrough("gemini", "skipped_malformed")

	if got := findCounter(t, reg, "reasoning_passthrough_total", "action=injected;provider=anthropic"); got != 2 {
		t.Errorf("injected[anthropic]: want 2, got %v", got)
	}
	if got := findCounter(t, reg, "reasoning_passthrough_total", "action=skipped_malformed;provider=gemini"); got != 1 {
		t.Errorf("skipped_malformed[gemini]: want 1, got %v", got)
	}
}

// TestRecordReasoningPassthrough_EmptyLabelsBecomeUnknown verifies the empty-
// string → "unknown" coercion fires for both labels.
func TestRecordReasoningPassthrough_EmptyLabelsBecomeUnknown(t *testing.T) {
	r, reg := newRecorderAndReg()

	r.RecordReasoningPassthrough("", "")

	if got := findCounter(t, reg, "reasoning_passthrough_total", "action=unknown;provider=unknown"); got != 1 {
		t.Errorf("unknown/unknown: want 1, got %v", got)
	}
}

// TestRecordReasoningPassthrough_NilReceiver_NoPanic.
func TestRecordReasoningPassthrough_NilReceiver_NoPanic(t *testing.T) {
	var r *Recorder
	r.RecordReasoningPassthrough("anthropic", "injected") // must not panic
}

// TestRecordReasoningPassthrough_NilCounter_NoPanic verifies that a recorder
// with a nil reasoningPassthroughTotal (defensive — should not occur in
// production but the code defends against it) does not panic.
func TestRecordReasoningPassthrough_NilCounter_NoPanic(t *testing.T) {
	r := &Recorder{} // all instrument fields nil
	r.RecordReasoningPassthrough("anthropic", "injected")
}


// TestRecordForwardHeaderDropped_HappyPath verifies the 3-label tuple
// (direction, adapter_type, header) increments and isolates per series.
func TestRecordForwardHeaderDropped_HappyPath(t *testing.T) {
	r, reg := newRecorderAndReg()

	r.RecordForwardHeaderDropped("request", "openai", "authorization")
	r.RecordForwardHeaderDropped("request", "openai", "authorization")
	r.RecordForwardHeaderDropped("response", "anthropic", "set-cookie")

	if got := findCounter(t, reg, "forward_header_dropped_total",
		"adapter_type=openai;direction=request;header=authorization"); got != 2 {
		t.Errorf("request/openai/authorization: want 2, got %v", got)
	}
	if got := findCounter(t, reg, "forward_header_dropped_total",
		"adapter_type=anthropic;direction=response;header=set-cookie"); got != 1 {
		t.Errorf("response/anthropic/set-cookie: want 1, got %v", got)
	}
}

// TestRecordForwardHeaderDropped_EmptyHeaderBecomesOther pins the
// header == "" → "other" coercion (not "unknown" like the other two
// — header has its own bucketed fallback).
func TestRecordForwardHeaderDropped_EmptyHeaderBecomesOther(t *testing.T) {
	r, reg := newRecorderAndReg()

	r.RecordForwardHeaderDropped("", "", "")

	wantDim := "adapter_type=unknown;direction=unknown;header=other"
	if got := findCounter(t, reg, "forward_header_dropped_total", wantDim); got != 1 {
		t.Errorf("forward_header_dropped[unknown,unknown,other]: want 1, got %v", got)
	}
}

// TestRecordForwardHeaderDropped_NilReceiver_NoPanic.
func TestRecordForwardHeaderDropped_NilReceiver_NoPanic(t *testing.T) {
	var r *Recorder
	r.RecordForwardHeaderDropped("request", "openai", "auth")
}

// TestRecordForwardHeaderDropped_NilCounter_NoPanic.
func TestRecordForwardHeaderDropped_NilCounter_NoPanic(t *testing.T) {
	r := &Recorder{}
	r.RecordForwardHeaderDropped("request", "openai", "auth")
}


// TestRecordRouterRetry_AllThreeOutcomes verifies each documented outcome
// bucket produces its own series.
func TestRecordRouterRetry_AllThreeOutcomes(t *testing.T) {
	r, reg := newRecorderAndReg()

	r.RecordRouterRetry("openai", "5xx", "retried_succeeded")
	r.RecordRouterRetry("openai", "timeout", "exhausted")
	r.RecordRouterRetry("anthropic", "429", "failover_class_excluded")

	if got := findCounter(t, reg, "router.retry_total",
		"class=5xx;outcome=retried_succeeded;provider=openai"); got != 1 {
		t.Errorf("retried_succeeded: want 1, got %v", got)
	}
	if got := findCounter(t, reg, "router.retry_total",
		"class=timeout;outcome=exhausted;provider=openai"); got != 1 {
		t.Errorf("exhausted: want 1, got %v", got)
	}
	if got := findCounter(t, reg, "router.retry_total",
		"class=429;outcome=failover_class_excluded;provider=anthropic"); got != 1 {
		t.Errorf("failover_class_excluded: want 1, got %v", got)
	}
}

// TestRecordRouterRetry_EmptyLabelsBecomeUnknown pins the triple-empty
// coercion (caller passed unresolved provider / unclassified error).
func TestRecordRouterRetry_EmptyLabelsBecomeUnknown(t *testing.T) {
	r, reg := newRecorderAndReg()

	r.RecordRouterRetry("", "", "")

	wantDim := "class=unknown;outcome=unknown;provider=unknown"
	if got := findCounter(t, reg, "router.retry_total", wantDim); got != 1 {
		t.Errorf("router.retry_total[unknown,unknown,unknown]: want 1, got %v", got)
	}
}

// TestRecordRouterRetry_NilReceiver_NoPanic.
func TestRecordRouterRetry_NilReceiver_NoPanic(t *testing.T) {
	var r *Recorder
	r.RecordRouterRetry("openai", "5xx", "retried_succeeded")
}

// TestRecordRouterRetry_NilCounter_NoPanic.
func TestRecordRouterRetry_NilCounter_NoPanic(t *testing.T) {
	r := &Recorder{}
	r.RecordRouterRetry("openai", "5xx", "retried_succeeded")
}


// TestRecordSchemaMismatch_HappyPath verifies the (ingress, provider) tuple
// increments and that the satisfied handler.SchemaMismatchRecorder interface
// stays observable.
func TestRecordSchemaMismatch_HappyPath(t *testing.T) {
	r, reg := newRecorderAndReg()

	r.RecordSchemaMismatch("chat", "anthropic")
	r.RecordSchemaMismatch("chat", "anthropic")
	r.RecordSchemaMismatch("responses", "anthropic")

	if got := findCounter(t, reg, "schema_mismatch_total", "ingress=chat;provider=anthropic"); got != 2 {
		t.Errorf("chat/anthropic: want 2, got %v", got)
	}
	if got := findCounter(t, reg, "schema_mismatch_total", "ingress=responses;provider=anthropic"); got != 1 {
		t.Errorf("responses/anthropic: want 1, got %v", got)
	}
}

// TestRecordSchemaMismatch_NilReceiver_NoPanic.
func TestRecordSchemaMismatch_NilReceiver_NoPanic(t *testing.T) {
	var r *Recorder
	r.RecordSchemaMismatch("chat", "anthropic")
}

// TestRecordSchemaMismatch_NilCounter_NoPanic.
func TestRecordSchemaMismatch_NilCounter_NoPanic(t *testing.T) {
	r := &Recorder{}
	r.RecordSchemaMismatch("chat", "anthropic")
}


// TestRecordError_HappyPath verifies the (provider, error_type) tuple
// increments and the snake_case prom name maps correctly.
func TestRecordError_HappyPath(t *testing.T) {
	r, reg := newRecorderAndReg()

	r.RecordError("openai", "upstream_5xx")
	r.RecordError("openai", "upstream_5xx")
	r.RecordError("anthropic", "timeout")

	if got := findCounter(t, reg, "errors_total", "error_type=upstream_5xx;provider=openai"); got != 2 {
		t.Errorf("openai/upstream_5xx: want 2, got %v", got)
	}
	if got := findCounter(t, reg, "errors_total", "error_type=timeout;provider=anthropic"); got != 1 {
		t.Errorf("anthropic/timeout: want 1, got %v", got)
	}
}


// TestRecordHookRequest_AllDecisions verifies every documented decision
// (approve/modify/block_soft/reject_hard/error/skipped) produces a distinct
// series and the (ingress_format, stage, decision) labels stay stable.
func TestRecordHookRequest_AllDecisions(t *testing.T) {
	r, reg := newRecorderAndReg()

	decisions := []string{"approve", "modify", "block_soft", "reject_hard", "error", "skipped"}
	for _, d := range decisions {
		r.RecordHookRequest("chat", "request", d)
	}

	for _, d := range decisions {
		wantDim := "decision=" + d + ";ingress_format=chat;stage=request"
		if got := findCounter(t, reg, "hook.pipeline_total", wantDim); got != 1 {
			t.Errorf("hook.pipeline_total[%s]: want 1, got %v", d, got)
		}
	}
}

// TestRecordHookRequest_EmptyLabelsBecomeUnknown pins all three "" → "unknown"
// coercions.
func TestRecordHookRequest_EmptyLabelsBecomeUnknown(t *testing.T) {
	r, reg := newRecorderAndReg()

	r.RecordHookRequest("", "", "")

	wantDim := "decision=unknown;ingress_format=unknown;stage=unknown"
	if got := findCounter(t, reg, "hook.pipeline_total", wantDim); got != 1 {
		t.Errorf("hook.pipeline_total[unknown,unknown,unknown]: want 1, got %v", got)
	}
}

// TestRecordHookRequest_NilReceiver_NoPanic.
func TestRecordHookRequest_NilReceiver_NoPanic(t *testing.T) {
	var r *Recorder
	r.RecordHookRequest("chat", "request", "approve")
}

// TestRecordHookRequest_NilCounter_NoPanic.
func TestRecordHookRequest_NilCounter_NoPanic(t *testing.T) {
	r := &Recorder{}
	r.RecordHookRequest("chat", "request", "approve")
}


// TestRecordTrafficExtract_AllOutcomes verifies the three direction × outcome
// shapes (success/error/skipped) increment per-tuple correctly.
func TestRecordTrafficExtract_AllOutcomes(t *testing.T) {
	r, reg := newRecorderAndReg()

	r.RecordTrafficExtract("chat", "request", "success")
	r.RecordTrafficExtract("chat", "request", "success")
	r.RecordTrafficExtract("chat", "response", "error")
	r.RecordTrafficExtract("chat", "stream", "skipped")

	if got := findCounter(t, reg, "traffic.extract_total",
		"direction=request;ingress_format=chat;outcome=success"); got != 2 {
		t.Errorf("request/success: want 2, got %v", got)
	}
	if got := findCounter(t, reg, "traffic.extract_total",
		"direction=response;ingress_format=chat;outcome=error"); got != 1 {
		t.Errorf("response/error: want 1, got %v", got)
	}
	if got := findCounter(t, reg, "traffic.extract_total",
		"direction=stream;ingress_format=chat;outcome=skipped"); got != 1 {
		t.Errorf("stream/skipped: want 1, got %v", got)
	}
}

// TestRecordTrafficExtract_EmptyLabelsBecomeUnknown pins the triple-empty
// coercion path.
func TestRecordTrafficExtract_EmptyLabelsBecomeUnknown(t *testing.T) {
	r, reg := newRecorderAndReg()
	r.RecordTrafficExtract("", "", "")
	wantDim := "direction=unknown;ingress_format=unknown;outcome=unknown"
	if got := findCounter(t, reg, "traffic.extract_total", wantDim); got != 1 {
		t.Errorf("traffic.extract_total[unknown,unknown,unknown]: want 1, got %v", got)
	}
}

// TestRecordTrafficExtract_NilReceiver_NoPanic.
func TestRecordTrafficExtract_NilReceiver_NoPanic(t *testing.T) {
	var r *Recorder
	r.RecordTrafficExtract("chat", "request", "success")
}

// TestRecordTrafficExtract_NilCounter_NoPanic.
func TestRecordTrafficExtract_NilCounter_NoPanic(t *testing.T) {
	r := &Recorder{}
	r.RecordTrafficExtract("chat", "request", "success")
}

// RecordRequest gap branches

// TestRecordRequest_PromptAndCompletionTokensIncrement pins the token-bump
// arms of RecordRequest that the existing alerting tests skip: PromptTokens >
// 0 and CompletionTokens > 0 each increment tokens_total under their own
// "direction" label.
func TestRecordRequest_PromptAndCompletionTokensIncrement(t *testing.T) {
	r, reg := newRecorderAndReg()

	r.RecordRequest("openai", "gpt-4o", "/v1/chat", 200, 50*time.Millisecond, Usage{
		PromptTokens:     100,
		CompletionTokens: 25,
		TotalTokens:      125,
	})

	if got := findCounter(t, reg, "tokens_total",
		"direction=prompt;model=gpt-4o;provider=openai"); got != 100 {
		t.Errorf("prompt tokens: want 100, got %v", got)
	}
	if got := findCounter(t, reg, "tokens_total",
		"direction=completion;model=gpt-4o;provider=openai"); got != 25 {
		t.Errorf("completion tokens: want 25, got %v", got)
	}
	// Status counter and duration histogram stay observable.
	if got := findCounter(t, reg, "requests_total",
		"endpoint=/v1/chat;model=gpt-4o;provider=openai;status=2xx"); got != 1 {
		t.Errorf("requests_total[2xx]: want 1, got %v", got)
	}
	bs := findHistogramBuckets(t, reg, "request_duration_ms",
		"endpoint=/v1/chat;model=gpt-4o;provider=openai")
	if histogramTotal(bs) != 1 {
		t.Errorf("request_duration_ms total: want 1, got %d (buckets=%v)", histogramTotal(bs), bs)
	}
}

// TestRecordRequest_ZeroTokensSkipBump verifies the inverse guard: zero or
// negative-or-zero token counts in Usage do NOT increment tokens_total so
// the "direction" series stays absent.
func TestRecordRequest_ZeroTokensSkipBump(t *testing.T) {
	r, reg := newRecorderAndReg()

	r.RecordRequest("openai", "gpt-4o", "/v1/chat", 200, time.Millisecond, Usage{
		PromptTokens:     0,
		CompletionTokens: 0,
	})

	// findCounter returns -1 when series absent — that's the desired state.
	if got := findCounter(t, reg, "tokens_total",
		"direction=prompt;model=gpt-4o;provider=openai"); got != -1 {
		t.Errorf("prompt tokens: want absent (-1), got %v", got)
	}
	if got := findCounter(t, reg, "tokens_total",
		"direction=completion;model=gpt-4o;provider=openai"); got != -1 {
		t.Errorf("completion tokens: want absent (-1), got %v", got)
	}
}

// TestRecordRequest_OnlyCompletionTokensBump exercises the asymmetric arm
// where the upstream reported completion but the request body never carried
// a prompt (synthetic test, dry-run estimate, …). Hits the
// "CompletionTokens > 0 but PromptTokens == 0" path that the combined-token
// test would mask.
func TestRecordRequest_OnlyCompletionTokensBump(t *testing.T) {
	r, reg := newRecorderAndReg()

	r.RecordRequest("openai", "gpt-4o", "/v1/chat", 200, time.Millisecond, Usage{
		CompletionTokens: 7,
	})

	if got := findCounter(t, reg, "tokens_total",
		"direction=prompt;model=gpt-4o;provider=openai"); got != -1 {
		t.Errorf("prompt tokens: want absent (-1), got %v", got)
	}
	if got := findCounter(t, reg, "tokens_total",
		"direction=completion;model=gpt-4o;provider=openai"); got != 7 {
		t.Errorf("completion tokens: want 7, got %v", got)
	}
}


// TestStatusBucket_AllBuckets pins every branch of the status → bucket map,
// including the `default` arm (status < 200 or in [300, 400)).
func TestStatusBucket_AllBuckets(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{0, "other"},   // default arm — never returned by net/http but defensive.
		{100, "other"}, // 1xx informational — uncommon, falls through default.
		{199, "other"}, // boundary below 2xx.
		{200, "2xx"},
		{201, "2xx"},
		{299, "2xx"},
		{300, "other"}, // 3xx redirect — falls through default per design.
		{399, "other"}, // boundary above 3xx.
		{400, "4xx"},
		{404, "4xx"},
		{499, "4xx"},
		{500, "5xx"},
		{503, "5xx"},
		{599, "5xx"},
		{600, "5xx"}, // > 599 unreachable in HTTP but still bucketed.
	}
	for _, c := range cases {
		if got := statusBucket(c.status); got != c.want {
			t.Errorf("statusBucket(%d): want %q, got %q", c.status, c.want, got)
		}
	}
}

// TestRecordRequest_3xxStatusFallsToOther exercises the statusBucket default
// arm end-to-end through RecordRequest: a 302 redirect must record under
// status="other", and must NOT bump alertFiveXX.
func TestRecordRequest_3xxStatusFallsToOther(t *testing.T) {
	r, reg := newRecorderAndReg()

	r.RecordRequest("openai", "gpt-4o", "/v1/chat", 302, time.Millisecond, Usage{})

	if got := findCounter(t, reg, "requests_total",
		"endpoint=/v1/chat;model=gpt-4o;provider=openai;status=other"); got != 1 {
		t.Errorf("requests_total[other]: want 1, got %v", got)
	}
	_, fiveXX := r.AlertingSnapshot()
	if fiveXX != 0 {
		t.Errorf("3xx must not bump 5xx alert counter, got %d", fiveXX)
	}
}


// TestExtractUsage_OpenAIShape verifies the canonical OpenAI usage block is
// extracted into the three int fields.
func TestExtractUsage_OpenAIShape(t *testing.T) {
	body := []byte(`{"id":"x","usage":{"prompt_tokens":120,"completion_tokens":35,"total_tokens":155}}`)
	got := ExtractUsage(body)
	if got.PromptTokens != 120 || got.CompletionTokens != 35 || got.TotalTokens != 155 {
		t.Errorf("ExtractUsage: got %+v", got)
	}
}

// TestExtractUsage_MissingUsage verifies the missing-field arm: gjson returns
// 0 for absent paths so Usage is all zero (not NaN, not error).
func TestExtractUsage_MissingUsage(t *testing.T) {
	body := []byte(`{"id":"x","choices":[]}`)
	got := ExtractUsage(body)
	if got != (Usage{}) {
		t.Errorf("ExtractUsage(no usage block): want zero Usage, got %+v", got)
	}
}

// TestExtractUsage_PartialUsage verifies only the present fields populate;
// gjson silently returns 0 for the missing ones.
func TestExtractUsage_PartialUsage(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":50}}`)
	got := ExtractUsage(body)
	if got.PromptTokens != 50 {
		t.Errorf("PromptTokens: want 50, got %d", got.PromptTokens)
	}
	if got.CompletionTokens != 0 || got.TotalTokens != 0 {
		t.Errorf("absent fields should be 0, got completion=%d total=%d",
			got.CompletionTokens, got.TotalTokens)
	}
}

// TestExtractUsage_GarbageBody verifies the malformed-JSON arm: gjson does
// not return an error, it returns a zero parse — so Usage is zero.
func TestExtractUsage_GarbageBody(t *testing.T) {
	got := ExtractUsage([]byte("not json"))
	if got != (Usage{}) {
		t.Errorf("ExtractUsage(garbage): want zero Usage, got %+v", got)
	}
}

// TestExtractUsage_EmptyBody — defensive check for the nil/empty body slice.
func TestExtractUsage_EmptyBody(t *testing.T) {
	got := ExtractUsage(nil)
	if got != (Usage{}) {
		t.Errorf("ExtractUsage(nil): want zero, got %+v", got)
	}
	got = ExtractUsage([]byte(""))
	if got != (Usage{}) {
		t.Errorf("ExtractUsage(empty): want zero, got %+v", got)
	}
}

// NewRecorder smoke

// TestNewRecorder_AllInstrumentsRegistered verifies that NewRecorder wires
// every documented instrument so subsequent Record* calls cannot encounter
// a nil opsmetrics handle.
func TestNewRecorder_AllInstrumentsRegistered(t *testing.T) {
	r, _ := newRecorderAndReg()
	if r == nil {
		t.Fatal("NewRecorder returned nil")
	}
	// Spot-check each field — a nil here means the constructor skipped it
	// and any subsequent caller would hit the nil-guard branch in prod.
	checks := []struct {
		name string
		got  any
	}{
		{"requestsTotal", r.requestsTotal},
		{"requestDurationMs", r.requestDurationMs},
		{"tokensTotal", r.tokensTotal},
		{"errorsTotal", r.errorsTotal},
		{"schemaMismatchTotal", r.schemaMismatchTotal},
		{"hookRequestTotal", r.hookRequestTotal},
		{"trafficExtractTotal", r.trafficExtractTotal},
		{"routerRetryTotal", r.routerRetryTotal},
		{"forwardHeaderDroppedTotal", r.forwardHeaderDroppedTotal},
		{"reasoningPassthroughTotal", r.reasoningPassthroughTotal},
		{"estimateRequestsTotal", r.estimateRequestsTotal},
		{"estimateDurationSeconds", r.estimateDurationSeconds},
		{"estimateCompareRequestsTotal", r.estimateCompareRequestsTotal},
		{"estimateCompareTargetsTotal", r.estimateCompareTargetsTotal},
		{"estimateCompareDurationSec", r.estimateCompareDurationSec},
	}
	for _, c := range checks {
		// Type-switch-free nil check via reflection-friendly equality on the
		// interface holder: a nil pointer in an interface compares != nil
		// against literal nil, so test the typed value instead.
		switch v := c.got.(type) {
		case *opsmetrics.Counter:
			if v == nil {
				t.Errorf("instrument %s: nil after NewRecorder", c.name)
			}
		case *opsmetrics.Histogram:
			if v == nil {
				t.Errorf("instrument %s: nil after NewRecorder", c.name)
			}
		default:
			t.Errorf("instrument %s: unexpected type %T", c.name, c.got)
		}
	}
}

// TestRecorder_CollectSurfacesEveryFamily verifies that after a single
// touch on every instrument family, Registry.Collect() surfaces a sample
// from each — the Hub-bound sampler contract.
func TestRecorder_CollectSurfacesEveryFamily(t *testing.T) {
	r, reg := newRecorderAndReg()

	r.RecordRequest("p", "m", "/x", 200, 10*time.Millisecond, Usage{PromptTokens: 1, CompletionTokens: 1})
	r.RecordError("p", "e")
	r.RecordSchemaMismatch("chat", "p")
	r.RecordHookRequest("chat", "request", "approve")
	r.RecordTrafficExtract("chat", "request", "success")
	r.RecordRouterRetry("p", "5xx", "exhausted")
	r.RecordForwardHeaderDropped("request", "openai", "authorization")
	r.RecordReasoningPassthrough("anthropic", "injected")
	r.RecordEstimate("chat", "m", "p", time.Millisecond)
	r.RecordEstimateCompare("chat", 2, time.Millisecond)

	samples := reg.Collect()
	seen := map[string]bool{}
	for _, s := range samples {
		seen[s.Name] = true
	}

	expected := []string{
		"requests_total", "request_duration_ms", "tokens_total",
		"errors_total", "schema_mismatch_total",
		"hook.pipeline_total", "traffic.extract_total",
		"router.retry_total", "forward_header_dropped_total",
		"reasoning_passthrough_total",
		"estimate_requests_total", "estimate_duration_seconds",
		"estimate_compare_requests_total", "estimate_compare_targets_total",
		"estimate_compare_duration_seconds",
	}
	for _, name := range expected {
		if !seen[name] {
			// Render what we actually saw to aid debugging.
			var got []string
			for k := range seen {
				got = append(got, k)
			}
			t.Errorf("Collect() missing %s; saw: %s", name, strings.Join(got, ", "))
		}
	}
}

// packages/ai-gateway/internal/policy/aiguard/classify.go
package aiguard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/inputstaging"
)

// Backend is the minimal interface classifyImpl needs to reach the judge
// model. The concrete implementations live in backend_external.go and
// backend_provider.go; tests inject stubs.
type Backend interface {
	Call(ctx context.Context, prompt string) (*Response, error)
}

// aiguardFallbackContextLimit is the conservative token budget used for
// inputstaging.Plan when AIGuardConfig.ModelContextLimit is 0 (unknown).
// 8192 covers all current small-to-mid judge models (GPT-4o-mini, Gemini
// Flash, Claude Haiku) without risk of over-truncation.
const aiguardFallbackContextLimit = 8192

// aiguardReserveOutput is the token budget reserved for the judge's JSON
// response. 512 is well above any realistic judge reply; it leaves the
// bulk of the context budget for the conversation being classified.
const aiguardReserveOutput = 512

// RuntimeConfig is the subset of configstore.AIGuardConfig that classifyImpl
// requires per call. Keeping this narrow makes the hot path independent of
// DB-shaped optional fields (credentials, provider IDs, etc.) — those are
// resolved once at Backend construction time.
type RuntimeConfig struct {
	BackendMode        string
	BackendFingerprint string
	PromptTemplate     string
	TimeoutMs          int
	CacheTTLSeconds    int
	// InputStrategy is one of the five inputstaging.Strategy constants.
	// Empty string → defaults to StrategySystemPlusLastUser at classify time.
	InputStrategy string
	// ModelContextLimit is the judge model's context window in tokens.
	// 0 → classify uses aiguardFallbackContextLimit (8192).
	ModelContextLimit int
}

// TrafficEvent is the internal audit record handed to the sink for every
// classify attempt (success, cache hit, or failure). The HTTP handler and
// in-process caller both funnel through classifyImpl, so this is the single
// emission point for ai-guard traffic events.
type TrafficEvent struct {
	DetectorType    string
	Decision        string
	JudgeLatencyMs  int
	CacheHit        bool
	BackendMode     string
	InternalPurpose string // always "ai-guard"
	ErrorDetail     string // non-empty on failure

	// TraceID carries the triggering user request's correlation id
	// (the inbound X-Nexus-Request-Id propagated on ctx). It is stamped
	// onto the ai-guard row's trace_id so the classifier's own cost row
	// (internal_purpose='ai-guard', fresh row id) can be joined back to the
	// user-traffic row that invoked the hook. Empty for ad-hoc callers
	// (tests, tooling) that never set a request id on the context.
	TraceID string

	// Stamped from Response.Metadata after a successful classifier call;
	// left zero on CacheHit, failures, or when AdapterBackend has no
	// PriceLookup wired. Sink writes these to traffic_event.{prompt_tokens,
	// completion_tokens, ai_guard_cost_usd}.
	PromptTokens     int
	CompletionTokens int
	CostUsd          float64
}

// TrafficSink is the minimal audit interface classifyImpl requires. The
// production implementation bridges into the existing traffic_event MQ
// pipeline; tests capture events in-memory.
type TrafficSink interface {
	Emit(ctx context.Context, e TrafficEvent)
}

// BackendUnavailable signals an upstream judge failure. The HTTP handler
// maps this to 503 with ErrorBody{Error:"backend_unavailable", Detail:...}.
// Validation errors (missing fields, bad prompt template) are returned as
// plain errors so handlers can map them to 400 instead.
type BackendUnavailable struct{ Detail string }

func (e *BackendUnavailable) Error() string { return "backend_unavailable: " + e.Detail }

const internalPurposeAIGuard = "ai-guard"

// Classify is the public entry used by the HTTP handler and the in-process
// InProcClient. It is a thin shim over classifyImpl so callers never touch
// the private signature directly.
func Classify(
	ctx context.Context,
	req Request,
	cfg *RuntimeConfig,
	backend Backend,
	cache *Cache,
	sink TrafficSink,
) (*Response, error) {
	return classifyImpl(ctx, req, cfg, backend, cache, sink)
}

// applyInputStaging runs inputstaging.Plan on req.Messages when they are
// non-empty, joins the result into a flat string, and returns it as the
// effective Content for the classify pipeline. When Messages is empty the
// caller's Content is returned unchanged.
//
// Overflow handling (fail-open): if Plan returns OverflowNone, the staged
// content is used as-is. On any overflow kind, a Prometheus counter is
// incremented and a warn is logged via slog.Default(), but the staged
// (truncated) content is still forwarded to the judge — blocking on
// overflow would turn a latency spike into a silent allow-all.
func applyInputStaging(req Request, cfg *RuntimeConfig) string {
	if len(req.Messages) == 0 {
		return req.Content
	}

	strategy := inputstaging.Strategy(cfg.InputStrategy)
	if !strategy.Valid() {
		strategy = inputstaging.StrategySystemPlusLastUser
	}

	contextLimit := cfg.ModelContextLimit
	if contextLimit <= 0 {
		contextLimit = aiguardFallbackContextLimit
	}

	plan, err := inputstaging.Plan(inputstaging.PlanInput{
		Messages:          req.Messages,
		ModelContextLimit: contextLimit,
		Strategy:          strategy,
		ReserveOutput:     aiguardReserveOutput,
	})
	if err != nil {
		// Only possible if strategy is invalid (guarded above) or context
		// limit < 1 (guarded above). Fall through using original Content.
		slog.Default().Warn("aiguard: inputstaging.Plan error; using original content",
			"error", err,
		)
		return req.Content
	}

	if plan.OverflowKind != inputstaging.OverflowNone {
		slog.Default().Warn("aiguard: classify input overflow after inputstaging",
			"overflow_kind", string(plan.OverflowKind),
			"input_tokens", plan.InputTokens,
			"budget", contextLimit-aiguardReserveOutput,
			"strategy", string(strategy),
		)
		InputOverflowTotal.WithLabelValues(string(plan.OverflowKind)).Inc()
	}

	if len(plan.Messages) == 0 {
		return req.Content
	}

	var sb strings.Builder
	for i, m := range plan.Messages {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(m.Content)
	}
	// Last-resort hard cut, applied here inside ai-guard (not at the hook
	// dispatch layer): inputstaging.Plan drops whole messages but never cuts
	// within one, so a single oversized turn would reach the judge over-limit.
	// Keep the newest content (tail) so the classification reflects the latest
	// user input, not a stale preamble.
	return inputstaging.TruncateToTokens(sb.String(), contextLimit-aiguardReserveOutput)
}

// classifyImpl runs the full classify pipeline:
//
//  0. apply inputstaging (if req.Messages non-empty)
//  1. validate required fields
//  2. normalize content + compute cache key
//  3. cache lookup (hit → return)
//  4. render prompt
//  5. call backend with timeout
//  6. persist to cache
//  7. emit audit event + bump counters
//
// Step order matches spec §4.4 (flow) and §4.7 (metrics emission points).
func classifyImpl(
	ctx context.Context,
	req Request,
	cfg *RuntimeConfig,
	backend Backend,
	cache *Cache,
	sink TrafficSink,
) (*Response, error) {
	// Correlation: the triggering user request's id rides on ctx (set by
	// the RequestID middleware from the inbound X-Nexus-Request-Id, and
	// inherited by in-process hook callers whose ctx descends from the
	// request ctx). Stamp it onto every emitted ai-guard event so the
	// classifier's own cost row is joinable to the user-traffic row.
	traceID := nexushttp.RequestIDFromContext(ctx)

	// Step 0: if req.Messages is provided, apply inputstaging.Plan to
	// select the subset that fits the judge model's context window and
	// join into req.Content. Fail-open: overflow is logged + counted but
	// the (truncated) content is still forwarded to the judge.
	req.Content = applyInputStaging(req, cfg)

	// Step 1: validation. These are caller-contract violations (400), not
	// upstream failures — do NOT wrap as BackendUnavailable.
	if req.DetectorType == "" || req.Content == "" {
		return nil, errors.New("aiguard: detector_type and content are required")
	}

	// Step 2: normalize + key.
	normalized := canonicalizeForCacheKey(req.Content)
	key := CacheKey(req.DetectorType, normalized, cfg.BackendFingerprint)

	// Step 3: cache lookup.
	if cached, hit, _ := cache.Get(ctx, key); hit && cached != nil {
		CacheHitsTotal.Inc()
		cached.Metadata.CacheHit = true
		cached.Metadata.BackendMode = cfg.BackendMode
		emit(ctx, sink, TrafficEvent{
			DetectorType:    req.DetectorType,
			Decision:        cached.Decision,
			JudgeLatencyMs:  0,
			CacheHit:        true,
			BackendMode:     cfg.BackendMode,
			InternalPurpose: internalPurposeAIGuard,
			TraceID:         traceID,
		})
		DecisionsTotal.WithLabelValues(req.DetectorType, cached.Decision).Inc()
		return cached, nil
	}
	CacheMissesTotal.Inc()

	// Step 4: render prompt.
	prompt, err := Render(cfg.PromptTemplate, RenderInput{
		DetectorType:   req.DetectorType,
		Content:        req.Content,
		UpstreamTags:   req.Context.UpstreamTags,
		TargetProvider: req.Context.TargetProvider,
		TargetModel:    req.Context.TargetModel,
	})
	if err != nil {
		JudgeErrorsTotal.WithLabelValues(cfg.BackendMode, "prompt_render").Inc()
		emit(ctx, sink, TrafficEvent{
			DetectorType:    req.DetectorType,
			BackendMode:     cfg.BackendMode,
			InternalPurpose: internalPurposeAIGuard,
			ErrorDetail:     fmt.Sprintf("prompt_render_failed: %v", err),
			TraceID:         traceID,
		})
		return nil, &BackendUnavailable{Detail: "prompt_render_failed"}
	}

	// Step 5: call backend with timeout.
	timeout := time.Duration(cfg.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		// Generous fallback for callers that left RuntimeConfig.TimeoutMs
		// unset. A populated config carries its own timeout_ms (the stored
		// default is 5s); this branch only fires when no config value
		// reached the classify call at all.
		timeout = 30 * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	resp, callErr := backend.Call(callCtx, prompt)
	elapsed := time.Since(start)
	JudgeLatencySeconds.WithLabelValues(cfg.BackendMode).Observe(elapsed.Seconds())

	if callErr != nil {
		kind := "unknown"
		if errors.Is(callCtx.Err(), context.DeadlineExceeded) {
			kind = "timeout"
		}
		JudgeErrorsTotal.WithLabelValues(cfg.BackendMode, kind).Inc()
		emit(ctx, sink, TrafficEvent{
			DetectorType:    req.DetectorType,
			JudgeLatencyMs:  int(elapsed.Milliseconds()),
			BackendMode:     cfg.BackendMode,
			InternalPurpose: internalPurposeAIGuard,
			ErrorDetail:     callErr.Error(),
			TraceID:         traceID,
		})
		return nil, &BackendUnavailable{Detail: callErr.Error()}
	}

	// Step 6: populate metadata + persist to cache.
	resp.Metadata.JudgeLatencyMs = int(elapsed.Milliseconds())
	resp.Metadata.CacheHit = false
	resp.Metadata.BackendMode = cfg.BackendMode

	ttl := time.Duration(cfg.CacheTTLSeconds) * time.Second
	if ttl > 0 {
		if setErr := cache.Set(ctx, key, resp, ttl); setErr == nil {
			CacheWritesTotal.Inc()
		}
	}

	// Step 7: audit + decision counter. PromptTokens/CompletionTokens/
	// CostUsd come from AdapterBackend.Call when it has a PriceLookup
	// wired; on backend modes without usage parsing they stay 0 and the
	// sink stamps NULL.
	emit(ctx, sink, TrafficEvent{
		DetectorType:     req.DetectorType,
		Decision:         resp.Decision,
		JudgeLatencyMs:   int(elapsed.Milliseconds()),
		CacheHit:         false,
		BackendMode:      cfg.BackendMode,
		InternalPurpose:  internalPurposeAIGuard,
		PromptTokens:     resp.Metadata.PromptTokens,
		CompletionTokens: resp.Metadata.CompletionTokens,
		CostUsd:          resp.Metadata.CostUsd,
		TraceID:          traceID,
	})
	DecisionsTotal.WithLabelValues(req.DetectorType, resp.Decision).Inc()
	return resp, nil
}

// emit is a nil-safe helper; a nil sink is tolerated so ad-hoc callers
// (tests, tooling) don't need a no-op stub.
func emit(ctx context.Context, sink TrafficSink, e TrafficEvent) {
	if sink == nil {
		return
	}
	sink.Emit(ctx, e)
}

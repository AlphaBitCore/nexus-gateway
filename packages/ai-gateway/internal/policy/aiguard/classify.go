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

// aiguardMinInputBudget is the content budget floor when the judge
// prompt template consumes the model window (misconfiguration): some
// newest content always reaches the judge, bounded, never the full
// unbounded conversation.
const aiguardMinInputBudget = 256

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
	// Empty string → defaults to StrategyFullTruncated at classify time.
	InputStrategy string
	// ModelContextLimit is the judge model's context window in tokens.
	// 0 → classify uses aiguardFallbackContextLimit (8192).
	ModelContextLimit int
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
// The default strategy is full_truncated: the judge's value is violation
// coverage, and dropping middle turns would blind it to content assembled
// across turns. Under budget pressure inputstaging's enforcement drops
// oldest messages first — completeness when space allows, newest content
// when tight. Admins narrow the input via RuntimeConfig.InputStrategy.
//
// Overflow handling (fail-open): if Plan returns OverflowNone, the staged
// content is used as-is. On any overflow kind, a Prometheus counter is
// incremented and a debug line is logged, but the staged (budget-bounded)
// content is still forwarded to the judge — blocking on overflow would
// turn a latency spike into a silent allow-all.
func applyInputStaging(req Request, cfg *RuntimeConfig) string {
	if len(req.Messages) == 0 {
		return req.Content
	}

	strategy := inputstaging.Strategy(cfg.InputStrategy)
	if !strategy.Valid() {
		strategy = inputstaging.StrategyFullTruncated
	}

	contextLimit := cfg.ModelContextLimit
	if contextLimit <= 0 {
		contextLimit = aiguardFallbackContextLimit
	}
	// The judge prompt template wraps the staged content in the same
	// window, so its tokens shrink the content budget. The raw template
	// string approximates the rendered overhead (placeholders and their
	// values are the same magnitude) without paying a render per call.
	// Conservative estimate: the template is subtracted, so under-counting it
	// would inflate the content budget and risk overflowing the judge model.
	contextLimit -= inputstaging.EstimateTokensConservative(cfg.PromptTemplate)
	if contextLimit < aiguardReserveOutput+aiguardMinInputBudget {
		// The template consumes the window (misconfiguration). Floor the
		// content budget instead of letting it collapse to zero — a zero
		// budget would fail open and forward the UNBOUNDED conversation.
		// Some newest content always ships; the judge call may still fail
		// upstream, and the warn + counter surface the misconfiguration.
		slog.Default().Warn("aiguard: judge context window too small for prompt template plus reply reserve; flooring content budget",
			"model_context_limit", cfg.ModelContextLimit,
			"floor", aiguardMinInputBudget,
		)
		InputOverflowTotal.WithLabelValues("template_budget_floor").Inc()
		contextLimit = aiguardReserveOutput + aiguardMinInputBudget
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
		// Debug, not Warn: with the full_truncated default, a bounded
		// overflow is steady-state on long conversations — the counter
		// below carries the operator signal.
		slog.Default().Debug("aiguard: classify input bounded by budget enforcement",
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
	// No local hard cut: inputstaging's default budget enforcement already
	// bounds the planned messages (oldest dropped first, an oversized
	// newest turn tail-truncated), so the joined content fits the budget.
	return sb.String()
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
			// cached.Metadata carries the provider that served the original
			// (cache-writing) call. No new call is made on a cache hit — cost
			// is correctly zero — but the provider identity is still real,
			// sourced from that original resolved call target.
			ProviderID:   cached.Metadata.ProviderID,
			ProviderName: cached.Metadata.ProviderName,
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
		// The cached share of PromptTokens, carried so the row records the
		// basis of its own charge rather than just the amount.
		CacheReadTokens:     resp.Metadata.CacheReadTokens,
		CacheCreationTokens: resp.Metadata.CacheCreationTokens,
		CostUsd:             resp.Metadata.CostUsd,
		TraceID:             traceID,
		ProviderID:          resp.Metadata.ProviderID,
		ProviderName:        resp.Metadata.ProviderName,
	})
	DecisionsTotal.WithLabelValues(req.DetectorType, resp.Decision).Inc()
	return resp, nil
}

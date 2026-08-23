// aiguard.go — AI Guard wiring.
//
// This file holds the adapter types that bridge the internal aiguard
// package into ai-gateway's runtime: a live Classifier (aiguard.Classifier
// contract) and a TrafficSink that funnels classify traffic events into
// the existing audit.Writer → MQ pipeline.
//
// The live classifier resolves its Backend on every call based on the
// current AIGuardConfig snapshot from aiguard.ConfigCache. Cache hits
// short-circuit before Backend construction, so this cost is only paid
// on cache misses.
package wiring

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/costing"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/aiguard"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provtarget "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/target"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/configstore"
)

// AIGuardModelLookup is the narrow surface buildBackend needs to
// translate a Nexus Model UUID into the vendor-side model id when the
// classify call is dispatched via "external_url". *cachelayer.Layer
// satisfies it; *store.DB also does, for tests.
type AIGuardModelLookup interface {
	GetModel(ctx context.Context, id string) (*store.Model, error)
}

// newModelPriceLookup returns a PriceLookup closure shared by the AI Guard
// configured_provider backend (see LiveClassifier.buildBackend above) and
// the smart router's LLM decider (see newRouterDecider in router.go) — both
// need the same "Nexus Model UUID -> four per-million rates" lookup against
// the catalog.
//
// It returns all four rates, not just input and output, because both callers
// bill a prompt that is largely cached. Their prompts are near-identical call
// to call (a fixed system prompt plus the model catalog for the router; a
// fixed judge template for AI Guard), so their provider-side cache hit rate is
// high — and on OpenAI and Gemini the reported prompt-token count INCLUDES the
// cached subset while cached tokens bill at 0.25-0.5x. A two-rate signature
// could not express that, which is what made both callers bill every cached
// token at the full input rate and over-estimate internal spend in proportion
// to their own call volume.
//
// Returns ok=false on a failed lookup or an unpriced model, on purpose: the
// caller must still be able to serve the request with cost left at zero rather
// than fail it (fail-open). Either way it logs one Warn naming the model id, so
// a pricing-row gap in the catalog is visible instead of silently reproducing a
// positive-only vendor-spend under-report — an unpriced router model previously
// produced $0 with no signal at all, which left the day looking reconciled and
// kept the branch's own no_basis alert asleep.
func newModelPriceLookup(ctx context.Context, models AIGuardModelLookup, logger *slog.Logger, caller string) func(modelID string) (costing.Rates, bool) {
	return func(modelID string) (costing.Rates, bool) {
		if models == nil {
			return costing.Rates{}, false
		}
		m, err := models.GetModel(ctx, modelID)
		if err != nil || m == nil {
			if logger != nil {
				logger.Warn("price lookup: model not found in catalog, cost will be recorded as zero",
					"caller", caller, "modelId", modelID, "error", err)
			}
			return costing.Rates{}, false
		}
		// Same NULL interpretation as the customer request path's
		// cachelayer.LookupCachePricing — a NULL cache rate means "no discount
		// configured", falling back to the input rate, and a NULL input price
		// means the model is unpriced.
		rates, priced := costing.RatesFromModel(
			m.InputPricePM, m.OutputPricePM, m.CachedInputReadPricePM, m.CachedInputWritePricePM)
		if !priced || !rates.Priced() {
			if logger != nil {
				logger.Warn("price lookup: model has no catalog price, cost will be recorded as zero",
					"caller", caller, "modelId", modelID)
			}
			return costing.Rates{}, false
		}
		return rates, true
	}
}

// LiveClassifier implements aiguard.Classifier by composing the
// live aiguard pipeline: config cache → Backend construction → classify.
// All shared state (Redis cache, audit sink, config cache, provider
// registry) is injected at construction; Classify is safe for concurrent
// use because every embedded component is already goroutine-safe.
type LiveClassifier struct {
	Cache         *aiguard.Cache
	Sink          aiguard.TrafficSink
	ConfigCache   *aiguard.ConfigCache
	Adapters      *provcore.Registry
	Resolver      provtarget.Resolver
	DB            AIGuardModelLookup
	ExtHTTPClient *http.Client
	Logger        *slog.Logger
}

// Classify resolves the current AIGuardConfig, builds a Backend matching
// the configured BackendMode, and delegates to aiguard.Classify. Errors
// bubble up unwrapped so the HTTP handler can route *aiguard.BackendUnavailable
// to 503 (see classify.ClassifyHandler.ServeClassify).
func (l *LiveClassifier) Classify(ctx context.Context, req aiguard.Request) (*aiguard.Response, error) {
	cfg, err := l.ConfigCache.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("aiguard: load config: %w", err)
	}
	rc := &aiguard.RuntimeConfig{
		BackendMode:        cfg.BackendMode,
		BackendFingerprint: cfg.BackendFingerprint,
		PromptTemplate:     cfg.PromptTemplate,
		TimeoutMs:          cfg.TimeoutMs,
		CacheTTLSeconds:    cfg.CacheTTLSeconds,
		InputStrategy:      cfg.InputStrategy,
		ModelContextLimit:  cfg.ModelContextLimit,
	}

	backend, err := l.buildBackend(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return aiguard.Classify(ctx, req, rc, backend, l.Cache, l.Sink)
}

// buildBackend maps BackendMode → Backend.
func (l *LiveClassifier) buildBackend(ctx context.Context, cfg *configstore.AIGuardConfig) (aiguard.Backend, error) {
	switch cfg.BackendMode {
	case "configured_provider":
		if cfg.ProviderID == nil || *cfg.ProviderID == "" {
			return nil, fmt.Errorf("aiguard: configured_provider missing providerId")
		}
		if cfg.ModelID == nil || *cfg.ModelID == "" {
			return nil, fmt.Errorf("aiguard: configured_provider missing modelId")
		}
		if l.Resolver == nil || l.Adapters == nil {
			return nil, fmt.Errorf("aiguard: resolver or adapter registry not available")
		}
		// PriceLookup hits the in-memory cachelayer (DB above is the
		// AIGuardModelLookup interface, satisfied by *cachelayer.Layer
		// in prod — snapshot reads, no DB roundtrip). Returns (0, 0)
		// on lookup failure so AdapterBackend leaves cost zero. Shared with
		// the smart router's LLM decider — see newModelPriceLookup below.
		return &aiguard.AdapterBackend{
			Resolver:    l.Resolver,
			Registry:    l.Adapters,
			ProviderID:  *cfg.ProviderID,
			ModelID:     *cfg.ModelID,
			Logger:      l.Logger,
			PriceLookup: newModelPriceLookup(ctx, l.DB, l.Logger, "aiguard.configured_provider"),
		}, nil

	case "external_url":
		if cfg.ExternalURL == nil || *cfg.ExternalURL == "" {
			return nil, fmt.Errorf("aiguard: external_url missing externalUrl")
		}
		// The external judge is the operator's OWN service and
		// authenticates only via CustomHeaders. No stored Credential is
		// resolved here — a Credential is always a real upstream provider
		// key, and forwarding one to an operator-chosen URL was a
		// key-exfiltration path. configured_provider mode is the only path
		// that touches a provider credential, and there it reaches its own
		// provider.
		headers := map[string]string{}
		for k, v := range cfg.CustomHeaders {
			if s, ok := v.(string); ok {
				headers[k] = s
			}
		}
		// cfg.ModelID is a Nexus Model UUID. The external endpoint expects a
		// vendor model id, so translate via the catalog before sending. When
		// ModelID is empty or unresolvable we send "" — the external service
		// handles missing model fields per its own contract.
		model := ""
		if cfg.ModelID != nil && *cfg.ModelID != "" && l.DB != nil {
			if m, err := l.DB.GetModel(ctx, *cfg.ModelID); err == nil && m != nil {
				model = m.ProviderModelID
			}
		}
		return &aiguard.ExternalBackend{
			URL:           *cfg.ExternalURL,
			Model:         model,
			CustomHeaders: headers,
			HTTPClient:    l.ExtHTTPClient,
		}, nil

	default:
		return nil, fmt.Errorf("aiguard: unknown backend_mode %q", cfg.BackendMode)
	}
}

// WriterBackedTrafficSink forwards aiguard.TrafficEvent into the existing
// audit.Writer pipeline with InternalPurpose="ai-guard" so the CP admin
// list view can hide these rows from billing displays by default.
type WriterBackedTrafficSink struct {
	Writer *audit.Writer
}

// Emit enqueues one audit.Record per classify event. Matches the
// traffic_event schema — decision + latency + source = "ai-gateway".
// The record carries an empty virtual-key identity (ai-guard is internal
// traffic, not customer-billed) and stashes detector/backend/cacheHit on
// Metadata for the consumer to persist into traffic_event.metadata.
//
// CacheStatus mapping: ai-guard has its own classify cache (separate
// from the response cache). The traffic_event.cache_status column
// answers "was this row served from a cache" — for ai-guard rows the
// answer is its own classify cache, so we map e.CacheHit true → HIT,
// false → MISS.
func (s *WriterBackedTrafficSink) Emit(ctx context.Context, e aiguard.TrafficEvent) {
	if s == nil || s.Writer == nil {
		return
	}
	cacheStatus := audit.CacheStatusMiss
	if e.CacheHit {
		cacheStatus = audit.CacheStatusHit
	}
	rec := &audit.Record{
		// No RequestID: this row is emitted by the gateway itself, not by a
		// caller, so there is no caller correlation value to record. Each row
		// gets its own traffic_event id from the audit writer, and TraceID
		// below joins it back to the request that triggered the classify.
		RequestID: "",
		// TraceID carries the triggering user request's correlation id so
		// this ai-guard cost row (fresh RequestID, internal_purpose='ai-guard')
		// is joinable back to the user-traffic row that invoked the hook.
		// Empty when the classify call carried no request id on its context.
		TraceID: e.TraceID,
		// status_code=200: this row genuinely is a successful classify call
		// (failed classify returns earlier with e.Decision empty and
		// ErrorDetail set, so this code path only stamps successful
		// classifications). This is NOT about folding ai_guard_cost_usd into
		// MetricBilledCostUSD — the Hub's rollup, in rollup_5m.go's
		// (*Rollup5mJob).emitEventMetrics (see the comment directly above its
		// `add(metrics.MetricBilledCostUSD, cost)` call), never folds
		// internal-ops cost into the billed series regardless of status code;
		// excludeInternalOpsFromBilled is vestigial there. MetricAIGuardCostUSD
		// itself is NOT status-gated either — it's accumulated unconditionally
		// on aiGuardCostUsd != 0, under emitEventMetrics's "Internal-ops cost:
		// embedding (L2 lookup) + AI-Guard classifier. Gross metrics — every
		// contributing row counted." comment, entirely outside the isSuccess
		// block. What StatusCode=200 actually gates on this row is the
		// status-count series, and — because this row also carries token
		// counts with a MISS cache status — the isSuccess && !cacheHitVal
		// block's `add(metrics.MetricBilledTokens, ...)` accumulation. Setting
		// 200 is still correct regardless: the row represents a classify that
		// genuinely succeeded.
		StatusCode:       http.StatusOK,
		Timestamp:        time.Now().UTC(),
		HookDecision:     e.Decision,
		InternalPurpose:  e.InternalPurpose,
		LatencyMs:        e.JudgeLatencyMs,
		CacheStatus:      cacheStatus,
		PromptTokens:     int64(e.PromptTokens),
		CompletionTokens: int64(e.CompletionTokens),
		TotalTokens:      int64(e.PromptTokens + e.CompletionTokens),
		// The cached share of PromptTokens, which is the total input — NOT an
		// addition to it, so TotalTokens above deliberately does not include
		// them again. These reuse the columns the customer path already fills,
		// so the classifier's charge is auditable from its own row: prompt,
		// how much of it was cached, and the resulting cost. Without them a
		// reader could not tell a correct cache-discounted charge from the
		// full-rate over-charge this row used to carry.
		CacheReadTokens:     int64(e.CacheReadTokens),
		CacheCreationTokens: int64(e.CacheCreationTokens),
		// RoutedProviderID/-Name identify the provider that actually served
		// this classify call, carried from the resolved call target via
		// TrafficEvent.ProviderID/-Name (empty when the backend has no
		// provider concept, e.g. ExternalBackend). Without a non-empty
		// routed provider the Hub's rollup emits no routed_provider
		// dimension for the row (see rollup_5m.go's dims append below
		// `if routedProviderID != ""`), so the classifier's cost could
		// never reach any provider's slice of vendor_spend_usd no matter
		// what the downstream aggregation does.
		RoutedProviderID:   e.ProviderID,
		RoutedProviderName: e.ProviderName,
		// AIGuardCostUsd lands on the classify-call's own row (the row
		// where internal_purpose='ai-guard', with its own minted id). The
		// user-traffic row that triggered the hook keeps NULL. The ai-guard
		// row carries the triggering request's trace_id (set above), so the
		// two ARE per-request joinable on trace_id — ai-guard cost can be
		// attributed to the user request, and is still excluded from billable
		// totals via internal_purpose='ai-guard'.
		AIGuardCostUsd: e.CostUsd,
		Metadata: map[string]any{
			"detectorType": e.DetectorType,
			"backendMode":  e.BackendMode,
			"errorDetail":  e.ErrorDetail,
		},
	}
	s.Writer.Enqueue(rec)
}

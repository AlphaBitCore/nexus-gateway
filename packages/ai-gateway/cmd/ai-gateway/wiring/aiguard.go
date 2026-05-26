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

	"github.com/google/uuid"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/aiguard"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	credmanager "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/credentials/manager"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provtarget "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/target"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/configstore"
)

// AIGuardModelLookup is the narrow surface buildBackend needs to
// translate a Nexus Model UUID into the vendor-side model id when the
// classify call is dispatched via "external_url". *cachelayer.Layer
// satisfies it; *store.DB also does, for tests.
type AIGuardModelLookup interface {
	GetModel(ctx context.Context, id string) (*store.Model, error)
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
	CredentialMgr *credmanager.Manager
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
		// on lookup failure so AdapterBackend leaves cost zero.
		priceLookup := func(modelID string) (float64, float64) {
			if l.DB == nil {
				return 0, 0
			}
			m, err := l.DB.GetModel(ctx, modelID)
			if err != nil || m == nil {
				return 0, 0
			}
			var in, out float64
			if m.InputPricePM != nil {
				in = *m.InputPricePM
			}
			if m.OutputPricePM != nil {
				out = *m.OutputPricePM
			}
			return in, out
		}
		return &aiguard.AdapterBackend{
			Resolver:    l.Resolver,
			Registry:    l.Adapters,
			ProviderID:  *cfg.ProviderID,
			ModelID:     *cfg.ModelID,
			Logger:      l.Logger,
			PriceLookup: priceLookup,
		}, nil

	case "external_url":
		if cfg.ExternalURL == nil || *cfg.ExternalURL == "" {
			return nil, fmt.Errorf("aiguard: external_url missing externalUrl")
		}
		apiKey := ""
		if cfg.ExternalCredentialID != nil && *cfg.ExternalCredentialID != "" {
			key, err := l.CredentialMgr.GetDecrypted(ctx, *cfg.ExternalCredentialID)
			if err != nil {
				return nil, fmt.Errorf("aiguard: external credential: %w", err)
			}
			apiKey = key
		}
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
			APIKey:        apiKey,
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
		// Generate a RequestID per emit — recordToMessage maps it into
		// msg.ID, which is the traffic_event PK. The Hub INSERT uses
		// ON CONFLICT (id) DO NOTHING, so two ai-guard rows sharing an
		// empty id silently lose all but the first.
		RequestID: uuid.NewString(),
		// status_code=200 so the rollup's isSuccess check passes and
		// aiGuardCostUsd folds into MetricBilledCostUSD when the operator
		// hasn't excluded internal-ops via yaml. Without this, the classify
		// call's cost is visible on the row but never reaches the
		// billed_cost_usd metric series. Failed classify (BackendUnavailable)
		// returns earlier with e.Decision empty and ErrorDetail set, so this
		// code path only stamps successful classifications.
		StatusCode:       http.StatusOK,
		Timestamp:        time.Now().UTC(),
		HookDecision:     e.Decision,
		InternalPurpose:  e.InternalPurpose,
		LatencyMs:        e.JudgeLatencyMs,
		CacheStatus:      cacheStatus,
		PromptTokens:     int64(e.PromptTokens),
		CompletionTokens: int64(e.CompletionTokens),
		TotalTokens:      int64(e.PromptTokens + e.CompletionTokens),
		// AIGuardCostUsd lands on the classify-call's own row (the row
		// where internal_purpose='ai-guard'). The user-traffic row that
		// triggered the hook keeps NULL — joining the two via trace_id
		// gives admins both views.
		AIGuardCostUsd: e.CostUsd,
		Metadata: map[string]any{
			"detectorType": e.DetectorType,
			"backendMode":  e.BackendMode,
			"errorDetail":  e.ErrorDetail,
		},
	}
	s.Writer.Enqueue(rec)
}

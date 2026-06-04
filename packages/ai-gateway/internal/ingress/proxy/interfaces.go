package proxy

import (
	"context"
	"net/http"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/semantic"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// RouteResolver resolves a requested model label to ordered routing targets.
// Production wires [*routingcore.Resolver]; tests may substitute stubs.
//
// Callers construct the *routingcore.RoutingContext — populating
// RequestedModel, Endpoint, VirtualKey, and the canonical Request payload
// (from normalize.Registry.Normalize). The resolver does not parse raw
// bytes or HTTP headers; it operates strictly on the typed RoutingContext.
type RouteResolver interface {
	ResolveTargets(ctx context.Context, rctx *routingcore.RoutingContext) (*routingcore.RouteResult, error)
}

// VKAuthenticator authenticates virtual keys from HTTP requests.
type VKAuthenticator interface {
	Authenticate(ctx context.Context, r *http.Request) (*vkauth.VKMeta, error)
}

// CredentialLookup resolves decrypted API keys for providers.
type CredentialLookup interface {
	GetForProvider(ctx context.Context, providerID string) (apiKey string, credentialID string, credentialName string, err error)
}

// ModelLookup reads model data from the database.
type ModelLookup interface {
	GetModel(ctx context.Context, id string) (*store.Model, error)
	GetModelByCode(ctx context.Context, idOrName string) (*store.Model, error)
	ListEnabledModels(ctx context.Context) ([]store.Model, error)
	// FetchModelPricing returns pricing rows for the requested IDs.
	// Production wires *cachelayer.Layer which reads from the in-memory
	// model snapshot — eliminates the last DB hit on the quota
	// downgrade path.
	FetchModelPricing(ctx context.Context, modelIDs []string) ([]store.ModelPricing, error)
}

// CachePricingLookup resolves the best-matching provider_pricing row for a
// given (adapter type, provider ID, model ID) triple. Returns nil when no
// rule matches; callers treat a nil result as zero cache costs.
type CachePricingLookup interface {
	LookupCachePricing(adapterType, providerID, modelID string) *store.ProviderPricing
}

// RateLimiter checks per-key rate limits.
type RateLimiter interface {
	Allow(key string, limit int, windowMs int64) (bool, int)
}

// MetricsRecorder records Prometheus metrics.
type MetricsRecorder interface {
	RecordRequest(provider, model, endpoint string, status int, duration time.Duration, usage metrics.Usage)
	// RecordHookRequest increments the hook-pipeline counter for a
	// given ingress wire format, hook stage, and terminal decision.
	// Called once per pipeline execution from runRequestHooks.
	RecordHookRequest(ingressFormat, stage, decision string)
	// RecordTrafficExtract increments the traffic-adapter extract
	// counter. Called on every Extract* invocation driven by the hook
	// pipeline so operators can distinguish per-format extraction
	// pressure and error rates.
	RecordTrafficExtract(ingressFormat, direction, outcome string)

	// RecordEstimate increments nexus_estimate_requests_total and
	// observes nexus_estimate_duration_seconds. Called once per
	// /v1/estimate compareTarget so dashboards have one fan-in.
	RecordEstimate(ingress, model, provider string, duration time.Duration)
	// RecordEstimateCompare increments
	// nexus_estimate_compare_requests_total (1) and
	// nexus_estimate_compare_targets_total (N) and observes
	// nexus_estimate_compare_duration_seconds. Called once per
	// top-level /v1/estimate request.
	RecordEstimateCompare(ingress string, targetCount int, duration time.Duration)
}

// SemanticReaderAPI is the narrow interface the proxy handler uses for L2
// semantic cache reads.  Production wires *semantic.Reader; tests may
// substitute a stub without depending on the concrete type's Redis + embedding
// infrastructure.
type SemanticReaderAPI interface {
	Read(ctx context.Context, req semantic.ReadRequest) (semantic.ReadResult, error)
}

// SemanticWriterAPI is the narrow interface the proxy handler uses for L2
// semantic cache write-back.  Production wires *semantic.Writer; tests may
// substitute a no-op stub.
type SemanticWriterAPI interface {
	Write(ctx context.Context, req semantic.WriteRequest) (semantic.WriteResult, error)
}

// Package handler implements the HTTP route handlers for the AI gateway.
// The proxy handler orchestrates the full pipeline: VK auth → rate limit →
// routing → request hooks → credential lookup → upstream fetch → response
// hooks (or live streaming compliance) → audit → response.
package proxy

import (
	"github.com/goccy/go-json"
	"log/slog"
	"net/http"
	"os"
	"time"

	cache "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/freshness"
	geminicache "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/gemini"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/semantic"
	streamcache "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/stream"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/executor"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/forwardheader"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/passthrough"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/quota"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	cfgpolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/policy"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/wirerewrite"
)

// NewUpstreamClient returns an http.Client configured for upstream provider
// requests. Uses a shared transport with connection pooling and timeouts.
// Tunables come from the live [specutil.ActiveConfig] snapshot so the
// legacy proxy path and the spec-adapter path share one set of values.
func NewUpstreamClient() *http.Client {
	cfg := specutil.ActiveConfig()
	return nexushttp.New(nexushttp.Config{
		Timeout:             cfg.Timeout + 5*time.Second,
		DialTimeout:         cfg.DialTimeout,
		KeepAlive:           cfg.KeepAlive,
		MaxIdleConns:        cfg.MaxIdleConns,
		MaxIdleConnsPerHost: cfg.MaxIdleConnsPerHost,
		IdleConnTimeout:     cfg.IdleConnTimeout,
		TLSHandshakeTimeout: cfg.TLSHandshakeTimeout,
		Caller:              "ai-gateway-upstream",
	})
}

// Deps holds all injected dependencies for the proxy handler.
type Deps struct {
	Models          ModelLookup               // was accessed via DB.GetModel etc.
	VKAuth          VKAuthenticator           // was *vkauth.Authenticator
	RateLimiter     RateLimiter               // was *ratelimit.Limiter
	CredManager     CredentialLookup          // was *credentials.Manager — used by models handler
	Router          RouteResolver             // typically *routingcore.Resolver
	Executor        executor.API              // upstream dispatch with retry/credential/health; production wires *executor.TargetExecutor
	HookConfigCache *pipeline.HookConfigCache // shared hook config cache
	ProviderReg     *provcore.Registry        // adapter-based provider registry
	HealthTracker   *store.HealthTracker      // stays concrete — used by background flush
	AuditWriter     *audit.Writer             // stays concrete
	Metrics         MetricsRecorder           // was *metrics.Recorder
	QuotaEngine     *quota.Engine             // hierarchical enforcement engine
	Cache           *cache.Cache              // response cache (nil = disabled)
	// BrokerRegistry fans live upstream calls out to multiple
	// concurrent subscribers per cache key. Concurrent requests with
	// the same cache key share one upstream call:
	// the first subscriber stamps audit.CacheStatusMiss and triggers
	// the leaderFn; joiners stamp audit.CacheStatusHitLive and
	// consume the same chunk timeline. On the broker's terminal
	// frame the cache layer persists the timeline so subsequent
	// cold lookups become true HITs. Nil disables broker fan-out;
	// every MISS opens its own upstream session in that case.
	BrokerRegistry *streamcache.Registry
	// CacheMetrics is the Prometheus instruments for the SSE cache +
	// broker subsystem. Nil disables instrumentation.
	CacheMetrics   *streamcache.Metrics
	UpstreamClient *http.Client
	// PayloadCapture is the atomically swappable payload-capture config
	// snapshot. Nil falls back to payloadcapture.DefaultConfig so a
	// caller that forgets to wire the store still gets the safe default
	// of "capture off, 256 KiB inline cutoff".
	PayloadCapture *payloadcapture.Store

	// StreamingPolicy is the hot-swappable streaming compliance policy
	// Store. The streaming shape stage reads Store.Get() per request to
	// dispatch between passthrough, live (chunked_async), and buffer
	// (buffer_full_block) modes. A nil Store defaults to chunked_async:
	// gateway clients are opted-in API callers (unlike tlsbump's
	// transparent host path, which defaults nil to passthrough), so
	// compliance hooks run even when no admin policy has been wired.
	// Production wiring boot-seeds the Store from system_metadata, so
	// nil only occurs in stripped-down test composition.
	StreamingPolicy *streampolicy.Store

	// StreamCaptureHardCap is the in-memory hard ceiling on streaming
	// response capture buffers (sourced from cfg.Spill.PerObjectCap()).
	// Bytes past this hard cap continue to flow on the wire but the
	// audit buffer no longer grows; Truncated flips true. <= 0 falls
	// back to 256 MiB.
	StreamCaptureHardCap int64
	// TrafficAdapters is the shared-traffic adapter registry used by
	// the hook pipeline. The handler instantiates a format-specific
	// adapter per request via [Handler.trafficAdapterFor] so hooks on
	// native ingress routes (Anthropic, Gemini, …) run through the
	// right content extractor instead of the hard-coded OpenAI one.
	// Nil falls back to [Deps.TrafficAdapter] (single-adapter tests)
	// or the package default.
	TrafficAdapters *traffic.AdapterRegistry
	// TrafficAdapter is retained for tests that wire a single adapter
	// instance (the proxy_hook_rewrite_test and friends). Production
	// wiring should set [Deps.TrafficAdapters] instead.
	TrafficAdapter traffic.Adapter
	// SchemaMismatchRecorder receives `(ingressFormat, providerFormat)`
	// tuples when cross-format routing rejects a target. The production
	// wiring increments the `schema_mismatch_total` opsmetrics counter;
	// tests may leave this nil.
	SchemaMismatchRecorder SchemaMismatchRecorder
	// CanonicalBridge performs ingress ↔ OpenAI-hub ↔ provider wire
	// translation for chat completions. Nil uses the legacy executor path.
	CanonicalBridge canonicalbridge.API
	// RoutingDefaultPolicy is the YAML-configured platform default retry
	// policy (cfg.Routing.DefaultRetryPolicy). Per-rule overrides on the
	// matched RoutingRule field-merge on top before the effective policy
	// is passed to the executor. Zero value falls back at call time to
	// cfgpolicy.DefaultRetryPolicy() so a misconfigured deployment
	// still gets a usable policy.
	RoutingDefaultPolicy cfgpolicy.RetryPolicy
	// Allowlist is the YAML-resolved forward-header allowlist.
	// Read at request time on both the live and cache-hit response
	// paths to filter upstream → client headers; its Hash() also feeds
	// the cache key as the allowlist version. Nil falls back to the
	// embedded defaults; production startup wires
	// forwardheader.Resolve(...) into this field.
	Allowlist *forwardheader.Resolved
	// CachePricing resolves per-model cache cost rates from the Model
	// snapshot (the single pricing source of truth). Used to compute
	// cache_write_cost_usd / cache_read_savings_usd / cache_net_savings_usd
	// on traffic_event. Nil disables cache-cost accounting.
	CachePricing CachePricingLookup
	// Normaliser is the prompt cache normalisation engine. Nil disables
	// both L0 key normalisation and L3 upstream body normalisation.
	Normaliser *wirerewrite.Engine
	// GeminiCacheMgrSet holds one Manager per Gemini/Vertex Provider,
	// each resolved against the three-tier prompt cache config. Nil
	// disables Gemini cache injection entirely; a nil Manager returned
	// from Get() means the provider is not in a Gemini family and the
	// request flows through unchanged.
	GeminiCacheMgrSet *geminicache.ManagerSet
	// NormalizeRegistry is the shared/normalize Registry that produces the
	// canonical *normcore.NormalizedPayload. The handler invokes Normalize
	// exactly once per request and threads the result through
	// *routingcore.RoutingContext.Request for the routing layer.
	// Nil falls back to no canonical payload — routing layer treats this
	// as "non-AI / unrecognised request" and smart strategies fall back
	// to default; production startup must wire this.
	NormalizeRegistry *normcore.Registry
	// PassthroughCache holds the runtime kill-switch configuration as an
	// atomic.Pointer-backed Snapshot. Hub pushes the merged 3-tier blob
	// via the gateway_passthrough_config shadow key; the handler resolves
	// the effective config for the routed target's provider and wraps the
	// result into ResolvedRequest. Nil falls back to "passthrough disabled
	// for every provider" — production startup must wire this.
	PassthroughCache *passthrough.Cache
	// LatencyDetail mirrors config.Observability.LatencyDetail — when
	// true the per-request latency_breakdown JSONB surfaces sub-ms
	// phases (floored to 1) so a perf investigation can verify every
	// expected phase actually ran. See traffic.PhaseTimer.SnapshotDetail.
	LatencyDetail bool
	// FreshnessDetector holds the atomically-updated time-sensitive
	// pattern detector. When non-nil AND the fleet-wide
	// extract_cache_config.apply_freshness_rules flag is on, the pre-lookup
	// classifier skips both L1 and L2 caches for messages that match the
	// detector's compiled rule set. Nil disables the check so deployments
	// that haven't pushed a freshness pattern config still work.
	FreshnessDetector *freshness.Detector
	// SemanticReader executes the L2 semantic cache lookup on every L1
	// miss. Nil disables L2 lookup entirely. Shared across all handler
	// instances. Production wires *semantic.Reader; tests may wire a stub.
	SemanticReader SemanticReaderAPI
	// SemanticWriter persists a fresh upstream response into the L2
	// semantic cache after a successful broker dispatch. Fired in a
	// detached goroutine with a 5-second deadline so it never delays
	// response delivery. Nil disables L2 write-back. Production wires
	// *semantic.Writer; tests may wire a stub.
	SemanticWriter SemanticWriterAPI
	// SemanticConfigCache is the fleet-wide singleton snapshot of
	// semantic_cache_config. L2 lookup/write is gated on
	// SemanticConfigCache.EffectiveEnabled() — when false (the default),
	// L2 is skipped entirely. When nil, L2 is also skipped.
	SemanticConfigCache *semantic.ConfigCache
	Logger              *slog.Logger
}

// Handler orchestrates the proxy request pipeline.
type Handler struct {
	deps *Deps
	// lazyCanonical is the lazy-canonical gate. Default ON: the request-path
	// canonical is computed only when a synchronous consumer (smart routing or the
	// response cache) needs it; otherwise it is left nil. The audit path never
	// consumes it — normalized is recomputed at view time from the stored raw
	// body — so on the clean path (hooks-off, simple routing, cache-off) this
	// skips the eager request-body Normalize entirely (~29% of request-path CPU
	// on a 50 KB body). Set NEXUS_LAZY_CANONICAL=0 to force always-compute.
	lazyCanonical bool
	// gate bounds in-flight proxy requests (nil = disabled). See admissionGate:
	// overload degrades into fast retryable 429s instead of unbounded in-heap
	// queueing toward the GOMEMLIMIT collapse.
	gate *admissionGate
}

// NewHandler creates a Handler with the given dependencies.
func NewHandler(deps *Deps) *Handler {
	return &Handler{
		deps:          deps,
		lazyCanonical: os.Getenv("NEXUS_LAZY_CANONICAL") != "0",
		gate:          newAdmissionGate(),
	}
}

// payloadCaptureConfig returns the active payload-capture snapshot for
// this request. A missing Store degrades to DefaultConfig so the hot
// path never needs to nil-check.
func (h *Handler) payloadCaptureConfig() payloadcapture.Config {
	if h.deps == nil || h.deps.PayloadCapture == nil {
		return payloadcapture.DefaultConfig()
	}
	return h.deps.PayloadCapture.Get()
}

// streamCaptureHardCap returns the byte ceiling for the streaming
// capture tee. Sourced from spill.perObjectCap; <= 0 defaults to 256 MiB.
func (h *Handler) streamCaptureHardCap() int64 {
	const fallback int64 = 256 * 1024 * 1024
	if h.deps == nil || h.deps.StreamCaptureHardCap <= 0 {
		return fallback
	}
	return h.deps.StreamCaptureHardCap
}

// ServeProxy returns an http.HandlerFunc for a given [Ingress]. The
// ingress descriptor declares the canonical endpoint kind, the wire
// body format a client will send, and whether the route path is
// streaming. The caller (main.go route table) passes a populated
// Ingress; downstream pipeline stages read it from the request
// context via [IngressFromContext].
//
// `x-nexus-aigw-body-format` is honoured as an explicit override only when
// the route's ingress is the OpenAI-compat family; on native routes
// the path is authoritative.
//
// The handler drives the request through an explicit stage chain — one
// type per stage, each in its stage_<name>.go file: admission (VK auth,
// rate limit, body read, canonical request context) → routing (target
// resolution, passthrough resolve, cross-format guards) → quota →
// request hooks → cache (pre-lookup classify, upstream body prepare,
// L1/L2 lookup) → execute (normalise, broker or direct dispatch) →
// respond. Shared per-request state travels in [proxyState]; a stage
// returning false has terminated the request and stops the chain. The
// accounting defer ([proxyState.finalizeAudit]) enqueues the audit
// record on every exit path.
func (h *Handler) ServeProxy(in Ingress) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// In-flight admission gate: reject-fast BEFORE any per-request setup
		// (no state, no audit record — the shed path's observability is the
		// admission_shed_total counter). Health/metrics/admin routes live
		// outside ServeProxy and are never gated.
		if h.gate != nil {
			if !h.gate.acquire() {
				writeOverloaded(w, in.BodyFormat)
				return
			}
			defer h.gate.release()
		}
		s, ok := h.newProxyState(in, w, r)
		if !ok {
			return // invalid body-format override; 400 already written
		}
		// Centralized audit + latency via defer, covering every stage exit.
		defer s.finalizeAudit()
		for _, stage := range []proxyStage{
			admissionStage{s},
			routingStage{s},
			quotaStage{s},
			requestHooksStage{s},
			cacheStage{s},
			executeStage{s},
			respondStage{s},
		} {
			if !stage.run() {
				return
			}
		}
	}
}

// finalize computes latency and enqueues the audit record. Called via defer.
//
// Latency uses ceiling-millisecond rounding (µs → ms with round-up) so a
// sub-millisecond cache hit reports as 1ms instead of 0. Reporting 0 here
// was a real bug: the wire format treats 0 as "field absent" (omitempty
// upstream + *int downstream), so the Hub stored NULL and the UI's
// Latency column rendered "-" on every fast cache hit.
func (h *Handler) finalize(rec *audit.Record, start time.Time) {
	if rec.LatencyMs == 0 {
		us := time.Since(start).Microseconds()
		ms := int((us + 999) / 1000)
		if ms < 1 {
			ms = 1
		}
		rec.LatencyMs = ms
	}
	h.deps.AuditWriter.Enqueue(rec)
}

// buildProviderRequest assembles the [provcore.Request] that
// fetchUpstream hands to the executor. It is split out so the wiring
// between the inbound http.Request and the provider adapter (in
// particular the per-format header allowlist driven by
// Request.Headers) is unit-testable without spinning up the full
// adapter stack — see TestBuildProviderRequest.
//
// Inbound headers are forwarded as-is; the spec adapter applies its
// own allowlist (general + per-format) on top, so security-sensitive
// headers (Authorization, Cookie, x-api-key, Nexus-internal) never
// reach the upstream — they are filtered there. Provider-specific
// betas (anthropic-beta, openai-beta, x-goog-user-project, ...) MUST
// reach the upstream for newer Anthropic / OpenAI / Google features
// (e.g. context_management, prompt caching) to be accepted.
func buildProviderRequest(r *http.Request, in Ingress, body []byte, isStream bool, maxResp int64) provcore.Request {
	var headers http.Header
	if r != nil {
		headers = r.Header
	}
	return provcore.Request{
		WireShape:        in.WireShape,
		BodyFormat:       in.BodyFormat,
		Body:             body,
		Headers:          headers,
		Stream:           isStream,
		MaxResponseBytes: maxResp,
	}
}

// parseRulePolicy unmarshals a routing rule's stored retryPolicy JSON
// into a *cfgpolicy.RetryPolicy. Returns nil for empty/null/invalid
// JSON; an unparseable value is logged but does not fail the request —
// the rule simply inherits the YAML default.
func (h *Handler) parseRulePolicy(raw json.RawMessage) *cfgpolicy.RetryPolicy {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var p cfgpolicy.RetryPolicy
	if err := json.Unmarshal(raw, &p); err != nil {
		if h != nil && h.deps != nil && h.deps.Logger != nil {
			h.deps.Logger.Warn("routing rule retryPolicy JSON unparseable; falling back to YAML default",
				slog.String("error", err.Error()),
				slog.String("raw", string(raw)),
			)
		}
		return nil
	}
	return &p
}

// effectiveRetryPolicy returns the policy the executor should honor for
// this request: the YAML default field-merged with the matched rule's
// per-rule override (if any). When the deps are missing a default
// (e.g. tests that did not wire RoutingDefaultPolicy), this falls back
// to cfgpolicy.DefaultRetryPolicy() so the executor never runs with a
// zero-valued policy (which would clamp MaxAttemptsPerTarget to 1 with
// nil RetryOn — "retry everything once" — instead of the documented
// platform defaults).
func (h *Handler) effectiveRetryPolicy(raw json.RawMessage, logger *slog.Logger) cfgpolicy.RetryPolicy {
	base := cfgpolicy.DefaultRetryPolicy()
	if h != nil && h.deps != nil {
		// Treat an all-zero RoutingDefaultPolicy as "not wired" — main.go
		// always populates it from cfg.Routing.DefaultRetryPolicy, which
		// the config loader merges against DefaultRetryPolicy() so a
		// real deployment always carries non-zero fields.
		dp := h.deps.RoutingDefaultPolicy
		if dp.MaxAttemptsPerTarget != 0 || dp.RetryOn != nil ||
			dp.BackoffInitial != 0 || dp.BackoffMax != 0 || dp.BackoffJitter != 0 {
			base = dp
		}
	}
	rule := h.parseRulePolicy(raw)
	policy := base.MergedWith(rule)
	policy.MaxAttemptsPerTarget = cfgpolicy.ClampMaxAttempts(policy.MaxAttemptsPerTarget)
	if logger != nil && rule != nil {
		logger.Debug("retry policy merged",
			slog.Int("maxAttemptsPerTarget", policy.MaxAttemptsPerTarget),
			slog.Any("retryOn", policy.RetryOn),
			slog.Duration("backoffInitial", policy.BackoffInitial),
			slog.Duration("backoffMax", policy.BackoffMax),
			slog.Float64("backoffJitter", policy.BackoffJitter),
		)
	}
	return policy
}

package audit

import (
	"fmt"
	"log/slog"
)

// HookExecRecord is the persisted summary of a single hook execution within
// a pipeline run. One Record carries an ordered slice of these — covering
// both the request stage and the response stage in execution order — so
// operators reviewing a traffic_event row can see exactly which hooks fired,
// in which order, what each one decided, and how long it took. Values
// missing on a given hook (no error, no reason code) marshal omitted.
type HookExecRecord struct {
	Stage      string `json:"stage"` // "request" | "response" | "connection"
	Order      int    `json:"order"`
	HookID     string `json:"hookId"`
	Name       string `json:"name,omitempty"`
	Decision   string `json:"decision"`
	Reason     string `json:"reason,omitempty"`
	ReasonCode string `json:"reasonCode,omitempty"`
	// LatencyMs is the truncated integer-ms floor of LatencyUs (backward compat,
	// never clamped); LatencyUs is the precise microsecond wall-clock per hook.
	LatencyMs int    `json:"latencyMs"`
	LatencyUs int    `json:"latencyUs"`
	Error     string `json:"error,omitempty"`
}

// CacheStatus is the UNIFIED rollup recorded on traffic_event.cache_status.
// Two values: HIT (some cache layer saved cost) or MISS (no savings). Filter
// UIs bind to this. Derived at audit-write time from the gateway and provider
// internal statuses via DeriveCacheStatus. Empty string is the zero value
// (cache phase didn't run / request rejected before Phase 5.5).
//
// Internal breakdown lives in the four Record fields below
// (GatewayCacheStatus, GatewayCacheSkipReason, GatewayCacheKind,
// ProviderCacheStatus). The audit drawer reads those to render one of three
// human layouts; see cost-estimation-architecture.md § 6.4 for the
// derivation table and rendering rules.
type CacheStatus string

const (
	CacheStatusHit  CacheStatus = "HIT"
	CacheStatusMiss CacheStatus = "MISS"
)

// GatewayCacheStatus is the internal gateway-cache decision recorded on
// traffic_event.gateway_cache_status. Drill-down only — never exposed to
// filter UIs.
type GatewayCacheStatus string

const (
	GatewayCacheHit         GatewayCacheStatus = "hit"          // extract cache served; upstream not called
	GatewayCacheHitInflight GatewayCacheStatus = "hit_inflight" // joined a singleflight coalescer; leader's response replayed
	GatewayCacheMiss        GatewayCacheStatus = "miss"         // cache looked up; not found; upstream called
	GatewayCacheSkipped     GatewayCacheStatus = "skipped"      // cache layer not consulted; see GatewayCacheSkipReason
)

// GatewayCacheSkipReason is recorded on traffic_event.gateway_cache_skip_reason.
// Populated only when GatewayCacheStatus == GatewayCacheSkipped.
type GatewayCacheSkipReason string

const (
	GatewayCacheSkipReasonDisabled    GatewayCacheSkipReason = "disabled"    // cache module nil (config off)
	GatewayCacheSkipReasonNoCache     GatewayCacheSkipReason = "no_cache"    // client sent x-nexus-aigw-no-cache
	GatewayCacheSkipReasonPassthrough GatewayCacheSkipReason = "passthrough" // emergency passthrough.BypassCache active

	// GatewayCacheSkipReasonEmbeddingsEndpoint is stamped when the request
	// targets the embeddings endpoint (typology.EndpointKindEmbeddings). The
	// response cache (both L1 exact-match and the HIT_LIVE broker dedup) is
	// short-circuited at pre-lookup: each embedding input is unique per
	// workflow step and not user-session-bound, so caching it yields minimal
	// hit value while occupying Redis with single-use entries and diluting the
	// chat cache-hit dashboards. The L2 semantic tier already self-skips
	// embeddings; this reason makes the L1/broker skip explicit and visible in
	// gateway_cache_skip_reason. Pre-lookup short-circuit, peer to disabled /
	// no_cache / passthrough — NOT a semantic-cache failure mode.
	GatewayCacheSkipReasonEmbeddingsEndpoint GatewayCacheSkipReason = "embeddings_endpoint"

	// GatewayCacheSkipReasonTimeSensitive is stamped when the freshness
	// detector (cache/freshness) matches a Hub-pushed time-sensitive pattern
	// (configKey response_cache.time_sensitive_patterns). Both L1 and L2 tiers
	// skip — lookup AND write — so neither tier serves stale content nor
	// poisons future lookups.
	GatewayCacheSkipReasonTimeSensitive GatewayCacheSkipReason = "time_sensitive"

	// GatewayCacheSkipReasonAgenticToolUse is stamped when the request is an
	// AGENTIC conversation — it declares a tools array, or its transcript
	// already carries tool-role messages. The L2 semantic tier skips BOTH
	// lookup and write for these: semantic similarity is the wrong notion for
	// an agent loop, where consecutive requests are near-identical prefixes
	// that differ only by the latest tool result. Observed in production: a
	// similarity hit replayed the same failing tool call 10 times in 6
	// seconds (the cached response re-matched every retry), and a cross-model
	// hit served one model's answer to another. L1 exact-match is unaffected
	// (byte-identical requests may still replay).
	GatewayCacheSkipReasonAgenticToolUse GatewayCacheSkipReason = "agentic_tool_use"

	// GatewayCacheSkipReasonOversizeForEmbedding is stamped when the L2
	// semantic-cache input-staging plan (inputstaging.Plan) determines that
	// even the strategy-truncated embedding input exceeds the embedding model's
	// context window. L2 lookup and write are skipped; L1 (extract) is
	// unaffected. See response-cache-architecture.md §3.4.
	GatewayCacheSkipReasonOversizeForEmbedding GatewayCacheSkipReason = "oversize_for_embedding"

	// GatewayCacheSkipReasonNoEmbeddableText is stamped when the L2
	// input-staging step (buildEmbeddingInput) produces no text to embed —
	// the request carried no text content (image-only / tool-only turns) or
	// the staging plan yielded an empty selection. Distinct from
	// oversize_for_embedding, which means text existed but exceeded the
	// embedding model's context window. L2 lookup and write are skipped;
	// L1 (extract) is unaffected.
	GatewayCacheSkipReasonNoEmbeddableText GatewayCacheSkipReason = "no_embeddable_text"

	// GatewayCacheSkipReasonValkeyUnavailable is stamped when the Valkey
	// connection is unavailable for the L2 FT.SEARCH lookup. The gateway
	// falls through to the upstream broker. L1 uses a separate Redis
	// connection and is unaffected by this skip reason.
	GatewayCacheSkipReasonValkeyUnavailable GatewayCacheSkipReason = "valkey_unavailable"

	// GatewayCacheSkipReasonEmbeddingTimeout is stamped when the embedding
	// HTTP call exceeds the hard timeout (default 100ms per §3.11). All
	// singleflight joiners receive the timeout error and fall through to
	// the broker. No L2 write fires after the upstream response.
	GatewayCacheSkipReasonEmbeddingTimeout GatewayCacheSkipReason = "embedding_timeout"

	// GatewayCacheSkipReasonEmbeddingProviderError is stamped when the
	// embedding provider returns a non-timeout error (4xx, 5xx, network
	// failure). Distinct from timeout so ops can correlate with provider
	// health dashboards.
	GatewayCacheSkipReasonEmbeddingProviderError GatewayCacheSkipReason = "embedding_provider_error"

	// GatewayCacheSkipReasonEmbeddingDimMismatch is stamped when the
	// embedding response returns a vector whose dimension differs from
	// semantic_cache_config.embedding_dimension. Indicates a config drift
	// (admin changed model but L1 dimension was not updated) or a provider
	// API change. L2 is skipped until L1 is corrected.
	GatewayCacheSkipReasonEmbeddingDimMismatch GatewayCacheSkipReason = "embedding_dim_mismatch"

	// GatewayCacheSkipReasonSemanticSearchError is stamped when the
	// FT.SEARCH command against the Valkey vector index returns an
	// unexpected error (not a connection failure, which is valkey_unavailable).
	// Example: index was dropped between the lookup and the search.
	GatewayCacheSkipReasonSemanticSearchError GatewayCacheSkipReason = "semantic_search_error"

	// GatewayCacheSkipReasonSemanticSearchTimeout is stamped when FT.SEARCH
	// exceeds its hard timeout (default 20ms per §3.11). The request falls
	// through to the broker; L2 write still fires after the upstream response
	// (we have the embedding, so the write can proceed even if the read timed
	// out).
	GatewayCacheSkipReasonSemanticSearchTimeout GatewayCacheSkipReason = "semantic_search_timeout"

	// GatewayCacheSkipReasonSemanticUnavailable is stamped when the L2 layer
	// is administratively disabled (semantic_cache_config.enabled = false, the
	// fleet-wide kill switch).
	GatewayCacheSkipReasonSemanticUnavailable GatewayCacheSkipReason = "semantic_unavailable"

	// GatewayCacheSkipReasonEmbeddingCircuitOpen is stamped when the per-
	// (provider, model) circuit breaker is in the open state (10 consecutive
	// failures within 60s tripped the breaker). Every L2-eligible request
	// stamps this reason and falls through to the broker without firing an
	// embedding call. Latency overhead in this state: <1ms. See
	// response-cache-architecture.md for the circuit-breaker behaviour.
	GatewayCacheSkipReasonEmbeddingCircuitOpen GatewayCacheSkipReason = "embedding_circuit_open"

	// GatewayCacheSkipReasonPoisoned is stamped when a semantic cache hit is
	// rejected because the admin explicitly marked that entry as a bad hit via
	// the negative-feedback channel. The entry key was added to the
	// per-(vkScope) poison list with a TTL of 10× the original entry TTL.
	GatewayCacheSkipReasonPoisoned GatewayCacheSkipReason = "poisoned"
)

// GatewayCacheKind is recorded on traffic_event.gateway_cache_kind. Populated
// only when GatewayCacheStatus ∈ {hit, hit_inflight}. "extract" is the
// exact-match response cache. "semantic" is the KNN-similarity L2 cache;
// the write path stamps this value on L2 hits.
type GatewayCacheKind string

const (
	GatewayCacheKindExtract  GatewayCacheKind = "extract"
	GatewayCacheKindSemantic GatewayCacheKind = "semantic"
)

// ProviderCacheStatus is the upstream provider prompt-cache outcome recorded
// on traffic_event.provider_cache_status.
//
//   - hit  — provider response reported cache_read_tokens > 0
//   - miss — provider was called and model supports prompt cache, but this turn
//     had no read hit. Covers both no-read-tokens (cache_read_tokens = 0) and
//     first-turn cache writes (cache_creation_tokens > 0, cache_read_tokens nil/0).
//     Either non-nil cache field in the upstream usage is taken as proof of
//     model support.
//   - na   — provider not called (gateway hit / singleflight coalesce) OR model
//     doesn't support prompt cache (e.g. self-hosted vLLM). Disambiguate by
//     checking Record.CacheReadTokens AND Record.CacheCreationTokens (both NULL
//     = no call or no support; either populated = called with cache-capable model).
type ProviderCacheStatus string

const (
	ProviderCacheHit  ProviderCacheStatus = "hit"
	ProviderCacheMiss ProviderCacheStatus = "miss"
	ProviderCacheNA   ProviderCacheStatus = "na"
)

// ClassifyProviderCache derives ProviderCacheStatus from upstream usage cache
// token pointers. Single source of truth for the classification rule; all proxy
// stamping sites call into this so a future edit can't drift one site out of
// sync with the others (the bug fixed alongside this helper was exactly that
// shape — the cache-write branch was missing from the read-only switch).
//
//   - cache_read_tokens > 0                 → hit  (provider served from its cache)
//   - read or creation non-nil otherwise    → miss (provider called, model supports cache,
//     no read hit — covers first-turn cache writes where creation>0, read nil/0)
//   - both nil                              → na   (provider not called OR model unsupported)
func ClassifyProviderCache(readTokens, creationTokens *int) ProviderCacheStatus {
	switch {
	case readTokens != nil && *readTokens > 0:
		return ProviderCacheHit
	case readTokens != nil || creationTokens != nil:
		return ProviderCacheMiss
	default:
		return ProviderCacheNA
	}
}

// DeriveCacheStatus computes the unified cache_status from the two internal
// statuses per cost-estimation-architecture.md § 6.4:
//
//	HIT  iff  gateway ∈ {hit, hit_inflight}  OR  provider = "hit"
//	MISS otherwise
//
// Returns an error for invalid combos (gateway hit/hit_inflight paired with
// non-"na" provider — impossible because when gateway serves, no provider
// call happens). Empty inputs are valid and produce CacheStatusMiss unless
// at least one of the two is set.
func DeriveCacheStatus(gw GatewayCacheStatus, pv ProviderCacheStatus) (CacheStatus, error) {
	gatewayServed := gw == GatewayCacheHit || gw == GatewayCacheHitInflight
	if gatewayServed && pv != "" && pv != ProviderCacheNA {
		return "", fmt.Errorf("audit: invalid cache state combo: gateway=%q implies no provider call but provider=%q", gw, pv)
	}
	if gatewayServed || pv == ProviderCacheHit {
		return CacheStatusHit, nil
	}
	return CacheStatusMiss, nil
}

// unifiedCacheStatus returns the unified cache outcome for a record. If the
// producer already set Record.CacheStatus (e.g. ai-guard's classify cache
// hit), it's honored. Otherwise we derive from the gateway + provider
// internal statuses via DeriveCacheStatus. Invalid combos log a warning at
// the package-level slog default and produce "" (NULL in DB).
func unifiedCacheStatus(rec *Record) CacheStatus {
	if rec.CacheStatus != "" {
		return rec.CacheStatus
	}
	if rec.GatewayCacheStatus == "" && rec.ProviderCacheStatus == "" {
		return ""
	}
	derived, err := DeriveCacheStatus(rec.GatewayCacheStatus, rec.ProviderCacheStatus)
	if err != nil {
		slog.Warn("audit: invalid cache state combo; writing NULL cache_status",
			slog.String("gatewayCacheStatus", string(rec.GatewayCacheStatus)),
			slog.String("providerCacheStatus", string(rec.ProviderCacheStatus)),
			slog.String("err", err.Error()))
		return ""
	}
	return derived
}

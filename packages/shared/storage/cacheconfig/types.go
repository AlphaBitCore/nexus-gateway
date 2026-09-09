// Package cacheconfig defines the canonical Go type shapes for the
// two-tier prompt cache configuration.
//
// Tier 2: cache_adapter_config (one row per adapter_type) — family defaults
//
//	plus the normalisation rule override map.
//
// Tier 3: cache_provider_config (one row per Provider with any override).
//
// (Historical numbering: Tier 1 — the global singleton carrying the cache
// master kill switch and the normaliser gate — is retired; the
// cache_global_config table is orphaned and pending a later drop.)
//
// The types live in shared because three independent modules need them:
//   - control-plane: DB I/O via store.CacheConfig, handler validation, blob
//     assembly for Hub.NotifyConfigChange.
//   - ai-gateway: shadow payload deserialization, per-provider resolution,
//     geminicache.ManagerSet config source.
//   - configreconcile: drift detection between DB source-of-truth and
//     thing.desired.cache.
//
// All pointer fields use omitempty + nil semantics: a nil pointer means
// "key absent at this tier" (i.e. inherit from a lower tier or fall back
// to code default). The Resolve() function in resolve.go composes the
// three tiers into a final ProviderEffective with concrete values.
package cacheconfig

// AdapterConfig is the Tier-2 blob shape. Field set is per adapter family;
// fields irrelevant to a particular adapter remain nil-omitted.
type AdapterConfig struct {
	// Anthropic family (anthropic, bedrock).
	MarkerInjectEnabled *bool `json:"marker_inject_enabled,omitempty"`
	// MarkerBoundary3Enabled adds a SECOND explicit cache breakpoint, one turn
	// behind the automatic one, and is meaningful only while
	// MarkerInjectEnabled is on.
	//
	// Anthropic's automatic caching places its breakpoint on the last cacheable
	// block and finds the previous turn's entry by walking backward — but only
	// 20 blocks. A turn that appends more than that (an agent round with many
	// tool_use / tool_result blocks) pushes the previous write out of reach and
	// the conversation silently stops hitting. The provider's own guidance for
	// that case is a second breakpoint nearer the last write, which is what this
	// knob places.
	//
	// It is OFF by default. Its historical anchor — the second-to-last user
	// message — was measurably wrong, writing 2.8 tokens of cache for every
	// token it read on staging traffic; the anchor below is the end of the
	// previous assistant turn instead.
	MarkerBoundary3Enabled *bool `json:"marker_boundary3_enabled,omitempty"`

	// Gemini family (gemini, vertex).
	CacheEnabled            *bool `json:"cache_enabled,omitempty"`
	MinSystemChars          *int  `json:"min_system_chars,omitempty"`
	TTLSeconds              *int  `json:"ttl_seconds,omitempty"`
	CircuitBreakerThreshold *int  `json:"circuit_breaker_threshold,omitempty"`
	CircuitBreakerOpenSecs  *int  `json:"circuit_breaker_open_secs,omitempty"`

	// Rules carries per-rule_id override (Tier 2 only). Rule metadata
	// (adapter_type, regex, body_path, key_normalize_safe) is code-baked;
	// admin can only toggle Enabled / DryRunAlways.
	Rules map[string]RuleOverride `json:"rules,omitempty"`
}

// ProviderConfig is the Tier-3 override shape. Strict subset of AdapterConfig
// (no Rules — rules stay Tier 2 only; per-provider rule overrides are not supported).
type ProviderConfig struct {
	MarkerInjectEnabled     *bool `json:"marker_inject_enabled,omitempty"`
	MarkerBoundary3Enabled  *bool `json:"marker_boundary3_enabled,omitempty"`
	CacheEnabled            *bool `json:"cache_enabled,omitempty"`
	MinSystemChars          *int  `json:"min_system_chars,omitempty"`
	TTLSeconds              *int  `json:"ttl_seconds,omitempty"`
	CircuitBreakerThreshold *int  `json:"circuit_breaker_threshold,omitempty"`
	CircuitBreakerOpenSecs  *int  `json:"circuit_breaker_open_secs,omitempty"`
}

// RuleOverride is the per-rule_id admin-settable override. Nil pointer =
// inherit code-baked default (from the rule's EnabledByDefault).
type RuleOverride struct {
	Enabled      *bool `json:"enabled,omitempty"`
	DryRunAlways *bool `json:"dry_run_always,omitempty"`
}

// CacheConfigBlob is the payload that flows over the Hub shadow key
// `cache` to the AI Gateway. The same shape is also what
// store.CacheConfig.AssembleBlob() returns when CP needs to push.
type CacheConfigBlob struct {
	Adapters  map[string]AdapterConfig  `json:"adapters"`  // keyed by adapter_type
	Providers map[string]ProviderConfig `json:"providers"` // keyed by provider_id; absent entry == no override
}

// ProviderEffective is the resolved cache config for one Provider — used by
// the AI Gateway hot path. All scalar fields are concrete (non-pointer);
// each field's Source companion tag indicates which tier supplied its value.
type ProviderEffective struct {
	ProviderID  string
	AdapterType string

	// Resolved knobs.
	MarkerInjectEnabled     bool
	MarkerBoundary3Enabled  bool
	CacheEnabled            bool
	MinSystemChars          int
	TTLSeconds              int
	CircuitBreakerThreshold int
	CircuitBreakerOpenSecs  int

	// Per-knob source attribution. Keyed by field name (JSON key in
	// AdapterConfig / ProviderConfig / GlobalConfig).
	Sources map[string]Source

	// RuleOverrides for this provider's adapter (Tier 2; copied through verbatim).
	RuleOverrides map[string]RuleOverride
}

// Source labels which tier supplied a given knob's effective value.
type Source string

const (
	SourceProviderOverride Source = "provider-override"
	SourceAdapterDefault   Source = "adapter-default"
	SourceCodeDefault      Source = "code-default"
)

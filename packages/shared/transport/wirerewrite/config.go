// Package wirerewrite is the byte-level wire rewriter that runs just before
// the upstream send (and again just before Nexus L1 cache key hashing).
// Distinct from `packages/shared/transport/normalize`, which is the canonical
// request/response shape framework — different concern entirely.
//
// It exposes two entry points:
//   - NormalizeKey: strips key-safe volatile fields so equivalent requests
//     hash to the same Nexus L1 cache key. Runs AFTER PrepareBody, BEFORE
//     Cache.BuildKey. Always fail-open.
//   - NormalizeUpstream: strips and/or injects bytes in the body sent to
//     the upstream provider. Runs AFTER L1 MISS, BEFORE runViaBroker.
//     Gated by the global `normaliser_enabled` config switch.
//
// Both functions are called with the adapter-wire body (PrepareBody output).
//
// Wire-format identifiers are stable interfaces: the JSON tag
// `normaliser_enabled` and rule IDs like `cache-normaliser` are admin /
// shadow / DB identifiers, preserved verbatim even though the Go package
// name has changed. Renaming them would require a coordinated config
// migration.
package wirerewrite

import (
	"regexp"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// AdapterType is the provider wire format identifier (e.g. "anthropic", "openai").
// Matches provider.adapter_type in the database.
type AdapterType = string

// Known adapter type values — must match providers.Format constants.
const (
	AdapterAnthropic   = "anthropic"
	AdapterOpenAI      = "openai"
	AdapterBedrock     = "bedrock"
	AdapterAzureOpenAI = "azure-openai"
	AdapterDeepSeek    = "deepseek"
	AdapterGLM         = "glm"
	AdapterMoonshot    = "moonshot"
	AdapterMistral     = "mistral"
	AdapterXai         = "xai"
	AdapterGroq        = "groq"
	AdapterPerplexity  = "perplexity"
	AdapterTogether    = "together"
	AdapterFireworks   = "fireworks"
	AdapterMiniMax     = "minimax"
)

// RuleType identifies the transformation a Rule applies.
type RuleType string

const (
	RuleTypeStrip              RuleType = "strip"
	RuleTypeFieldOrder         RuleType = "field_order_normalize"
	RuleTypeCacheControlInject RuleType = "cache_control_inject"
)

// Rule describes a single normalisation rule.
type Rule struct {
	// ID is the canonical identifier, e.g. "claude-code-cch-strip".
	ID string
	// AdapterType scopes the rule to a specific provider wire format.
	AdapterType AdapterType
	// Type determines which transformation is applied.
	Type RuleType
	// Enabled is the runtime toggle; defaults to EnabledByDefault when no
	// config override is present.
	Enabled bool
	// EnabledByDefault is the factory default before any config override.
	EnabledByDefault bool
	// DryRunAlways records metrics without modifying bytes. Used for
	// safe-trial rollout of new rules.
	DryRunAlways bool
	// KeyNormalizeSafe marks the rule as safe to apply during L0 key
	// normalisation (NormalizeKey). Rules with KeyNormalizeSafe=false are
	// only applied in NormalizeUpstream.
	KeyNormalizeSafe bool

	// strip-rule fields
	BodyPath string         // gjson path selector applied before the regex
	Regex    *regexp.Regexp // compiled pattern to remove from matched values
}

// Config is the runtime configuration projected from the `cache` config-key
// blob (configkey.Cache) that the AI Gateway watches on its shadow. The zero
// value is a safe default (all off).
type Config struct {
	// NormaliserEnabled gates NormalizeUpstream (L3). NormalizeKey is
	// always active regardless of this switch.
	NormaliserEnabled bool `json:"normaliser_enabled"`
	// Rules maps adapter_type → (rule_id → RuleOverride).
	Rules map[string]map[string]RuleOverride `json:"rules,omitempty"`
	// Providers carries per-Provider cache settings keyed by Provider UUID.
	Providers map[string]ProviderCacheConfig `json:"providers,omitempty"`
	// Global holds platform-wide cache settings.
	Global GlobalCacheConfig `json:"global,omitempty"`
}

// RuleOverride carries the operator-configurable per-rule toggles.
type RuleOverride struct {
	Enabled      *bool `json:"enabled,omitempty"`
	DryRunAlways *bool `json:"dry_run_always,omitempty"`
}

// ProviderCacheConfig holds per-Provider marker injection settings.
type ProviderCacheConfig struct {
	// CacheMarkerInjectEnabled enables L4 cache_control injection for
	// Anthropic-wire requests routed to this Provider.
	CacheMarkerInjectEnabled bool `json:"cache_marker_inject_enabled"`
	// CacheMarkerBoundary3Enabled enables the conversation-history boundary
	// (messages[-2]) when CacheMarkerInjectEnabled is true.
	CacheMarkerBoundary3Enabled bool `json:"cache_marker_boundary3_enabled"`
}

// GlobalCacheConfig holds platform-wide settings.
type GlobalCacheConfig struct {
	// ExtendedTTLEnabled is retained for JSON deserialization compatibility
	// but no longer affects injection behavior — all markers use "ephemeral".
	ExtendedTTLEnabled bool `json:"extended_ttl_enabled"`
}

// Result is the normalisation outcome returned by NormalizeUpstream.
type Result struct {
	StripCount      int
	StripBytes      int
	MarkersInjected int
	// DryRun is true when all active rules ran in dry-run mode and the
	// returned body equals the input body.
	DryRun bool

	// TransformSpans is the byte-level audit record of every strip /
	// inject this engine performed. Source values:
	//   cache-normaliser     — strips that removed bytes from the
	//                          upstream-bound body (L3).
	//   cache-control-inject — cache_control markers added (L4).
	//   cache-key-strip      — L0 strips that affect only the cache key.
	// Spans are consumed in-process (cache-key derivation, strip metrics);
	// they are not persisted to traffic_event_normalized.
	TransformSpans []normalize.TransformSpan
}

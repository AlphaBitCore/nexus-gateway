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
//     Demand-driven: it no-ops unless an enabled strip rule or a
//     marker-injecting Provider is configured (there is no global switch).
//
// Both functions are called with the adapter-wire body (PrepareBody output).
//
// Wire-format identifiers are stable interfaces: rule IDs like
// `cache-normaliser` are admin / shadow / DB identifiers, preserved verbatim
// even though the Go package name has changed. Renaming them would require a
// coordinated config migration.
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
	RuleTypeStrip RuleType = "strip"
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

	// strip-rule fields.
	//
	// BodyPaths lists every gjson selector the regex is applied at. A wire
	// field can legitimately arrive in more than one JSON shape, and a rule
	// that names only one of them is silently absent on the others: Anthropic's
	// `system` is an ARRAY of content blocks when a native client sends it, and
	// a plain STRING when this gateway's own codec rebuilds it on the
	// cross-format leg. A rule declaring only "system.#.text" therefore worked
	// for a /v1/messages caller and did nothing at all for the same body
	// arriving through /v1/chat/completions.
	//
	// The selectors are applied in order and are expected to be mutually
	// exclusive — each no-ops on a shape it does not match — so listing both
	// costs one failed gjson lookup, not a double strip.
	BodyPaths []string       // gjson path selectors applied before the regex
	Regex     *regexp.Regexp // compiled pattern to remove from matched values
}

// Config is the runtime configuration projected from the `cache` config-key
// blob (configkey.Cache) that the AI Gateway watches on its shadow. The zero
// value is a safe default (all off).
type Config struct {
	// The upstream rewrite (L3 strip) is demand-driven: the engine derives a
	// hasWork gate at Reload from the resolved rules below. There is no global
	// on/off knob — enabling a rule IS the demand. NormalizeKey (L0 cache-key)
	// is always active regardless.
	//
	// The same blob also carries per-provider prompt-cache settings. Those are
	// NOT projected here: the marker they control is written by the codec that
	// owns the wire carrying it, which reads the live blob per request. This
	// engine holds no per-provider state at all, which is what makes it immune
	// to the order the config loader happens to apply shadow keys in.

	// Rules maps adapter_type → (rule_id → RuleOverride).
	Rules map[string]map[string]RuleOverride `json:"rules,omitempty"`
}

// RuleOverride carries the operator-configurable per-rule toggles.
type RuleOverride struct {
	Enabled      *bool `json:"enabled,omitempty"`
	DryRunAlways *bool `json:"dry_run_always,omitempty"`
}

// Result is the normalisation outcome returned by NormalizeUpstream.
type Result struct {
	StripCount int
	StripBytes int
	// DryRun is true when every active rule ran in dry-run mode, which means
	// the returned body equals the input body and StripCount / StripBytes
	// describe what WOULD have been removed. An audit row that carries the
	// counts without this bit cannot tell a measurement from an edit.
	DryRun bool

	// TransformSpans is the byte-level audit record of every strip /
	// inject this engine performed. Source values:
	//   cache-normaliser     — strips that removed bytes from the
	//                          upstream-bound body (L3).
	//   cache-key-strip      — L0 strips that affect only the cache key.
	// Spans are consumed in-process (cache-key derivation, strip metrics);
	// they are not persisted to traffic_event_normalized.
	TransformSpans []normalize.TransformSpan
}

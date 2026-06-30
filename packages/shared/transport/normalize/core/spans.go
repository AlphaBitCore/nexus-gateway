// TransformSpan and its source taxonomy: the audit record of every byte
// region a Nexus subsystem modified in a captured body — hook redactions,
// AI-guard suggestions, cache normalisation — carried beside the
// normalized payload so viewers can mark each change inline.
package core

// TransformSource identifies the subsystem that produced a TransformSpan.
// Used for audit grouping ("show me everything cache_normaliser changed
// this week") and policy filtering (compliance officer reviewing only
// hook-attributable changes).
type TransformSource string

const (
	// SourceHook — content-touching hook redact (pii-detector / keyword-
	// filter / content-safety / rulepack-engine), attributed to a rule.
	SourceHook TransformSource = "hook"
	// SourceAIGuard — LLM-as-judge classifier suggested a redact span.
	// Distinguished from SourceHook so operators can audit AI-driven
	// modifications separately.
	SourceAIGuard TransformSource = "aiguard"
	// SourceCacheNormaliser — prompt-cache normaliser stripped volatile
	// bytes from the request body before sending upstream (helps the
	// provider's prompt-cache hit rate).
	SourceCacheNormaliser TransformSource = "cache-normaliser"
	// SourceCacheControlInject — cache_control marker injection
	// (Nexus added markers to direct the provider's prompt cache).
	SourceCacheControlInject TransformSource = "cache-control-inject"
	// SourceCacheKeyStrip — Nexus L1 cache-key normalisation removed
	// volatile bytes for the cache key computation only; upstream
	// body unaffected. Recorded for audit completeness.
	SourceCacheKeyStrip TransformSource = "cache-key-strip"
)

// TransformAction classifies what kind of edit a TransformSpan made.
type TransformAction string

const (
	ActionRedact  TransformAction = "redact"  // sensitive content replaced
	ActionStrip   TransformAction = "strip"   // volatile bytes removed
	ActionInject  TransformAction = "inject"  // bytes added
	ActionReplace TransformAction = "replace" // generic substitution
)

// TransformSpan describes one byte-level modification on a
// NormalizedPayload. Spans canonicalize every modification a Nexus
// subsystem made between the client and the upstream: hook redactions,
// AI-Guard suggestions, cache-normaliser strips, cache_control inject.
// A single span set drives both inflight rewrite (TrafficAdapter
// applies them to the upstream-bound body) and storage rewrite (the
// audit-log copy stored in traffic_event_normalized).
//
// Reconstructing the wire-level body from the audit log:
//
//	upstream_body = ApplySpans(request_normalized, request_transform_spans)
//	client_body   = ApplySpans(response_normalized, response_transform_spans)
//
// ContentAddress encodes the addressed content block:
//
//   - AI kinds: "messages.<i>.content.<j>" (e.g. "messages.0.content.1")
//   - HTTP kinds: "http.bodyView" (whole body) or "http.bodyView.form.<key>"
//
// Start / End are UTF-8 byte offsets into the addressed content's text.
// For ActionInject, Start == End and Replacement holds the injected bytes.
type TransformSpan struct {
	Source         TransformSource `json:"source"`
	SourceID       string          `json:"sourceId,omitempty"` // rule ID, hook ID, normaliser rule ID, or ""
	Action         TransformAction `json:"action"`
	ContentAddress string          `json:"contentAddress"`
	Start          int             `json:"start"`
	End            int             `json:"end"`
	Replacement    string          `json:"replacement,omitempty"`
	Reason         string          `json:"reason,omitempty"`
}

// RedactionSpan is retained as an alias for backward semantic clarity
// in narrow APIs (hook results), but new code should use TransformSpan
// directly so non-redact sources (cache normaliser, cache_control
// inject) flow through the same audit channel.
type RedactionSpan = TransformSpan

// AddressAuditOnlySentinel is the ContentAddress a span carries when it
// records WHAT was flagged but cannot be applied to any payload — the
// webhook-forward hook emits it because the webhook reports wire offsets
// against a flat body the gateway never reconstructs. ApplySpans drops
// such a span (its address resolves to no content block), so it lands in
// the audit row only, never mutating delivered or stored bytes.
//
// It is the single source of truth for that sentinel: the webhook producer
// and IsAuditOnlySentinelAddress both reference this const so they cannot
// drift. A future audit-only span family adds its sentinel here.
const AddressAuditOnlySentinel = "webhook.flat"

// IsAuditOnlySentinelAddress reports whether a ContentAddress is a known
// audit-only sentinel — a span that records a flag for the audit log but
// is NOT applicable to delivered/stored bytes.
//
// Callers deciding "does this result carry an APPLICABLE redaction"
// (decision.CompliancePipelineResult via the pipeline aggregator) use the
// NEGATION of this as a deliberate DENYLIST, not an allowlist of resolvable
// address grammars: an unknown address is treated as applicable (fail-safe
// over-block on a dormant path), whereas an allowlist would mis-classify a
// future resolvable address family as inapplicable and risk a leak. This is
// NOT a payload-aware resolvability check — a malformed/empty address also
// fails ApplySpans yet is not a sentinel; the predicate answers only "is
// this a recognised audit-only marker".
func IsAuditOnlySentinelAddress(addr string) bool {
	return addr == AddressAuditOnlySentinel
}

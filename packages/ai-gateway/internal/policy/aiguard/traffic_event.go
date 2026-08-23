// packages/ai-gateway/internal/policy/aiguard/traffic_event.go
package aiguard

import "context"

// TrafficEvent is the internal audit record handed to the sink for every
// classify attempt (success, cache hit, or failure). The HTTP handler and
// in-process caller both funnel through classifyImpl, so this is the single
// emission point for ai-guard traffic events.
type TrafficEvent struct {
	DetectorType    string
	Decision        string
	JudgeLatencyMs  int
	CacheHit        bool
	BackendMode     string
	InternalPurpose string // always "ai-guard"
	ErrorDetail     string // non-empty on failure

	// TraceID carries the triggering user request's correlation id
	// (the inbound X-Nexus-Request-Id propagated on ctx). It is stamped
	// onto the ai-guard row's trace_id so the classifier's own cost row
	// (internal_purpose='ai-guard', fresh row id) can be joined back to the
	// user-traffic row that invoked the hook. Empty for ad-hoc callers
	// (tests, tooling) that never set a request id on the context.
	TraceID string

	// Stamped from Response.Metadata after a successful classifier call;
	// left zero on CacheHit, failures, or when AdapterBackend has no
	// PriceLookup wired. Sink writes these to traffic_event.{prompt_tokens,
	// completion_tokens, ai_guard_cost_usd}.
	PromptTokens     int
	CompletionTokens int
	CostUsd          float64

	// CacheReadTokens / CacheCreationTokens are the provider-cached share of
	// PromptTokens (the total input). Carried so the classifier's own row can
	// record WHY its cost is what it is: without the split, an ai-guard row
	// showing a large prompt and a small charge is unauditable, which is
	// exactly the gap that made the pre-fix over-estimate undetectable in the
	// stored data and unrecoverable in hindsight. Sink writes them to
	// traffic_event.{cache_read_tokens, cache_creation_tokens}.
	CacheReadTokens     int
	CacheCreationTokens int

	// ProviderID / ProviderName identify the provider that served the
	// classifier call, copied from Response.Metadata (itself stamped by
	// AdapterBackend from the resolved call target). Set on the cache-hit
	// and success paths, where a backend response — live or cached — is
	// available; left empty on the early-return failure paths (prompt
	// render failure, backend call error), which never reached a backend
	// and therefore never incurred a cost to attribute. The sink maps
	// ProviderID onto traffic_event.routed_provider_id so the classifier's
	// cost reaches the rollup's per-provider dimension.
	ProviderID   string
	ProviderName string
}

// TrafficSink is the minimal audit interface classifyImpl requires. The
// production implementation bridges into the existing traffic_event MQ
// pipeline; tests capture events in-memory.
type TrafficSink interface {
	Emit(ctx context.Context, e TrafficEvent)
}

// emit is a nil-safe helper; a nil sink is tolerated so ad-hoc callers
// (tests, tooling) don't need a no-op stub.
func emit(ctx context.Context, sink TrafficSink, e TrafficEvent) {
	if sink == nil {
		return
	}
	sink.Emit(ctx, e)
}

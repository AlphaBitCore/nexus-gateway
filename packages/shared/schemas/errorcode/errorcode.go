// Package errorcode is the single source of truth for the values carried in
// traffic_event.error_code — the vocabulary the AI Gateway writes and the Hub's
// alert aggregators read.
//
// It exists because that coupling runs through a database column. The gateway
// stamps a string; the Hub selects on it. Neither side can see the other's
// literal, so a drifted value produces no compile error and no test failure —
// it produces an alert that silently stops firing. That is not hypothetical:
// provider_upstream_error selected on `error_code == ""` and had never fired in
// production, because the gateway classifies every upstream failure and the
// condition was unreachable.
//
// Same shape as schemas/credstate, which owns the circuit-breaker vocabulary
// for the same reason and the same three readers.
//
// # Two vocabularies, deliberately
//
// traffic_event.error_code carries values from two sets, and they answer
// different questions:
//
//   - The canonical codes below — the UPSTREAM rejected the request, and why.
//     The gateway's provider adapters normalise every native error envelope
//     onto these (see providers/core: ProviderError.Code).
//   - UPPER_SNAKE codes (ROUTING_NO_MATCH, QUOTA_EXCEEDED, MODEL_NOT_ALLOWED,
//     CLIENT_CLOSED, AUTH_INVALID_KEY, …) — the GATEWAY rejected or could not
//     place the request, and why. It never reached an upstream.
//
// Do not merge them. A consumer that needs "was this the upstream's fault"
// should ask [IsUpstream] rather than hardcode either list.
package errorcode

// The canonical provider error codes. These mirror the ProviderError.Code
// constants in the AI Gateway's providers/core package, which is where the
// adapters produce them; the values — not the identifiers — are the contract.
//
// They are duplicated rather than moved because providers/core.Code* is bound
// to the ProviderError type and the adapter layer that constructs it; relocating
// that type to shared/ would drag the adapter surface with it. The wire values
// are pinned by tests on both sides instead.
const (
	// InvalidRequest: the upstream rejected the request shape or parameters.
	InvalidRequest = "invalid_request"
	// AuthFailed: the upstream rejected our credential (401/403).
	AuthFailed = "auth_failed"
	// RateLimited: the upstream throttled us (429).
	RateLimited = "rate_limited"
	// ProviderQuotaExhausted: the upstream ACCOUNT's budget is spent. Distinct
	// from RateLimited, which clears in seconds: this clears when the billing
	// window resets or the customer raises the limit, and it is account-scoped
	// rather than per-request, so every model behind that provider is equally
	// unusable until then.
	ProviderQuotaExhausted = "provider_quota_exhausted"
	// Timeout: the upstream did not answer within the request budget.
	Timeout = "timeout"
	// UpstreamError: the upstream failed in a way that carries no more specific
	// code — a 5xx, or a transport failure that never produced an envelope.
	UpstreamError = "upstream_error"
	// EndpointUnsupported: the upstream does not serve this endpoint.
	EndpointUnsupported = "endpoint_unsupported"
	// ContextOverflow: the prompt exceeds the model's context window.
	ContextOverflow = "context_overflow"
	// NotImplemented: the adapter has no implementation for this call.
	NotImplemented = "not_implemented"
	// NoCompatibleProvider: no target could serve the request. NOTE this is a
	// GATEWAY decision despite living in the canonical set — no upstream was
	// ever asked, so [IsUpstream] deliberately returns false for it.
	NoCompatibleProvider = "no_compatible_provider"
)

// upstreamCodes is the subset of the canonical codes that means an upstream
// actually answered and rejected us. NoCompatibleProvider is excluded on
// purpose: it means we never reached one.
var upstreamCodes = map[string]struct{}{
	InvalidRequest:         {},
	AuthFailed:             {},
	RateLimited:            {},
	ProviderQuotaExhausted: {},
	Timeout:                {},
	UpstreamError:          {},
	EndpointUnsupported:    {},
	ContextOverflow:        {},
	NotImplemented:         {},
}

// IsUpstream reports whether a traffic_event.error_code value means the upstream
// rejected the request, as opposed to the gateway rejecting it before or instead
// of calling one.
//
// This is the question the alert rules actually need. They used to approximate
// it with `error_code == ""`, on the contract that the gateway left upstream
// failures unclassified — a contract the gateway does not honour, which left
// those rules unable to fire. Ask this instead: it stays correct as codes are
// added, and an empty code (a producer that forgot to classify) reads as false
// rather than silently counting as an upstream failure.
func IsUpstream(code string) bool {
	_, ok := upstreamCodes[code]
	return ok
}

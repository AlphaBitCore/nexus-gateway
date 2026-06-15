// Package thingtype is the single source of truth for the well-known
// `thing.type` values the platform recognizes. Every package that emits
// or matches a thing-type string should import this package instead of
// hardcoding a literal — a typo or stray "agent-desktop" / "agent-mobile"
// silently orphans the row from configreconcile, IAM gates, ops rollup
// jobs, and admin overrides.
//
// The constant set is closed: adding a new platform Thing requires a
// deliberate change here, which forces a sweep of all consumers (the
// `Known` predicate gives Boot-time audits a single API to call).
package thingtype

// Canonical thing-type strings. Stored in DB `thing.type` and matched
// against by every place that filters / dispatches on Thing identity.
const (
	// Agent — end-user desktop agent (macOS / Linux / Windows). Singular
	// "agent" regardless of platform OS; per-OS variants belong in
	// staticInfo.os, NOT in thing.type. Hub enrollment_handler defaults
	// missing ThingType to this value.
	Agent = "agent"

	// AIGateway — `/v1/*` AI traffic surface.
	AIGateway = "ai-gateway"

	// ComplianceProxy — TLS bump intercept proxy on port 3128.
	ComplianceProxy = "compliance-proxy"

	// ControlPlane — admin BFF + SPA.
	ControlPlane = "control-plane"

	// NexusHub — platform ops center (this very binary, when it self-
	// registers as a Thing for its own dashboards).
	NexusHub = "nexus-hub"
)

// known is the closed set of legal thing.type strings. Adding a Thing
// kind to the platform requires updating this set + the const block.
//
// Unexported on purpose: external callers should not mutate the policy.
// Use IsKnown to query.
var known = map[string]bool{
	Agent:           true,
	AIGateway:       true,
	ComplianceProxy: true,
	ControlPlane:    true,
	NexusHub:        true,
}

// IsKnown returns true when t matches one of the canonical thing types.
// Designed for CP / Hub startup audits that scan DB rows and warn on
// orphan rows whose type has drifted out of the closed set. Empty
// strings return false (treat as malformed).
func IsKnown(t string) bool {
	return known[t]
}

// IsBackendService reports whether t is one of the internal platform service
// thing types (ai-gateway / compliance-proxy / control-plane / nexus-hub) — i.e.
// a Thing that authenticates to Hub with the shared INTERNAL_SERVICE_TOKEN rather
// than a per-device agent token. Agents and unknown/empty types return false.
//
// Used by the internal-things API to deny a service-token caller from
// acting as an AGENT Thing: an honest backend service only ever self-operates on
// its own service-type row, so a service-token request that targets an agent (or
// any non-service type) is an impersonation attempt and is refused.
func IsBackendService(t string) bool {
	return known[t] && t != Agent
}

// All returns a fresh slice of the legal thing-type strings, suitable
// for SQL IN-list construction or operator dashboards. Mutating the
// returned slice does not affect the policy.
func All() []string {
	out := make([]string, 0, len(known))
	for k := range known {
		out = append(out, k)
	}
	return out
}

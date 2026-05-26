package proxy

import (
	"net/http"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// AgentMarker holds the per-request state needed to inject the canonical
// X-Nexus-* response headers into an upstream HTTP response forwarded to
// the client. Built from the RequestInspector result so that flow ID and
// the hook pipeline outcome are stamped onto the response.
//
// All fields are optional: injectInto falls back gracefully when they are
// empty (e.g. no hook pipeline ran or inspector was nil).
type AgentMarker struct {
	// FlowID is the agent flow correlation ID. Reflected onto the response
	// as X-Nexus-Request-Id (the single canonical correlation header for
	// both request and response directions); this is the value the agent
	// also stamps on the outbound upstream request so downstream services
	// log it under the same key.
	FlowID string
	// HookOutcome is the aggregated hook pipeline result from the request
	// inspector. Formatted via traffic.FormatHookOutcome and prepended to
	// the X-Nexus-Hook chain via traffic.PrependChain so multi-hop
	// alignment with X-Nexus-Via is preserved.
	HookOutcome traffic.HookOutcomeInput
}

// injectInto stamps the canonical X-Nexus-* marker set onto h. Called
// inside MITMRelay's response path before serializeResponseHead serializes
// the headers, so the injected headers flow to the client.
//
// Headers set (per nexus-response-markers.md):
//   - X-Nexus-Via — prepended with "agent" (existing chain preserved)
//   - X-Nexus-Mode — prepended with "mitm" via PrependChain so 1:1 with via
//   - X-Nexus-Hook — prepended via PrependChain with FormatHookOutcome
//   - X-Nexus-Request-Id — reflected from the agent flow ID when non-empty
//
// Access-Control-Expose-Headers is extended (not replaced) so upstream
// CORS headers are preserved.
func (m *AgentMarker) injectInto(h http.Header) {
	traffic.PrependVia(h, "agent")
	traffic.PrependChain(h, "X-Nexus-Mode", "mitm")
	traffic.PrependChain(h, "X-Nexus-Hook", traffic.FormatHookOutcome(m.HookOutcome))
	if m.FlowID != "" {
		h.Set("X-Nexus-Request-Id", m.FlowID)
	}
	traffic.MergeExposeHeaders(h,
		traffic.HeaderVia,
		"X-Nexus-Mode",
		"X-Nexus-Hook",
		"X-Nexus-Request-Id",
	)
}

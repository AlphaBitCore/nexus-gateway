package core

import "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"

// EndpointTypeFromPath maps a path-segment string (e.g. "chat/completions",
// "embeddings") to the canonical EndpointType used for hook pipeline filtering.
//
// E87-S2 (2026-05-25): delegates to typology.KindFromPathSegment — the
// single source of truth shared with ai-gateway/internal/platform/audit.
// EndpointTypeFromPath. Adding a new segment requires updating only the
// typology table; this helper stays unchanged.
//
// Returns an empty string for unknown paths so callers that have not yet
// classified the endpoint continue to pass all hooks.
func EndpointTypeFromPath(s string) EndpointType {
	return typology.KindFromPathSegment(s)
}

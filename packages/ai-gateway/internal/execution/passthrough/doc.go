// Package passthrough is the AI Gateway's runtime kill-switch system.
//
// When a layer of the L4 policy plane (hooks, cache, normalize) regresses
// on a specific provider, an operator can flip the gateway into a
// "passthrough" mode for that provider — selectively bypassing one or
// more layers while keeping the rest of the fleet on the full
// compliance / normalization / smart-routing path.
//
// # Config shape
//
// Each effective config carries three independent toggles:
//
//   - BypassHooks: skip request hooks + response hooks + SSE live compliance.
//   - BypassCache: skip cache lookup + cache write for matched traffic.
//   - BypassNormalize: skip emission of the response-side normalized
//     projection to traffic_event_normalized. It does NOT change request
//     normalization or cache-key derivation — PrepareBody + the cache-key
//     NormalizeKey step run unconditionally in proxy.go regardless of this
//     flag. (The admin API couples it with BypassCache as a product rule,
//     not because the cache key depends on it.)
//
// The toggles are guarded by a master Enabled flag and a mandatory
// ExpiresAt (≤ 8h). When Enabled is false or the effective ExpiresAt is
// past, all three bypass flags are inert.
//
// # 3-tier resolution
//
// Operators configure passthrough at three scopes:
//
//   - global (singleton, all traffic)
//   - per adapter_type (e.g. "anthropic" — all anthropic-shape providers)
//   - per provider_id (a single Provider row)
//
// The handler resolves the effective config for the routed target's
// provider_id by JSONB-merging the three tiers (provider > adapter > global).
// The merge logic lives in the Cache type.
//
// # Fail-closed cold-start
//
// A nil *Config behaves as the empty Config — AnyBypassActive() returns
// false, Flags() returns nil. The gateway boots with an empty cache;
// until Hub pushes the effective state, every effective lookup yields
// nil, and every request runs the full compliance path. A brief
// over-restriction window at boot is preferable to a brief silent bypass.
//
// # Audit trail
//
// Every traffic_event row that fires any bypass records the canonical
// flag set on traffic_event.passthrough_flags. The canonical order from
// Flags() is [bypassHooks, bypassCache, bypassNormalize] — operators
// grep / SQL filter on these literals.
package passthrough

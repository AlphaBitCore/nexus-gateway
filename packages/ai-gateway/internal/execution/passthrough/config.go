package passthrough

import "time"

// Config is the effective passthrough configuration for a single
// (provider_id, request) pair, resolved by the runtime Cache by merging
// the three configuration tiers (global / adapter / provider).
//
// Treat as read-only after construction. The runtime cache returns
// shared *Config pointers via an atomic.Pointer; mutating one would
// race with other requests using the same snapshot.
type Config struct {
	// Enabled is the master switch. When false, AnyBypassActive returns
	// false regardless of the individual flag values. The runtime cache
	// only emits Configs whose effective enabled state is true; downstream
	// readers should not see Enabled=false here, but the field is
	// preserved for telemetry / debugging.
	Enabled bool

	// BypassHooks skips the request + response hooks pipelines (including
	// SSE live compliance).
	BypassHooks bool

	// BypassCache skips cache lookup + cache write.
	BypassCache bool

	// BypassNormalize skips normalize.Registry.Normalize and response-side
	// normalize emission to traffic_event_normalized. Implies BypassCache
	// (enforced at the admin API + UI layer since the cache key derives
	// from the canonical normalized payload).
	BypassNormalize bool

	// ExpiresAt is the effective expiry (tightest active tier wins).
	// Zero value when Enabled is false.
	ExpiresAt time.Time

	// EnabledBy is the NexusUser.id of the operator who flipped the
	// active tier on. Surfaces in audit traffic_event.passthrough_reason
	// for incident correlation.
	EnabledBy string

	// Reason is the free-form justification from the active tier.
	// Mandatory ≥ 20 chars per DB CHECK + admin API validation.
	// Surfaces in audit traffic_event.passthrough_reason.
	Reason string
}

// flagKinds defines the canonical ordering of bypass-kind strings.
// Used by Flags() so the audit log + UI badges sort consistently
// regardless of which tier set which flag.
var flagKinds = []struct {
	name string
	pick func(c *Config) bool
}{
	{"bypassHooks", func(c *Config) bool { return c.BypassHooks }},
	{"bypassCache", func(c *Config) bool { return c.BypassCache }},
	{"bypassNormalize", func(c *Config) bool { return c.BypassNormalize }},
}

// AnyBypassActive reports whether the request should skip at least one
// L4 layer for this Config. False on a nil receiver, on a disabled
// Config, or when all three bypass flags are false.
func (c *Config) AnyBypassActive() bool {
	if c == nil || !c.Enabled {
		return false
	}
	return c.BypassHooks || c.BypassCache || c.BypassNormalize
}

// Flags returns the canonical ordered slice of bypass-kind strings
// that fired for this Config. Returns nil when no bypass is active
// (covers nil receiver, disabled Config, all-flags-false). The slice
// lands directly in traffic_event.passthrough_flags (TEXT[]).
func (c *Config) Flags() []string {
	if !c.AnyBypassActive() {
		return nil
	}
	out := make([]string, 0, len(flagKinds))
	for _, fk := range flagKinds {
		if fk.pick(c) {
			out = append(out, fk.name)
		}
	}
	return out
}

package policy

// nonOverridableConfigKeys lists per-Thing-override-forbidden config keys.
// The CP admin handler MUST reject 400 BadRequest on attempts to set an
// override for any key in this set.
//
// credentials: provider credentials are governed centrally; per-Thing
// divergence multiplies leak surface and breaks rotation semantics.
// virtual_keys: VK is tenant identity / billing principal; the product
// requires globally consistent VK state.
//
// Adding entries here is a deliberate policy change that must be reflected
// in the SDD + spec.
//
// Unexported on purpose: external packages must not be able to mutate the
// policy. Use IsOverridable / IsBlacklisted for the predicate, and
// BlacklistedKeys for the read-only list (returns a fresh slice).
var nonOverridableConfigKeys = map[string]bool{
	"credentials":  true,
	"virtual_keys": true,
}

// IsOverridable returns false if the key is in the blacklist, true otherwise.
// Empty keys are technically overridable from this function's perspective;
// the CP admin handler rejects empty configKey separately at the routing
// layer (path parameter required).
func IsOverridable(configKey string) bool {
	return !nonOverridableConfigKeys[configKey]
}

// IsBlacklisted is the inverse of IsOverridable for call sites that read
// more naturally as a positive predicate.
func IsBlacklisted(configKey string) bool {
	return nonOverridableConfigKeys[configKey]
}

// BlacklistedKeys returns a fresh slice of the blacklisted keys for tests
// and audit-output paths that need to enumerate the policy. Mutating the
// returned slice does not affect the policy.
func BlacklistedKeys() []string {
	out := make([]string, 0, len(nonOverridableConfigKeys))
	for k := range nonOverridableConfigKeys {
		out = append(out, k)
	}
	return out
}

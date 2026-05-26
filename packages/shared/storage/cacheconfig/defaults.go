package cacheconfig

// CodeDefaults returns the code-baked baseline for every knob. Used as the
// last-resort fallback when neither Tier 1 / 2 / 3 sets a particular knob.
//
// These values are conservative: cache features default to false (off) and
// numeric thresholds match the recommended values documented in the SDD.
// Operators can override every value via the Tier 1 / 2 / 3 storage tables;
// the only reason to hit CodeDefaults is a freshly-installed environment
// where no admin has yet touched cache config, or a knob that was newly
// added to the Go struct since the last admin save (forward-compat).
func CodeDefaults() ProviderEffective {
	return ProviderEffective{
		NormaliserEnabled:       false,
		CacheMasterKillSwitch:   false,
		MarkerInjectEnabled:     false,
		MarkerBoundary3Enabled:  false,
		CacheEnabled:            false,
		MinSystemChars:          4096,
		TTLSeconds:              3600,
		CircuitBreakerThreshold: 5,
		CircuitBreakerOpenSecs:  300,
	}
}

// AdapterFamily classifies an adapter_type into its cache-knob family.
type AdapterFamily int

const (
	FamilyNone AdapterFamily = iota
	FamilyAnthropic
	FamilyGemini
)

// FamilyOf returns the cache-knob family for a Provider's adapter_type.
// Adapters not in any family have FamilyNone and accept no cache knobs.
func FamilyOf(adapterType string) AdapterFamily {
	switch adapterType {
	case "anthropic", "bedrock":
		return FamilyAnthropic
	case "gemini", "vertex":
		return FamilyGemini
	default:
		return FamilyNone
	}
}

// AllowedKnobs returns the set of JSON key names that may legitimately appear
// in a Tier-2 AdapterConfig or Tier-3 ProviderConfig for the given adapter
// family. Used by the CP handler for PUT validation.
func AllowedKnobs(family AdapterFamily) []string {
	switch family {
	case FamilyAnthropic:
		return []string{"marker_inject_enabled", "marker_boundary3_enabled"}
	case FamilyGemini:
		return []string{
			"cache_enabled", "min_system_chars", "ttl_seconds",
			"circuit_breaker_threshold", "circuit_breaker_open_secs",
		}
	default:
		return nil
	}
}

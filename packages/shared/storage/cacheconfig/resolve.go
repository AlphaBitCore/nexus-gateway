package cacheconfig

// Resolve composes the three-tier inheritance into a flat ProviderEffective.
//
// Resolution chain per knob:
//
//	provider_override[K] ?? adapter_default[K] ?? global_default[K] ?? code_default[K]
//
// There is no Tier 1. Emergency cache-off is served by Emergency Passthrough
// / the fleet disable-all, and the upstream rewrite is demand-driven off the
// Tier-2/3 rule + marker-inject settings.
// Tier 2 and Tier 3 use pointer fields where nil == "not set at this tier".
// Each knob's source is recorded in Sources for UI badge rendering.
func Resolve(blob CacheConfigBlob, providerID, adapterType string) ProviderEffective {
	eff := CodeDefaults()
	eff.ProviderID = providerID
	eff.AdapterType = adapterType
	eff.Sources = map[string]Source{}

	// ── Tier 2 (adapter family) ─────────────────────────────────────────
	adapter := blob.Adapters[adapterType]
	if adapter.MarkerInjectEnabled != nil {
		eff.MarkerInjectEnabled = *adapter.MarkerInjectEnabled
		eff.Sources["marker_inject_enabled"] = SourceAdapterDefault
	} else {
		eff.Sources["marker_inject_enabled"] = SourceCodeDefault
	}
	if adapter.MarkerBoundary3Enabled != nil {
		eff.MarkerBoundary3Enabled = *adapter.MarkerBoundary3Enabled
		eff.Sources["marker_boundary3_enabled"] = SourceAdapterDefault
	} else {
		eff.Sources["marker_boundary3_enabled"] = SourceCodeDefault
	}
	if adapter.CacheEnabled != nil {
		eff.CacheEnabled = *adapter.CacheEnabled
		eff.Sources["cache_enabled"] = SourceAdapterDefault
	} else {
		eff.Sources["cache_enabled"] = SourceCodeDefault
	}
	if adapter.MinSystemChars != nil {
		eff.MinSystemChars = *adapter.MinSystemChars
		eff.Sources["min_system_chars"] = SourceAdapterDefault
	} else {
		eff.Sources["min_system_chars"] = SourceCodeDefault
	}
	if adapter.TTLSeconds != nil {
		eff.TTLSeconds = *adapter.TTLSeconds
		eff.Sources["ttl_seconds"] = SourceAdapterDefault
	} else {
		eff.Sources["ttl_seconds"] = SourceCodeDefault
	}
	if adapter.CircuitBreakerThreshold != nil {
		eff.CircuitBreakerThreshold = *adapter.CircuitBreakerThreshold
		eff.Sources["circuit_breaker_threshold"] = SourceAdapterDefault
	} else {
		eff.Sources["circuit_breaker_threshold"] = SourceCodeDefault
	}
	if adapter.CircuitBreakerOpenSecs != nil {
		eff.CircuitBreakerOpenSecs = *adapter.CircuitBreakerOpenSecs
		eff.Sources["circuit_breaker_open_secs"] = SourceAdapterDefault
	} else {
		eff.Sources["circuit_breaker_open_secs"] = SourceCodeDefault
	}

	// Rules: copy the adapter-level map verbatim (Tier 3 does not override).
	if len(adapter.Rules) > 0 {
		eff.RuleOverrides = make(map[string]RuleOverride, len(adapter.Rules))
		for k, v := range adapter.Rules {
			eff.RuleOverrides[k] = v
		}
	}

	// ── Tier 3 (per-provider override) ──────────────────────────────────
	override, hasOverride := blob.Providers[providerID]
	if !hasOverride {
		return eff
	}
	if override.MarkerInjectEnabled != nil {
		eff.MarkerInjectEnabled = *override.MarkerInjectEnabled
		eff.Sources["marker_inject_enabled"] = SourceProviderOverride
	}
	if override.MarkerBoundary3Enabled != nil {
		eff.MarkerBoundary3Enabled = *override.MarkerBoundary3Enabled
		eff.Sources["marker_boundary3_enabled"] = SourceProviderOverride
	}
	if override.CacheEnabled != nil {
		eff.CacheEnabled = *override.CacheEnabled
		eff.Sources["cache_enabled"] = SourceProviderOverride
	}
	if override.MinSystemChars != nil {
		eff.MinSystemChars = *override.MinSystemChars
		eff.Sources["min_system_chars"] = SourceProviderOverride
	}
	if override.TTLSeconds != nil {
		eff.TTLSeconds = *override.TTLSeconds
		eff.Sources["ttl_seconds"] = SourceProviderOverride
	}
	if override.CircuitBreakerThreshold != nil {
		eff.CircuitBreakerThreshold = *override.CircuitBreakerThreshold
		eff.Sources["circuit_breaker_threshold"] = SourceProviderOverride
	}
	if override.CircuitBreakerOpenSecs != nil {
		eff.CircuitBreakerOpenSecs = *override.CircuitBreakerOpenSecs
		eff.Sources["circuit_breaker_open_secs"] = SourceProviderOverride
	}
	return eff
}

// MarkerInjectEnabledFor resolves the single knob the request path asks for on
// every Anthropic-bound call: does this provider want prompt-cache markers.
//
// It follows the same Tier 3 → Tier 2 → code-default chain as Resolve, and
// exists beside it because Resolve builds a ProviderEffective with a Sources
// map for the admin UI's per-knob attribution badges. That map is an
// allocation, and the hot path needs the boolean, not the provenance. Two map
// index operations and two pointer dereferences, no allocation.
//
// Adapters outside the Anthropic family have no marker knob at all — FamilyOf
// is the one definition of which those are, so no caller has to carry its own
// list of adapter names to answer the question.
func MarkerInjectEnabledFor(blob CacheConfigBlob, providerID, adapterType string) bool {
	if FamilyOf(adapterType) != FamilyAnthropic {
		return false
	}
	if override, ok := blob.Providers[providerID]; ok && override.MarkerInjectEnabled != nil {
		return *override.MarkerInjectEnabled
	}
	if adapter, ok := blob.Adapters[adapterType]; ok && adapter.MarkerInjectEnabled != nil {
		return *adapter.MarkerInjectEnabled
	}
	return CodeDefaults().MarkerInjectEnabled
}

// MarkerBoundary3EnabledFor resolves the conversation-boundary knob on the same
// Tier 3 -> Tier 2 -> code-default chain as MarkerInjectEnabledFor, and with the
// same allocation-free shape. It is meaningful only when marker injection is on;
// the caller checks that first.
func MarkerBoundary3EnabledFor(blob CacheConfigBlob, providerID, adapterType string) bool {
	if FamilyOf(adapterType) != FamilyAnthropic {
		return false
	}
	if override, ok := blob.Providers[providerID]; ok && override.MarkerBoundary3Enabled != nil {
		return *override.MarkerBoundary3Enabled
	}
	if adapter, ok := blob.Adapters[adapterType]; ok && adapter.MarkerBoundary3Enabled != nil {
		return *adapter.MarkerBoundary3Enabled
	}
	return CodeDefaults().MarkerBoundary3Enabled
}

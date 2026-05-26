package cacheconfig

// Resolve composes the three-tier inheritance into a flat ProviderEffective.
//
// Resolution chain per knob:
//
//	provider_override[K] ?? adapter_default[K] ?? global_default[K] ?? code_default[K]
//
// Tier 1 only carries pipeline-wide knobs (NormaliserEnabled, CacheMasterKillSwitch).
// Tier 2 and Tier 3 use pointer fields where nil == "not set at this tier".
// Each knob's source is recorded in Sources for UI badge rendering.
func Resolve(blob CacheConfigBlob, providerID, adapterType string) ProviderEffective {
	eff := CodeDefaults()
	eff.ProviderID = providerID
	eff.AdapterType = adapterType
	eff.Sources = map[string]Source{}

	// ── Tier 1 (global) ────────────────────────────────────────────────
	// Booleans in GlobalConfig are non-pointer; we treat the row's absence
	// as "use code default". The blob always carries a GlobalConfig (the
	// row is a singleton seeded at migration); fields that were never set
	// by an admin will be their JSON zero value, which is also our code
	// default, so the source tag is the dominant signal of intent.
	eff.NormaliserEnabled = blob.Global.NormaliserEnabled
	eff.CacheMasterKillSwitch = blob.Global.CacheMasterKillSwitch
	eff.Sources["normaliser_enabled"] = SourceGlobalDefault
	eff.Sources["cache_master_kill_switch"] = SourceGlobalDefault

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

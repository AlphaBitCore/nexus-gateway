package cacheconfig

import (
	"encoding/json"
	"testing"
)

func bp(b bool) *bool { return &b }
func ip(i int) *int   { return &i }

func TestResolve_AllTiersFallthrough(t *testing.T) {
	// Empty blob → every knob falls back to code default; source is recorded.
	blob := CacheConfigBlob{}
	eff := Resolve(blob, "p1", "gemini")

	codeDef := CodeDefaults()
	if eff.CacheEnabled != codeDef.CacheEnabled {
		t.Errorf("cache_enabled: want code default %v, got %v", codeDef.CacheEnabled, eff.CacheEnabled)
	}
	if eff.MinSystemChars != codeDef.MinSystemChars {
		t.Errorf("min_system_chars: want code default %d, got %d", codeDef.MinSystemChars, eff.MinSystemChars)
	}
	if eff.Sources["cache_enabled"] != SourceCodeDefault {
		t.Errorf("cache_enabled source: want %q, got %q", SourceCodeDefault, eff.Sources["cache_enabled"])
	}
}

func TestResolve_NoGlobalTier_OnlyAdapterAndProviderSourcesAppear(t *testing.T) {
	// There is no Tier 1. Every knob must resolve from Tier 2 / Tier 3 / code
	// default — no knob may ever be attributed to a global tier — and neither
	// normaliser_enabled nor cache_master_kill_switch may appear in the
	// effective view at all.
	blob := CacheConfigBlob{
		Adapters: map[string]AdapterConfig{
			"anthropic": {MarkerInjectEnabled: bp(true)},
		},
		Providers: map[string]ProviderConfig{
			"p1": {MarkerBoundary3Enabled: bp(true)},
		},
	}
	eff := Resolve(blob, "p1", "anthropic")

	if !eff.MarkerInjectEnabled || eff.Sources["marker_inject_enabled"] != SourceAdapterDefault {
		t.Errorf("marker_inject_enabled: want true/adapter-default, got %v/%q",
			eff.MarkerInjectEnabled, eff.Sources["marker_inject_enabled"])
	}
	if !eff.MarkerBoundary3Enabled || eff.Sources["marker_boundary3_enabled"] != SourceProviderOverride {
		t.Errorf("marker_boundary3_enabled: want true/provider-override, got %v/%q",
			eff.MarkerBoundary3Enabled, eff.Sources["marker_boundary3_enabled"])
	}
	for knob, src := range eff.Sources {
		if src == Source("global-default") {
			t.Errorf("knob %q attributed to the retired global tier", knob)
		}
	}
	for _, retired := range []string{"normaliser_enabled", "cache_master_kill_switch"} {
		if _, present := eff.Sources[retired]; present {
			t.Errorf("retired Tier-1 knob %q still resolved into the effective view", retired)
		}
	}
}

func TestCacheConfigBlob_WireShapeIsAdaptersAndProvidersOnly(t *testing.T) {
	// The blob is the Hub shadow `cache` payload. With Tier 1 gone the wire
	// shape is exactly {adapters, providers}; a stray `global` key would mean
	// the gateway is still being handed a retired tier.
	raw, err := json.Marshal(CacheConfigBlob{
		Adapters:  map[string]AdapterConfig{"anthropic": {MarkerInjectEnabled: bp(true)}},
		Providers: map[string]ProviderConfig{"p1": {TTLSeconds: ip(7200)}},
	})
	if err != nil {
		t.Fatalf("marshal blob: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("unmarshal blob: %v", err)
	}
	if len(keys) != 2 || keys["adapters"] == nil || keys["providers"] == nil {
		t.Fatalf("blob wire shape: want exactly {adapters, providers}, got %s", raw)
	}
}

func TestResolve_AdapterDefaultsApplied(t *testing.T) {
	blob := CacheConfigBlob{
		Adapters: map[string]AdapterConfig{
			"gemini": {
				CacheEnabled:   bp(true),
				TTLSeconds:     ip(7200),
				MinSystemChars: ip(8192),
			},
		},
	}
	eff := Resolve(blob, "p1", "gemini")
	if !eff.CacheEnabled || eff.TTLSeconds != 7200 || eff.MinSystemChars != 8192 {
		t.Errorf("adapter defaults not applied: got %+v", eff)
	}
	if eff.Sources["cache_enabled"] != SourceAdapterDefault {
		t.Errorf("cache_enabled source: want %q, got %q", SourceAdapterDefault, eff.Sources["cache_enabled"])
	}
	if eff.Sources["ttl_seconds"] != SourceAdapterDefault {
		t.Errorf("ttl_seconds source: want %q, got %q", SourceAdapterDefault, eff.Sources["ttl_seconds"])
	}
}

func TestResolve_ProviderOverridesAdapter(t *testing.T) {
	blob := CacheConfigBlob{
		Adapters: map[string]AdapterConfig{
			"gemini": {CacheEnabled: bp(true), TTLSeconds: ip(3600)},
		},
		Providers: map[string]ProviderConfig{
			"p1": {TTLSeconds: ip(7200)}, // override TTL only, inherit cache_enabled
		},
	}
	eff := Resolve(blob, "p1", "gemini")
	if eff.TTLSeconds != 7200 {
		t.Errorf("ttl_seconds: want 7200 from Tier 3 override, got %d", eff.TTLSeconds)
	}
	if eff.Sources["ttl_seconds"] != SourceProviderOverride {
		t.Errorf("ttl_seconds source: want %q, got %q", SourceProviderOverride, eff.Sources["ttl_seconds"])
	}
	if !eff.CacheEnabled {
		t.Errorf("cache_enabled: want true (inherited from Tier 2), got false")
	}
	if eff.Sources["cache_enabled"] != SourceAdapterDefault {
		t.Errorf("cache_enabled source: want %q (inherited), got %q", SourceAdapterDefault, eff.Sources["cache_enabled"])
	}
}

func TestResolve_RulesCopiedFromAdapter(t *testing.T) {
	blob := CacheConfigBlob{
		Adapters: map[string]AdapterConfig{
			"anthropic": {
				Rules: map[string]RuleOverride{
					"claude-code-cch-strip": {Enabled: bp(true)},
				},
			},
		},
	}
	eff := Resolve(blob, "ap1", "anthropic")
	if eff.RuleOverrides == nil {
		t.Fatalf("rule overrides not copied through")
	}
	got, ok := eff.RuleOverrides["claude-code-cch-strip"]
	if !ok {
		t.Fatalf("claude-code-cch-strip override not present")
	}
	if got.Enabled == nil || !*got.Enabled {
		t.Errorf("claude-code-cch-strip.enabled: want true, got %v", got.Enabled)
	}
}

func TestResolve_AnthropicProviderIgnoresGeminiTier3Fields(t *testing.T) {
	// Defensive: even if a Tier-3 row somehow has gemini fields set for an
	// anthropic provider (which the handler is supposed to reject at write),
	// Resolve still applies them mechanically. The structural defence lives
	// in the handler-layer validator, NOT in Resolve — this is the cost of
	// keeping Resolve a pure data-merge function.
	blob := CacheConfigBlob{
		Providers: map[string]ProviderConfig{
			"ap1": {TTLSeconds: ip(9999)}, // illegal for anthropic but accepted by Resolve
		},
	}
	eff := Resolve(blob, "ap1", "anthropic")
	if eff.TTLSeconds != 9999 {
		t.Errorf("Resolve should mechanically apply the override even for the wrong family; got %d", eff.TTLSeconds)
	}
}

func TestResolve_ProviderOverrideEveryKnob(t *testing.T) {
	// Pin: each knob in ProviderConfig has its own override branch.
	// A refactor that misses one would silently inherit Tier 2 value
	// instead of honouring the per-provider override. Test every knob.
	blob := CacheConfigBlob{
		Adapters: map[string]AdapterConfig{
			"gemini": {
				MarkerInjectEnabled:     bp(false),
				MarkerBoundary3Enabled:  bp(false),
				CacheEnabled:            bp(false),
				MinSystemChars:          ip(100),
				TTLSeconds:              ip(3600),
				CircuitBreakerThreshold: ip(5),
				CircuitBreakerOpenSecs:  ip(60),
			},
		},
		Providers: map[string]ProviderConfig{
			"p1": {
				MarkerInjectEnabled:     bp(true),
				MarkerBoundary3Enabled:  bp(true),
				CacheEnabled:            bp(true),
				MinSystemChars:          ip(200),
				TTLSeconds:              ip(7200),
				CircuitBreakerThreshold: ip(10),
				CircuitBreakerOpenSecs:  ip(120),
			},
		},
	}
	eff := Resolve(blob, "p1", "gemini")

	if !eff.MarkerInjectEnabled {
		t.Errorf("MarkerInjectEnabled not overridden")
	}
	if !eff.MarkerBoundary3Enabled {
		t.Errorf("MarkerBoundary3Enabled not overridden")
	}
	if !eff.CacheEnabled {
		t.Errorf("CacheEnabled not overridden")
	}
	if eff.MinSystemChars != 200 {
		t.Errorf("MinSystemChars: %d", eff.MinSystemChars)
	}
	if eff.TTLSeconds != 7200 {
		t.Errorf("TTLSeconds: %d", eff.TTLSeconds)
	}
	if eff.CircuitBreakerThreshold != 10 {
		t.Errorf("CircuitBreakerThreshold: %d", eff.CircuitBreakerThreshold)
	}
	if eff.CircuitBreakerOpenSecs != 120 {
		t.Errorf("CircuitBreakerOpenSecs: %d", eff.CircuitBreakerOpenSecs)
	}
	// All 7 knobs must show Tier 3 source.
	knobs := []string{
		"marker_inject_enabled", "marker_boundary3_enabled", "cache_enabled",
		"min_system_chars", "ttl_seconds",
		"circuit_breaker_threshold", "circuit_breaker_open_secs",
	}
	for _, k := range knobs {
		if eff.Sources[k] != SourceProviderOverride {
			t.Errorf("source[%s]: got %q, want %q", k, eff.Sources[k], SourceProviderOverride)
		}
	}
}

func TestResolve_NoOverrideKeepsTier2(t *testing.T) {
	// When Tier 3 row is absent entirely, Tier 2 wins for every knob.
	blob := CacheConfigBlob{
		Adapters: map[string]AdapterConfig{
			"openai": {CacheEnabled: bp(true), TTLSeconds: ip(1800)},
		},
		// no Providers[*]
	}
	eff := Resolve(blob, "missing-provider", "openai")
	if !eff.CacheEnabled {
		t.Errorf("CacheEnabled: %v, want true", eff.CacheEnabled)
	}
	if eff.TTLSeconds != 1800 {
		t.Errorf("TTLSeconds: %d", eff.TTLSeconds)
	}
	if eff.Sources["cache_enabled"] != SourceAdapterDefault {
		t.Errorf("source[cache_enabled]: %q", eff.Sources["cache_enabled"])
	}
}

func TestFamilyOf(t *testing.T) {
	cases := map[string]AdapterFamily{
		"anthropic": FamilyAnthropic,
		"bedrock":   FamilyAnthropic,
		"gemini":    FamilyGemini,
		"vertex":    FamilyGemini,
		"openai":    FamilyNone,
		"deepseek":  FamilyNone,
		"":          FamilyNone,
	}
	for adapter, want := range cases {
		if got := FamilyOf(adapter); got != want {
			t.Errorf("FamilyOf(%q) = %v, want %v", adapter, got, want)
		}
	}
}

func TestAllowedKnobs(t *testing.T) {
	if got := len(AllowedKnobs(FamilyAnthropic)); got != 2 {
		t.Errorf("Anthropic allowed knobs: want 2, got %d", got)
	}
	if got := len(AllowedKnobs(FamilyGemini)); got != 5 {
		t.Errorf("Gemini allowed knobs: want 5, got %d", got)
	}
	if got := AllowedKnobs(FamilyNone); got != nil {
		t.Errorf("None family allowed knobs: want nil, got %v", got)
	}
}

// MarkerInjectEnabledFor is the hot-path sibling of Resolve: the request path
// asks it on every Anthropic-bound call, so it must agree with Resolve exactly
// while allocating nothing. Asserting the AGREEMENT rather than a hand-written
// expectation is what stops the two from drifting into different answers for
// the same blob.
func TestMarkerInjectEnabledFor_AgreesWithResolve(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name    string
		blob    CacheConfigBlob
		adapter string
	}{
		{"empty blob", CacheConfigBlob{}, "anthropic"},
		{"family on", CacheConfigBlob{Adapters: map[string]AdapterConfig{
			"anthropic": {MarkerInjectEnabled: &yes}}}, "anthropic"},
		{"family off", CacheConfigBlob{Adapters: map[string]AdapterConfig{
			"anthropic": {MarkerInjectEnabled: &no}}}, "anthropic"},
		{"provider override beats family", CacheConfigBlob{
			Adapters:  map[string]AdapterConfig{"anthropic": {MarkerInjectEnabled: &yes}},
			Providers: map[string]ProviderConfig{"p": {MarkerInjectEnabled: &no}}}, "anthropic"},
		{"provider override opts in", CacheConfigBlob{
			Adapters:  map[string]AdapterConfig{"anthropic": {MarkerInjectEnabled: &no}},
			Providers: map[string]ProviderConfig{"p": {MarkerInjectEnabled: &yes}}}, "anthropic"},
		{"bedrock shares the family", CacheConfigBlob{Adapters: map[string]AdapterConfig{
			"bedrock": {MarkerInjectEnabled: &yes}}}, "bedrock"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := Resolve(tc.blob, "p", tc.adapter).MarkerInjectEnabled
			if got := MarkerInjectEnabledFor(tc.blob, "p", tc.adapter); got != want {
				t.Fatalf("lean lookup = %v, Resolve = %v — the two must never disagree", got, want)
			}
		})
	}
}

// Adapters outside the Anthropic family have no marker knob at all. FamilyOf
// is the single definition of which those are, so a caller cannot answer this
// question by carrying its own list of adapter names.
func TestMarkerInjectEnabledFor_NonAnthropicFamilyIsAlwaysOff(t *testing.T) {
	yes := true
	blob := CacheConfigBlob{
		Adapters:  map[string]AdapterConfig{"openai": {MarkerInjectEnabled: &yes}},
		Providers: map[string]ProviderConfig{"p": {MarkerInjectEnabled: &yes}},
	}
	for _, adapter := range []string{"openai", "gemini", "vertex", "deepseek", ""} {
		if MarkerInjectEnabledFor(blob, "p", adapter) {
			t.Errorf("adapter %q is not an Anthropic-wire family and must answer off", adapter)
		}
	}
}

// The request path calls this on every Anthropic-bound request, so it must not
// allocate. Resolve does, because it builds a Sources map for the admin UI —
// that difference is the whole reason this function exists.
func TestMarkerInjectEnabledFor_AllocatesNothing(t *testing.T) {
	yes := true
	blob := CacheConfigBlob{Adapters: map[string]AdapterConfig{
		"anthropic": {MarkerInjectEnabled: &yes}}}
	if n := testing.AllocsPerRun(100, func() {
		_ = MarkerInjectEnabledFor(blob, "p", "anthropic")
	}); n != 0 {
		t.Fatalf("hot-path lookup allocated %v times per call, want 0", n)
	}
}

// The boundary lookup is the marker lookup's sibling and must agree with
// Resolve for the same reason: two answers to one question drift.
func TestMarkerBoundary3EnabledFor_AgreesWithResolve(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name    string
		blob    CacheConfigBlob
		adapter string
	}{
		{"empty", CacheConfigBlob{}, "anthropic"},
		{"family on", CacheConfigBlob{Adapters: map[string]AdapterConfig{
			"anthropic": {MarkerBoundary3Enabled: &yes}}}, "anthropic"},
		{"provider override off", CacheConfigBlob{
			Adapters:  map[string]AdapterConfig{"anthropic": {MarkerBoundary3Enabled: &yes}},
			Providers: map[string]ProviderConfig{"p": {MarkerBoundary3Enabled: &no}}}, "anthropic"},
		{"bedrock shares the family", CacheConfigBlob{Adapters: map[string]AdapterConfig{
			"bedrock": {MarkerBoundary3Enabled: &yes}}}, "bedrock"},
		{"non-anthropic wire", CacheConfigBlob{Adapters: map[string]AdapterConfig{
			"openai": {MarkerBoundary3Enabled: &yes}}}, "openai"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := Resolve(tc.blob, "p", tc.adapter).MarkerBoundary3Enabled
			if tc.adapter != "anthropic" && tc.adapter != "bedrock" {
				want = false // Resolve fills code defaults for any adapter; the lean lookup gates on family.
			}
			if got := MarkerBoundary3EnabledFor(tc.blob, "p", tc.adapter); got != want {
				t.Fatalf("lean lookup = %v, want %v", got, want)
			}
		})
	}
}

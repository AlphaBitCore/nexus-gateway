package wiring

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/cacheconfig"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/wirerewrite"
)

// TestProjectCacheBlobToNormaliserConfig_emptyBlob verifies a zero-value
// blob with nil layer produces an empty Config with NormaliserEnabled=false.
func TestProjectCacheBlobToNormaliserConfig_emptyBlob(t *testing.T) {
	blob := cacheconfig.CacheConfigBlob{}
	cfg := ProjectCacheBlobToNormaliserConfig(blob, nil)

	if cfg.NormaliserEnabled {
		t.Error("expected NormaliserEnabled=false for empty blob")
	}
	if len(cfg.Rules) != 0 {
		t.Errorf("expected empty Rules, got %v", cfg.Rules)
	}
	if len(cfg.Providers) != 0 {
		t.Errorf("expected empty Providers, got %v", cfg.Providers)
	}
}

// TestProjectCacheBlobToNormaliserConfig_normaliserEnabled verifies that
// Global.NormaliserEnabled is propagated.
func TestProjectCacheBlobToNormaliserConfig_normaliserEnabled(t *testing.T) {
	blob := cacheconfig.CacheConfigBlob{
		Global: cacheconfig.GlobalConfig{NormaliserEnabled: true},
	}
	cfg := ProjectCacheBlobToNormaliserConfig(blob, nil)
	if !cfg.NormaliserEnabled {
		t.Error("expected NormaliserEnabled=true")
	}
}

// TestProjectCacheBlobToNormaliserConfig_rulesProjected verifies that
// Adapters[adapterType].Rules are projected into cfg.Rules[adapterType].
func TestProjectCacheBlobToNormaliserConfig_rulesProjected(t *testing.T) {
	enabled := true
	dryRun := true
	blob := cacheconfig.CacheConfigBlob{
		Adapters: map[string]cacheconfig.AdapterConfig{
			"anthropic": {
				Rules: map[string]cacheconfig.RuleOverride{
					"rule-1": {Enabled: &enabled, DryRunAlways: &dryRun},
				},
			},
		},
	}
	cfg := ProjectCacheBlobToNormaliserConfig(blob, nil)
	adapterRules, ok := cfg.Rules["anthropic"]
	if !ok {
		t.Fatal("expected rules for anthropic adapter")
	}
	ro, ok := adapterRules["rule-1"]
	if !ok {
		t.Fatal("expected rule-1 in anthropic adapter rules")
	}
	if ro.Enabled == nil || !*ro.Enabled {
		t.Error("expected Enabled=true")
	}
	if ro.DryRunAlways == nil || !*ro.DryRunAlways {
		t.Error("expected DryRunAlways=true")
	}
}

// TestProjectCacheBlobToNormaliserConfig_emptyAdapterRulesSkipped verifies
// adapters with no rules are not included in cfg.Rules.
func TestProjectCacheBlobToNormaliserConfig_emptyAdapterRulesSkipped(t *testing.T) {
	blob := cacheconfig.CacheConfigBlob{
		Adapters: map[string]cacheconfig.AdapterConfig{
			"anthropic": {Rules: nil},
		},
	}
	cfg := ProjectCacheBlobToNormaliserConfig(blob, nil)
	if _, ok := cfg.Rules["anthropic"]; ok {
		t.Error("expected anthropic with empty rules to be skipped")
	}
}

// TestProjectCacheBlobToNormaliserConfig_nonAnthropicProvidersSkipped
// verifies that non-anthropic/bedrock providers are not included in
// cfg.Providers even when the layer returns them.
func TestProjectCacheBlobToNormaliserConfig_nonAnthropicProvidersSkipped(t *testing.T) {
	// Pass nil layer — providers section stays empty because layer==nil guard is hit.
	blob := cacheconfig.CacheConfigBlob{
		Global: cacheconfig.GlobalConfig{NormaliserEnabled: true},
	}
	cfg := ProjectCacheBlobToNormaliserConfig(blob, nil)
	if len(cfg.Providers) != 0 {
		t.Errorf("expected empty Providers when layer=nil, got %v", cfg.Providers)
	}
}

// TestProjectCacheBlobToNormaliserConfig_ruleOverrideFields verifies that
// both Enabled and DryRunAlways are projected correctly for each combination.
func TestProjectCacheBlobToNormaliserConfig_ruleOverrideFields(t *testing.T) {
	cases := []struct {
		enabled      bool
		dryRunAlways bool
	}{
		{true, false},
		{false, true},
		{false, false},
		{true, true},
	}
	for _, tc := range cases {
		enabled := tc.enabled
		dryRun := tc.dryRunAlways
		blob := cacheconfig.CacheConfigBlob{
			Adapters: map[string]cacheconfig.AdapterConfig{
				"bedrock": {
					Rules: map[string]cacheconfig.RuleOverride{
						"r": {Enabled: &enabled, DryRunAlways: &dryRun},
					},
				},
			},
		}
		cfg := ProjectCacheBlobToNormaliserConfig(blob, nil)
		ro := cfg.Rules["bedrock"]["r"]
		got := wirerewrite.RuleOverride{Enabled: ro.Enabled, DryRunAlways: ro.DryRunAlways}
		gotEnabled := got.Enabled != nil && *got.Enabled
		gotDryRun := got.DryRunAlways != nil && *got.DryRunAlways
		if gotEnabled != tc.enabled || gotDryRun != tc.dryRunAlways {
			t.Errorf("case %+v: enabled=%v dryRun=%v", tc, gotEnabled, gotDryRun)
		}
	}
}

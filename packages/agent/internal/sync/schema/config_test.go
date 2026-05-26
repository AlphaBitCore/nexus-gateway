package schema

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadYAML_Valid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.yaml")
	_ = os.WriteFile(path, []byte(`
hubHTTPURL: "https://hub.example.com"
deviceID: "dev-001"
auditDBPath: "/tmp/audit.db"
heartbeatIntervalSec: 30
`), 0644)

	cfg, err := LoadFromFile(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.HubHTTPURL != "https://hub.example.com" {
		t.Errorf("expected hub URL, got %s", cfg.HubHTTPURL)
	}
	if cfg.HeartbeatIntervalSec != 30 {
		t.Errorf("expected 30, got %d", cfg.HeartbeatIntervalSec)
	}
}

func TestLoadYAML_MissingRequired(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.yaml")
	_ = os.WriteFile(path, []byte(`deviceID: "dev-001"`), 0644)

	_, err := LoadFromFile(path)
	if err == nil {
		t.Fatal("expected error for missing hubHTTPURL")
	}
}

func TestLoadYAML_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.yaml")
	_ = os.WriteFile(path, []byte(`{invalid yaml <<<`), 0644)

	_, err := LoadFromFile(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestMerge_LocalPathWinsAndRemoteIntervalsOverride(t *testing.T) {
	local := &AgentConfig{
		HubHTTPURL:  "https://hub.example.com",
		AuditDBPath: "/local/audit.db",
	}
	remote := map[string]any{
		"heartbeatIntervalSec": float64(120),
	}

	merged := MergeConfig(local, remote)
	if merged.AuditDBPath != "/local/audit.db" {
		t.Error("local path should win")
	}
	if merged.HeartbeatIntervalSec != 120 {
		t.Errorf("expected 120, got %d", merged.HeartbeatIntervalSec)
	}
}

func TestDefaults(t *testing.T) {
	cfg := &AgentConfig{HubHTTPURL: "https://hub.example.com"}
	applyDefaults(cfg)
	if cfg.HeartbeatIntervalSec != 60 {
		t.Errorf("expected default 60, got %d", cfg.HeartbeatIntervalSec)
	}
	if cfg.AuditDrainIntervalSec != 30 {
		t.Errorf("expected default 30, got %d", cfg.AuditDrainIntervalSec)
	}
	if cfg.DefaultAction != "passthrough" {
		t.Errorf("expected passthrough, got %s", cfg.DefaultAction)
	}
}

func TestManager_SwapNotifiesSubscribers(t *testing.T) {
	initial := &AgentConfig{HubHTTPURL: "https://old.example.com"}
	m := NewManager(initial)
	ch := m.Subscribe()

	next := &AgentConfig{HubHTTPURL: "https://new.example.com"}
	m.Swap(next)

	diff := <-ch
	if diff.Old.HubHTTPURL != "https://old.example.com" {
		t.Errorf("expected old URL, got %s", diff.Old.HubHTTPURL)
	}
	if diff.New.HubHTTPURL != "https://new.example.com" {
		t.Errorf("expected new URL, got %s", diff.New.HubHTTPURL)
	}
	if m.Get().HubHTTPURL != "https://new.example.com" {
		t.Error("Get() should return new config")
	}
}

// TestEffectiveHubCA_HubCAOverride covers AgentConfig.EffectiveHubCA
// — HubCACertFile wins when set; otherwise it falls back to
// CACertFile (shared device CA). Without this, a misconfigured agent
// would pin Hub TLS to the device CA chain even with a separate Hub
// CA configured.
func TestEffectiveHubCA_HubCAOverride(t *testing.T) {
	c := &AgentConfig{CACertFile: "/etc/device-ca.pem", HubCACertFile: "/etc/hub-ca.pem"}
	if got := c.EffectiveHubCA(); got != "/etc/hub-ca.pem" {
		t.Errorf("HubCACertFile must win when set; got %q", got)
	}
}

// TestEffectiveHubCA_FallbackToDeviceCA covers the empty-HubCA branch.
func TestEffectiveHubCA_FallbackToDeviceCA(t *testing.T) {
	c := &AgentConfig{CACertFile: "/etc/device-ca.pem"}
	if got := c.EffectiveHubCA(); got != "/etc/device-ca.pem" {
		t.Errorf("empty HubCACertFile should fall back to CACertFile; got %q", got)
	}
}

// TestMergeConfig_EveryKnob exercises every remote-wins branch in
// MergeConfig so a future refactor that drops a field would surface
// a clear test failure.
func TestMergeConfig_EveryKnob(t *testing.T) {
	local := &AgentConfig{
		HubHTTPURL:           "https://hub.example",
		HeartbeatIntervalSec: 60,
	}
	remote := map[string]any{
		"heartbeatIntervalSec":     float64(30),
		"auditDrainIntervalSec":    float64(15),
		"defaultAction":            "allow",
		"quitAllowed":              true,
		"trafficUploadLevel":       "all",
		"themeId":                  "morningstar",
		"forceQUICFallbackBundles": []any{"com.openai", "com.anthropic"},
		"interceptMode":            "ne",
		"otel": map[string]any{
			"enabled":      true,
			"endpoint":     "https://otlp.example",
			"serviceName":  "nexus-agent",
			"samplingRate": float64(0.25),
		},
		"exemptions": map[string]any{
			"enabled":          true,
			"failureThreshold": float64(5),
			"windowSec":        float64(120),
			"durationSec":      float64(3600),
			"allowlist":        []any{"a.example.com", "b.example.com"},
			"denylist":         []any{"c.example.com"},
		},
	}
	got := MergeConfig(local, remote)

	if got.HeartbeatIntervalSec != 30 {
		t.Errorf("HeartbeatIntervalSec: got %d, want 30", got.HeartbeatIntervalSec)
	}
	if got.AuditDrainIntervalSec != 15 {
		t.Errorf("AuditDrainIntervalSec: got %d, want 15", got.AuditDrainIntervalSec)
	}
	if got.DefaultAction != "allow" {
		t.Errorf("DefaultAction: got %q", got.DefaultAction)
	}
	if got.QuitAllowed == nil || !*got.QuitAllowed {
		t.Errorf("QuitAllowed: got %v", got.QuitAllowed)
	}
	if got.TrafficUploadLevel != "all" {
		t.Errorf("TrafficUploadLevel: got %q", got.TrafficUploadLevel)
	}
	if got.ThemeID != "morningstar" {
		t.Errorf("ThemeID: got %q, want morningstar", got.ThemeID)
	}
	if len(got.ForceQUICFallbackBundles) != 2 {
		t.Errorf("ForceQUICFallbackBundles: got %+v", got.ForceQUICFallbackBundles)
	}
	if got.InterceptMode != "ne" {
		t.Errorf("InterceptMode: got %q, want ne", got.InterceptMode)
	}
	if !got.OtelEnabled || got.OtelEndpoint != "https://otlp.example" {
		t.Errorf("Otel fields: %+v", got)
	}
	if got.OtelSamplingRate != 0.25 {
		t.Errorf("OtelSamplingRate: %v", got.OtelSamplingRate)
	}
	if !got.ExemptionEnabled || got.ExemptionFailureThreshold != 5 || got.ExemptionWindowSec != 120 || got.ExemptionDurationSec != 3600 {
		t.Errorf("Exemption fields: %+v", got)
	}
	if len(got.ExemptionAllowlist) != 2 || len(got.ExemptionDenylist) != 1 {
		t.Errorf("Exemption allow/deny lists: %+v %+v", got.ExemptionAllowlist, got.ExemptionDenylist)
	}
}

// TestMergeConfig_InvalidTrafficUploadLevelIgnored covers the
// switch-default silent-drop branch — never panic on a misconfigured
// admin push.
func TestMergeConfig_InvalidTrafficUploadLevelIgnored(t *testing.T) {
	local := &AgentConfig{TrafficUploadLevel: "processed"}
	got := MergeConfig(local, map[string]any{"trafficUploadLevel": "bogus"})
	if got.TrafficUploadLevel != "processed" {
		t.Errorf("invalid value should be ignored, keeping local; got %q", got.TrafficUploadLevel)
	}
}

// TestMergeConfig_InterceptModeEnum exercises the closed-enum guard for
// the interceptMode key: only "ne" overwrites the local value now (pf
// path retired pre-ship). Any other remote value (including the old
// "pf") leaves the merged config's InterceptMode as-is — fail-safe
// behaviour matches trafficUploadLevel.
func TestMergeConfig_InterceptModeEnum(t *testing.T) {
	cases := []struct {
		name    string
		local   string
		remote  any
		wantOut string
	}{
		{"valid_ne_wins", "", "ne", "ne"},
		{"retired_pf_rejected_keeps_local", "ne", "pf", "ne"},
		{"hybrid_rejected_keeps_local", "ne", "hybrid", "ne"},
		{"empty_remote_keeps_local", "ne", "", "ne"},
		{"non_string_keeps_local", "ne", float64(1), "ne"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			local := &AgentConfig{InterceptMode: tc.local}
			got := MergeConfig(local, map[string]any{"interceptMode": tc.remote})
			if got.InterceptMode != tc.wantOut {
				t.Errorf("InterceptMode: got %q, want %q", got.InterceptMode, tc.wantOut)
			}
		})
	}
}

// TestStringSliceFromAny covers the helper used by MergeConfig — the
// non-string elements must be silently dropped (admin YAML may
// accidentally contain numbers/nulls).
func TestStringSliceFromAny(t *testing.T) {
	got := stringSliceFromAny([]any{"a", 1, "b", nil, "c"})
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("stringSliceFromAny: got %+v, want [a b c]", got)
	}
}

// TestManager_CloseUnblocksSubscribers covers Close() — subscribers
// must observe channel close so a `for diff := range ch` loop exits
// cleanly on agent shutdown (otherwise the goroutine leaks).
func TestManager_CloseUnblocksSubscribers(t *testing.T) {
	m := NewManager(&AgentConfig{})
	ch := m.Subscribe()
	m.Close()
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("Close should close subscriber channels (recv ok=false expected)")
		}
	default:
		t.Error("Close did not close the subscriber channel")
	}
}

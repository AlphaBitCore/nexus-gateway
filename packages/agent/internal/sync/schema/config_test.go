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

func TestMerge_LocalFieldsSurviveStaleIntervalPush(t *testing.T) {
	local := &AgentConfig{
		HubHTTPURL:           "https://hub.example.com",
		AuditDBPath:          "/local/audit.db",
		HeartbeatIntervalSec: 60,
	}
	remote := map[string]any{
		"heartbeatIntervalSec": float64(120), // legacy blob field — must be ignored
	}

	merged := MergeConfig(local, remote)
	if merged.AuditDBPath != "/local/audit.db" {
		t.Error("local path should win")
	}
	if merged.HeartbeatIntervalSec != 60 {
		t.Errorf("heartbeat is local-yaml-only; a stale shadow value must not override it: got %d, want 60", merged.HeartbeatIntervalSec)
	}
}

func TestDefaults(t *testing.T) {
	cfg := &AgentConfig{HubHTTPURL: "https://hub.example.com"}
	applyDefaults(cfg)
	if cfg.HeartbeatIntervalSec != 60 {
		t.Errorf("expected default 60, got %d", cfg.HeartbeatIntervalSec)
	}
	if cfg.AuditDrainIntervalSec != 5 {
		t.Errorf("expected default 5, got %d", cfg.AuditDrainIntervalSec)
	}
	if cfg.DefaultAction != "passthrough" {
		t.Errorf("expected passthrough, got %s", cfg.DefaultAction)
	}
	// LocalBodyCapture defaults to true (unset → always-on local capture).
	if cfg.LocalBodyCapture == nil || !*cfg.LocalBodyCapture {
		t.Errorf("LocalBodyCapture should default to true, got %v", cfg.LocalBodyCapture)
	}
}

func TestDefaults_LocalBodyCaptureExplicitFalsePreserved(t *testing.T) {
	f := false
	cfg := &AgentConfig{HubHTTPURL: "https://hub.example.com", LocalBodyCapture: &f}
	applyDefaults(cfg)
	if cfg.LocalBodyCapture == nil || *cfg.LocalBodyCapture {
		t.Errorf("explicit LocalBodyCapture=false must be preserved, got %v", cfg.LocalBodyCapture)
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

func TestApplyDefaults_LogLevelEnvOverrides(t *testing.T) {
	// The LOG_LEVEL env var must win over both the empty default and any
	// YAML value — it's the operator's last-resort knob for turning up
	// verbosity on a deployed daemon without editing config.
	t.Setenv("LOG_LEVEL", "debug")
	cfg := &AgentConfig{}
	cfg.Log.Level = "warn" // simulate a YAML-set value
	applyDefaults(cfg)
	if cfg.Log.Level != "debug" {
		t.Errorf("LOG_LEVEL env must override config; got %q want debug", cfg.Log.Level)
	}
}

func TestManager_SwapDropsWhenSubscriberFull(t *testing.T) {
	// A subscriber that never drains must NOT block Swap (a stuck Dashboard
	// reader cannot stall config propagation to the rest of the daemon).
	// The notification is dropped for that subscriber; Swap still publishes
	// the new live config.
	m := NewManager(&AgentConfig{HubHTTPURL: "https://old"})
	ch := m.Subscribe() // buffered cap 4
	for range 4 {
		m.Swap(&AgentConfig{HubHTTPURL: "https://fill"})
	}
	// Channel is now full and undrained; this Swap must hit the drop branch
	// without blocking, and still update the live config.
	m.Swap(&AgentConfig{HubHTTPURL: "https://new"})
	if got := m.Get().HubHTTPURL; got != "https://new" {
		t.Errorf("Swap must publish live config even when subscriber full; got %q", got)
	}
	_ = ch
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

// TestMergeConfig_LiveKnobs exercises the three shadow-overridable fields —
// the ONLY fields the sole caller (cmd/agent configappliers) publishes.
// Local-yaml-only fields must pass through untouched even when a legacy
// shadow blob still carries them (heartbeat/drain/otel/exemptions/theme were
// merge-able once; CP now strips or never sends them, and MergeConfig must
// not resurrect them from stale desired-state JSON).
func TestMergeConfig_LiveKnobs(t *testing.T) {
	local := &AgentConfig{
		HubHTTPURL:           "https://hub.example",
		HeartbeatIntervalSec: 60,
		DefaultAction:        "passthrough",
	}
	remote := map[string]any{
		"quitAllowed":              true,
		"trafficUploadLevel":       "all",
		"forceQUICFallbackBundles": []any{"com.openai", "com.anthropic"},
		"bypassBundles":            []any{"com.anthropic.claude-code"},
		// Legacy/stray fields a stale shadow blob may still carry —
		// every one of these MUST be ignored:
		"heartbeatIntervalSec":  float64(30),
		"auditDrainIntervalSec": float64(15),
		"defaultAction":         "allow",
		"themeId":               "morningstar",
		"otel":                  map[string]any{"enabled": true, "endpoint": "https://otlp.example"},
		"exemptions":            map[string]any{"enabled": true, "failureThreshold": float64(5)},
	}
	got := MergeConfig(local, remote)

	// The three live knobs apply:
	if got.QuitAllowed == nil || !*got.QuitAllowed {
		t.Errorf("QuitAllowed: got %v", got.QuitAllowed)
	}
	if got.TrafficUploadLevel != "all" {
		t.Errorf("TrafficUploadLevel: got %q", got.TrafficUploadLevel)
	}
	if len(got.ForceQUICFallbackBundles) != 2 {
		t.Errorf("ForceQUICFallbackBundles: got %+v", got.ForceQUICFallbackBundles)
	}
	if len(got.BypassBundles) != 1 || got.BypassBundles[0] != "com.anthropic.claude-code" {
		t.Errorf("BypassBundles: got %+v, want [com.anthropic.claude-code]", got.BypassBundles)
	}

	// Local-yaml-only fields survive a stale blob untouched:
	if got.HeartbeatIntervalSec != 60 {
		t.Errorf("HeartbeatIntervalSec must stay local: got %d, want 60", got.HeartbeatIntervalSec)
	}
	if got.DefaultAction != "passthrough" {
		t.Errorf("DefaultAction must stay local: got %q", got.DefaultAction)
	}
	if got.OtelEnabled || got.OtelEndpoint != "" {
		t.Errorf("Otel must stay local: %+v", got)
	}
	if got.ExemptionEnabled || got.ExemptionFailureThreshold != 0 {
		t.Errorf("Exemptions must stay local: %+v", got)
	}
	// And the original local config is not mutated (merge returns a copy).
	if local.QuitAllowed != nil || local.TrafficUploadLevel != "" {
		t.Errorf("MergeConfig mutated its input: %+v", local)
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

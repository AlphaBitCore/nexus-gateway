package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	configtypes "github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/policy"
)

func writeYAML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "cfg.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	return p
}

func TestLoad_Cache_FromYAML(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cfg.yaml")
	_ = os.WriteFile(p, []byte(`
cache:
  enabled: true
  ttl: 5m
  prefix: "ai-gw:"
`), 0o644)
	setRequiredEnvBaseline(t)

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Cache.Enabled {
		t.Errorf("Enabled = false, want true")
	}
	if cfg.Cache.TTL != 5*time.Minute {
		t.Errorf("TTL = %v, want 5m", cfg.Cache.TTL)
	}
	if cfg.Cache.Prefix != "ai-gw:" {
		t.Errorf("Prefix = %q, want %q", cfg.Cache.Prefix, "ai-gw:")
	}
}

func TestLoad_Cache_EnvOverride(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cfg.yaml")
	_ = os.WriteFile(p, []byte(`
cache:
  enabled: false
  ttl: 1m
  prefix: "yaml-prefix:"
`), 0o644)

	setRequiredEnvBaseline(t)
	t.Setenv("AI_GATEWAY_CACHE_ENABLED", "true")
	t.Setenv("AI_GATEWAY_CACHE_TTL", "10m")
	t.Setenv("AI_GATEWAY_CACHE_PREFIX", "env-prefix:")

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Cache.Enabled {
		t.Errorf("Enabled = false, want true (env)")
	}
	if cfg.Cache.TTL != 10*time.Minute {
		t.Errorf("TTL = %v, want 10m (env)", cfg.Cache.TTL)
	}
	if cfg.Cache.Prefix != "env-prefix:" {
		t.Errorf("Prefix = %q, want %q (env)", cfg.Cache.Prefix, "env-prefix:")
	}
}

func TestLoad_Audit_EnvOverride(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cfg.yaml")
	_ = os.WriteFile(p, []byte("server:\n  port: 3050\n"), 0o644)

	setRequiredEnvBaseline(t)
	t.Setenv("AI_GATEWAY_AUDIT_SPOOL_DIR", "/tmp/spool-x")
	t.Setenv("AI_GATEWAY_AUDIT_MAX_QUEUED_RECORDS", "777")
	t.Setenv("AI_GATEWAY_AUDIT_LOSS_MODE", "spill")
	t.Setenv("AI_GATEWAY_AUDIT_COMPRESS", "false")
	t.Setenv("AI_GATEWAY_AUDIT_COMPRESS_MIN_BYTES", "2048")
	t.Setenv("AI_GATEWAY_AUDIT_COMPRESS_LEVEL", "5")
	t.Setenv("AI_GATEWAY_AUDIT_SPILL_RECOVERY_INTERVAL_MS", "1500")
	t.Setenv("AI_GATEWAY_AUDIT_SPILL_RECOVERY_PACE_MS", "25")

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a := cfg.Audit
	if a.SpoolDir != "/tmp/spool-x" || a.MaxQueuedRecords != 777 || a.LossMode != "spill" {
		t.Errorf("spool/queue/loss = %q/%d/%q", a.SpoolDir, a.MaxQueuedRecords, a.LossMode)
	}
	if a.Compress || a.CompressMinBytes != 2048 || a.CompressLevel != 5 {
		t.Errorf("compress = %v/%d/%d", a.Compress, a.CompressMinBytes, a.CompressLevel)
	}
	if a.SpillRecoveryIntervalMs != 1500 || a.SpillRecoveryPaceMs != 25 {
		t.Errorf("recovery = %d/%d, want 1500/25", a.SpillRecoveryIntervalMs, a.SpillRecoveryPaceMs)
	}
}

func TestLoad_Otel_FromYAML(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cfg.yaml")
	_ = os.WriteFile(p, []byte(`
otel:
  endpoint: "http://otel:4318"
  serviceName: "custom-ai-gw"
`), 0o644)
	setRequiredEnvBaseline(t)

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Otel.Endpoint != "http://otel:4318" {
		t.Errorf("Endpoint = %q, want %q", cfg.Otel.Endpoint, "http://otel:4318")
	}
	if cfg.Otel.ServiceName != "custom-ai-gw" {
		t.Errorf("ServiceName = %q", cfg.Otel.ServiceName)
	}
}

func TestLoad_Otel_EnvOverride(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cfg.yaml")
	_ = os.WriteFile(p, []byte(`
otel:
  endpoint: "http://yaml:4318"
  serviceName: "yaml-name"
`), 0o644)
	setRequiredEnvBaseline(t)
	t.Setenv("OTEL_ENDPOINT", "http://env:4318")
	t.Setenv("OTEL_SERVICE_NAME", "env-name")

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Otel.Endpoint != "http://env:4318" {
		t.Errorf("Endpoint override failed: got %q", cfg.Otel.Endpoint)
	}
	if cfg.Otel.ServiceName != "env-name" {
		t.Errorf("ServiceName override failed: got %q", cfg.Otel.ServiceName)
	}
}

func TestLoad_DefaultRetryPolicy_FromYAML(t *testing.T) {
	p := writeYAML(t, `
routing:
  defaultRetryPolicy:
    maxAttemptsPerTarget: 3
    retryOn: ["timeout", "5xx"]
    backoffInitial: 100ms
    backoffMax: 2s
    backoffJitter: 0.1
`)
	setRequiredEnvBaseline(t)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rp := cfg.Routing.DefaultRetryPolicy
	if rp.MaxAttemptsPerTarget != 3 {
		t.Errorf("MaxAttemptsPerTarget = %d, want 3", rp.MaxAttemptsPerTarget)
	}
	if len(rp.RetryOn) != 2 ||
		rp.RetryOn[0] != configtypes.ErrorClassTimeout ||
		rp.RetryOn[1] != configtypes.ErrorClass5xx {
		t.Errorf("RetryOn = %v, want [timeout 5xx]", rp.RetryOn)
	}
	if rp.BackoffInitial != 100*time.Millisecond {
		t.Errorf("BackoffInitial = %v, want 100ms", rp.BackoffInitial)
	}
	if rp.BackoffMax != 2*time.Second {
		t.Errorf("BackoffMax = %v, want 2s", rp.BackoffMax)
	}
	if rp.BackoffJitter != 0.1 {
		t.Errorf("BackoffJitter = %v, want 0.1", rp.BackoffJitter)
	}
}

func TestLoad_DefaultRetryPolicy_FillsMissingFromDefault(t *testing.T) {
	p := writeYAML(t, `
routing:
  defaultRetryPolicy:
    maxAttemptsPerTarget: 2
`)
	setRequiredEnvBaseline(t)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rp := cfg.Routing.DefaultRetryPolicy
	if rp.MaxAttemptsPerTarget != 2 {
		t.Errorf("MaxAttemptsPerTarget = %d, want 2 (explicit YAML)", rp.MaxAttemptsPerTarget)
	}
	if len(rp.RetryOn) != 4 {
		t.Errorf("RetryOn len = %d, want 4 (default fill)", len(rp.RetryOn))
	}
	if rp.BackoffInitial != 250*time.Millisecond {
		t.Errorf("BackoffInitial = %v, want 250ms (default fill)", rp.BackoffInitial)
	}
	if rp.BackoffMax != 5*time.Second {
		t.Errorf("BackoffMax = %v, want 5s (default fill)", rp.BackoffMax)
	}
	if rp.BackoffJitter != 0.2 {
		t.Errorf("BackoffJitter = %v, want 0.2 (default fill)", rp.BackoffJitter)
	}
}

func TestLoad_DefaultRetryPolicy_AbsentSectionUsesDefaults(t *testing.T) {
	p := writeYAML(t, `# no routing block at all
cache:
  enabled: false
`)
	setRequiredEnvBaseline(t)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rp := cfg.Routing.DefaultRetryPolicy
	want := configtypes.DefaultRetryPolicy()
	if rp.MaxAttemptsPerTarget != want.MaxAttemptsPerTarget {
		t.Errorf("MaxAttemptsPerTarget = %d, want %d (platform default)", rp.MaxAttemptsPerTarget, want.MaxAttemptsPerTarget)
	}
	if len(rp.RetryOn) != len(want.RetryOn) {
		t.Errorf("RetryOn len = %d, want %d (platform default)", len(rp.RetryOn), len(want.RetryOn))
	}
	if rp.BackoffInitial != want.BackoffInitial {
		t.Errorf("BackoffInitial = %v, want %v (platform default)", rp.BackoffInitial, want.BackoffInitial)
	}
	if rp.BackoffMax != want.BackoffMax {
		t.Errorf("BackoffMax = %v, want %v (platform default)", rp.BackoffMax, want.BackoffMax)
	}
	if rp.BackoffJitter != want.BackoffJitter {
		t.Errorf("BackoffJitter = %v, want %v (platform default)", rp.BackoffJitter, want.BackoffJitter)
	}
}

func TestLoad_DefaultRetryPolicy_MaxAttemptsClamped(t *testing.T) {
	cases := []struct {
		yaml string
		want int
	}{
		{
			yaml: `
routing:
  defaultRetryPolicy:
    maxAttemptsPerTarget: 99
`,
			want: 5,
		},
		{
			yaml: `
routing:
  defaultRetryPolicy:
    maxAttemptsPerTarget: -1
`,
			want: 1,
		},
	}
	for _, tc := range cases {
		p := writeYAML(t, tc.yaml)
		setRequiredEnvBaseline(t)
		cfg, err := Load(p)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := cfg.Routing.DefaultRetryPolicy.MaxAttemptsPerTarget; got != tc.want {
			t.Errorf("MaxAttemptsPerTarget = %d, want %d (clamped)", got, tc.want)
		}
	}
}

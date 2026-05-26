package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// setRequiredEnvBaseline stamps every env-side input that validate() now
// requires, so a test reaches the branch it actually wants to exercise.
// Tests that drive a specific required field empty MUST override after.
// Required set is documented in validate() of this package.
func setRequiredEnvBaseline(t *testing.T) {
	t.Helper()
	t.Setenv("INTERNAL_SERVICE_TOKEN", "tok")
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("AI_GATEWAY_PUBLIC_URL", "http://localhost:3050")
	t.Setenv("REDIS_ADDRS", "localhost:6379")
	t.Setenv("MQ_DRIVER", "nats")
	t.Setenv("NATS_URL", "nats://localhost:4222")
	t.Setenv("ADMIN_KEY_HMAC_SECRET", "hmac-sentinel")
	t.Setenv("CREDENTIAL_ENCRYPTION_KEY", "cred-master-sentinel")
	t.Setenv("NEXUS_HUB_URL", "http://localhost:3060")
}

// TestLoad_MissingFile_UsesDefaults verifies that a non-existent path is NOT an
// error and the seeded defaults (Port, Upstream, HTTPClients) are returned.
// This is the file-not-exist branch of Load.
func TestLoad_MissingFile_UsesDefaults(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "does-not-exist.yaml")
	setRequiredEnvBaseline(t)

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load missing file: %v", err)
	}
	if cfg.Server.Port != 8080 {
		t.Errorf("default Port = %d, want 8080", cfg.Server.Port)
	}
	if cfg.Server.ReadTimeout != 30*time.Second {
		t.Errorf("default ReadTimeout = %v, want 30s", cfg.Server.ReadTimeout)
	}
	if cfg.Server.WriteTimeout != 60*time.Second {
		t.Errorf("default WriteTimeout = %v, want 60s", cfg.Server.WriteTimeout)
	}
	if cfg.Upstream.TimeoutSec != 120 {
		t.Errorf("default Upstream.TimeoutSec = %d, want 120", cfg.Upstream.TimeoutSec)
	}
	if cfg.Upstream.DialTimeoutSec != 15 {
		t.Errorf("default Upstream.DialTimeoutSec = %d, want 15", cfg.Upstream.DialTimeoutSec)
	}
	if cfg.Upstream.MaxIdleConns != 200 {
		t.Errorf("default Upstream.MaxIdleConns = %d, want 200", cfg.Upstream.MaxIdleConns)
	}
	if cfg.HTTPClients.Webhook.TimeoutSec != 10 {
		t.Errorf("default Webhook.TimeoutSec = %d, want 10", cfg.HTTPClients.Webhook.TimeoutSec)
	}
	if cfg.HTTPClients.External.TimeoutSec != 30 {
		t.Errorf("default External.TimeoutSec = %d, want 30", cfg.HTTPClients.External.TimeoutSec)
	}
}

// TestLoad_ParseError surfaces a wrapped "parse config" error when the YAML
// is malformed. Covers the yaml.Unmarshal failure branch.
func TestLoad_ParseError(t *testing.T) {
	p := writeYAML(t, "server: : : not valid yaml ::\n\tbad-indent")
	_, err := Load(p)
	if err == nil {
		t.Fatal("Load returned nil for malformed YAML")
	}
	if !strings.Contains(err.Error(), "parse config") {
		t.Fatalf("error %q must wrap with 'parse config'", err)
	}
}

// TestLoad_ReadError covers the `err != nil && !os.IsNotExist(err)` branch by
// pointing Load at a path that is a directory (yields a non-IsNotExist read
// error on every supported OS).
func TestLoad_ReadError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.ReadFile of a directory has different behavior on Windows")
	}
	dir := t.TempDir() // dir exists, but reading it as a file fails non-IsNotExist
	_, err := Load(dir)
	if err == nil {
		t.Fatal("Load returned nil for directory path")
	}
	if !strings.Contains(err.Error(), "read config") {
		t.Fatalf("error %q must wrap with 'read config'", err)
	}
}

// TestLoad_AllEnvOverrides covers every env-var branch in Load that the
// existing tests don't already exercise. Each env value is unique so a
// regression in one branch is visible without cross-talk.
func TestLoad_AllEnvOverrides(t *testing.T) {
	p := writeYAML(t, "# minimal yaml so file exists\n")

	// Baseline provides REDIS_ADDRS + AI_GATEWAY_PUBLIC_URL that validate
	// requires; the per-knob Setenv calls below shadow the fields this
	// test actually verifies. Order matters: baseline first.
	setRequiredEnvBaseline(t)
	t.Setenv("DATABASE_URL", "postgres://env-db/aigw")
	// REDIS_* env knobs are consumed by redisfactory.LoadEnv at wiring time,
	// not at config.Load. See packages/shared/storage/redisfactory.
	t.Setenv("ADMIN_KEY_HMAC_SECRET", "env-hmac")
	t.Setenv("CREDENTIAL_ENCRYPTION_KEY", "env-cred-key")
	t.Setenv("CREDENTIAL_KEY_MAP", "v1:abc,v2:def")
	t.Setenv("AI_GATEWAY_PORT", "9999")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("LOG_FORMAT", "text")
	t.Setenv("NEXUS_HUB_URL", "http://hub.env:3000")
	t.Setenv("INTERNAL_SERVICE_TOKEN", "env-internal-token")
	t.Setenv("MQ_DRIVER", "nats")
	t.Setenv("NATS_URL", "nats://env-nats:4222")
	t.Setenv("AI_GATEWAY_CORS_ENABLED", "true")
	t.Setenv("AI_GATEWAY_CORS_ALLOWED_ORIGINS", "https://a.example,https://b.example")

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	checks := []struct {
		name string
		got  string
		want string
	}{
		{"Database.URL", cfg.Database.URL, "postgres://env-db/aigw"},
		{"Auth.HMACSecret", cfg.Auth.HMACSecret, "env-hmac"},
		{"Auth.CredentialMasterKey", cfg.Auth.CredentialMasterKey, "env-cred-key"},
		{"Auth.CredentialKeyMap", cfg.Auth.CredentialKeyMap, "v1:abc,v2:def"},
		{"Log.Level", cfg.Log.Level, "debug"},
		{"Log.Format", cfg.Log.Format, "text"},
		{"Registry.NexusHubURL", cfg.Registry.NexusHubURL, "http://hub.env:3000"},
		{"Auth.InternalServiceToken", cfg.Auth.InternalServiceToken, "env-internal-token"},
		{"MQ.Driver", cfg.MQ.Driver, "nats"},
		{"MQ.NATS.URL", cfg.MQ.NATS.URL, "nats://env-nats:4222"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	if cfg.Server.Port != 9999 {
		t.Errorf("Server.Port = %d, want 9999 (env override)", cfg.Server.Port)
	}
	if !cfg.CORS.Enabled {
		t.Error("CORS.Enabled = false, want true (env)")
	}
	if len(cfg.CORS.AllowedOrigins) != 2 || cfg.CORS.AllowedOrigins[0] != "https://a.example" || cfg.CORS.AllowedOrigins[1] != "https://b.example" {
		t.Errorf("CORS.AllowedOrigins = %v, want [https://a.example https://b.example]", cfg.CORS.AllowedOrigins)
	}
}

// TestLoad_CORSEnabled_OneAlias verifies that AI_GATEWAY_CORS_ENABLED
// accepts "1" as an alternative to "true" — the `v == "true" || v == "1"`
// branch.
func TestLoad_CORSEnabled_OneAlias(t *testing.T) {
	p := writeYAML(t, "# minimal\n")
	setRequiredEnvBaseline(t)
	t.Setenv("AI_GATEWAY_CORS_ENABLED", "1")
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.CORS.Enabled {
		t.Error("CORS.Enabled = false, want true (AI_GATEWAY_CORS_ENABLED=1)")
	}
}

// TestLoad_CacheEnabled_OneAlias mirrors CORS for the cache toggle.
func TestLoad_CacheEnabled_OneAlias(t *testing.T) {
	p := writeYAML(t, "# minimal\n")
	setRequiredEnvBaseline(t)
	t.Setenv("AI_GATEWAY_CACHE_ENABLED", "1")
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Cache.Enabled {
		t.Error("Cache.Enabled = false, want true (AI_GATEWAY_CACHE_ENABLED=1)")
	}
}

// TestLoad_CacheTTL_InvalidIsIgnored covers the `if err == nil` branch of
// AI_GATEWAY_CACHE_TTL parsing — a malformed duration must NOT overwrite
// the existing YAML/zero value (best-effort behavior is the documented
// contract).
func TestLoad_CacheTTL_InvalidIsIgnored(t *testing.T) {
	p := writeYAML(t, `
cache:
  ttl: 7s
`)
	setRequiredEnvBaseline(t)
	t.Setenv("AI_GATEWAY_CACHE_TTL", "not-a-duration")
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Cache.TTL.String() != "7s" {
		t.Errorf("TTL = %v, want 7s (invalid env should be ignored)", cfg.Cache.TTL)
	}
}

// TestLoad_AIGatewayPort_InvalidLeavesDefault asserts the documented best-effort
// Sscanf behavior: a non-integer port leaves the default 8080 in place.
func TestLoad_AIGatewayPort_InvalidLeavesDefault(t *testing.T) {
	p := writeYAML(t, "# minimal\n")
	setRequiredEnvBaseline(t)
	t.Setenv("AI_GATEWAY_PORT", "not-a-port")
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Port != 8080 {
		t.Errorf("Server.Port = %d, want 8080 (invalid env should leave default)", cfg.Server.Port)
	}
}

// TestLoad_CORSEnabled_OtherValueStaysFalse confirms that unrelated values
// (e.g. "yes") do not flip the CORS toggle. The branch returns the implicit
// default false.
func TestLoad_CORSEnabled_OtherValueStaysFalse(t *testing.T) {
	p := writeYAML(t, "# minimal\n")
	setRequiredEnvBaseline(t)
	t.Setenv("AI_GATEWAY_CORS_ENABLED", "yes")
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CORS.Enabled {
		t.Error("CORS.Enabled = true, want false for AI_GATEWAY_CORS_ENABLED=yes")
	}
}

// TestLoad_ForwardHeadersBlockParsed exercises the optional ForwardHeaders
// pointer field — when present in YAML it round-trips through yaml.Unmarshal.
func TestLoad_ForwardHeadersBlockParsed(t *testing.T) {
	p := writeYAML(t, `
forwardHeaders:
  request:
    default:
      vendorAllowlist: ["X-My-Header"]
`)
	setRequiredEnvBaseline(t)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ForwardHeaders == nil {
		t.Fatal("ForwardHeaders block parsed as nil")
	}
}

// writeYAMLAlt provides a second helper signature so we don't shadow tests
// that share writeYAML — kept private to this file only via the leading _.
var _ = os.PathSeparator

// TestValidate_RequiredFields cycles each business-required field through
// the empty path, asserting validate trips with a contextual message.
// Mirrors the Hub + CP validate-coverage pattern. Each subtest drives ONE
// required field empty after the baseline, isolating that branch.
func TestValidate_RequiredFields(t *testing.T) {
	type tc struct {
		name      string
		mutate    func(t *testing.T) string // returns optional yaml path
		wantInErr string
	}
	cases := []tc{
		{
			name: "missing PublicURL",
			mutate: func(t *testing.T) string {
				t.Setenv("AI_GATEWAY_PUBLIC_URL", "")
				return ""
			},
			wantInErr: "publicURL is required",
		},
		{
			name: "missing Database.URL",
			mutate: func(t *testing.T) string {
				t.Setenv("DATABASE_URL", "")
				return ""
			},
			wantInErr: "database.url is required",
		},
		{
			name: "missing Auth.InternalServiceToken",
			mutate: func(t *testing.T) string {
				t.Setenv("INTERNAL_SERVICE_TOKEN", "")
				return ""
			},
			wantInErr: "auth.internalServiceToken is required",
		},
		{
			name: "missing Auth.HMACSecret",
			mutate: func(t *testing.T) string {
				t.Setenv("ADMIN_KEY_HMAC_SECRET", "")
				return ""
			},
			wantInErr: "auth.hmacSecret is required",
		},
		{
			name: "missing Auth.CredentialMasterKey",
			mutate: func(t *testing.T) string {
				t.Setenv("CREDENTIAL_ENCRYPTION_KEY", "")
				return ""
			},
			wantInErr: "auth.credentialMasterKey is required",
		},
		{
			name: "missing Redis.Addrs (yaml empty AND env empty)",
			mutate: func(t *testing.T) string {
				t.Setenv("REDIS_ADDRS", "")
				return ""
			},
			wantInErr: "redis.addrs is required",
		},
		{
			name: "missing MQ.Driver (yaml explicit empty)",
			mutate: func(t *testing.T) string {
				t.Setenv("MQ_DRIVER", "")
				return writeYAML(t, "mq:\n  driver: \"\"\n")
			},
			wantInErr: "mq.driver is required",
		},
		{
			name: "missing MQ.NATS.URL when Driver=nats",
			mutate: func(t *testing.T) string {
				t.Setenv("MQ_DRIVER", "nats")
				t.Setenv("NATS_URL", "")
				return writeYAML(t, "mq:\n  driver: \"nats\"\n  nats:\n    url: \"\"\n")
			},
			wantInErr: "mq.nats.url is required",
		},
		{
			name: "missing Registry.NexusHubURL",
			mutate: func(t *testing.T) string {
				t.Setenv("NEXUS_HUB_URL", "")
				return writeYAML(t, "registry:\n  nexusHubUrl: \"\"\n")
			},
			wantInErr: "registry.nexusHubUrl is required",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setRequiredEnvBaseline(t)
			path := c.mutate(t)
			if path == "" {
				path = filepath.Join(t.TempDir(), "absent.yaml")
			}
			_, err := Load(path)
			if err == nil {
				t.Fatalf("expected error for %q; got nil", c.name)
			}
			if !strings.Contains(err.Error(), c.wantInErr) {
				t.Errorf("error should mention %q; got %q", c.wantInErr, err.Error())
			}
		})
	}
}

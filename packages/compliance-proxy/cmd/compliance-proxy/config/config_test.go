package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// validYAML is the minimal valid configuration covering all required fields.
const validYAML = `
listener:
  address: ":3128"
ca:
  certPath: "/etc/proxy/ca.crt"
  keyPath: "/etc/proxy/ca.key"
redis:
  address: "localhost:6379"
  dialTimeout: "5s"
  readTimeout: "3s"
  writeTimeout: "3s"
connections:
  maxConcurrentTunnels: 10000
  idleTimeout: "300s"
  shutdownGracePeriod: "30s"
upstream:
  maxConnsPerHost: 100
  idleConnTimeout: "90s"
  dialTimeout: "10s"
limits:
  requestBodyLimit: "10MB"
  responseBodyLimit: "10MB"
  sseBufferLimit: "8MB"
log:
  level: "info"
metrics:
  address: ":9090"
`

func writeTempYAML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

// setRequiredEnvBaseline stamps every env-side input that validate() now
// requires, so the test reaches the branch it actually wants to exercise.
// Tests that drive a specific required field empty MUST override after.
// Required set documented in validate() of this package.
func setRequiredEnvBaseline(t *testing.T) {
	t.Helper()
	t.Setenv("INTERNAL_SERVICE_TOKEN", "tok")
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("COMPLIANCE_PROXY_PUBLIC_URL", "http://localhost:3128")
	t.Setenv("REDIS_ADDRS", "localhost:6379")
	t.Setenv("MQ_DRIVER", "nats")
	t.Setenv("NATS_URL", "nats://localhost:4222")
	t.Setenv("NEXUS_HUB_URL", "http://localhost:3060")
}

func TestLoad_ValidConfig(t *testing.T) {
	setRequiredEnvBaseline(t)
	cfg, err := Load(writeTempYAML(t, validYAML))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Listener.Address != ":3128" {
		t.Errorf("listener.address = %q; want %q", cfg.Listener.Address, ":3128")
	}
	if cfg.CA.CertPath != "/etc/proxy/ca.crt" {
		t.Errorf("ca.certPath = %q; want %q", cfg.CA.CertPath, "/etc/proxy/ca.crt")
	}
	if cfg.CA.KeyPath != "/etc/proxy/ca.key" {
		t.Errorf("ca.keyPath = %q; want %q", cfg.CA.KeyPath, "/etc/proxy/ca.key")
	}
	if cfg.Connections.MaxConcurrentTunnels != 10000 {
		t.Errorf("maxConcurrentTunnels = %d; want 10000", cfg.Connections.MaxConcurrentTunnels)
	}
	if cfg.Log.Level != "info" {
		t.Errorf("log.level = %q; want %q", cfg.Log.Level, "info")
	}
}

// TestLoad_MissingFile asserts that an absent path is tolerated (defaults +
// env path) but Load surfaces a validate-time error when env baseline is not
// supplied. Mirrors the Hub/CP/AIG missing-file tolerance contract.
func TestLoad_MissingFile(t *testing.T) {
	// Deliberately no setRequiredEnvBaseline — defaults() alone cannot
	// satisfy validate (PublicURL etc. have no safe default).
	_, err := Load("/nonexistent/path/config.yaml")
	if err == nil {
		t.Fatal("expected validate error when missing file leaves required fields empty")
	}
}

// TestLoad_MissingFileWithFullEnv asserts the happy path: absent yaml file +
// full env baseline → Load returns a valid Config built from defaults+env.
func TestLoad_MissingFileWithFullEnv(t *testing.T) {
	setRequiredEnvBaseline(t)
	// Listener.Address + CA paths have no env override on this service —
	// supply them via t.Setenv on the same-named env vars is not wired,
	// so we provide a minimal yaml that fills them while baseline env
	// covers the rest of the required-set. Use a real file path.
	yaml := `
listener:
  address: ":3128"
ca:
  certPath: "/etc/proxy/ca.crt"
  keyPath: "/etc/proxy/ca.key"
`
	if _, err := Load(writeTempYAML(t, yaml)); err != nil {
		t.Fatalf("Load with full env baseline + minimal listener/ca yaml: %v", err)
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	p := writeTempYAML(t, "{{not yaml}}")
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestLoad_MissingListenerAddress(t *testing.T) {
	setRequiredEnvBaseline(t)
	yaml := `
listener:
  address: ""
ca:
  certPath: "/ca.crt"
  keyPath: "/ca.key"
`
	_, err := Load(writeTempYAML(t, yaml))
	if err == nil {
		t.Fatal("expected validation error for empty listener.address")
	}
}

func TestLoad_MissingCACertPath(t *testing.T) {
	setRequiredEnvBaseline(t)
	yaml := `
listener:
  address: ":3128"
ca:
  certPath: ""
  keyPath: "/ca.key"
`
	_, err := Load(writeTempYAML(t, yaml))
	if err == nil {
		t.Fatal("expected validation error for empty ca.certPath")
	}
}

func TestLoad_MissingCAKeyPath(t *testing.T) {
	setRequiredEnvBaseline(t)
	yaml := `
listener:
  address: ":3128"
ca:
  certPath: "/ca.crt"
  keyPath: ""
`
	_, err := Load(writeTempYAML(t, yaml))
	if err == nil {
		t.Fatal("expected validation error for empty ca.keyPath")
	}
}

// TestLoad_InvalidRedisDialTimeout proves yaml parsing rejects a non-duration
// for redis.dialTimeout. The historical separate "negative timeout" check
// moved into redisfactory.Config + go-redis (negative durations are treated
// as "no timeout" by the upstream library).
func TestLoad_InvalidRedisDialTimeout(t *testing.T) {
	setRequiredEnvBaseline(t)
	yaml := `
listener:
  address: ":3128"
ca:
  certPath: "/ca.crt"
  keyPath: "/ca.key"
redis:
  dialTimeout: "not-a-duration"
`
	_, err := Load(writeTempYAML(t, yaml))
	if err == nil {
		t.Fatal("expected yaml parse error for invalid redis.dialTimeout")
	}
}

func TestLoad_InvalidByteSize(t *testing.T) {
	setRequiredEnvBaseline(t)
	yaml := `
listener:
  address: ":3128"
ca:
  certPath: "/ca.crt"
  keyPath: "/ca.key"
limits:
  requestBodyLimit: "abc"
`
	_, err := Load(writeTempYAML(t, yaml))
	if err == nil {
		t.Fatal("expected validation error for invalid byte size")
	}
}

func TestLoad_InvalidLogLevel(t *testing.T) {
	setRequiredEnvBaseline(t)
	yaml := `
listener:
  address: ":3128"
ca:
  certPath: "/ca.crt"
  keyPath: "/ca.key"
log:
  level: "verbose"
`
	_, err := Load(writeTempYAML(t, yaml))
	if err == nil {
		t.Fatal("expected validation error for invalid log level")
	}
}

func TestParseByteSize(t *testing.T) {
	tests := []struct {
		input string
		want  int64
	}{
		{"100", 100},
		{"100B", 100},
		{"1KB", 1024},
		{"10MB", 10 * 1024 * 1024},
		{"2GB", 2 * 1024 * 1024 * 1024},
		{"1TB", 1024 * 1024 * 1024 * 1024},
		{"10mb", 10 * 1024 * 1024}, // case-insensitive
		{" 5 MB ", 5 * 1024 * 1024},
	}

	for _, tt := range tests {
		got, err := ParseByteSize(tt.input)
		if err != nil {
			t.Errorf("ParseByteSize(%q) error: %v", tt.input, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseByteSize(%q) = %d; want %d", tt.input, got, tt.want)
		}
	}
}

func TestParseByteSize_Invalid(t *testing.T) {
	invalid := []string{"", "abc", "10PB", "MB", "-5MB"}
	for _, s := range invalid {
		_, err := ParseByteSize(s)
		if err == nil {
			t.Errorf("ParseByteSize(%q) expected error", s)
		}
	}
}

func TestParseDuration(t *testing.T) {
	tests := []struct {
		input string
		want  time.Duration
	}{
		{"5s", 5 * time.Second},
		{"300ms", 300 * time.Millisecond},
		{"1m30s", 90 * time.Second},
		{"2h", 2 * time.Hour},
	}
	for _, tt := range tests {
		got, err := ParseDuration(tt.input)
		if err != nil {
			t.Errorf("ParseDuration(%q) error: %v", tt.input, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseDuration(%q) = %v; want %v", tt.input, got, tt.want)
		}
	}
}

func TestParseDuration_Invalid(t *testing.T) {
	_, err := ParseDuration("not-a-duration")
	if err == nil {
		t.Fatal("expected error for invalid duration")
	}
}

// TestLoad_EnvOverrides covers the four env-var override branches in
// Load() so a deploy override surfaces in the parsed Config. Without
// these the orchestrator's `INTERNAL_SERVICE_TOKEN` injection would
// remain untested at the unit level.
func TestLoad_EnvOverrides(t *testing.T) {
	setRequiredEnvBaseline(t)
	t.Setenv("NEXUS_HUB_URL", "https://override.example")
	t.Setenv("INTERNAL_SERVICE_TOKEN", "secret-token")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("LOG_FORMAT", "json")

	cfg, err := Load(writeTempYAML(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Registry.NexusHubURL != "https://override.example" {
		t.Errorf("NEXUS_HUB_URL override: got %q", cfg.Registry.NexusHubURL)
	}
	if cfg.Auth.InternalServiceToken != "secret-token" {
		t.Errorf("INTERNAL_SERVICE_TOKEN override: got %q", cfg.Auth.InternalServiceToken)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("LOG_LEVEL override: got %q", cfg.Log.Level)
	}
	if cfg.Log.Format != "json" {
		t.Errorf("LOG_FORMAT override: got %q", cfg.Log.Format)
	}
}

// TestLoad_NegativeBodySize covers the
// `if sz <= 0 { return ... must be > 0 }` branch in validate() for
// body-size limits. Without it, a zero value would only fail the
// regex check (which it doesn't — "0" parses cleanly to 0 bytes) and
// the proxy would start with an unenforceable limit.
func TestLoad_NegativeBodySize(t *testing.T) {
	setRequiredEnvBaseline(t)
	yaml := `
listener:
  address: ":3128"
ca:
  certPath: "/ca.crt"
  keyPath: "/ca.key"
limits:
  requestBodyLimit: "0B"
`
	_, err := Load(writeTempYAML(t, yaml))
	if err == nil {
		t.Fatal("expected validation error for zero body-size limit")
	}
}

// TestLoad_InvalidLogFormat covers the
// `log.format must be json or text` branch — Log.Format is
// optional, but if set to a garbage value the proxy must refuse.
func TestLoad_InvalidLogFormat(t *testing.T) {
	setRequiredEnvBaseline(t)
	yaml := `
listener:
  address: ":3128"
ca:
  certPath: "/ca.crt"
  keyPath: "/ca.key"
log:
  format: "yaml"
`
	_, err := Load(writeTempYAML(t, yaml))
	if err == nil {
		t.Fatal("expected validation error for unknown log.format")
	}
}

// (TestLoad_ComplianceStreamingModeValidation deleted in #115 —
// streamingMode yaml field is gone; admin policy drives mode now,
// validation lives in streampolicy.DecodeGlobalPolicy. Unknown yaml
// keys are silently ignored by yaml.v3, so leaving the field in an
// operator's pre-existing config is non-breaking.)

// TestParseByteSize_ParseIntFailure covers a regex-matched number that
// overflows int64 — without this, the strconv.ParseInt error branch
// stays uncovered.
func TestParseByteSize_ParseIntFailure(t *testing.T) {
	_, err := ParseByteSize("99999999999999999999999999B") // overflows int64
	if err == nil {
		t.Fatal("expected ParseInt overflow error")
	}
}

// _ = time.Second touch so the import is used (already imported above).

// TestValidate_RequiredFields cycles each new business-required field
// (PublicURL / Database.URL / Auth.InternalServiceToken / Redis.Addrs /
// MQ.Driver / MQ.NATS.URL / Registry.NexusHubURL) through its empty path
// and asserts validate trips with a contextual message. Listener/CA
// branches are covered by the older TestLoad_Missing* tests above.
func TestValidate_RequiredFields(t *testing.T) {
	type tc struct {
		name      string
		mutate    func(t *testing.T) string
		wantInErr string
	}
	// Every subtest writes a minimal valid yaml (with Listener+CA so the
	// older branches stay satisfied) and drives ONE new required field
	// empty. baseline-then-override pattern.
	minimalYAML := `
listener:
  address: ":3128"
ca:
  certPath: "/ca.crt"
  keyPath: "/ca.key"
`
	cases := []tc{
		{
			name: "missing PublicURL",
			mutate: func(t *testing.T) string {
				t.Setenv("COMPLIANCE_PROXY_PUBLIC_URL", "")
				return writeTempYAML(t, minimalYAML)
			},
			wantInErr: "publicURL is required",
		},
		{
			name: "missing Database.URL",
			mutate: func(t *testing.T) string {
				t.Setenv("DATABASE_URL", "")
				return writeTempYAML(t, minimalYAML)
			},
			wantInErr: "database.url is required",
		},
		{
			name: "missing Auth.InternalServiceToken",
			mutate: func(t *testing.T) string {
				t.Setenv("INTERNAL_SERVICE_TOKEN", "")
				return writeTempYAML(t, minimalYAML)
			},
			wantInErr: "auth.internalServiceToken is required",
		},
		{
			name: "missing Redis.Addrs",
			mutate: func(t *testing.T) string {
				t.Setenv("REDIS_ADDRS", "")
				return writeTempYAML(t, minimalYAML)
			},
			wantInErr: "redis.addrs is required",
		},
		{
			name: "missing MQ.Driver (yaml explicit empty)",
			mutate: func(t *testing.T) string {
				t.Setenv("MQ_DRIVER", "")
				yaml := minimalYAML + "mq:\n  driver: \"\"\n"
				return writeTempYAML(t, yaml)
			},
			wantInErr: "mq.driver is required",
		},
		{
			name: "missing MQ.NATS.URL when Driver=nats",
			mutate: func(t *testing.T) string {
				t.Setenv("MQ_DRIVER", "nats")
				t.Setenv("NATS_URL", "")
				yaml := minimalYAML + "mq:\n  driver: \"nats\"\n  nats:\n    url: \"\"\n"
				return writeTempYAML(t, yaml)
			},
			wantInErr: "mq.nats.url is required",
		},
		{
			name: "missing Registry.NexusHubURL",
			mutate: func(t *testing.T) string {
				t.Setenv("NEXUS_HUB_URL", "")
				yaml := minimalYAML + "registry:\n  nexusHubUrl: \"\"\n"
				return writeTempYAML(t, yaml)
			},
			wantInErr: "registry.nexusHubUrl is required",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setRequiredEnvBaseline(t)
			path := c.mutate(t)
			_, err := Load(path)
			if err == nil {
				t.Fatalf("expected error for %q; got nil", c.name)
			}
			// validate errors are wrapped with "config: validation:" by Load.
			if !strings.Contains(err.Error(), c.wantInErr) {
				t.Errorf("error should mention %q; got %q", c.wantInErr, err.Error())
			}
		})
	}
}

var _ = time.Second

package config

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setRequiredEnvBaseline stamps every env-side input that validate() now
// requires, so the test reaches the branch it actually wants to exercise.
// Tests that want to drive a specific required field empty MUST override
// it (via t.Setenv with "") AFTER calling this helper.
//
// Required fields are documented in HubConfig.validate; the set is:
//
//	PublicURL, Database.URL, Auth.InternalServiceToken, Auth.HubConfigToken,
//	Hub.ID, Redis.Addrs, MQ.Driver, (MQ.NATS.URL when Driver=="nats").
//
// Hub.ID is intentionally NOT stamped here — it auto-defaults via
// defaults() seeding from os.Hostname(); tests rarely need to override it.
func setRequiredEnvBaseline(t *testing.T) {
	t.Helper()
	t.Setenv("INTERNAL_SERVICE_TOKEN", "tok")
	// HubConfigToken is a required env input.
	t.Setenv("HUB_CONFIG_TOKEN", "hub-config-tok")
	// CREDENTIAL_ENCRYPTION_KEY is a required validate()
	// input (custody-resolved; encrypts alert-channel secrets at rest). Under the
	// default noop provider this plaintext passes through.
	t.Setenv("CREDENTIAL_ENCRYPTION_KEY", "test-credential-master-key")
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("NEXUS_HUB_PUBLIC_URL", "http://localhost:3060")
	t.Setenv("REDIS_ADDRS", "localhost:6379")
	t.Setenv("MQ_DRIVER", "nats")
	t.Setenv("NATS_URL", "nats://localhost:4222")
}

// TestLoad_SecretCustody_CommandUnwrapsCredKey pins the secret-custody wiring
// for Hub: with secretCustody.provider="command", Load() resolves the shared
// crown jewel CREDENTIAL_ENCRYPTION_KEY as a base64 wrapped blob and unwraps it
// once at boot into Auth.CredentialMasterKey. `cat {file}` is an identity
// decrypt, so a base64-encoded plaintext round-trips — proving Hub routes the
// key through custody rather than reading it raw at the alert cipher, so a
// wrapped env var no longer makes Hub read a blob as 64-hex and fail to boot.
func TestLoad_SecretCustody_CommandUnwrapsCredKey(t *testing.T) {
	setRequiredEnvBaseline(t)
	t.Setenv("CREDENTIAL_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte("unwrapped-cred-key")))

	dir := t.TempDir()
	p := filepath.Join(dir, "cfg.yaml")
	if err := os.WriteFile(p, []byte("secretCustody:\n  provider: command\n  command: [\"cat\", \"{file}\"]\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.CredentialMasterKey != "unwrapped-cred-key" {
		t.Errorf("CredentialMasterKey = %q, want the unwrapped plaintext", cfg.Auth.CredentialMasterKey)
	}
}

// TestLoad_SecretCustody_NoopRawPassthrough proves the default provider returns
// the raw env value byte-identically (dev path) — non-breaking when unconfigured.
func TestLoad_SecretCustody_NoopRawPassthrough(t *testing.T) {
	setRequiredEnvBaseline(t)
	t.Setenv("CREDENTIAL_ENCRYPTION_KEY", "raw-plain-key")

	dir := t.TempDir()
	p := filepath.Join(dir, "cfg.yaml")
	_ = os.WriteFile(p, []byte("{}\n"), 0o644)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.CredentialMasterKey != "raw-plain-key" {
		t.Errorf("CredentialMasterKey = %q, want raw env passthrough", cfg.Auth.CredentialMasterKey)
	}
}

// TestLoad_SecretCustody_CommandFailClosed: under provider=command a crown jewel
// that is not a valid wrapped blob aborts boot rather than feeding ciphertext on
// to the alert cipher.
func TestLoad_SecretCustody_CommandFailClosed(t *testing.T) {
	setRequiredEnvBaseline(t)
	t.Setenv("CREDENTIAL_ENCRYPTION_KEY", "not-valid-base64!!")

	dir := t.TempDir()
	p := filepath.Join(dir, "cfg.yaml")
	_ = os.WriteFile(p, []byte("secretCustody:\n  provider: command\n  command: [\"cat\", \"{file}\"]\n"), 0o644)
	if _, err := Load(p); err == nil {
		t.Fatal("expected fail-closed error for an unwrappable crown jewel under provider=command")
	}
}

// TestLoad_SecretCustody_UnknownProviderFailsClosed: an unrecognised provider is
// a typo that must abort the boot, never silently fall back to raw-env (which
// would defeat custody). NewCustody rejects it inside resolveCustodySecrets.
func TestLoad_SecretCustody_UnknownProviderFailsClosed(t *testing.T) {
	setRequiredEnvBaseline(t)

	dir := t.TempDir()
	p := filepath.Join(dir, "cfg.yaml")
	_ = os.WriteFile(p, []byte("secretCustody:\n  provider: bogus\n"), 0o644)
	if _, err := Load(p); err == nil {
		t.Fatal("expected fail-closed error for an unknown secretCustody provider")
	}
}

func TestLoadValidYAML(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(p, []byte(`
server:
  port: 4060
database:
  url: "postgres://test:test@localhost:5432/test"
  maxConns: 10
hub:
  id: "hub-test"
`), 0644)
	// auth.internalServiceToken is env-only per the "Secrets are env-only"
	// binding — yaml field is ignored, must be supplied via env.
	setRequiredEnvBaseline(t)
	t.Setenv("INTERNAL_SERVICE_TOKEN", "test-token")

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.Port != 4060 {
		t.Errorf("port = %d, want 4060", cfg.Server.Port)
	}
	if cfg.Database.MaxConns != 10 {
		t.Errorf("maxConns = %d, want 10", cfg.Database.MaxConns)
	}
	if cfg.Hub.ID != "hub-test" {
		t.Errorf("hub.id = %q, want %q", cfg.Hub.ID, "hub-test")
	}
}

func TestDefaults(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(p, []byte(`
database:
  url: "postgres://test:test@localhost:5432/test"
`), 0644)
	setRequiredEnvBaseline(t)

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.Port != 3060 {
		t.Errorf("default port = %d, want 3060", cfg.Server.Port)
	}
	if cfg.Database.MaxConns != 20 {
		t.Errorf("default maxConns = %d, want 20", cfg.Database.MaxConns)
	}
	if cfg.Scheduler.Enabled != true {
		t.Error("default scheduler.enabled should be true")
	}
	if cfg.Scheduler.DriftCheckInterval != 60*time.Second {
		t.Errorf("default driftCheckInterval = %v, want 60s", cfg.Scheduler.DriftCheckInterval)
	}
	if cfg.Scheduler.OverrideExpiryInterval != 60*time.Second {
		t.Errorf("default overrideExpiryInterval = %v, want 60s", cfg.Scheduler.OverrideExpiryInterval)
	}
	if cfg.Scheduler.AuditChainVerifyInterval != time.Hour {
		t.Errorf("default auditChainVerifyInterval = %v, want 1h", cfg.Scheduler.AuditChainVerifyInterval)
	}
	if cfg.Log.Level != "info" {
		t.Errorf("default log.level = %q, want %q", cfg.Log.Level, "info")
	}
	if cfg.Consumers.BatchSize != 2000 {
		t.Errorf("default consumers.batchSize = %d, want 2000 (shipped optimum, pairs with COPY)", cfg.Consumers.BatchSize)
	}
}

func TestEnvVarOverride(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(p, []byte(`
database:
  url: "postgres://test:test@localhost:5432/test"
`), 0644)

	setRequiredEnvBaseline(t)
	t.Setenv("NEXUS_HUB_PORT", "5060")
	t.Setenv("NEXUS_HUB_ID", "hub-env")

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.Port != 5060 {
		t.Errorf("port = %d, want 5060 (from env)", cfg.Server.Port)
	}
	if cfg.Hub.ID != "hub-env" {
		t.Errorf("hub.id = %q, want %q", cfg.Hub.ID, "hub-env")
	}
}

func TestServerBindAddr(t *testing.T) {
	// Empty Host → all interfaces (":port"), the historical default that
	// container / Kubernetes / direct deployments rely on.
	if got := (ServerConfig{Port: 3060}).BindAddr(); got != ":3060" {
		t.Errorf("empty Host BindAddr = %q, want \":3060\"", got)
	}
	// Explicit loopback (appliance) → host:port.
	if got := (ServerConfig{Host: "127.0.0.1", Port: 3060}).BindAddr(); got != "127.0.0.1:3060" {
		t.Errorf("loopback BindAddr = %q, want \"127.0.0.1:3060\"", got)
	}
}

func TestEnvVarOverride_Host(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(p, []byte("database:\n  url: \"postgres://test@localhost/test\"\n"), 0644)

	setRequiredEnvBaseline(t)
	t.Setenv("NEXUS_HUB_HOST", "127.0.0.1")

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.Host != "127.0.0.1" {
		t.Errorf("Server.Host = %q, want 127.0.0.1 (from NEXUS_HUB_HOST)", cfg.Server.Host)
	}
}

// TestDevModeDefaultsFalseAndEnvEnables covers the dev-mode default at the config layer: the
// Hub WebSocket dev-mode (which relaxes the origin allowlist to accept
// localhost) must default to false so production never auto-allows localhost,
// and must be enabled only via an explicit NEXUS_HUB_DEV_MODE=true.
func TestDevModeDefaultsFalseAndEnvEnables(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(p, []byte("database:\n  url: \"postgres://test@localhost/test\"\n"), 0644)

	setRequiredEnvBaseline(t)
	// No NEXUS_HUB_DEV_MODE set → default must be false (production-safe).
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Hub.DevMode {
		t.Error("Hub.DevMode must default to false (production-safe origin policy)")
	}

	t.Setenv("NEXUS_HUB_DEV_MODE", "true")
	cfg, err = Load(p)
	if err != nil {
		t.Fatalf("Load with NEXUS_HUB_DEV_MODE=true: %v", err)
	}
	if !cfg.Hub.DevMode {
		t.Error("NEXUS_HUB_DEV_MODE=true must enable Hub.DevMode")
	}
}

func TestMissingDatabaseURL(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(p, []byte(`
database:
  url: ""
`), 0644)
	setRequiredEnvBaseline(t)
	// Drive database.url empty; baseline DATABASE_URL would otherwise satisfy it.
	t.Setenv("DATABASE_URL", "")

	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for missing database.url")
	}
	if !strings.Contains(err.Error(), "database.url is required") {
		t.Errorf("error should mention database.url; got %q", err.Error())
	}
}

func TestMissingAuthToken(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(p, []byte(`
database:
  url: "postgres://localhost/test"
`), 0644)
	setRequiredEnvBaseline(t)
	// Drive the token empty AFTER baseline so every other required field is
	// satisfied — validate must still trip on the missing token.
	t.Setenv("INTERNAL_SERVICE_TOKEN", "")

	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for missing INTERNAL_SERVICE_TOKEN")
	}
}

// TestMissingHubConfigToken pins the fail-closed boot guard:
// without HUB_CONFIG_TOKEN, Hub must refuse to start rather than fall back to
// ServiceAuth("") (which would accept an empty bearer and open the config-write
// surface). The error must name the missing field.
func TestMissingHubConfigToken(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(p, []byte(`
database:
  url: "postgres://localhost/test"
`), 0644)
	setRequiredEnvBaseline(t)
	t.Setenv("HUB_CONFIG_TOKEN", "")

	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for missing HUB_CONFIG_TOKEN")
	}
	if !strings.Contains(err.Error(), "hubConfigToken") {
		t.Fatalf("error should name hubConfigToken; got %q", err.Error())
	}
}

// TestMissingCredentialMasterKey is the regression test: Hub's
// validate() now fail-closes when CREDENTIAL_ENCRYPTION_KEY is unset (it always
// wires the alert cipher), gating it at config load for symmetry with CP + ai-gw
// rather than only at InitAlerts.
func TestMissingCredentialMasterKey(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(p, []byte(`
database:
  url: "postgres://localhost/test"
`), 0644)
	setRequiredEnvBaseline(t)
	t.Setenv("CREDENTIAL_ENCRYPTION_KEY", "")

	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for missing CREDENTIAL_ENCRYPTION_KEY")
	}
	if !strings.Contains(err.Error(), "credentialMasterKey") {
		t.Fatalf("error should name credentialMasterKey; got %q", err.Error())
	}
}

func TestInvalidYAML(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(p, []byte(`{invalid yaml::::`), 0644)

	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestHubIDDefault(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(p, []byte(`
database:
  url: "postgres://localhost/test"
`), 0644)
	setRequiredEnvBaseline(t)

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	hostname, _ := os.Hostname()
	want := "hub-" + hostname
	if cfg.Hub.ID != want {
		t.Errorf("hub.id = %q, want %q", cfg.Hub.ID, want)
	}
}

func TestSchedulerDisabledViaEnv(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(p, []byte(`
database:
  url: "postgres://localhost/test"
`), 0644)

	setRequiredEnvBaseline(t)
	t.Setenv("NEXUS_HUB_SCHEDULER_ENABLED", "false")

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Scheduler.Enabled {
		t.Error("scheduler.enabled should be false when env=false")
	}
}

func TestMissingFileUsesDefaults(t *testing.T) {
	setRequiredEnvBaseline(t)

	cfg, err := Load("/nonexistent/path/config.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.Port != 3060 {
		t.Errorf("port = %d, want 3060", cfg.Server.Port)
	}
}

// TestParseIntEnv_AbsentNoOp covers the `v := os.Getenv(...)` empty
// branch — when the env var is unset, parseIntEnv must leave dst
// unchanged so YAML defaults survive.
func TestParseIntEnv_AbsentNoOp(t *testing.T) {
	t.Setenv("UNUSED_INT_VAR", "")
	want := 42
	got := want
	parseIntEnv("UNUSED_INT_VAR", &got)
	if got != want {
		t.Errorf("parseIntEnv with empty env should leave dst unchanged; got %d, want %d", got, want)
	}
}

// TestParseIntEnv_ValidOverwrites covers the success branch — env
// var present + parses cleanly → dst overwritten.
func TestParseIntEnv_ValidOverwrites(t *testing.T) {
	t.Setenv("USED_INT_VAR", "99")
	got := 0
	parseIntEnv("USED_INT_VAR", &got)
	if got != 99 {
		t.Errorf("parseIntEnv valid: got %d, want 99", got)
	}
}

// TestParseIntEnv_InvalidNoOp covers the strconv.Atoi err branch — a
// malformed value must be silently ignored (NOT crash startup), so
// dst keeps its prior value.
func TestParseIntEnv_InvalidNoOp(t *testing.T) {
	t.Setenv("BAD_INT_VAR", "not-an-int")
	want := 7
	got := want
	parseIntEnv("BAD_INT_VAR", &got)
	if got != want {
		t.Errorf("parseIntEnv with malformed env should leave dst unchanged; got %d, want %d", got, want)
	}
}

// TestParseDurationEnv_EveryBranch mirrors parseIntEnv coverage for
// the duration variant: absent / valid / malformed.
func TestParseDurationEnv_EveryBranch(t *testing.T) {
	t.Run("absent leaves default", func(t *testing.T) {
		t.Setenv("UNUSED_DUR_VAR", "")
		want := 5 * time.Minute
		got := want
		parseDurationEnv("UNUSED_DUR_VAR", &got)
		if got != want {
			t.Errorf("got %v, want %v", got, want)
		}
	})
	t.Run("valid overwrites", func(t *testing.T) {
		t.Setenv("USED_DUR_VAR", "1h30m")
		got := time.Duration(0)
		parseDurationEnv("USED_DUR_VAR", &got)
		if got != 90*time.Minute {
			t.Errorf("got %v, want 1h30m", got)
		}
	})
	t.Run("malformed leaves default", func(t *testing.T) {
		t.Setenv("BAD_DUR_VAR", "notaduration")
		want := 10 * time.Second
		got := want
		parseDurationEnv("BAD_DUR_VAR", &got)
		if got != want {
			t.Errorf("malformed dur should leave dst unchanged; got %v, want %v", got, want)
		}
	})
}

// TestLoad_ReadFileNonNotExistError pins the os.ReadFile error branch
// where the error is something OTHER than os.IsNotExist (e.g. the path
// resolves to a directory, which yields "is a directory" / EISDIR rather
// than ENOENT). The defaults path is only taken for IsNotExist; every
// other I/O error must be wrapped + surfaced so an operator misconfig
// (wrong path pointing at a dir, permission-denied) cannot silently fall
// back to defaults.
func TestLoad_ReadFileNonNotExistError(t *testing.T) {
	dir := t.TempDir()
	// Use the directory itself as the "file" path — os.ReadFile returns
	// a non-IsNotExist error (EISDIR on Unix) which exercises the
	// return-error branch in Load.
	setRequiredEnvBaseline(t)

	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected error when config path is a directory, got nil")
	}
	// The error must be wrapped with the "read config" context so ops
	// can distinguish read failures from parse/validate failures.
	if !strings.Contains(err.Error(), "read config") {
		t.Errorf("error should mention 'read config'; got %q", err.Error())
	}
}

// TestValidate_MissingHubID pins the third validate branch — hub.id
// empty must fail. Because defaults() seeds hub.id from os.Hostname(),
// the only way to drive it empty is to set yaml hub.id to "" AND
// override hostname (impossible) — so we feed the yaml-empty case via
// explicit yaml AND assert env override path leaves it empty. We rely
// on yaml's explicit empty string overwriting the default.
func TestValidate_MissingHubID(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	// Explicit empty hub.id in yaml overwrites the hostname-seeded default.
	_ = os.WriteFile(p, []byte(`
database:
  url: "postgres://localhost/test"
hub:
  id: ""
`), 0644)
	setRequiredEnvBaseline(t)
	// Ensure no env override resurrects it.
	t.Setenv("NEXUS_HUB_ID", "")

	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for empty hub.id")
	}
	if !strings.Contains(err.Error(), "hub.id is required") {
		t.Errorf("error should mention hub.id; got %q", err.Error())
	}
}

// TestEnvOverrides_AllStringFields walks every string-field env override
// in applyEnvOverrides so each `if v := os.Getenv(...)` true-branch is
// observed end-to-end through Load. We use the file-missing path so the
// yaml step is a no-op; defaults() + env overrides + validate are the
// only stages exercised. Each assertion checks the env value actually
// landed in the config struct — bare presence is not enough.
func TestEnvOverrides_AllStringFields(t *testing.T) {
	// Database / Redis / MQ / NATS — infra URLs. validate() requires
	// REDIS_ADDRS too (yaml OR env); REDIS_* knobs themselves are consumed by
	// redisfactory.LoadEnv at wiring time, but the presence-check fires here.
	setRequiredEnvBaseline(t)
	t.Setenv("INTERNAL_SERVICE_TOKEN", "the-token")
	t.Setenv("HUB_CONFIG_TOKEN", "the-config-token")
	t.Setenv("DATABASE_URL", "postgres://envhost:5432/envdb")
	t.Setenv("NEXUS_HUB_PUBLIC_URL", "https://hub.env.example")
	t.Setenv("MQ_DRIVER", "nats-env")
	t.Setenv("NATS_URL", "nats://envhost:4222")

	// AuthServer (shared OAuth/OIDC URLs/issuer — non-secret yaml fields, env-overridable).
	t.Setenv("AUTH_SERVER_JWKS_URL", "https://cp.env/.well-known/jwks.json")
	t.Setenv("AUTH_SERVER_ISSUER", "https://cp.env/")
	t.Setenv("AUTH_SERVER_URL", "https://cp.env")

	// Log.
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("LOG_FORMAT", "text")

	// Hub identity.
	t.Setenv("NEXUS_HUB_ID", "hub-from-env")
	t.Setenv("NEXUS_HUB_ADVERTISE_ADDR", "10.0.0.5:3060")
	t.Setenv("NEXUS_HUB_ALLOWED_ORIGINS", " https://a.example , https://b.example ")

	// AgentCA paths.
	t.Setenv("AGENT_CA_CERT_FILE", "/etc/nexus/ca.crt")
	t.Setenv("AGENT_CA_KEY_FILE", "/etc/nexus/ca.key")
	t.Setenv("AGENT_CA_DIR", "/etc/nexus/agentca")

	cfg, err := Load("/nonexistent/path/config.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	checks := []struct {
		name string
		got  string
		want string
	}{
		{"PublicURL", cfg.PublicURL, "https://hub.env.example"},
		{"Database.URL", cfg.Database.URL, "postgres://envhost:5432/envdb"},
		{"MQ.Driver", cfg.MQ.Driver, "nats-env"},
		{"MQ.NATS.URL", cfg.MQ.NATS.URL, "nats://envhost:4222"},
		{"Auth.InternalServiceToken", cfg.Auth.InternalServiceToken, "the-token"},
		{"Auth.HubConfigToken", cfg.Auth.HubConfigToken, "the-config-token"},
		{"AuthServer.JWKSURL", cfg.AuthServer.JWKSURL, "https://cp.env/.well-known/jwks.json"},
		{"AuthServer.Issuer", cfg.AuthServer.Issuer, "https://cp.env/"},
		{"AuthServer.URL", cfg.AuthServer.URL, "https://cp.env"},
		{"Log.Level", cfg.Log.Level, "debug"},
		{"Log.Format", cfg.Log.Format, "text"},
		{"Hub.ID", cfg.Hub.ID, "hub-from-env"},
		{"Hub.AdvertiseAddr", cfg.Hub.AdvertiseAddr, "10.0.0.5:3060"},
		{"AgentCA.CertFile", cfg.AgentCA.CertFile, "/etc/nexus/ca.crt"},
		{"AgentCA.KeyFile", cfg.AgentCA.KeyFile, "/etc/nexus/ca.key"},
		{"AgentCA.Dir", cfg.AgentCA.Dir, "/etc/nexus/agentca"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}

	// Allowed origins is parsed via splitAndTrim — verify the parsed slice
	// (whitespace stripped, empties dropped). This guards the
	// NEXUS_HUB_ALLOWED_ORIGINS env override.
	want := []string{"https://a.example", "https://b.example"}
	if len(cfg.Hub.AllowedOrigins) != len(want) {
		t.Fatalf("AllowedOrigins len = %d (%v), want %d (%v)",
			len(cfg.Hub.AllowedOrigins), cfg.Hub.AllowedOrigins, len(want), want)
	}
	for i, v := range want {
		if cfg.Hub.AllowedOrigins[i] != v {
			t.Errorf("AllowedOrigins[%d] = %q, want %q", i, cfg.Hub.AllowedOrigins[i], v)
		}
	}
}

// TestEnvOverrides_SchedulerEnabledTrueBranch pins the `=="1"` path of
// the scheduler-enabled override; the existing TestSchedulerDisabledViaEnv
// only covers the false case. We disable via yaml first then re-enable
// via env using the alternate "1" syntax (NOT "true") to exercise the
// second arm of the OR.
func TestEnvOverrides_SchedulerEnabledTrueBranch(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(p, []byte(`
database:
  url: "postgres://localhost/test"
scheduler:
  enabled: false
`), 0644)
	setRequiredEnvBaseline(t)
	t.Setenv("NEXUS_HUB_SCHEDULER_ENABLED", "1")

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Scheduler.Enabled {
		t.Error("scheduler.enabled should be true when env=1")
	}
}

// TestEnvOverrides_PortInvalidIgnored exercises the strconv.Atoi-error
// arm of NEXUS_HUB_PORT — a malformed value must NOT crash startup and
// must leave the yaml/default port intact. Matches the silent-ignore
// pattern parseIntEnv uses for the same class of failures.
func TestEnvOverrides_PortInvalidIgnored(t *testing.T) {
	setRequiredEnvBaseline(t)
	t.Setenv("NEXUS_HUB_PORT", "not-a-number")

	cfg, err := Load("/nonexistent/path/config.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Port != 3060 {
		t.Errorf("port = %d, want 3060 (default preserved on malformed env)", cfg.Server.Port)
	}
}

// TestEnvOverrides_RetentionAndIntervals pins a representative cross-section
// of parseIntEnv (retention days) + parseDurationEnv (per-job intervals)
// overrides driven through applyEnvOverrides. Verifies the wiring between
// each env name and the destination field (a typo in the dest pointer is
// the most likely real bug here).
func TestEnvOverrides_RetentionAndIntervals(t *testing.T) {
	setRequiredEnvBaseline(t)

	// One example from each retention knob.
	t.Setenv("NEXUS_HUB_RETENTION_TRAFFIC_EVENT_DAYS", "180")
	t.Setenv("NEXUS_HUB_RETENTION_TRAFFIC_EVENT_PAYLOAD_DAYS", "60")
	t.Setenv("NEXUS_HUB_RETENTION_ADMIN_AUDIT_DAYS", "730")
	t.Setenv("NEXUS_HUB_RETENTION_ROLLUP_5M_DAYS", "14")
	t.Setenv("NEXUS_HUB_RETENTION_ROLLUP_1H_DAYS", "180")
	t.Setenv("NEXUS_HUB_RETENTION_ROLLUP_1D_DAYS", "730")
	t.Setenv("NEXUS_HUB_RETENTION_ROLLUP_1MO_DAYS", "3650")

	// One example from each interval knob.
	t.Setenv("NEXUS_HUB_SCHEDULER_METRICS_ROLLUP_INTERVAL", "2h")
	t.Setenv("NEXUS_HUB_SCHEDULER_DATA_RETENTION_INTERVAL", "12h")
	t.Setenv("NEXUS_HUB_SCHEDULER_ROLLUP_5M_INTERVAL", "30s")
	t.Setenv("NEXUS_HUB_SCHEDULER_MERGE_1H_INTERVAL", "10m")
	t.Setenv("NEXUS_HUB_SCHEDULER_MERGE_1D_INTERVAL", "2h")
	t.Setenv("NEXUS_HUB_SCHEDULER_MERGE_1MO_INTERVAL", "48h")
	t.Setenv("NEXUS_HUB_SCHEDULER_ROLLUP_CORRECTION_INTERVAL", "12h")
	t.Setenv("NEXUS_HUB_SCHEDULER_ROLLUP_RETENTION_INTERVAL", "6h")
	t.Setenv("NEXUS_HUB_SCHEDULER_QUOTA_ALERT_INTERVAL", "30s")
	t.Setenv("NEXUS_HUB_SCHEDULER_VK_EXPIRY_INTERVAL", "2h")
	t.Setenv("NEXUS_HUB_SCHEDULER_EXEMPTION_GC_INTERVAL", "10m")
	t.Setenv("NEXUS_HUB_SCHEDULER_THING_OFFLINE_ALERTS_INTERVAL", "30s")
	t.Setenv("NEXUS_HUB_SCHEDULER_PROVIDER_UNAVAILABLE_ALERTS_INTERVAL", "45s")
	t.Setenv("NEXUS_HUB_SCHEDULER_OPS_ROLLUP_1H_INTERVAL", "10m")
	t.Setenv("NEXUS_HUB_SCHEDULER_OPS_ROLLUP_1D_INTERVAL", "2h")
	t.Setenv("NEXUS_HUB_SCHEDULER_OPS_ROLLUP_1MO_INTERVAL", "48h")
	t.Setenv("NEXUS_HUB_SCHEDULER_OPS_RETENTION_INTERVAL", "12h")
	t.Setenv("NEXUS_HUB_SCHEDULER_DIAG_MODE_EXPIRY_INTERVAL", "30s")
	t.Setenv("NEXUS_HUB_SCHEDULER_CACHE_QUALITY_MONITOR_INTERVAL", "10m")
	t.Setenv("NEXUS_HUB_SCHEDULER_CREDENTIAL_EXPIRY_INTERVAL", "2h")
	t.Setenv("NEXUS_HUB_SCHEDULER_CREDENTIAL_STATS_FLUSH_INTERVAL", "120s")
	t.Setenv("NEXUS_HUB_SCHEDULER_CREDENTIAL_CIRCUIT_FLUSH_INTERVAL", "15s")
	t.Setenv("NEXUS_HUB_SCHEDULER_CREDENTIAL_HEALTH_ROLLUP_INTERVAL", "10m")
	t.Setenv("NEXUS_HUB_SCHEDULER_CREDENTIAL_RELIABILITY_ALERTS_INTERVAL", "30s")
	t.Setenv("NEXUS_HUB_SCHEDULER_CREDENTIAL_RETIRE_INTERVAL", "30m")

	cfg, err := Load("/nonexistent/path/config.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r := cfg.Scheduler.Retention
	if r.TrafficEventDays != 180 || r.TrafficEventPayloadDays != 60 ||
		r.AdminAuditLogDays != 730 || r.Rollup5mDays != 14 ||
		r.Rollup1hDays != 180 || r.Rollup1dDays != 730 || r.Rollup1moDays != 3650 {
		t.Errorf("retention env-overrides not all applied; got %+v", r)
	}
	iv := cfg.Scheduler.Intervals
	type fc struct {
		name string
		got  time.Duration
		want time.Duration
	}
	wants := []fc{
		{"MetricsRollup", iv.MetricsRollup, 2 * time.Hour},
		{"DataRetention", iv.DataRetention, 12 * time.Hour},
		{"Rollup5m", iv.Rollup5m, 30 * time.Second},
		{"Merge1h", iv.Merge1h, 10 * time.Minute},
		{"Merge1d", iv.Merge1d, 2 * time.Hour},
		{"Merge1mo", iv.Merge1mo, 48 * time.Hour},
		{"RollupCorrection", iv.RollupCorrection, 12 * time.Hour},
		{"RollupRetention", iv.RollupRetention, 6 * time.Hour},
		{"QuotaAlertCheck", iv.QuotaAlertCheck, 30 * time.Second},
		{"VKExpiry", iv.VKExpiry, 2 * time.Hour},
		{"ExemptionGC", iv.ExemptionGC, 10 * time.Minute},
		{"ThingOfflineAlerts", iv.ThingOfflineAlerts, 30 * time.Second},
		{"ProviderUnavailableAlerts", iv.ProviderUnavailableAlerts, 45 * time.Second},
		{"OpsRollup1h", iv.OpsRollup1h, 10 * time.Minute},
		{"OpsRollup1d", iv.OpsRollup1d, 2 * time.Hour},
		{"OpsRollup1mo", iv.OpsRollup1mo, 48 * time.Hour},
		{"OpsRetention", iv.OpsRetention, 12 * time.Hour},
		{"CacheQualityMonitor", iv.CacheQualityMonitor, 10 * time.Minute},
		{"CredentialExpiry", iv.CredentialExpiry, 2 * time.Hour},
		{"CredentialStatsFlush", iv.CredentialStatsFlush, 120 * time.Second},
		{"CredentialCircuitFlush", iv.CredentialCircuitFlush, 15 * time.Second},
		{"CredentialHealthRollup", iv.CredentialHealthRollup, 10 * time.Minute},
		{"CredentialReliabilityAlerts", iv.CredentialReliabilityAlerts, 30 * time.Second},
		{"CredentialRetire", iv.CredentialRetire, 30 * time.Minute},
	}
	for _, w := range wants {
		if w.got != w.want {
			t.Errorf("Intervals.%s = %v, want %v", w.name, w.got, w.want)
		}
	}
}

// TestValidate_MissingPublicURL pins the first branch of validate — an
// empty PublicURL must fail because it's reported to the Thing Registry as
// staticInfo + surfaced in the admin UI for service-discovery.
func TestValidate_MissingPublicURL(t *testing.T) {
	setRequiredEnvBaseline(t)
	// Drive PublicURL empty after baseline; everything else satisfied.
	t.Setenv("NEXUS_HUB_PUBLIC_URL", "")

	_, err := Load("/nonexistent/path/config.yaml")
	if err == nil {
		t.Fatal("expected error for empty PublicURL")
	}
	if !strings.Contains(err.Error(), "publicURL is required") {
		t.Errorf("error should mention publicURL; got %q", err.Error())
	}
}

// TestValidate_MissingRedis pins the Redis required-presence branch.
// validate reads either cfg.Redis.Addrs (yaml) OR REDIS_ADDRS (env) —
// mirrors the env-merge contract redisfactory uses at wiring time.
// defaults() seeds Redis.Addrs with localhost, so we need an explicit
// empty list in yaml to drive the validate branch (env-override only
// fires on non-empty, so an empty env cannot clear the default).
func TestValidate_MissingRedis(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(p, []byte(`
redis:
  addrs: []
`), 0644)
	setRequiredEnvBaseline(t)
	t.Setenv("REDIS_ADDRS", "")

	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for missing redis.addrs")
	}
	if !strings.Contains(err.Error(), "redis.addrs is required") {
		t.Errorf("error should mention redis.addrs; got %q", err.Error())
	}
}

// TestValidate_MissingMQDriver pins the MQ.Driver required branch. Without
// a driver the Hub cannot publish nexus.hub.signal cross-Hub events nor
// receive Cat A shadow change-signals. defaults() seeds Driver="nats",
// so yaml must explicitly clear it.
func TestValidate_MissingMQDriver(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(p, []byte(`
mq:
  driver: ""
`), 0644)
	setRequiredEnvBaseline(t)
	t.Setenv("MQ_DRIVER", "")

	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for missing mq.driver")
	}
	if !strings.Contains(err.Error(), "mq.driver is required") {
		t.Errorf("error should mention mq.driver; got %q", err.Error())
	}
}

// TestValidate_MissingNATSURL pins the conditional MQ.NATS.URL branch —
// only required when driver=="nats". defaults() seeds NATS.URL=
// nats://localhost:4222, so yaml must explicitly clear it.
func TestValidate_MissingNATSURL(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(p, []byte(`
mq:
  driver: "nats"
  nats:
    url: ""
`), 0644)
	setRequiredEnvBaseline(t)
	t.Setenv("MQ_DRIVER", "nats")
	t.Setenv("NATS_URL", "")

	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for missing mq.nats.url when driver=nats")
	}
	if !strings.Contains(err.Error(), "mq.nats.url is required") {
		t.Errorf("error should mention mq.nats.url; got %q", err.Error())
	}
}

// TestSplitAndTrim_Cases covers all branches: empty input,
// whitespace-only entries, mix of valid + empty after trim.
// Used to expand NEXUS_HUB_ALLOWED_ORIGINS — silent skips on empty
// matter because an empty value must NOT admit "" as an origin.
func TestSplitAndTrim_Cases(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", []string{}},
		{",,", []string{}},
		{" , , ", []string{}},
		{"a,b,c", []string{"a", "b", "c"}},
		{" a , b ,, c ", []string{"a", "b", "c"}},
	}
	for _, tc := range cases {
		got := splitAndTrim(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("splitAndTrim(%q) len = %d, want %d (%v)", tc.in, len(got), len(tc.want), got)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("splitAndTrim(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

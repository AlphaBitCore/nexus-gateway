package config

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLoad_SecretCustody_CommandUnwrapsCrownJewel pins the SEC-W2-03 Layer C
// wiring: with secretCustody.provider="command", Load() resolves the crown-jewel
// env vars as base64 wrapped blobs and unwraps each once at boot. `cat {file}` is
// an identity decrypt, so a base64-encoded plaintext round-trips into the config
// field — proving Load routes CREDENTIAL_ENCRYPTION_KEY / ADMIN_KEY_HMAC_SECRET
// through the custody provider rather than reading them raw.
func TestLoad_SecretCustody_CommandUnwrapsCrownJewel(t *testing.T) {
	clearAllEnv(t)
	setRequiredEnvBaseline(t)
	// Crown jewels arrive as base64 "wrapped" blobs; `cat {file}` returns them
	// verbatim, so the plaintext is whatever we base64-encoded.
	t.Setenv("CREDENTIAL_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte("unwrapped-cred-key")))
	t.Setenv("ADMIN_KEY_HMAC_SECRET", base64.StdEncoding.EncodeToString([]byte("unwrapped-hmac")))

	dir := t.TempDir()
	p := filepath.Join(dir, "cfg.yaml")
	if err := os.WriteFile(p, []byte("secretCustody:\n  provider: command\n  command: [\"cat\", \"{file}\"]\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Crypto.EncryptionKey != "unwrapped-cred-key" {
		t.Errorf("EncryptionKey = %q, want the unwrapped plaintext", cfg.Crypto.EncryptionKey)
	}
	if cfg.Auth.HMACSecret != "unwrapped-hmac" {
		t.Errorf("HMACSecret = %q, want the unwrapped plaintext", cfg.Auth.HMACSecret)
	}
}

// TestLoad_SecretCustody_CommandFailClosed: under provider=command a crown jewel
// that is not a valid wrapped blob aborts boot rather than silently treating the
// ciphertext as plaintext.
func TestLoad_SecretCustody_CommandFailClosed(t *testing.T) {
	clearAllEnv(t)
	setRequiredEnvBaseline(t)
	t.Setenv("CREDENTIAL_ENCRYPTION_KEY", "not-valid-base64!!")

	dir := t.TempDir()
	p := filepath.Join(dir, "cfg.yaml")
	_ = os.WriteFile(p, []byte("secretCustody:\n  provider: command\n  command: [\"cat\", \"{file}\"]\n"), 0o644)

	if _, err := Load(p); err == nil {
		t.Fatal("expected fail-closed error for an unwrappable crown jewel under provider=command")
	}
}

// setRequiredEnvBaseline stamps every env-side input that validate() now
// requires, so the test reaches the branch it actually wants to exercise.
// Tests that drive a specific required field empty MUST override after.
// Mirrors the Hub pattern in packages/nexus-hub/internal/config — required
// set is documented in validate() of this package.
func setRequiredEnvBaseline(t *testing.T) {
	t.Helper()
	t.Setenv("INTERNAL_SERVICE_TOKEN", "tok")
	// SEC-W2-02 FIX-5/C: HubConfigToken is now a required env input.
	t.Setenv("HUB_CONFIG_TOKEN", "hub-config-tok")
	// SEC-W2-03 Layer C: ADMIN_KEY_HMAC_SECRET is now a required validate() input
	// (it is injected into the apikey hashing layer at boot). Resolved through the
	// custody loader; under the default noop provider this plaintext passes through.
	t.Setenv("ADMIN_KEY_HMAC_SECRET", "test-hmac-secret")
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("CONTROL_PLANE_PUBLIC_URL", "http://localhost:3001")
	t.Setenv("REDIS_ADDRS", "localhost:6379")
	t.Setenv("MQ_DRIVER", "nats")
	t.Setenv("NATS_URL", "nats://localhost:4222")
}

// TestLoad_HMACSecret_RequiredFailClosed is the SEC-W2-03 Layer C regression:
// validate() now hard-fails when ADMIN_KEY_HMAC_SECRET is unset, so an operator
// who forgets it can never boot a Control Plane that would otherwise hash every
// admin key + VK under an empty secret. Previously the only gate read the env var
// directly in the bootstrap layer; the requirement now lives in config.validate()
// against the custody-resolved field.
func TestLoad_HMACSecret_RequiredFailClosed(t *testing.T) {
	clearAllEnv(t)
	setRequiredEnvBaseline(t)
	// Remove only the HMAC secret; everything else required stays set, so the
	// failure isolates to the HMAC guard.
	t.Setenv("ADMIN_KEY_HMAC_SECRET", "")

	dir := t.TempDir()
	p := filepath.Join(dir, "cfg.yaml")
	_ = os.WriteFile(p, []byte("{}\n"), 0o644)

	_, err := Load(p)
	if err == nil {
		t.Fatal("expected fail-closed error when ADMIN_KEY_HMAC_SECRET is unset")
	}
	if !strings.Contains(err.Error(), "an HMAC secret is required") {
		t.Errorf("error=%v; want the auth.hmacSecret required guard", err)
	}
}

// TestLoad_InternalServiceToken_YAMLIgnored verifies the secrets-env-only
// binding: even if a yaml file carries `auth.internalServiceToken`, it must
// NOT populate cfg.Auth.InternalServiceToken (the yaml:"-" tag enforces
// this). The only way to set this value is the INTERNAL_SERVICE_TOKEN env
// var; the proof is that cfg ends up holding the env value, not the yaml
// value (env wins because yaml field is dropped entirely).
func TestLoad_InternalServiceToken_YAMLIgnored(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(p, []byte(`
auth:
  internalServiceToken: "from-yaml-should-be-ignored"
`), 0o644)

	setRequiredEnvBaseline(t)
	// Override INTERNAL_SERVICE_TOKEN with a sentinel distinct from the
	// yaml value — if yaml were ever honoured, cfg would hold the yaml
	// string; the env-only contract says cfg must hold the sentinel.
	t.Setenv("INTERNAL_SERVICE_TOKEN", "env-wins-sentinel")

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.InternalServiceToken != "env-wins-sentinel" {
		t.Errorf("InternalServiceToken = %q; yaml field leaked through (env-only contract broken)", cfg.Auth.InternalServiceToken)
	}
}

func TestLoad_InternalServiceToken_FromEnv(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(p, []byte(""), 0o644)
	setRequiredEnvBaseline(t)
	t.Setenv("INTERNAL_SERVICE_TOKEN", "from-env")

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.InternalServiceToken != "from-env" {
		t.Errorf("InternalServiceToken = %q, want %q", cfg.Auth.InternalServiceToken, "from-env")
	}
}

func TestServerBindAddr(t *testing.T) {
	if got := (ServerConfig{Port: 3001}).BindAddr(); got != ":3001" {
		t.Errorf("empty Host BindAddr = %q, want \":3001\"", got)
	}
	if got := (ServerConfig{Host: "127.0.0.1", Port: 3001}).BindAddr(); got != "127.0.0.1:3001" {
		t.Errorf("loopback BindAddr = %q, want \"127.0.0.1:3001\"", got)
	}
}

func TestLoad_ServerHost_FromEnv(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(p, []byte(""), 0o644)
	setRequiredEnvBaseline(t)
	t.Setenv("CONTROL_PLANE_HOST", "127.0.0.1")

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Host != "127.0.0.1" {
		t.Errorf("Server.Host = %q, want 127.0.0.1 (from CONTROL_PLANE_HOST)", cfg.Server.Host)
	}
}

func TestLoad_CryptoProduction_FromYAML(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(p, []byte(`
crypto:
  production: true
`), 0o644)
	setRequiredEnvBaseline(t)

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Crypto.Production {
		t.Errorf("Production = false, want true")
	}
}

func TestLoad_CryptoProduction_EnvOverride(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(p, []byte(`
crypto:
  production: false
`), 0o644)
	setRequiredEnvBaseline(t)
	t.Setenv("CONTROL_PLANE_CRYPTO_PRODUCTION", "true")

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Crypto.Production {
		t.Errorf("Production = false, want true (env override)")
	}
}

// clearAllEnv unsets every env var Load consults so a test starts from a
// clean slate. We can't use t.Setenv("", "") since Setenv refuses empty
// values on some platforms; explicit Unsetenv via t.Setenv-equivalent is
// done by Setenv("X", "") + cleanup. Use a helper that calls t.Setenv for
// each so the Go test framework restores values automatically.
func clearAllEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"CONTROL_PLANE_PORT", "CONTROL_PLANE_PUBLIC_URL",
		"DATABASE_URL", "REDIS_ADDRS", "LOG_LEVEL",
		"COMPLIANCE_PROXY_URL", "AI_GATEWAY_URL", "NEXUS_HUB_URL",
		"CREDENTIAL_ENCRYPTION_KEY", "CREDENTIAL_KEY_MAP",
		"AGENT_CA_DIR", "COMPLIANCE_PROXY_RUNTIME_URL",
		"COMPLIANCE_PROXY_API_TOKEN",
		"OTEL_ENDPOINT", "OTEL_SERVICE_NAME",
		"MQ_DRIVER", "NATS_URL",
		"ADMIN_KEY_HMAC_SECRET", "INTERNAL_SERVICE_TOKEN", "HUB_CONFIG_TOKEN",
		"CONTROL_PLANE_CRYPTO_PRODUCTION",
		"AUTH_SERVER_ISSUER", "AUTH_SERVER_KEYSTORE_DIR",
	} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
}

// TestLoad_MissingFile asserts Load returns defaults when the YAML file is
// absent — documented behaviour (Load comment: "Missing file is not an error
// — defaults are used.").
func TestLoad_MissingFile(t *testing.T) {
	clearAllEnv(t)
	setRequiredEnvBaseline(t)
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Spot-check the well-known defaults.
	if cfg.Server.Port != 3001 {
		t.Errorf("Server.Port = %d, want 3001", cfg.Server.Port)
	}
	if cfg.Server.ShutdownTimeout != 10*time.Second {
		t.Errorf("Server.ShutdownTimeout = %v, want 10s", cfg.Server.ShutdownTimeout)
	}
	if cfg.Database.MaxConns != 25 || cfg.Database.MinConns != 5 || cfg.Database.MaxConnLifetime != 300*time.Second {
		t.Errorf("Database defaults wrong: %+v", cfg.Database)
	}
	if cfg.Log.Level != "info" || cfg.Log.Format != "json" {
		t.Errorf("Log defaults wrong: %+v", cfg.Log)
	}
	if cfg.BFF.ComplianceProxyURL != "http://127.0.0.1:3040" ||
		cfg.BFF.AIGatewayURL != "http://127.0.0.1:3050" ||
		cfg.BFF.ComplianceProxyRuntimeURL != "http://127.0.0.1:3040" {
		t.Errorf("BFF defaults wrong: %+v", cfg.BFF)
	}
	if cfg.Registry.NexusHubURL != "http://127.0.0.1:3060" {
		t.Errorf("Registry.NexusHubURL = %q, want http://127.0.0.1:3060", cfg.Registry.NexusHubURL)
	}
	if cfg.Agent.CADir != ".agent-ca" {
		t.Errorf("Agent.CADir = %q, want %q", cfg.Agent.CADir, ".agent-ca")
	}
	if cfg.AuthServer.KeystoreDir != ".nexus/authkeys" {
		t.Errorf("AuthServer.KeystoreDir = %q, want %q", cfg.AuthServer.KeystoreDir, ".nexus/authkeys")
	}
	if cfg.AIGuard.DispatchTimeoutSec != 60 {
		t.Errorf("AIGuard.DispatchTimeoutSec = %d, want 60", cfg.AIGuard.DispatchTimeoutSec)
	}
	if cfg.HTTPClients.Hub.TimeoutSec != 30 ||
		cfg.HTTPClients.HubProxy.TimeoutSec != 10 ||
		cfg.HTTPClients.ComplianceProxyAdmin.TimeoutSec != 10 {
		t.Errorf("HTTPClients defaults wrong: %+v", cfg.HTTPClients)
	}
}

// TestLoad_ReadError points Load at a directory — os.ReadFile returns a
// non-ENOENT error which Load is required to surface (wrap + return).
func TestLoad_ReadError(t *testing.T) {
	clearAllEnv(t)
	setRequiredEnvBaseline(t)
	// Pass the directory path itself; ReadFile returns an EISDIR-class error
	// that is NOT os.IsNotExist, so Load must wrap and return it BEFORE
	// reaching validate — baseline envs would otherwise pass.
	dir := t.TempDir()
	cfg, err := Load(dir)
	if err == nil {
		t.Fatalf("Load(directory) returned nil err, cfg=%+v", cfg)
	}
	if !strings.Contains(err.Error(), "read config") {
		t.Errorf("err = %q; want wrap prefix 'read config'", err.Error())
	}
}

// TestLoad_ParseError feeds malformed YAML and asserts Load returns the
// wrapped parse error.
func TestLoad_ParseError(t *testing.T) {
	clearAllEnv(t)
	setRequiredEnvBaseline(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.yaml")
	// Tabs are invalid in YAML indentation — yaml.Unmarshal rejects this.
	if err := os.WriteFile(p, []byte("server:\n\tport: 3001\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(p)
	if err == nil {
		t.Fatalf("Load(malformed yaml) returned nil err, cfg=%+v", cfg)
	}
	if !strings.Contains(err.Error(), "parse config") {
		t.Errorf("err = %q; want wrap prefix 'parse config'", err.Error())
	}
}

// TestLoad_YAMLFieldsParsed cycles every exported YAML-mapped field
// through Load to confirm the unmarshal target is wired correctly. A new
// field added to Config without a matching yaml tag would surface here.
func TestLoad_YAMLFieldsParsed(t *testing.T) {
	clearAllEnv(t)
	// This test asserts that every yaml field unmarshals into cfg —
	// calling setRequiredEnvBaseline would let envs override yaml values
	// (env wins post-applyEnvOverrides) and break those assertions. Only
	// stamp the required fields yaml cannot supply (env-only secrets).
	t.Setenv("INTERNAL_SERVICE_TOKEN", "tok")
	t.Setenv("HUB_CONFIG_TOKEN", "hub-config-tok")
	// SEC-W2-03 Layer C: ADMIN_KEY_HMAC_SECRET is an env-only required secret
	// validate() now enforces — yaml cannot supply it.
	t.Setenv("ADMIN_KEY_HMAC_SECRET", "test-hmac-secret")
	dir := t.TempDir()
	p := filepath.Join(dir, "full.yaml")
	yamlBody := `
id: "cp-instance-7"
publicURL: "https://example.invalid"
server:
  port: 4001
  shutdownTimeout: "25s"
  advertiseHost: "10.0.0.5"
database:
  url: "postgres://u:p@h/db"
  maxConns: 50
  minConns: 10
  maxConnLifetime: "600s"
redis:
  mode: standalone
  addrs: ["1.2.3.4:6379"]
  db: 0
log:
  level: "debug"
  format: "text"
  file: "/var/log/cp.log"
  stackOnError: true
bff:
  complianceProxyUrl: "http://cp.local:3040"
  aiGatewayUrl: "http://ai.local:3050"
  complianceProxyRuntimeUrl: "http://cp.local:3041"
registry:
  nexusHubUrl: "http://hub.local:3060"
crypto:
  production: true
agent:
  caDir: "/etc/agent-ca"
otel:
  endpoint: "otel:4317"
  serviceName: "cp"
mq:
  driver: "nats"
  nats:
    url: "nats://1.2.3.4:4222"
authServer:
  issuer: "https://issuer.example/"
  keystoreDir: "/etc/keys"
  revocationIntrospectUrl: "https://issuer.example/i"
  revocationReplayUrl: "https://issuer.example/r"
aiGuard:
  dispatchTimeoutSec: 90
httpClients:
  hub:
    timeoutSec: 45
  hubProxy:
    timeoutSec: 15
  complianceProxyAdmin:
    timeoutSec: 20
`
	if err := os.WriteFile(p, []byte(yamlBody), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ID != "cp-instance-7" {
		t.Errorf("ID = %q", cfg.ID)
	}
	if cfg.PublicURL != "https://example.invalid" {
		t.Errorf("PublicURL = %q", cfg.PublicURL)
	}
	if cfg.Server.Port != 4001 || cfg.Server.ShutdownTimeout != 25*time.Second || cfg.Server.AdvertiseHost != "10.0.0.5" {
		t.Errorf("Server = %+v", cfg.Server)
	}
	if cfg.Database.URL != "postgres://u:p@h/db" ||
		cfg.Database.MaxConns != 50 || cfg.Database.MinConns != 10 || cfg.Database.MaxConnLifetime != 600*time.Second {
		t.Errorf("Database = %+v", cfg.Database)
	}
	if len(cfg.Redis.Addrs) != 1 || cfg.Redis.Addrs[0] != "1.2.3.4:6379" {
		t.Errorf("Redis = %+v", cfg.Redis)
	}
	if cfg.Log.Level != "debug" || cfg.Log.Format != "text" || cfg.Log.File != "/var/log/cp.log" || !cfg.Log.StackOnError {
		t.Errorf("Log = %+v", cfg.Log)
	}
	if cfg.BFF.ComplianceProxyURL != "http://cp.local:3040" ||
		cfg.BFF.AIGatewayURL != "http://ai.local:3050" ||
		cfg.BFF.ComplianceProxyRuntimeURL != "http://cp.local:3041" {
		t.Errorf("BFF = %+v", cfg.BFF)
	}
	if cfg.Registry.NexusHubURL != "http://hub.local:3060" {
		t.Errorf("Registry.NexusHubURL = %q", cfg.Registry.NexusHubURL)
	}
	if !cfg.Crypto.Production {
		t.Errorf("Crypto.Production = false, want true")
	}
	if cfg.Agent.CADir != "/etc/agent-ca" {
		t.Errorf("Agent.CADir = %q", cfg.Agent.CADir)
	}
	if cfg.Otel.Endpoint != "otel:4317" || cfg.Otel.ServiceName != "cp" {
		t.Errorf("Otel = %+v", cfg.Otel)
	}
	if cfg.MQ.Driver != "nats" || cfg.MQ.NATS.URL != "nats://1.2.3.4:4222" {
		t.Errorf("MQ = %+v", cfg.MQ)
	}
	if cfg.AuthServer.Issuer != "https://issuer.example/" ||
		cfg.AuthServer.KeystoreDir != "/etc/keys" ||
		cfg.AuthServer.RevocationIntrospectURL != "https://issuer.example/i" ||
		cfg.AuthServer.RevocationReplayURL != "https://issuer.example/r" {
		t.Errorf("AuthServer = %+v", cfg.AuthServer)
	}
	if cfg.AIGuard.DispatchTimeoutSec != 90 {
		t.Errorf("AIGuard.DispatchTimeoutSec = %d", cfg.AIGuard.DispatchTimeoutSec)
	}
	if cfg.HTTPClients.Hub.TimeoutSec != 45 ||
		cfg.HTTPClients.HubProxy.TimeoutSec != 15 ||
		cfg.HTTPClients.ComplianceProxyAdmin.TimeoutSec != 20 {
		t.Errorf("HTTPClients = %+v", cfg.HTTPClients)
	}
}

// TestLoad_SecretYAMLFieldsIgnored is the multi-field counterpart to
// TestLoad_InternalServiceToken_YAMLIgnored: every yaml:"-" secret field
// must stay zero-valued when the same key is present in YAML — confirming
// that secrets are env-only across the board.
func TestLoad_SecretYAMLFieldsIgnored(t *testing.T) {
	clearAllEnv(t)
	// Baseline supplies env values for required fields; we still expect
	// every yaml:"-" SECRET to stay zero-valued (proves yaml is dropped).
	// Note: Auth.InternalServiceToken is ALSO required by validate, and
	// it will be set via baseline — assertion below skips that field.
	setRequiredEnvBaseline(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "secrets.yaml")
	body := `
auth:
  hmacSecret: "y-hmac"
  internalServiceToken: "y-ist"
crypto:
  encryptionKey: "y-key"
  credentialKeyMap: "y-keymap"
bff:
  complianceProxyAPIToken: "y-cp-api"
`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Secrets without an env baseline must end up zero-valued — proves the
	// yaml:"-" tag dropped them. InternalServiceToken and HMACSecret ARE
	// required and ARE stamped via baseline, so we assert each equals its env
	// value (NOT the yaml string) which is a stronger proof: yaml dropped, env
	// won.
	checks := map[string]string{
		"Crypto.EncryptionKey":        cfg.Crypto.EncryptionKey,
		"Crypto.CredentialKeyMap":     cfg.Crypto.CredentialKeyMap,
		"BFF.ComplianceProxyAPIToken": cfg.BFF.ComplianceProxyAPIToken,
	}
	for name, got := range checks {
		if got != "" {
			t.Errorf("%s = %q; yaml field must be ignored (env-only)", name, got)
		}
	}
	if cfg.Auth.InternalServiceToken != "tok" {
		t.Errorf("Auth.InternalServiceToken = %q; expected env value \"tok\" (yaml \"y-ist\" must be dropped, env must win)", cfg.Auth.InternalServiceToken)
	}
	// SEC-W2-03 Layer C: HMACSecret is required + custody-resolved. The env
	// baseline sets "test-hmac-secret"; the yaml "y-hmac" must be dropped.
	if cfg.Auth.HMACSecret != "test-hmac-secret" {
		t.Errorf("Auth.HMACSecret = %q; expected env value \"test-hmac-secret\" (yaml \"y-hmac\" must be dropped, env must win)", cfg.Auth.HMACSecret)
	}
}

// TestLoad_AllEnvOverrides exercises every env-var branch in applyEnv with
// a single test that sets each one and verifies the resulting Config.
func TestLoad_AllEnvOverrides(t *testing.T) {
	clearAllEnv(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "empty.yaml")
	if err := os.WriteFile(p, []byte(""), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Baseline supplies REDIS_ADDRS + CONTROL_PLANE_PUBLIC_URL that
	// validate now requires; the per-knob Setenv calls below shadow the
	// fields this test actually verifies.
	setRequiredEnvBaseline(t)
	t.Setenv("CONTROL_PLANE_PORT", "5555")
	t.Setenv("DATABASE_URL", "postgres://envdb")
	// REDIS_* env knobs are consumed by redisfactory.LoadEnv at wiring time,
	// not at config.Load. See packages/shared/storage/redisfactory.
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("COMPLIANCE_PROXY_URL", "http://envcp")
	t.Setenv("AI_GATEWAY_URL", "http://envai")
	t.Setenv("NEXUS_HUB_URL", "http://envhub")
	t.Setenv("CREDENTIAL_ENCRYPTION_KEY", "envkey")
	t.Setenv("CREDENTIAL_KEY_MAP", "v1:dead,v2:beef")
	t.Setenv("AGENT_CA_DIR", "/env/ca")
	t.Setenv("COMPLIANCE_PROXY_RUNTIME_URL", "http://envcpruntime")
	t.Setenv("COMPLIANCE_PROXY_API_TOKEN", "envcptoken")
	t.Setenv("OTEL_ENDPOINT", "envotel:4317")
	t.Setenv("OTEL_SERVICE_NAME", "envsvc")
	t.Setenv("MQ_DRIVER", "nats")
	t.Setenv("NATS_URL", "nats://envnats")
	t.Setenv("ADMIN_KEY_HMAC_SECRET", "envhmac")
	t.Setenv("INTERNAL_SERVICE_TOKEN", "envist")
	t.Setenv("HUB_CONFIG_TOKEN", "envhcfg")
	t.Setenv("AUTH_SERVER_ISSUER", "https://envissuer")
	t.Setenv("AUTH_SERVER_KEYSTORE_DIR", "/env/keys")

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Server.Port != 5555 {
		t.Errorf("Server.Port = %d, want 5555 (CONTROL_PLANE_PORT env)", cfg.Server.Port)
	}
	if cfg.Database.URL != "postgres://envdb" {
		t.Errorf("Database.URL = %q", cfg.Database.URL)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("Log.Level = %q", cfg.Log.Level)
	}
	if cfg.BFF.ComplianceProxyURL != "http://envcp" ||
		cfg.BFF.AIGatewayURL != "http://envai" ||
		cfg.BFF.ComplianceProxyRuntimeURL != "http://envcpruntime" ||
		cfg.BFF.ComplianceProxyAPIToken != "envcptoken" {
		t.Errorf("BFF = %+v", cfg.BFF)
	}
	if cfg.Registry.NexusHubURL != "http://envhub" {
		t.Errorf("Registry.NexusHubURL = %q", cfg.Registry.NexusHubURL)
	}
	if cfg.Crypto.EncryptionKey != "envkey" ||
		cfg.Crypto.CredentialKeyMap != "v1:dead,v2:beef" {
		t.Errorf("Crypto = %+v", cfg.Crypto)
	}
	if cfg.Agent.CADir != "/env/ca" {
		t.Errorf("Agent.CADir = %q", cfg.Agent.CADir)
	}
	if cfg.Otel.Endpoint != "envotel:4317" || cfg.Otel.ServiceName != "envsvc" {
		t.Errorf("Otel = %+v", cfg.Otel)
	}
	if cfg.MQ.Driver != "nats" || cfg.MQ.NATS.URL != "nats://envnats" {
		t.Errorf("MQ = %+v", cfg.MQ)
	}
	if cfg.Auth.HMACSecret != "envhmac" || cfg.Auth.InternalServiceToken != "envist" ||
		cfg.Auth.HubConfigToken != "envhcfg" {
		t.Errorf("Auth = %+v", cfg.Auth)
	}
	if cfg.AuthServer.Issuer != "https://envissuer" || cfg.AuthServer.KeystoreDir != "/env/keys" {
		t.Errorf("AuthServer = %+v", cfg.AuthServer)
	}
}

// TestLoad_EnvOverridesYAML asserts env wins over a value already present
// in the yaml file — applyEnv runs after the unmarshal.
func TestLoad_EnvOverridesYAML(t *testing.T) {
	clearAllEnv(t)
	setRequiredEnvBaseline(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	body := `
server:
  port: 1111
database:
  url: "yaml-db"
log:
  level: "warn"
`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("CONTROL_PLANE_PORT", "2222")
	t.Setenv("DATABASE_URL", "env-db")
	t.Setenv("LOG_LEVEL", "error")

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Port != 2222 {
		t.Errorf("Server.Port = %d, want 2222 (env override)", cfg.Server.Port)
	}
	if cfg.Database.URL != "env-db" {
		t.Errorf("Database.URL = %q, want env-db", cfg.Database.URL)
	}
	if cfg.Log.Level != "error" {
		t.Errorf("Log.Level = %q, want error", cfg.Log.Level)
	}
}

// TestLoad_PortEnv_NonNumeric documents the current Sscanf behaviour: a
// non-numeric CONTROL_PLANE_PORT silently leaves Server.Port at its default.
// fmt.Sscanf returns (0, err) and the result is discarded.
func TestLoad_PortEnv_NonNumeric(t *testing.T) {
	clearAllEnv(t)
	setRequiredEnvBaseline(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(""), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("CONTROL_PLANE_PORT", "not-a-number")
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Port != 3001 {
		t.Errorf("Server.Port = %d, want 3001 (default — non-numeric CONTROL_PLANE_PORT must be ignored)", cfg.Server.Port)
	}
}

// TestLoad_CryptoProduction_EnvFalsy verifies the truthy-only contract on
// CONTROL_PLANE_CRYPTO_PRODUCTION: only "true" and "1" flip the flag on;
// "false", "0", "" and arbitrary garbage leave it unchanged.
func TestLoad_CryptoProduction_EnvFalsy(t *testing.T) {
	cases := []struct {
		envVal string
		yaml   bool
		want   bool
	}{
		{"1", false, true},
		{"true", false, true},
		{"false", false, false},
		{"0", false, false},
		{"yes", false, false},
		{"", true, true}, // env empty → applyEnv block skipped → yaml value sticks
	}
	for _, tc := range cases {
		t.Run(tc.envVal+"-yaml="+boolStr(tc.yaml), func(t *testing.T) {
			clearAllEnv(t)
			setRequiredEnvBaseline(t)
			dir := t.TempDir()
			p := filepath.Join(dir, "config.yaml")
			body := "crypto:\n  production: " + boolStr(tc.yaml) + "\n"
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			if tc.envVal != "" {
				t.Setenv("CONTROL_PLANE_CRYPTO_PRODUCTION", tc.envVal)
			}
			cfg, err := Load(p)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Crypto.Production != tc.want {
				t.Errorf("envVal=%q yaml=%v: Production = %v, want %v", tc.envVal, tc.yaml, cfg.Crypto.Production, tc.want)
			}
		})
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// TestValidate_RequiredFields cycles each business-required field through
// the empty path, asserting validate trips with a contextual message.
// Mirrors the Hub validate-coverage pattern. Each subtest drives ONE
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
				t.Setenv("CONTROL_PLANE_PUBLIC_URL", "")
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
			// SEC-W2-02 FIX-5/C: CP must fail closed at config load without the
			// dedicated config token — an empty token would 403 every Hub config
			// push (and Hub itself refuses to boot without it).
			name: "missing Auth.HubConfigToken",
			mutate: func(t *testing.T) string {
				t.Setenv("HUB_CONFIG_TOKEN", "")
				return ""
			},
			wantInErr: "auth.hubConfigToken is required",
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
			name: "missing MQ.Driver (defaults nil; yaml explicit empty)",
			mutate: func(t *testing.T) string {
				t.Setenv("MQ_DRIVER", "")
				dir := t.TempDir()
				p := filepath.Join(dir, "cfg.yaml")
				_ = os.WriteFile(p, []byte("mq:\n  driver: \"\"\n"), 0o644)
				return p
			},
			wantInErr: "mq.driver is required",
		},
		{
			name: "missing MQ.NATS.URL when Driver=nats",
			mutate: func(t *testing.T) string {
				t.Setenv("MQ_DRIVER", "nats")
				t.Setenv("NATS_URL", "")
				dir := t.TempDir()
				p := filepath.Join(dir, "cfg.yaml")
				_ = os.WriteFile(p, []byte("mq:\n  driver: \"nats\"\n  nats:\n    url: \"\"\n"), 0o644)
				return p
			},
			wantInErr: "mq.nats.url is required",
		},
		{
			name: "missing Registry.NexusHubURL (defaults seeds; yaml empty)",
			mutate: func(t *testing.T) string {
				t.Setenv("NEXUS_HUB_URL", "")
				dir := t.TempDir()
				p := filepath.Join(dir, "cfg.yaml")
				_ = os.WriteFile(p, []byte("registry:\n  nexusHubUrl: \"\"\n"), 0o644)
				return p
			},
			wantInErr: "registry.nexusHubUrl is required",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearAllEnv(t)
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

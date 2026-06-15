package wiring

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitBootstrap_NonexistentConfigPath_UsesDefaults(t *testing.T) {
	// A non-existent config file is not an error — defaults are used.
	// We must set AUTH_SERVER_ISSUER via ADMIN_KEY_HMAC_SECRET fallback is dev-ok.
	// We also need AuthServer.Issuer set, which requires env AUTH_SERVER_ISSUER.
	if err := os.Setenv("AUTH_SERVER_ISSUER", "http://localhost:3001"); err != nil {
		t.Fatalf("setenv: %v", err)
	}
	defer os.Unsetenv("AUTH_SERVER_ISSUER")
	// Stamp the env-side baseline that config.validate() requires; without
	// it Load trips on PublicURL/Database/Redis/MQ/Token before defaults
	// have a chance to settle.
	t.Setenv("INTERNAL_SERVICE_TOKEN", "tok")
	t.Setenv("HUB_CONFIG_TOKEN", "hub-config-tok")
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("CONTROL_PLANE_PUBLIC_URL", "http://localhost:3001")
	t.Setenv("REDIS_ADDRS", "localhost:6379")
	t.Setenv("MQ_DRIVER", "nats")
	t.Setenv("NATS_URL", "nats://localhost:4222")
	// ADMIN_KEY_HMAC_SECRET is mandatory in every environment (SEC-M9-01/02: no
	// dev fallback), so the bootstrap HMAC guard needs it set even for this
	// defaults-loading test.
	t.Setenv("ADMIN_KEY_HMAC_SECRET", "test-hmac-secret")

	b, err := InitBootstrap("/nonexistent/config/path.yaml")
	if err != nil {
		t.Fatalf("InitBootstrap failed: %v", err)
	}
	if b.Config == nil {
		t.Error("expected non-nil Config")
	}
	if b.Logger == nil {
		t.Error("expected non-nil Logger")
	}
	if b.StartTime.IsZero() {
		t.Error("expected non-zero StartTime")
	}
}

func TestInitBootstrap_MissingIssuer_ReturnsError(t *testing.T) {
	// AUTH_SERVER_ISSUER must be unset so Issuer is empty → error.
	prevIssuer := os.Getenv("AUTH_SERVER_ISSUER")
	os.Unsetenv("AUTH_SERVER_ISSUER")
	defer func() {
		if prevIssuer != "" {
			os.Setenv("AUTH_SERVER_ISSUER", prevIssuer)
		}
	}()
	// Satisfy the config baseline + the mandatory HMAC guard (SEC-M9-01/02) so
	// the failure isolates to the issuer check rather than tripping earlier.
	t.Setenv("ADMIN_KEY_HMAC_SECRET", "test-hmac-secret")
	t.Setenv("INTERNAL_SERVICE_TOKEN", "tok")
	t.Setenv("HUB_CONFIG_TOKEN", "hub-config-tok")
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("CONTROL_PLANE_PUBLIC_URL", "http://localhost:3001")
	t.Setenv("REDIS_ADDRS", "localhost:6379")
	t.Setenv("MQ_DRIVER", "nats")
	t.Setenv("NATS_URL", "nats://localhost:4222")
	os.Unsetenv("NODE_ENV")

	_, err := InitBootstrap("/nonexistent/path.yaml")
	if err == nil {
		t.Fatal("expected error when issuer is empty, got nil")
	}
	if !strings.Contains(err.Error(), "authServer.issuer is required") {
		t.Errorf("error=%v; want the issuer guard (HMAC/config baseline must pass first)", err)
	}
}

func TestInitBootstrap_InvalidYAML_ReturnsError(t *testing.T) {
	// Write a file with invalid YAML to trigger config.Load error.
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "bad.yaml")
	if err := os.WriteFile(cfgPath, []byte(":\tinvalid: yaml: [\n"), 0644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	_, err := InitBootstrap(cfgPath)
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
}

// TestInitBootstrap_HMACSecretIsEnvOnly is the F-0075 regression: the
// HMAC secret is env-only (config.Auth.HMACSecret is `yaml:"-"`). A YAML
// `auth.hmacSecret` value must be IGNORED — there is no YAML→env bridge.
// The secret comes solely from ADMIN_KEY_HMAC_SECRET.
func TestInitBootstrap_HMACSecretIsEnvOnly(t *testing.T) {
	// No env secret set; a YAML auth.hmacSecret must NOT satisfy the field.
	prevEnv := os.Getenv("ADMIN_KEY_HMAC_SECRET")
	os.Unsetenv("ADMIN_KEY_HMAC_SECRET")
	defer func() {
		if prevEnv != "" {
			os.Setenv("ADMIN_KEY_HMAC_SECRET", prevEnv)
		} else {
			os.Unsetenv("ADMIN_KEY_HMAC_SECRET")
		}
	}()

	if err := os.Setenv("AUTH_SERVER_ISSUER", "http://localhost:3001"); err != nil {
		t.Fatalf("setenv: %v", err)
	}
	defer os.Unsetenv("AUTH_SERVER_ISSUER")
	// Stamp the env-side baseline that config.validate() requires.
	t.Setenv("INTERNAL_SERVICE_TOKEN", "tok")
	t.Setenv("HUB_CONFIG_TOKEN", "hub-config-tok")
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("CONTROL_PLANE_PUBLIC_URL", "http://localhost:3001")
	t.Setenv("REDIS_ADDRS", "localhost:6379")
	t.Setenv("MQ_DRIVER", "nats")
	t.Setenv("NATS_URL", "nats://localhost:4222")
	os.Unsetenv("NODE_ENV") // proves the env-only HMAC guard does not depend on NODE_ENV

	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "cfg.yaml")
	yaml := "auth:\n  hmacSecret: \"yaml-secret-should-be-ignored\"\n"
	if err := os.WriteFile(cfgPath, []byte(yaml), 0644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	// SEC-M9-01/02 + SEC-W2-03 Layer C: ADMIN_KEY_HMAC_SECRET is env-only AND
	// mandatory (no dev fallback). With the secret present only in YAML (and
	// absent from env), InitBootstrap must fail the HMAC guard — proving the YAML
	// value neither satisfies the requirement nor is bridged into the env. The
	// guard now lives in config.validate() (against the custody-resolved field),
	// so the error surfaces from config.Load.
	_, err := InitBootstrap(cfgPath)
	if err == nil {
		t.Fatal("expected error — a YAML auth.hmacSecret must not satisfy the env-only HMAC requirement")
	}
	if !strings.Contains(err.Error(), "an HMAC secret is required") {
		t.Errorf("error=%v; want the HMAC-required guard (proving YAML is ignored)", err)
	}
	// The YAML value must not have been written into the env by a bridge.
	if v := os.Getenv("ADMIN_KEY_HMAC_SECRET"); v != "" {
		t.Errorf("ADMIN_KEY_HMAC_SECRET=%q want empty — bootstrap must not bridge YAML into env", v)
	}
}

func TestInitBootstrap_MissingHMAC_ReturnsError(t *testing.T) {
	// SEC-W2-03 Layer C: a missing HMAC secret must abort the boot UNCONDITIONALLY
	// (dev and prod alike — the guard no longer depends on any production flag).
	// With every other required input present, the failure isolates to the
	// HMAC-secret guard, which now lives in config.validate() against the
	// custody-resolved field.
	prevHMAC := os.Getenv("ADMIN_KEY_HMAC_SECRET")
	os.Unsetenv("ADMIN_KEY_HMAC_SECRET")
	defer func() {
		if prevHMAC != "" {
			os.Setenv("ADMIN_KEY_HMAC_SECRET", prevHMAC) //nolint:errcheck
		}
	}()
	os.Unsetenv("NODE_ENV")
	// Baseline so config otherwise loads cleanly, isolating the failure to the
	// HMAC guard.
	t.Setenv("AUTH_SERVER_ISSUER", "http://localhost:3001")
	t.Setenv("INTERNAL_SERVICE_TOKEN", "tok")
	t.Setenv("HUB_CONFIG_TOKEN", "hub-config-tok")
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("CONTROL_PLANE_PUBLIC_URL", "http://localhost:3001")
	t.Setenv("REDIS_ADDRS", "localhost:6379")
	t.Setenv("MQ_DRIVER", "nats")
	t.Setenv("NATS_URL", "nats://localhost:4222")

	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "cfg.yaml")
	// A config that loads cleanly but carries no auth.hmacSecret.
	if err := os.WriteFile(cfgPath, []byte("auth: {}\n"), 0644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	_, err := InitBootstrap(cfgPath)
	if err == nil {
		t.Fatal("expected error without an HMAC secret, got nil")
	}
	if !strings.Contains(err.Error(), "an HMAC secret is required") {
		t.Errorf("expected the HMAC-secret guard to fire, got a different error: %v", err)
	}
}

// TestInitBootstrap_LogFileInUnwritableDir_ReturnsError verifies the logger
// initialization error path: when LOG_FILE points to an unwritable directory
// tree, logging.NewLogger returns an error and InitBootstrap propagates it.
func TestInitBootstrap_LogFileInUnwritableDir_ReturnsError(t *testing.T) {
	// Set LOG_FILE to a path whose parent directory cannot be created.
	// /proc exists on Linux but is read-only; on macOS /private/var/root is
	// restricted. Use a path under an existing file (not a dir) so MkdirAll
	// reliably fails cross-platform.
	tmp := t.TempDir()
	// Create a regular file then attempt to use it as a directory component.
	fileNotDir := filepath.Join(tmp, "not-a-dir")
	if err := os.WriteFile(fileNotDir, []byte("x"), 0644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// LOG_FILE = <file>/child/cp.log  — MkdirAll(<file>/child) will fail
	// because <file> is a regular file, not a directory.
	logFile := filepath.Join(fileNotDir, "child", "cp.log")
	prev := os.Getenv("LOG_FILE")
	if err := os.Setenv("LOG_FILE", logFile); err != nil {
		t.Fatalf("setenv: %v", err)
	}
	defer func() {
		if prev != "" {
			os.Setenv("LOG_FILE", prev)
		} else {
			os.Unsetenv("LOG_FILE")
		}
	}()

	// Also ensure HMAC + issuer are valid so we reach the logger init step.
	os.Unsetenv("NODE_ENV")
	prevIssuer := os.Getenv("AUTH_SERVER_ISSUER")
	os.Setenv("AUTH_SERVER_ISSUER", "http://localhost:3001")
	defer func() {
		if prevIssuer != "" {
			os.Setenv("AUTH_SERVER_ISSUER", prevIssuer)
		} else {
			os.Unsetenv("AUTH_SERVER_ISSUER")
		}
	}()

	_, err := InitBootstrap("/nonexistent/config.yaml")
	if err == nil {
		t.Fatal("expected error when LOG_FILE points to an unwritable path, got nil")
	}
}

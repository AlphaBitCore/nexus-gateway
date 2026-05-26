package wiring

import (
	"os"
	"path/filepath"
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
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("CONTROL_PLANE_PUBLIC_URL", "http://localhost:3001")
	t.Setenv("REDIS_ADDRS", "localhost:6379")
	t.Setenv("MQ_DRIVER", "nats")
	t.Setenv("NATS_URL", "nats://localhost:4222")
	// Ensure we're not in production so HMAC check passes without env var.
	prevEnv := os.Getenv("NODE_ENV")
	os.Unsetenv("NODE_ENV")
	defer func() {
		if prevEnv != "" {
			os.Setenv("NODE_ENV", prevEnv)
		}
	}()

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
	// Not production, so HMAC check passes.
	os.Unsetenv("NODE_ENV")

	_, err := InitBootstrap("/nonexistent/path.yaml")
	if err == nil {
		t.Fatal("expected error when issuer is empty, got nil")
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

func TestInitBootstrap_HMACSecretFromYAML_EnvSet(t *testing.T) {
	// Write a valid YAML with auth.hmacSecret so it bridges into env.
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
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("CONTROL_PLANE_PUBLIC_URL", "http://localhost:3001")
	t.Setenv("REDIS_ADDRS", "localhost:6379")
	t.Setenv("MQ_DRIVER", "nats")
	t.Setenv("NATS_URL", "nats://localhost:4222")
	os.Unsetenv("NODE_ENV")

	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "cfg.yaml")
	yaml := "auth:\n  hmacSecret: \"test-secret\"\n"
	if err := os.WriteFile(cfgPath, []byte(yaml), 0644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	b, err := InitBootstrap(cfgPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b.Config == nil {
		t.Error("expected non-nil Config")
	}
}

func TestInitBootstrap_ProdMissingHMAC_ReturnsError(t *testing.T) {
	prevNodeEnv := os.Getenv("NODE_ENV")
	prevHMAC := os.Getenv("ADMIN_KEY_HMAC_SECRET")
	os.Setenv("NODE_ENV", "production")
	os.Unsetenv("ADMIN_KEY_HMAC_SECRET")
	defer func() {
		if prevNodeEnv != "" {
			os.Setenv("NODE_ENV", prevNodeEnv)
		} else {
			os.Unsetenv("NODE_ENV")
		}
		if prevHMAC != "" {
			os.Setenv("ADMIN_KEY_HMAC_SECRET", prevHMAC)
		}
	}()

	_, err := InitBootstrap("/nonexistent/config.yaml")
	if err == nil {
		t.Fatal("expected error in production mode without HMAC secret, got nil")
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

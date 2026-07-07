package config

import (
	"testing"
)

// TestLoad_AuditSpoolMB_EnvOverride drives the two audit spool-size env knobs
// that the env-override block parses with Sscanf: a valid value is applied to
// the resolved config; a malformed value leaves the compiled-in default in
// place (best-effort parse, documented in applyEnvOverrides).
func TestLoad_AuditSpoolMB_EnvOverride(t *testing.T) {
	p := writeYAML(t, "server:\n  port: 3050\n")
	setRequiredEnvBaseline(t)
	t.Setenv("AI_GATEWAY_AUDIT_SPOOL_MAX_FILE_MB", "256")
	t.Setenv("AI_GATEWAY_AUDIT_SPOOL_MAX_TOTAL_MB", "8192")

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Audit.SpoolMaxFileMB != 256 {
		t.Errorf("SpoolMaxFileMB = %d, want 256 (from env)", cfg.Audit.SpoolMaxFileMB)
	}
	if cfg.Audit.SpoolMaxTotalMB != 8192 {
		t.Errorf("SpoolMaxTotalMB = %d, want 8192 (from env)", cfg.Audit.SpoolMaxTotalMB)
	}
}

// AI_GATEWAY_AUDIT_MEM_MAX_BYTES flows verbatim into Audit.MemMaxBytes (the
// audit writer parses it and keeps its auto default on a bad value), and an
// unset env leaves the default empty string (= auto).
func TestLoad_AuditMemMaxBytes_EnvOverride(t *testing.T) {
	p := writeYAML(t, "server:\n  port: 3050\n")
	setRequiredEnvBaseline(t)
	t.Setenv("AI_GATEWAY_AUDIT_MEM_MAX_BYTES", "8GB")

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Audit.MemMaxBytes != "8GB" {
		t.Errorf("Audit.MemMaxBytes = %q, want %q (from env)", cfg.Audit.MemMaxBytes, "8GB")
	}
	if def := defaults().Audit.MemMaxBytes; def != "" {
		t.Errorf("default Audit.MemMaxBytes = %q, want empty (= auto)", def)
	}
}

// TestLoad_AuditSpoolMB_MalformedKeepsDefault asserts the best-effort parse
// contract: a non-numeric value is ignored and the default survives. We capture
// the default from a clean Load (env unset) and prove the malformed Load yields
// the identical value rather than zeroing the field.
func TestLoad_AuditSpoolMB_MalformedKeepsDefault(t *testing.T) {
	defFile := defaults().Audit.SpoolMaxFileMB
	defTotal := defaults().Audit.SpoolMaxTotalMB

	p := writeYAML(t, "server:\n  port: 3050\n")
	setRequiredEnvBaseline(t)
	t.Setenv("AI_GATEWAY_AUDIT_SPOOL_MAX_FILE_MB", "not-a-number")
	t.Setenv("AI_GATEWAY_AUDIT_SPOOL_MAX_TOTAL_MB", "garbage")

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Audit.SpoolMaxFileMB != defFile {
		t.Errorf("SpoolMaxFileMB = %d, want default %d (malformed env ignored)", cfg.Audit.SpoolMaxFileMB, defFile)
	}
	if cfg.Audit.SpoolMaxTotalMB != defTotal {
		t.Errorf("SpoolMaxTotalMB = %d, want default %d (malformed env ignored)", cfg.Audit.SpoolMaxTotalMB, defTotal)
	}
}

// TestLoad_RequestReadBufKB_EnvOverride drives AI_GATEWAY_REQUEST_READ_BUF_KB:
// a valid value lands on Server.RequestReadBufKB; a malformed value leaves the
// compiled-in default (128) untouched.
func TestLoad_RequestReadBufKB_EnvOverride(t *testing.T) {
	def := defaults().Server.RequestReadBufKB

	p := writeYAML(t, "server:\n  port: 3050\n")
	setRequiredEnvBaseline(t)
	t.Setenv("AI_GATEWAY_REQUEST_READ_BUF_KB", "512")

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.RequestReadBufKB != 512 {
		t.Errorf("RequestReadBufKB = %d, want 512 (from env)", cfg.Server.RequestReadBufKB)
	}

	// Malformed value -> default preserved.
	t.Setenv("AI_GATEWAY_REQUEST_READ_BUF_KB", "huge")
	cfg2, err := Load(p)
	if err != nil {
		t.Fatalf("Load (malformed): %v", err)
	}
	if cfg2.Server.RequestReadBufKB != def {
		t.Errorf("RequestReadBufKB = %d, want default %d (malformed env ignored)", cfg2.Server.RequestReadBufKB, def)
	}
}

// TestLoad_UpstreamPool_EnvOverride drives the three upstream connection-pool
// env knobs (the recently-added MaxConnsPerHost among them). Valid values land
// on the resolved Upstream config.
func TestLoad_UpstreamPool_EnvOverride(t *testing.T) {
	p := writeYAML(t, "server:\n  port: 3050\n")
	setRequiredEnvBaseline(t)
	t.Setenv("AI_GATEWAY_UPSTREAM_MAX_IDLE_CONNS_PER_HOST", "750")
	t.Setenv("AI_GATEWAY_UPSTREAM_MAX_IDLE_CONNS", "3000")
	t.Setenv("AI_GATEWAY_UPSTREAM_MAX_CONNS_PER_HOST", "1234")

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Upstream.MaxIdleConnsPerHost != 750 {
		t.Errorf("MaxIdleConnsPerHost = %d, want 750 (from env)", cfg.Upstream.MaxIdleConnsPerHost)
	}
	if cfg.Upstream.MaxIdleConns != 3000 {
		t.Errorf("MaxIdleConns = %d, want 3000 (from env)", cfg.Upstream.MaxIdleConns)
	}
	if cfg.Upstream.MaxConnsPerHost != 1234 {
		t.Errorf("MaxConnsPerHost = %d, want 1234 (from env)", cfg.Upstream.MaxConnsPerHost)
	}
}

// TestLoad_UpstreamPool_DefaultsWhenUnset proves the compiled-in defaults are
// the resolved values when no env override is present. The MaxConnsPerHost
// default (5000) is the recently-added pool cap.
func TestLoad_UpstreamPool_DefaultsWhenUnset(t *testing.T) {
	p := writeYAML(t, "server:\n  port: 3050\n")
	setRequiredEnvBaseline(t)

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Upstream.MaxConnsPerHost != 5000 {
		t.Errorf("MaxConnsPerHost = %d, want default 5000 (env unset)", cfg.Upstream.MaxConnsPerHost)
	}
	if cfg.Upstream.MaxIdleConns != 2000 {
		t.Errorf("MaxIdleConns = %d, want default 2000 (env unset)", cfg.Upstream.MaxIdleConns)
	}
	if cfg.Upstream.MaxIdleConnsPerHost != 500 {
		t.Errorf("MaxIdleConnsPerHost = %d, want default 500 (env unset)", cfg.Upstream.MaxIdleConnsPerHost)
	}
}

// TestLoad_AuditLossMode_DefaultAndOverride asserts both arms of the LossMode
// resolution: the compiled-in default "spillblock" when the env is unset, and
// the "block" value when AI_GATEWAY_AUDIT_LOSS_MODE is set. spillblock is the
// default because it is zero-loss (no spill-saturation drop window) at spill's
// throughput — see config.go defaults().
func TestLoad_AuditLossMode_DefaultAndOverride(t *testing.T) {
	if got := defaults().Audit.LossMode; got != "spillblock" {
		t.Fatalf("default LossMode = %q, want %q", got, "spillblock")
	}

	p := writeYAML(t, "server:\n  port: 3050\n")

	// Default: env unset -> "spillblock".
	setRequiredEnvBaseline(t)
	cfgDef, err := Load(p)
	if err != nil {
		t.Fatalf("Load (default): %v", err)
	}
	if cfgDef.Audit.LossMode != "spillblock" {
		t.Errorf("LossMode = %q, want default %q (env unset)", cfgDef.Audit.LossMode, "spillblock")
	}

	// Override: env=block -> "block".
	t.Setenv("AI_GATEWAY_AUDIT_LOSS_MODE", "block")
	cfgOv, err := Load(p)
	if err != nil {
		t.Fatalf("Load (override): %v", err)
	}
	if cfgOv.Audit.LossMode != "block" {
		t.Errorf("LossMode = %q, want %q (from env)", cfgOv.Audit.LossMode, "block")
	}
}

// TestLoad_SecretCustody_UnknownProviderFailsClosed exercises the
// resolveCustodySecrets error branch: an unrecognized custody provider makes
// kms.NewCustody fail, which must abort Load() (fail-closed) rather than boot
// with unresolved crown jewels.
func TestLoad_SecretCustody_UnknownProviderFailsClosed(t *testing.T) {
	setRequiredEnvBaseline(t)
	p := writeYAML(t, "secretCustody:\n  provider: not-a-real-provider\n")

	if _, err := Load(p); err == nil {
		t.Fatal("expected fail-closed error for an unknown secretCustody provider")
	}
}

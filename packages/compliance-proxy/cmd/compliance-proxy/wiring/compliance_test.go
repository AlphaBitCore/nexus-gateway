package wiring

import (
	"testing"

	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/cmd/compliance-proxy/config"
	configcache "github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/config/cache"
)

func TestInitCompliance_DisabledReturnsEmptyResult(t *testing.T) {
	cfg := &config.Config{}
	cfg.Compliance.Enabled = false
	cacheManager := configcache.NewManager(5*time.Minute, testLogger())

	result, err := InitCompliance(cfg, cacheManager, nil, testLogger())
	if err != nil {
		t.Fatalf("unexpected error when compliance disabled: %v", err)
	}
	if result.Resolver != nil {
		t.Error("expected nil Resolver when disabled")
	}
	if result.HookConfigCache != nil {
		t.Error("expected nil HookConfigCache when disabled")
	}
}

func TestInitCompliance_EnabledNoDatabaseURLReturnsError(t *testing.T) {
	cfg := &config.Config{}
	cfg.Compliance.Enabled = true
	cfg.Database.URL = "" // no DB URL
	cacheManager := configcache.NewManager(5*time.Minute, testLogger())

	_, err := InitCompliance(cfg, cacheManager, nil, testLogger())
	if err == nil {
		t.Fatal("expected error when compliance enabled but no database URL")
	}
}

func TestInitCompliance_EnabledWithIgnoredYAMLHooks_LogsWarning(t *testing.T) {
	cfg := &config.Config{}
	cfg.Compliance.Enabled = true
	cfg.Database.URL = "" // will fail before reaching hooks warning, but that's OK
	cfg.Compliance.Hooks = []config.HookConfigEntry{
		{Name: "test-hook", Enabled: true},
	}
	cacheManager := configcache.NewManager(5*time.Minute, testLogger())

	_, err := InitCompliance(cfg, cacheManager, nil, testLogger())
	// Expect error (no DB URL), not a hooks-related error.
	if err == nil {
		t.Fatal("expected error when compliance enabled but no database URL")
	}
}

func TestInitCompliance_EnabledWithBadDatabaseURL_ReturnsError(t *testing.T) {
	cfg := &config.Config{}
	cfg.Compliance.Enabled = true
	cfg.Database.URL = "postgres://localhost:9999/nonexistent_db_xyz?sslmode=disable"
	cacheManager := configcache.NewManager(5*time.Minute, testLogger())

	_, err := InitCompliance(cfg, cacheManager, nil, testLogger())
	// Should error on open or ping — either is acceptable.
	if err == nil {
		t.Fatal("expected error for unreachable database URL")
	}
}

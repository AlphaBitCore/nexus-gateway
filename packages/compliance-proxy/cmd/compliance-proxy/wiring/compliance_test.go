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

// limits.sseBufferLimit was parsed by config validation — a typo failed the
// boot — and then never reached streaming.LiveConfig, so the shared package's
// 8 MB default stood whatever an operator wrote. Validation is what made it
// dangerous rather than merely dead: rejecting a malformed value reads as
// confirmation that the value took effect.
//
// The assertions are on the VALUE that reaches LiveConfig, not on err == nil.
// A test that only checked the config parsed would have been green throughout
// the defect.
func TestInitCompliance_SSEBufferLimitReachesTheStreamingConfig(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{"explicit limit is honoured", "2MB", 2 * 1024 * 1024},
		{"a value below the package default is honoured too", "64KB", 64 * 1024},
		// Zero means "let the streaming package choose", which is how an
		// operator who never set the key keeps the 8 MB default.
		{"unset leaves the package default in charge", "", 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Compliance.Enabled = false // no database needed for this path
			cfg.Limits.SSEBufferLimit = tc.raw

			res, err := InitCompliance(cfg, configcache.NewManager(5*time.Minute, testLogger()), nil, testLogger())
			if err != nil {
				t.Fatalf("InitCompliance: %v", err)
			}
			if res.LiveConfig.MaxBufferSize != tc.want {
				t.Errorf("MaxBufferSize = %d, want %d — the operator's limits.sseBufferLimit "+
					"never reached the streaming pipeline", res.LiveConfig.MaxBufferSize, tc.want)
			}
		})
	}
}

// A malformed value is rejected at Load(); this covers the same parse failing
// at wiring time, which is reachable if a caller builds a Config by hand.
func TestInitCompliance_MalformedSSEBufferLimitIsAnError(t *testing.T) {
	cfg := &config.Config{}
	cfg.Compliance.Enabled = false
	cfg.Limits.SSEBufferLimit = "not-a-size"

	if _, err := InitCompliance(cfg, configcache.NewManager(5*time.Minute, testLogger()), nil, testLogger()); err == nil {
		t.Fatal("a malformed sseBufferLimit must not fall through to the package default in silence")
	}
}

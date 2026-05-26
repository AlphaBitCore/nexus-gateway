// reliability.go — credential reliability threshold config wiring.
package wiring

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"

	cachelayer "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/layer"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/credstate"
)

// ReliabilityConfigKey is the system_metadata key holding the global
// credential reliability thresholds.
const ReliabilityConfigKey = "gateway.credential_reliability.config"

// MetadataReader is the read surface a *store.DB satisfies.
// Exported so configdispatch can compose the ReliabilityReloader
// interface without importing store directly.
type MetadataReader interface {
	GetSystemMetadata(ctx context.Context, key string) (json.RawMessage, error)
}

// ReliabilityReloader narrows *ReliabilityConfig to Reload-only for
// TCInitDeps and configdispatch.Deps, avoiding a direct import of the
// concrete type across package boundaries.
type ReliabilityReloader interface {
	Reload(ctx context.Context, reader MetadataReader) error
}

// ReliabilityConfig holds the AI Gateway's view of effective per-credential
// reliability thresholds. The hot-path resolver in credstats.Buffer calls
// Resolve(credentialID) for every upstream attempt.
type ReliabilityConfig struct {
	global atomic.Pointer[credstate.Thresholds]
	cache  *cachelayer.Layer // optional — when nil, per-cred override falls back to global
	logger *slog.Logger
}

// NewReliabilityConfig wires the resolver against the credential cache
// and seeds the global snapshot with credstate.DefaultThresholds.
func NewReliabilityConfig(cache *cachelayer.Layer, logger *slog.Logger) *ReliabilityConfig {
	rc := &ReliabilityConfig{cache: cache, logger: logger}
	def := credstate.DefaultThresholds
	rc.global.Store(&def)
	return rc
}

// Reload loads the latest reliability thresholds from system_metadata
// and atomically swaps the global snapshot.
func (rc *ReliabilityConfig) Reload(ctx context.Context, reader MetadataReader) error {
	if reader == nil {
		return errors.New("reliability config: nil metadata reader")
	}
	raw, err := reader.GetSystemMetadata(ctx, ReliabilityConfigKey)
	if err != nil {
		return fmt.Errorf("reliability config: read system_metadata: %w", err)
	}
	if len(raw) == 0 {
		return nil // no global override; keep defaults
	}
	var loaded credstate.Thresholds
	if err := json.Unmarshal(raw, &loaded); err != nil {
		return fmt.Errorf("reliability config: parse: %w", err)
	}
	if err := loaded.Validate(); err != nil {
		return fmt.Errorf("reliability config: invalid: %w", err)
	}
	rc.global.Store(&loaded)
	if rc.logger != nil {
		rc.logger.Info("reliability config reloaded",
			"authFailThreshold", loaded.AuthFailThreshold,
			"healthyThresholdPct", loaded.HealthyThresholdPct,
			"degradedThresholdPct", loaded.DegradedThresholdPct,
			"healthMinSamples", loaded.HealthMinSamples,
			"healthWindowSeconds", loaded.HealthWindowSeconds,
		)
	}
	return nil
}

// Resolve returns the effective Thresholds for credentialID. Composition:
// credstate.DefaultThresholds (compiled in) → Hub-shadow global (rc.global)
// → per-credential override (Credential.reliabilityOverrides).
func (rc *ReliabilityConfig) Resolve(credentialID string) credstate.Thresholds {
	global := *rc.global.Load()
	if rc.cache == nil || credentialID == "" {
		return global
	}
	cred, err := rc.cache.GetCredentialByID(context.Background(), credentialID)
	if err != nil || cred == nil || len(cred.ReliabilityOverrides) == 0 {
		return global
	}
	var override credstate.Thresholds
	if err := json.Unmarshal(cred.ReliabilityOverrides, &override); err != nil {
		return global
	}
	return global.Merge(override)
}

// Snapshot returns a copy of the current global thresholds.
func (rc *ReliabilityConfig) Snapshot() credstate.Thresholds {
	return *rc.global.Load()
}

// Compile-time guard against drift.
var _ json.RawMessage = (store.Credential{}).ReliabilityOverrides

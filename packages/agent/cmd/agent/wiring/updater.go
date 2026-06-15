package wiring

import (
	"context"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/host/updater"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/hub"
	shareddiag "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag"
)

// InitUpdater creates the background updater (always polls availability for
// the UI banner; only auto-installs when enabled is true).
//
// The availability check runs unconditionally so the Dashboard "Update
// available" banner lights up even in deployments without an Ed25519
// public key (= no auto-install path).
//
// dataDir is the directory where the updater persists the version floor file
// (updater-floor.json). It should be the same persistent state directory used
// by the rest of the agent (derived from cfg.AuditDBPath's parent directory).
// Passing an empty string disables floor persistence (defense-in-depth only).
func InitUpdater(
	hubClient *hub.Client,
	enabled bool,
	checkIntervalSec int,
	version, goos, selfPath, dataDir string,
) *updater.Updater {
	return updater.NewUpdater(hubClient, updater.Config{
		Enabled:       enabled,
		CheckInterval: time.Duration(checkIntervalSec) * time.Second,
		DataDir:       dataDir,
	}, version, goos, selfPath)
}

// StartUpdater runs the updater's availability/auto-install loop in a
// recovered goroutine; availableFn receives "update available" transitions
// for the Dashboard banner.
func StartUpdater(ctx context.Context, up *updater.Updater, recoveryCfg shareddiag.RecoveryConfig, availableFn func(bool)) {
	go func() {
		rcfg := recoveryCfg
		rcfg.Source = "updater"
		defer shareddiag.Recover(rcfg, nil)
		up.RunWithAvailabilityCallback(ctx, availableFn)
	}()
}

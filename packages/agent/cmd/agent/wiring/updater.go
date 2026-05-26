package wiring

import (
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/host/updater"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/hub"
)

// InitUpdater creates the background updater (always polls availability for
// the UI banner; only auto-installs when enabled is true).
//
// The availability check runs unconditionally so the Dashboard "Update
// available" banner lights up even in deployments without an Ed25519
// public key (= no auto-install path).
func InitUpdater(
	hubClient *hub.Client,
	enabled bool,
	checkIntervalSec int,
	version, goos, selfPath string,
) *updater.Updater {
	return updater.NewUpdater(hubClient, updater.Config{
		Enabled:       enabled,
		CheckInterval: time.Duration(checkIntervalSec) * time.Second,
	}, version, goos, selfPath)
}

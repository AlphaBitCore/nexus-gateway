package wiring

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/observability/siem"
)

// InitSIEMBridge constructs the SIEM bridge unconditionally so the scheduler
// can register an always-on job. The bridge starts in the disabled state (no
// active sink) and self-configures from system_metadata['siem.config'] on its
// first Poll cycle.
//
// This replaces the prior "return nil when SIEM is disabled at startup" path
// so admin UI enables / disables propagate live within one poll interval
// without an operator restart.
func InitSIEMBridge(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) *siem.Bridge {
	bridge := siem.NewBridge(pool, nil, siem.BridgeConfig{}, logger)
	// Best-effort one-shot reload so the bridge starts hot if SIEM was
	// previously enabled. Errors are logged and recovered on the first Poll.
	if err := bridge.Reload(ctx); err != nil {
		logger.Warn("SIEM bridge: initial reload failed, will retry on next poll", "error", err)
	}
	if s := bridge.ActiveSinkName(); s != "" {
		logger.Info("SIEM bridge: initialized", "sink", s, "pollInterval", bridge.PollInterval())
	} else {
		logger.Info("SIEM bridge: registered in disabled state — will activate when siem.config.enabled becomes true")
	}
	return bridge
}

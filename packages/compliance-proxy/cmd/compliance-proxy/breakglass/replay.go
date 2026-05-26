// Package breakglass provides the background break-glass replay drain
// loop and the shadow probe health adapter.
package breakglass

import (
	"context"
	"log/slog"
	"time"

	runtimeserver "github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/server"
)

// replayInterval is how often the background loop checks for a
// pending break-glass report. Short enough that a recovering Hub is picked
// up quickly, long enough that a sustained outage does not pound the
// shadow endpoint. Tests inject their own interval via runReplayWith.
const replayInterval = 30 * time.Second

// replayer is the subset of *runtimeserver.Server that RunReplay requires.
// The narrow interface allows tests to inject a fake without constructing
// the full HTTP server; *runtimeserver.Server satisfies it in production.
type replayer interface {
	ReplayPending(ctx context.Context) (bool, error)
}

// RunReplay is the background drain loop. Exits when ctx is done.
// Delivery errors are logged at Warn; the next tick will try again because
// ReplayPending leaves the pending file in place on failure.
func RunReplay(ctx context.Context, srv *runtimeserver.Server, logger *slog.Logger) {
	runReplayWith(ctx, srv, logger, replayInterval)
}

// runReplayWith is the testable core of RunReplay. Production code calls
// RunReplay (which passes the package-level replayInterval); tests call
// runReplayWith directly with a short interval to exercise the ticker loop
// without real 30-second delays.
func runReplayWith(ctx context.Context, srv replayer, logger *slog.Logger, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tickCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			drained, err := srv.ReplayPending(tickCtx)
			cancel()
			if err != nil {
				logger.Warn("break-glass replay tick failed", "error", err)
				continue
			}
			if drained {
				logger.Info("break-glass pending drained on tick")
			}
		}
	}
}

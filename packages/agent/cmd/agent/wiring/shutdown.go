package wiring

import (
	"log/slog"
	"sync"
	"time"

	shareddiag "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag"
)

// WaitForAuditDrain blocks until the audit drain supervisor finishes its
// in-flight batch, or 10 seconds elapse — whichever comes first. Called
// during graceful shutdown so queued audit events get a final upload window
// without letting a stuck Hub hold the process open.
func WaitForAuditDrain(drainWg *sync.WaitGroup, recoveryCfg shareddiag.RecoveryConfig) {
	slog.Info("draining audit queue...")
	drainDone := make(chan struct{})
	go func() {
		rcfg := recoveryCfg
		rcfg.Source = "shutdown-wait"
		defer shareddiag.Recover(rcfg, nil)
		drainWg.Wait()
		close(drainDone)
	}()
	select {
	case <-drainDone:
		slog.Info("audit queue drained")
	case <-time.After(10 * time.Second):
		slog.Warn("audit drain timeout after 10s")
	}
}

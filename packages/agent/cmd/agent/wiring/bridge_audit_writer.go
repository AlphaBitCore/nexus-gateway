package wiring

import (
	"log/slog"

	auditqueue "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/queue"
	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

// NewBridgeAuditWriter builds the ONE audit writer the inspect path uses for the
// life of the process, selects the configured overflow policy on it, and says so
// at INFO.
//
// Two things this function exists to prevent, both of which were live-measured on a
// containerized agent rather than reasoned about:
//
//  1. A writer per flow. The queue writer owns a 4096-slot event channel, a
//     256-slot overflow channel and two goroutines. BumpFlow used to construct one
//     per intercepted TLS connection and never close it: 30 flows moved the daemon
//     from 70 to 130 goroutines (exactly +2 each) and its heap from 94.9 MB to
//     209.7 MB (+3.8 MB each), with nothing released afterwards. On a server that
//     is a leak; on the agent it is the user's own laptop.
//
//  2. A silent loss-mode. The AI Gateway and the compliance proxy both log the
//     configured and the resolved mode at startup. The agent — the one service
//     whose shipped default deliberately DIVERGES from the shared no-loss default
//     (spill, for CLAUDE.md's never-stall-the-host rule) — logged nothing, so
//     neither the divergence nor a typo silently resolving back to the blocking
//     default was visible to anyone running it.
//
// A nil queue returns a nil Writer: the caller's inspect path then fails its own
// nil-sink guard loudly instead of running with audit silently discarded.
func NewBridgeAuditWriter(q *auditqueue.Queue, lossMode string, logger *slog.Logger) sharedaudit.Writer {
	if q == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	w := auditqueue.NewQueueWriter(q).WithLossMode(lossMode)
	logger.Info("agent audit writer wired (one per process, not one per flow)",
		"configured", lossMode,
		"resolved", string(w.LossMode()),
	)
	return w
}

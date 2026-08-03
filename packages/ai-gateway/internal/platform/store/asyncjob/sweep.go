package asyncjob

// sweep.go — the retention sweep over gateway_async_job (e88-s6 §5).
//
// Locus decision (recorded in the SDD): the sweep runs GATEWAY-SIDE, reusing
// this package's MarkExpired, so the job-status vocabulary lives in exactly
// one place — a Hub job definition would have to duplicate the two-arm status
// SQL in a second service (drift risk) because Hub jobs cannot import
// gateway-internal packages.
//
// Mark-expired, never hard-delete: the row's status is a last-observed cache
// (a "stale non-terminal" row may be provider-complete), and the
// attribution/audit record must survive; an expired row serves 410 Gone.

import (
	"context"
	"log/slog"
	"time"
)

const (
	// SweepInterval is how often the retention sweep runs (plus one sweep at
	// boot so a restart clears backlog immediately).
	SweepInterval = time.Hour
	// TerminalRetention keeps finished (completed/failed/canceled) rows
	// pollable-with-history before they expire to 410.
	TerminalRetention = 30 * 24 * time.Hour
	// NonTerminalRetention bounds how long an apparently-running row can
	// count against the render bound: real renders finish in minutes, so a
	// week-old non-terminal row is an abandoned correlation (the client never
	// polled it to a terminal state), not a live render.
	NonTerminalRetention = 7 * 24 * time.Hour
)

// sweepPassTimeout bounds one MarkExpired statement so a wedged pass cannot
// block every subsequent hourly tick until shutdown.
const sweepPassTimeout = time.Minute

// Sweep runs one retention pass: terminal rows older than TerminalRetention
// and non-terminal rows older than NonTerminalRetention are marked expired.
// Errors are logged, not returned — the sweep is periodic and self-retrying.
func Sweep(ctx context.Context, s Store, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, sweepPassTimeout)
	defer cancel()
	now := time.Now().UTC()
	marked, err := s.MarkExpired(ctx, now.Add(-TerminalRetention), now.Add(-NonTerminalRetention))
	if err != nil {
		logger.Warn("asyncjob retention sweep failed; next interval retries",
			slog.String("error", err.Error()))
		return
	}
	if marked > 0 {
		logger.Info("asyncjob retention sweep marked rows expired",
			slog.Int64("rows", marked))
	}
}

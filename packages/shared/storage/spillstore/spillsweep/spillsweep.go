// Package spillsweep runs a periodic SpillStore.Sweep so a backend's retention
// horizon and total-size cap are actually enforced. Each service that owns a
// SpillStore starts one Run loop; the store is process-local, so sweeping is
// per-process rather than centralised — a localfs store is swept by the process
// that owns its directory, and a shared S3 bucket is swept (idempotently) by
// every process pointed at it.
package spillsweep

import (
	"context"
	"log/slog"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
)

// DefaultInterval is the sweep cadence used when Options.Interval is unset.
const DefaultInterval = 6 * time.Hour

// DefaultRetention is a sensible age horizon for callers that have no
// configured RetentionDays of their own (the agent's fixed local store). It
// matches the localfs backend's own default retention.
const DefaultRetention = 30 * 24 * time.Hour

// DBQuerier resolves which spill blob keys are still referenced by a live
// `traffic_event.spill_ref` row. The sweep consults it so a blob whose row
// still points at it is never deleted (orphaning the spill) and a blob whose
// row was already erased (GDPR / retention pruning) is collected promptly
// rather than lingering until its age window expires.
//
// HasSpillRefs is handed the age-eligible candidate keys in one batch and
// returns the subset still referenced (value true). The canonical pgx
// implementation runs:
//
//	SELECT DISTINCT spill_ref FROM "TrafficEvent"
//	 WHERE spill_ref IS NOT NULL AND spill_ref = ANY($1)
//
// On error the sweep deletes nothing and logs — a DB hiccup must never be
// read as "no rows reference these keys" (fail-safe). The wiring layer in
// each service supplies the pgx-backed implementation.
type DBQuerier interface {
	HasSpillRefs(ctx context.Context, keys []string) (referenced map[string]bool, err error)
}

// Options configures the sweep loop.
type Options struct {
	// Interval between sweeps. Defaults to DefaultInterval when <= 0.
	Interval time.Duration

	// Retention is the age horizon: each sweep deletes objects older than
	// now-Retention. A non-positive Retention disables the loop — the caller
	// asked to keep bodies indefinitely.
	Retention time.Duration

	// DB, when non-nil AND the store implements spillstore.RefAwareSweeper,
	// turns the age-based sweep into a reference-checked sweep: only blobs
	// that are both older than the retention window AND no longer referenced
	// by any traffic_event.spill_ref row are deleted. Leave nil to keep the
	// pure age-based behaviour (e.g. the agent's local store, which has no
	// traffic_event table to consult).
	DB DBQuerier
}

// dbFilter adapts a DBQuerier to spillstore.SweepFilter so the store-side
// sweep can run the reference check without importing this package.
type dbFilter struct{ db DBQuerier }

func (f dbFilter) KeepReferenced(ctx context.Context, candidateKeys []string) (map[string]bool, error) {
	return f.db.HasSpillRefs(ctx, candidateKeys)
}

// Run sweeps store every Options.Interval until ctx is cancelled. It blocks for
// the lifetime of the loop, so callers start it in a goroutine. It sweeps once
// immediately so a freshly-started process prunes without waiting a full
// interval, then on each tick. A nil store or a non-positive Retention makes
// Run a no-op so callers can wire it unconditionally.
func Run(ctx context.Context, store spillstore.SpillStore, opts Options, logger *slog.Logger) {
	if store == nil || opts.Retention <= 0 {
		return
	}
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("component", "spillsweep", "backend", store.Backend())

	// Resolve the reference-checked path once: only when a DBQuerier is
	// configured AND the backend can run a filtered sweep. Otherwise fall
	// back to the plain age-based Sweep.
	var filter spillstore.SweepFilter
	refAware, _ := store.(spillstore.RefAwareSweeper)
	if opts.DB != nil && refAware != nil {
		filter = dbFilter{db: opts.DB}
		logger = logger.With("reference_check", true)
	} else {
		logger = logger.With("reference_check", false)
		if opts.DB != nil && refAware == nil {
			logger.Warn("spill sweep: DBQuerier set but backend is not reference-aware; sweeping age-only")
		}
	}

	sweepOnce(ctx, store, refAware, filter, opts.Retention, logger)

	ticker := time.NewTicker(opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweepOnce(ctx, store, refAware, filter, opts.Retention, logger)
		}
	}
}

// sweepOnce runs a single sweep against the store. When a filter is supplied
// (DBQuerier configured and backend reference-aware), it runs the filtered
// sweep so still-referenced blobs are never deleted; otherwise it runs the
// plain age-based Sweep. Errors are logged and swallowed — a failed sweep is a
// transient storage problem, not a reason to stop the loop or crash the
// service. A reference-check error therefore deletes nothing this cycle and
// retries next interval (fail-safe).
func sweepOnce(
	ctx context.Context,
	store spillstore.SpillStore,
	refAware spillstore.RefAwareSweeper,
	filter spillstore.SweepFilter,
	retention time.Duration,
	logger *slog.Logger,
) {
	cutoff := time.Now().Add(-retention)
	var (
		deleted int
		err     error
	)
	if filter != nil && refAware != nil {
		deleted, err = refAware.SweepFiltered(ctx, cutoff, filter)
	} else {
		deleted, err = store.Sweep(ctx, cutoff)
	}
	if err != nil {
		logger.Warn("spill sweep failed", "error", err, "cutoff", cutoff.Format(time.RFC3339))
		return
	}
	if deleted > 0 {
		logger.Info("spill sweep", "deleted", deleted, "cutoff", cutoff.Format(time.RFC3339))
	}
}

// Package vendorbill (jobs/defs) hosts the daily vendor-bill reconciliation
// job: it pulls each covered provider's authoritative daily billed USD from the
// vendor's cost API (via internal/vendorbill sources), reads our own estimated
// spend from metric_rollup_1d, and UPSERTs the per-provider-per-day comparison
// into vendor_bill_reconciliation for the drift alert + CP report to consume.
//
// Design: docs/superpowers/specs/2026-07-19-vendor-bill-reconciliation-design.md.
package vendorbill

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/defs"
	billsrc "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/vendorbill"
	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

const (
	reconcileJobID   = "vendor-bill-reconcile"
	reconcileJobName = "Vendor Bill Reconciliation"
	reconcileJobDesc = "Once per day, pulls OpenAI/Anthropic authoritative daily billed USD and diffs it against our estimated spend (metric_rollup_1d billed_cost_usd), writing per-provider-per-day rows to vendor_bill_reconciliation."

	// defaultLookbackDays (N): how many trailing days each run re-reconciles,
	// so late vendor revisions self-heal on a subsequent run.
	defaultLookbackDays = 4
	// defaultFinalizeLagDays: reconcile only up to today-2 (UTC); the most
	// recent day(s) are not yet finalized on the vendor side.
	defaultFinalizeLagDays = 2

	coverageScoped      = "scoped"
	coverageOrgOnly     = "org_only"
	coverageFetchFailed = "fetch_failed"
)

// sourceRegistry is the minimal surface the job needs from vendorbill.Registry.
type sourceRegistry interface {
	Resolve(adapterType string) billsrc.VendorBillSource
}

// VendorBillReconcileJob implements scheduler.Job.
type VendorBillReconcileJob struct {
	pool     defs.PgxPool
	registry sourceRegistry
	interval time.Duration
	logger   *slog.Logger

	lookbackDays    int
	finalizeLagDays int
	now             func() time.Time // injectable clock for tests
}

// NewVendorBillReconcileJob constructs the daily job. interval defaults to 24h.
func NewVendorBillReconcileJob(pool *pgxpool.Pool, registry *billsrc.Registry, interval time.Duration, logger *slog.Logger) *VendorBillReconcileJob {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	return &VendorBillReconcileJob{
		pool:            pool,
		registry:        registry,
		interval:        interval,
		logger:          logger.With("job", reconcileJobID),
		lookbackDays:    defaultLookbackDays,
		finalizeLagDays: defaultFinalizeLagDays,
		now:             func() time.Time { return time.Now().UTC() },
	}
}

func (j *VendorBillReconcileJob) ID() string              { return reconcileJobID }
func (j *VendorBillReconcileJob) Name() string            { return reconcileJobName }
func (j *VendorBillReconcileJob) Description() string     { return reconcileJobDesc }
func (j *VendorBillReconcileJob) Interval() time.Duration { return j.interval }

// RunOnStart runs one pass right after boot rather than waiting a full day.
func (j *VendorBillReconcileJob) RunOnStart() bool { return true }

// window returns the inclusive [from, to] UTC-day reconcile window: ending at
// today-finalizeLagDays, spanning lookbackDays days.
func (j *VendorBillReconcileJob) window() (from, to time.Time) {
	to = j.now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -j.finalizeLagDays)
	from = to.AddDate(0, 0, -(j.lookbackDays - 1))
	return from, to
}

func (j *VendorBillReconcileJob) Run(ctx context.Context) error {
	from, to := j.window()
	providers, err := j.listCoveredProviders(ctx)
	if err != nil {
		return fmt.Errorf("list providers: %w", err)
	}
	for _, cp := range providers {
		if err := j.reconcileProvider(ctx, cp.id, cp.src, from, to); err != nil {
			// One provider's failure must not abort the others.
			j.logger.Error("reconcile provider failed", "provider", cp.id, "error", err)
		}
	}
	return nil
}

// coveredProvider is a Provider row already paired with its resolved source, so
// Run neither re-resolves nor needs a defensive nil check.
type coveredProvider struct {
	id  string
	src billsrc.VendorBillSource
}

// listCoveredProviders returns enabled providers whose adapter has a source,
// each paired with that source.
func (j *VendorBillReconcileJob) listCoveredProviders(ctx context.Context) ([]coveredProvider, error) {
	rows, err := j.pool.Query(ctx, `SELECT id, adapter_type FROM "Provider" WHERE enabled = true`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []coveredProvider
	for rows.Next() {
		var id, adapterType string
		if err := rows.Scan(&id, &adapterType); err != nil {
			return nil, err
		}
		if src := j.registry.Resolve(adapterType); src != nil {
			out = append(out, coveredProvider{id: id, src: src})
		}
	}
	return out, rows.Err()
}

func (j *VendorBillReconcileJob) reconcileProvider(ctx context.Context, providerID string, src billsrc.VendorBillSource, from, to time.Time) error {
	bills, err := src.FetchDailyBill(ctx, from, to)
	if err != nil {
		// Fetch failed: seed a fetch_failed placeholder for any day we have never
		// reconciled so operators see the gap — but NEVER overwrite an existing
		// row. A transient vendor error (500/429/network) must not erase a
		// previously-good vendor_reported_usd; if it did, and the day then aged
		// out of the trailing re-reconcile window, the number would be lost
		// silently (per-provider isolation keeps job_run=success and the drift
		// alert never fires on fetch_failed). The write is therefore
		// non-destructive; a later successful run heals a placeholder via upsert.
		j.logger.Warn("vendor fetch failed", "provider", providerID, "error", err)
		for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
			our, qerr := j.ourBilled(ctx, providerID, d)
			if qerr != nil {
				return qerr
			}
			if err := j.insertFetchFailedIfAbsent(ctx, providerID, d, our); err != nil {
				return err
			}
		}
		return nil
	}

	byDay := make(map[time.Time]billsrc.VendorDailyBill, len(bills))
	for _, b := range bills {
		byDay[b.Day.UTC().Truncate(24*time.Hour)] = b
	}

	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		b, ok := byDay[d]
		if !ok {
			// Vendor reported no data for this day: not yet finalized. Absence is
			// not zero — skip; a later run within the window fills it in once the
			// vendor finalizes.
			continue
		}
		our, err := j.ourBilled(ctx, providerID, d)
		if err != nil {
			return err
		}
		diffUSD := b.AmountUSD - our
		diffPct := pctDiff(b.AmountUSD, our)
		coverage := coverageScoped
		if b.ScopeKind == scopeOrgKind {
			coverage = coverageOrgOnly
		}
		if err := j.upsert(ctx, providerID, d, our, b.AmountUSD, diffUSD, diffPct, b.ScopeKind, coverage); err != nil {
			return err
		}
	}
	return nil
}

const scopeOrgKind = "org"

// pctDiff returns the estimate's relative error against the vendor's
// authoritative number, as a signed fraction: (vendor-our)/vendor. Positive
// means we under-estimated, negative means we over-estimated.
//
// The denominator is the VENDOR value, not max(vendor, our): the vendor bill is
// the ground truth the estimate is being measured against, so it is what the
// error is relative to. An earlier max()-based denominator silently compressed
// over-estimates — estimating 200 against a true 100 reported -50% rather than
// -100%, making a 2x over-estimate look milder than a 2x under-estimate.
//
// Edge case: a vendor-zero day with a non-zero estimate has no finite relative
// error, so it falls back to the estimate as denominator, yielding exactly
// -100% ("we billed for something the vendor did not"). Both zero is 0.
func pctDiff(vendor, our float64) float64 {
	if vendor == 0 {
		if our == 0 {
			return 0
		}
		return -1
	}
	return (vendor - our) / vendor
}

// ourBilled sums metric_rollup_1d billed_cost_usd for routed_provider=<id> on
// the given UTC day.
func (j *VendorBillReconcileJob) ourBilled(ctx context.Context, providerID string, day time.Time) (float64, error) {
	var v float64
	err := j.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM("value"), 0)
		FROM metric_rollup_1d
		WHERE "metricName" = $1
		  AND "dimensionKey" = $2
		  AND "bucketStart" >= $3 AND "bucketStart" < $4
	`,
		metrics.MetricBilledCostUSD,
		metrics.BuildDimensionKey("routed_provider", providerID),
		day, day.AddDate(0, 0, 1),
	).Scan(&v)
	return v, err
}

// insertFetchFailedIfAbsent records a fetch_failed placeholder for a day ONLY
// when no row yet exists for (provider, day) — ON CONFLICT DO NOTHING. It must
// never overwrite a previously reconciled row: a transient vendor error would
// otherwise replace a known-good vendor_reported_usd with NULL, and once the day
// ages out of the trailing re-reconcile window that number is permanently lost,
// silently. Never-seen days still surface as fetch_failed so operators see the
// gap; a later successful run heals a placeholder through upsert's DO UPDATE.
func (j *VendorBillReconcileJob) insertFetchFailedIfAbsent(ctx context.Context, providerID string, day time.Time, our float64) error {
	_, err := j.pool.Exec(ctx, `
		INSERT INTO vendor_bill_reconciliation
		  (provider_id, day, our_billed_usd, vendor_reported_usd, diff_usd, diff_pct, scope_kind, coverage, status, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'open', now())
		ON CONFLICT (provider_id, day) DO NOTHING
	`, providerID, day, our, nil, nil, nil, scopeOrgKind, coverageFetchFailed)
	return err
}

// upsert writes one reconciliation row. On conflict it refreshes the numbers
// and coverage but preserves an operator's review state (status/reviewed_by/
// note) so a self-healing re-run does not silently un-review a row.
func (j *VendorBillReconcileJob) upsert(ctx context.Context, providerID string, day time.Time, our float64, vendor, diffUSD, diffPct any, scopeKind, coverage string) error {
	_, err := j.pool.Exec(ctx, `
		INSERT INTO vendor_bill_reconciliation
		  (provider_id, day, our_billed_usd, vendor_reported_usd, diff_usd, diff_pct, scope_kind, coverage, status, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'open', now())
		ON CONFLICT (provider_id, day) DO UPDATE SET
		  our_billed_usd      = EXCLUDED.our_billed_usd,
		  vendor_reported_usd = EXCLUDED.vendor_reported_usd,
		  diff_usd            = EXCLUDED.diff_usd,
		  diff_pct            = EXCLUDED.diff_pct,
		  scope_kind          = EXCLUDED.scope_kind,
		  coverage            = EXCLUDED.coverage,
		  updated_at          = now()
	`, providerID, day, our, vendor, diffUSD, diffPct, scopeKind, coverage)
	return err
}

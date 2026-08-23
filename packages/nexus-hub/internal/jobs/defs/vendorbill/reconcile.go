// Package vendorbill (jobs/defs) hosts the daily vendor-bill reconciliation
// job: it pulls each covered provider's authoritative daily billed USD from the
// vendor's cost API (via internal/vendorbill sources), reads our own recorded
// vendor spend from metric_rollup_1d, and UPSERTs the per-provider-per-day
// comparison into vendor_bill_reconciliation for the drift alert + CP report to
// consume.
//
// Design: docs/superpowers/specs/2026-07-19-vendor-bill-reconciliation-design.md.
package vendorbill

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/defs"
	defjobs_rollup "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/defs/rollup"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/quota/rollup"
	billsrc "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/vendorbill"
	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

const (
	reconcileJobID   = "vendor-bill-reconcile"
	reconcileJobName = "Vendor Bill Reconciliation"
	reconcileJobDesc = "Once per day, pulls OpenAI/Anthropic authoritative daily billed USD and diffs it against our recorded vendor spend (metric_rollup_1d vendor_spend_usd), writing per-provider-per-day rows to vendor_bill_reconciliation."

	// defaultLookbackDays (N): how many trailing days each run re-reconciles,
	// so late vendor revisions self-heal on a subsequent run.
	//
	// Sized to outlast a stalled correction watermark, not just a late vendor
	// revision. A day is only reconcilable once the correction pass has rebuilt
	// it, so every day the correction job fails to advance is a day this window
	// must still be covering when it recovers. At the previous value of 4 the
	// margin was a single day: the 2026-08-06..08-08 stall (see awaitCorrection)
	// left 2026-08-03 holding placeholder rows that were about to age out of the
	// window while metric_rollup_1d already held the real vendor spend for it,
	// which would have frozen wrong numbers permanently. 8 days covers a full
	// correction outage over a weekend and still costs only
	// providers × 8 rows per run.
	defaultLookbackDays = 8
	// defaultFinalizeLagDays: reconcile only up to today-2 (UTC); the most
	// recent day(s) are not yet finalized on the vendor side.
	defaultFinalizeLagDays = 2

	// defaultCorrectionWait bounds how long a run waits for the rollup
	// correction pass to reach the end of its window before proceeding with
	// whatever the watermark has reached. See awaitCorrection.
	defaultCorrectionWait = 10 * time.Minute
	// defaultCorrectionPoll is how often that wait re-reads the watermark.
	defaultCorrectionPoll = 15 * time.Second

	coverageScoped      = "scoped"
	coverageOrgOnly     = "org_only"
	coverageFetchFailed = "fetch_failed"
	// coverageNoBasis: the vendor reported a real number but our side has no
	// recorded vendor spend for that provider×day, so there is nothing to
	// compare it against. Display-only, drift-alert-suppressed (the drift job
	// filters on `scoped`), but NOT silent: VendorBillSyncAlertsJob counts these
	// alongside fetch_failed, so a persistent gap raises vendor.bill_sync_failed.
	coverageNoBasis = "no_basis"
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

	// correctionWait / correctionPoll bound awaitCorrection. sleep is the
	// delay seam tests replace so they can drive the poll loop without
	// wall-clock time.
	correctionWait time.Duration
	correctionPoll time.Duration
	sleep          func(ctx context.Context, d time.Duration) error
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
		correctionWait:  defaultCorrectionWait,
		correctionPoll:  defaultCorrectionPoll,
		sleep:           sleepCtx,
	}
}

// sleepCtx waits for d, or returns early if the run's context is cancelled so
// a shutdown mid-wait does not hold the scheduler open.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
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
	// How far the rollup-correction pass has re-aggregated the 1d tier from
	// traffic_event. Days past this point are NOT reconciled at all — see
	// reconcileProvider for why a day the correction has not rebuilt can be
	// neither compared nor declared basis-less.
	corrected, err := j.awaitCorrection(ctx, to)
	if err != nil {
		return err
	}
	if corrected.Before(from) {
		// Not one day in the window is reconcilable, so this run would walk every
		// provider, defer every day, write nothing, and return nil — which is
		// exactly what happened on stg from 2026-08-06 to 08-08: three
		// consecutive runs recorded job_run=success while the report silently
		// stopped at 2026-08-03. A run that cannot do any of its work has failed,
		// and saying so is what puts it on the Jobs page and in the failure
		// alerting instead of leaving the gap to be noticed by eye weeks later.
		return fmt.Errorf("rollup correction has not reached the reconcile window (corrected through %s, window starts %s): no day in the window can be reconciled",
			formatWatermark(corrected), from.Format(dayFormat))
	}
	if corrected.Before(to) {
		// Partial: the older days reconcile now and the rest are picked up by a
		// later run once the correction pass has passed them. Logged because the
		// alternative is silence: a deferred day writes no row at all, so nothing
		// else in the system would show that the window is waiting on the
		// correction job.
		j.logger.Warn("rollup correction has not reached the end of the reconcile window; the days past it are deferred",
			"correctedThrough", formatWatermark(corrected),
			"windowTo", to.Format(dayFormat))
	}
	providers, err := j.listCoveredProviders(ctx)
	if err != nil {
		return fmt.Errorf("list providers: %w", err)
	}
	for _, cp := range providers {
		if err := j.reconcileProvider(ctx, cp.id, cp.src, from, to, corrected); err != nil {
			// One provider's failure must not abort the others.
			j.logger.Error("reconcile provider failed", "provider", cp.id, "error", err)
		}
	}
	return nil
}

// awaitCorrection returns how far the rollup-correction pass has re-aggregated
// the 1d tier, waiting up to correctionWait for it to reach `to` (the end of
// the reconcile window) before giving up and returning whatever it has reached.
//
// The wait exists because this job and rollup-correction are two independently
// registered jobs on one scheduler with the same 24h interval, so every tick
// fires both at the same instant. This job finishes in ~2s; the correction pass
// takes ~90s and only publishes its watermark at the very end, after every
// layer of the window has committed. A single read therefore always observes
// the PREVIOUS day's watermark, which at steady state lands exactly on `to`
// with zero margin — and the moment anything costs the correction job a run,
// the margin is gone and this job silently reconciles nothing.
//
// That is not hypothetical. On stg the watermark row did not exist at all until
// the correction job's first success on 2026-08-08 02:58 (the 08-07 run died on
// a Postgres deadlock), so every run from the 08-06 deploy onward read the zero
// value, deferred all four days of its window, and wrote nothing while
// reporting success. Waiting for the value this run actually depends on, rather
// than racing it by 24 milliseconds, is what makes the dependency real.
//
// Bounded rather than unbounded: if the correction pass is failing outright,
// blocking forever would just move the silence into a job that never returns.
// On timeout the caller decides — a window that is partly reconcilable proceeds
// with a warning, one that is not at all fails loudly.
func (j *VendorBillReconcileJob) awaitCorrection(ctx context.Context, to time.Time) (time.Time, error) {
	deadline := j.now().Add(j.correctionWait)
	for attempt := 0; ; attempt++ {
		corrected, err := rollupstore.GetWatermark(ctx, j.pool, defjobs_rollup.WatermarkCorrection)
		if err != nil {
			return time.Time{}, fmt.Errorf("read correction watermark: %w", err)
		}
		if !corrected.Before(to) {
			if attempt > 0 {
				j.logger.Info("rollup correction reached the reconcile window while waiting",
					"correctedThrough", formatWatermark(corrected),
					"windowTo", to.Format(dayFormat),
					"polls", attempt)
			}
			return corrected, nil
		}
		if !j.now().Before(deadline) {
			j.logger.Warn("gave up waiting for the rollup correction pass",
				"correctedThrough", formatWatermark(corrected),
				"windowTo", to.Format(dayFormat),
				"waited", j.correctionWait.String())
			return corrected, nil
		}
		if attempt == 0 {
			j.logger.Info("waiting for the rollup correction pass to reach the reconcile window",
				"correctedThrough", formatWatermark(corrected),
				"windowTo", to.Format(dayFormat),
				"timeout", j.correctionWait.String())
		}
		if err := j.sleep(ctx, j.correctionPoll); err != nil {
			return time.Time{}, fmt.Errorf("waiting for rollup correction: %w", err)
		}
	}
}

// dayFormat is the UTC-day rendering used in every log field and SQL day value
// this job emits.
const dayFormat = "2006-01-02"

// formatWatermark renders a correction watermark for a log field, spelling out
// the zero value rather than printing year 1 — a watermark that has never been
// set is the normal state between this job's first run and the correction job's
// first run, and "never" says so.
func formatWatermark(wm time.Time) string {
	if wm.IsZero() {
		return "never"
	}
	return wm.Format(dayFormat)
}

// coveredProvider is a Provider row already paired with its resolved source, so
// Run neither re-resolves nor needs a defensive nil check.
type coveredProvider struct {
	id  string
	src billsrc.VendorBillSource
}

// listCoveredProviders returns enabled providers whose adapter has a source
// AND whose baseUrl names that source's own billing host, each paired with the
// source.
//
// Both halves are required because adapter_type names a WIRE FORMAT, not a
// vendor. A self-hosted model, an OpenAI-compatible appliance, or a local
// inference box is configured as adapter_type "openai" and bills the OpenAI
// organization nothing; resolving on the type alone would give it the real
// vendor's daily total as its own vendor_reported_usd — the same vendor
// dollars counted twice, under a provider that never spent them, against a
// vendor_spend series that is correctly attributed per provider id and so
// would show as a total drift. See billsrc.SameBillingHost.
func (j *VendorBillReconcileJob) listCoveredProviders(ctx context.Context) ([]coveredProvider, error) {
	rows, err := j.pool.Query(ctx, `SELECT id, adapter_type, COALESCE("baseUrl", '') FROM "Provider" WHERE enabled = true`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []coveredProvider
	for rows.Next() {
		var id, adapterType, baseURL string
		if err := rows.Scan(&id, &adapterType, &baseURL); err != nil {
			return nil, err
		}
		src := j.registry.Resolve(adapterType)
		if src == nil {
			continue
		}
		if !billsrc.SameBillingHost(baseURL, src.BillingHost()) {
			// Logged rather than skipped silently: "my provider has no
			// reconciliation row" is otherwise indistinguishable from a broken
			// job, and this is the one exclusion an operator can cause purely
			// by configuration.
			j.logger.Info("provider shares an adapter type with a covered vendor but is served from a different host; not reconciled against that vendor's bill",
				"provider", id,
				"adapterType", adapterType,
				"providerBaseUrl", baseURL,
				"vendorBillingHost", src.BillingHost())
			continue
		}
		out = append(out, coveredProvider{id: id, src: src})
	}
	return out, rows.Err()
}

func (j *VendorBillReconcileJob) reconcileProvider(ctx context.Context, providerID string, src billsrc.VendorBillSource, from, to, corrected time.Time) error {
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
		if d.After(corrected) {
			// The rollup-correction pass has not rebuilt this day from
			// traffic_event yet, so metric_rollup_1d cannot answer either question
			// this job asks of it, and BOTH wrong answers are damaging:
			//
			//   - The series is absent because it was never aggregated, not
			//     because no money was spent. Writing `no_basis` here claims the
			//     gateway recorded nothing for a day the vendor billed, and
			//     VendorBillSyncAlertsJob raises vendor.bill_sync_failed 25 hours
			//     later on a placeholder that was wrong when written. This is what
			//     the vendor-spend cutover hit on stg (2026-08-04/05): the reconcile
			//     job read the tier ~40s into a correction pass that took ~100s to
			//     write it, every run, and every day in the window came out
			//     no_basis while the rows landed minutes afterwards.
			//   - The series is present but partial — the live 5m aggregation
			//     covered only part of the day, typically the part after a deploy.
			//     That value looks like a real basis and yields a real-looking
			//     diff, so it does not merely go unnoticed: it fires the drift
			//     alert with a fabricated under-record.
			//
			// Deferring writes nothing at all. The day stays in the trailing
			// window and is reconciled by a later run once the correction pass has
			// passed it, which at steady state is the run of the following day.
			j.logger.Info("day not yet re-aggregated by rollup correction, deferring",
				"provider", providerID,
				"day", d.Format(dayFormat),
				"correctedThrough", formatWatermark(corrected))
			continue
		}
		spend, spendRecorded, err := j.rollupSum(ctx, metrics.MetricVendorSpendUSD, providerID, d)
		if err != nil {
			return err
		}
		if !spendRecorded {
			// No vendor_spend_usd row exists for this (provider, day) at all,
			// on a day the correction pass HAS re-aggregated (the gate above
			// already deferred the days it has not). Absence is not zero — the
			// rollup drops zero-valued rows, so what this says is that the day
			// was rebuilt from traffic_event and still produced no vendor spend
			// under any provider dimension. Overwriting the row with
			// diff = vendor - 0 would report the day at -100% and fire the drift
			// alert, so the comparison is NOT computed and the existing row (if
			// any) is left exactly as it was: pre-cutover rows keep their
			// old-basis values (design §7 — the router cost was never recorded,
			// so no backfill can make them comparable).
			//
			// But the day must not vanish either. A provider×day whose cost
			// stamping is broken — one missing pricing row zeroing
			// estimated_cost_usd with no router or embedding cost — reaches here
			// too, and that is a 100% under-record of real vendor money, exactly
			// what this feature exists to expose. So the day is logged and a
			// display-only `no_basis` placeholder is inserted for days that have
			// never been reconciled at all. The insert is ON CONFLICT DO NOTHING,
			// so it can never touch the pre-cutover rows the paragraph above
			// protects; the only rows it creates are for the suspicious set.
			//
			// A vendor-reported $0 day is NOT one of those: the vendor charged
			// nothing and the gateway recorded no vendor spend, which is a
			// genuinely consistent day, not a gap. Flagging it as `no_basis`
			// would raise `vendor.bill_sync_failed` 25 hours later for a day
			// with nothing wrong with it, so it takes the zero-row branch
			// immediately below instead.
			if b.AmountUSD == 0 {
				// The vendor charged nothing and the rebuilt rollup recorded
				// nothing: a genuinely idle day, consistent on both sides.
				//
				// It is WRITTEN, as a real `scoped` row of zeros, rather than
				// skipped. An absent row and a zero row are the same blank in
				// the report, so skipping asks an operator to tell "no traffic
				// that day" apart from "the job never reconciled that day" —
				// which is precisely the question vendor_bill_reconciliation
				// carries a coverage column to answer.
				//
				// Going through upsert (not the insert-if-absent placeholder
				// path) is load-bearing: it is what HEALS a fetch_failed
				// placeholder that a transient vendor error left on an idle
				// day. Nothing else in this loop can — the vendor has no
				// non-zero figure to report for such a day, ever — so the row
				// would otherwise sit at fetch_failed until it aged out of the
				// trailing window and froze there, still raising
				// vendor.bill_sync_failed (prod OpenAI 2026-08-16).
				//
				// This is not the fabricated zero the enclosing guard exists to
				// prevent. There, our side is missing and the vendor's is real,
				// so a difference would be a -100% invented out of an absent
				// series. Here the vendor itself reports zero: both operands
				// are known, they agree, and the diff is a true 0.
				//
				// Reached only after the correction-watermark gate above, so
				// "the rollup recorded nothing" means the day was rebuilt and
				// produced nothing — not that it has yet to be rebuilt.
				our, ourErr := j.ourBilled(ctx, providerID, d)
				if ourErr != nil {
					return ourErr
				}
				zeroCoverage := coverageScoped
				if b.ScopeKind == scopeOrgKind {
					zeroCoverage = coverageOrgOnly
				}
				if err := j.upsert(ctx, providerID, d, our, 0, 0, 0.0, 0.0, 0.0, b.ScopeKind, zeroCoverage); err != nil {
					return err
				}
				continue
			}
			j.logger.Warn("vendor billed a day we have no recorded vendor spend for",
				"provider", providerID,
				"day", d.Format(dayFormat),
				"vendorReportedUsd", b.AmountUSD,
				"coverage", coverageNoBasis)
			if err := j.insertNoBasisIfAbsent(ctx, providerID, d, b.AmountUSD, b.ScopeKind); err != nil {
				return err
			}
			continue
		}
		// internalOps is legitimately absent on a day with only customer traffic,
		// so its zero is a real zero, not a missing series.
		internalOps, _, err := j.rollupSum(ctx, metrics.MetricVendorSpendInternalUSD, providerID, d)
		if err != nil {
			return err
		}
		// Read last: a skipped day above must not pay for a query nobody uses.
		our, err := j.ourBilled(ctx, providerID, d)
		if err != nil {
			return err
		}
		// The diff measures the estimate against the bill, so it is computed from
		// the VENDOR-SPEND side: every dollar we caused the vendor to charge,
		// including internal-ops calls the customer-quota series omits.
		// our_billed_usd stays on the row as the quota-basis figure for continuity.
		//
		// diff_usd is quantised to cents (see roundToCents): the vendor bill is
		// only meaningful to the cent, so a sub-cent disagreement in the AMOUNT
		// is the vendor's own rounding, not drift.
		//
		// diff_pct is NOT, and deliberately so. A ratio taken from the rounded
		// pair inherits the rounding as error scaled by 1/vendor, which is
		// unbounded as the day's spend shrinks: on a $1.28 day a real 1.285%
		// drift lands both operands in the same cent and reports exactly 0.0%,
		// erasing the signal on precisely the low-volume days where a
		// percentage is the only way to see it. The percentage therefore reads
		// the unrounded values, where its own resolution is not the vendor's.
		// The two figures answering the same question at different resolutions
		// is intended: diff_usd is what the vendor could have billed
		// differently, diff_pct is how far off the estimate actually is.
		//
		// The unrounded b.AmountUSD and spend are still what gets persisted
		// below — only diffUSD is computed from the rounded pair.
		diffUSD := roundToCents(b.AmountUSD) - roundToCents(spend)
		diffPct := pctDiff(b.AmountUSD, spend)
		coverage := coverageScoped
		if b.ScopeKind == scopeOrgKind {
			coverage = coverageOrgOnly
		}
		if err := j.upsert(ctx, providerID, d, our, spend, internalOps, b.AmountUSD, diffUSD, diffPct, b.ScopeKind, coverage); err != nil {
			return err
		}
	}
	return nil
}

const scopeOrgKind = "org"

// roundToCents quantises a USD amount to the vendor's own reporting
// resolution — the cent — using round-half-away-from-zero (math.Round), never
// truncation. It exists ONLY to make diff_usd comparable at the resolution the
// vendor actually reports (diff_pct reads the unrounded pair — see the comment
// at its call site for why a ratio must not inherit this rounding): both
// OpenAI's and Anthropic's cost APIs are cent-denominated (see the "Amount
// scale" section of docs/operators/ops/runbooks/vendor-bill-reconciliation.md),
// so comparing them against our 6-to-10-decimal-place estimate reports the
// vendor's own rounding as our drift. Storage precision is untouched — every
// caller keeps writing the unrounded amount to its Decimal(20,10) column;
// only the diff computation reads the rounded pair.
func roundToCents(v float64) float64 {
	return math.Round(v*100) / 100
}

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

// rollupSum sums one metric_rollup_1d series for routed_provider=<id> over a
// single UTC day. Shared by the billed / vendor-spend / internal-ops reads so
// the three cannot drift in dimension or window.
//
// The second return distinguishes "the series summed to zero" from "the tier
// holds no row for this (metric, provider, day)". The vendor-spend series only
// has rows for buckets the aggregator has actually processed, so absence means
// "not aggregated", never "no spend"; callers that would otherwise write a
// fabricated 100% difference must skip on !recorded. COUNT(*) rather than a
// nullable SUM keeps both values plainly scannable.
//
// There is deliberately no global-dimension total for the vendor-spend series:
// BuildDimensionKey(dim, "") is the global row, and unattributable money is
// excluded from these series entirely, so only per-provider keys are read.
func (j *VendorBillReconcileJob) rollupSum(ctx context.Context, metricName, providerID string, day time.Time) (sum float64, recorded bool, err error) {
	var n int64
	err = j.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM("value"), 0), COUNT(*)
		FROM metric_rollup_1d
		WHERE "metricName" = $1
		  AND "dimensionKey" = $2
		  AND "bucketStart" >= $3 AND "bucketStart" < $4
	`,
		metricName,
		metrics.BuildDimensionKey("routed_provider", providerID),
		day, day.AddDate(0, 0, 1),
	).Scan(&sum, &n)
	if err != nil {
		return 0, false, err
	}
	return sum, n > 0, nil
}

// ourBilled sums metric_rollup_1d billed_cost_usd for routed_provider=<id> on
// the given UTC day. This is the customer-quota basis persisted as
// our_billed_usd — NOT the reconciliation basis; an absent series is reported
// as 0 here because the fetch-failed placeholder has no diff to fabricate.
func (j *VendorBillReconcileJob) ourBilled(ctx context.Context, providerID string, day time.Time) (float64, error) {
	v, _, err := j.rollupSum(ctx, metrics.MetricBilledCostUSD, providerID, day)
	return v, err
}

// insertFetchFailedIfAbsent records a fetch_failed placeholder for a day ONLY
// when no row yet exists for (provider, day) — ON CONFLICT DO NOTHING. It must
// never overwrite a previously reconciled row: a transient vendor error would
// otherwise replace a known-good vendor_reported_usd with NULL, and once the day
// ages out of the trailing re-reconcile window that number is permanently lost,
// silently. Never-seen days still surface as fetch_failed so operators see the
// gap; a later successful run heals a placeholder through upsert's DO UPDATE.
//
// The reconciliation columns are seeded at 0: there is no vendor number to
// compare against on a fetch_failed row, so no basis is meaningful yet. A later
// successful run overwrites both through upsert.
func (j *VendorBillReconcileJob) insertFetchFailedIfAbsent(ctx context.Context, providerID string, day time.Time, our float64) error {
	return j.insertPlaceholderIfAbsent(ctx, providerID, day, our, nil, scopeOrgKind, coverageFetchFailed)
}

// insertNoBasisIfAbsent records a display-only placeholder for a day the vendor
// DID bill but which our side has no recorded vendor spend for, so the two
// cannot be compared (see the reconcileProvider skip path for why the
// comparison must not be fabricated).
//
// Unlike fetch_failed, the vendor number IS known and is written, because it is
// the whole point of the row: an operator sees "the vendor charged $41.56 and we
// have no comparable figure for that day". diff_usd / diff_pct stay NULL — a
// difference against an absent basis would be fiction — which also keeps the row
// out of the drift job's `diff_usd IS NOT NULL` scan on top of its coverage
// filter.
//
// our_billed_usd and both reconciliation columns are written as 0. None of them
// is read for this row: 0 is the schema default for the two new columns and the
// non-nullable our_billed_usd needs a value, and `coverage = 'no_basis'` is the
// marker that says the money columns carry no meaning here. The CP report
// renders them as em-dashes rather than $0.00 for exactly that reason.
//
// ON CONFLICT DO NOTHING is load-bearing twice over: it must never overwrite a
// pre-cutover row (whose old-basis numbers are the only record of that day), and
// it must never re-stamp updated_at on an existing placeholder, which would
// reset the staleness clock VendorBillSyncAlertsJob measures and prevent the
// alert from ever firing.
func (j *VendorBillReconcileJob) insertNoBasisIfAbsent(ctx context.Context, providerID string, day time.Time, vendor float64, scopeKind string) error {
	return j.insertPlaceholderIfAbsent(ctx, providerID, day, 0, vendor, scopeKind, coverageNoBasis)
}

// insertPlaceholderIfAbsent writes a diff-less placeholder row for a day we
// could not reconcile, ONLY when no row yet exists for (provider, day). Shared
// by the fetch_failed and no_basis paths so the non-destructive semantics —
// and the exact column list — cannot drift between them.
func (j *VendorBillReconcileJob) insertPlaceholderIfAbsent(ctx context.Context, providerID string, day time.Time, our float64, vendor any, scopeKind, coverage string) error {
	_, err := j.pool.Exec(ctx, `
		INSERT INTO vendor_bill_reconciliation
		  (provider_id, day, our_billed_usd, our_vendor_spend_usd, our_internal_ops_usd,
		   vendor_reported_usd, diff_usd, diff_pct, scope_kind, coverage, status, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'open', now())
		ON CONFLICT (provider_id, day) DO NOTHING
	`, providerID, day, our, 0.0, 0.0, vendor, nil, nil, scopeKind, coverage)
	return err
}

// upsert writes one reconciliation row. On conflict it refreshes the numbers
// and coverage but preserves an operator's review state (status/reviewed_by/
// note) so a self-healing re-run does not silently un-review a row.
func (j *VendorBillReconcileJob) upsert(ctx context.Context, providerID string, day time.Time, our, spend, internalOps float64, vendor, diffUSD, diffPct any, scopeKind, coverage string) error {
	_, err := j.pool.Exec(ctx, `
		INSERT INTO vendor_bill_reconciliation
		  (provider_id, day, our_billed_usd, our_vendor_spend_usd, our_internal_ops_usd,
		   vendor_reported_usd, diff_usd, diff_pct, scope_kind, coverage, status, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'open', now())
		ON CONFLICT (provider_id, day) DO UPDATE SET
		  our_billed_usd       = EXCLUDED.our_billed_usd,
		  our_vendor_spend_usd = EXCLUDED.our_vendor_spend_usd,
		  our_internal_ops_usd = EXCLUDED.our_internal_ops_usd,
		  vendor_reported_usd  = EXCLUDED.vendor_reported_usd,
		  diff_usd             = EXCLUDED.diff_usd,
		  diff_pct             = EXCLUDED.diff_pct,
		  scope_kind           = EXCLUDED.scope_kind,
		  coverage             = EXCLUDED.coverage,
		  updated_at           = now()
	`, providerID, day, our, spend, internalOps, vendor, diffUSD, diffPct, scopeKind, coverage)
	return err
}

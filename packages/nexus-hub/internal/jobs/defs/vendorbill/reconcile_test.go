package vendorbill

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	defjobs_rollup "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/defs/rollup"
	billsrc "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/vendorbill"
	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// fakeSource is a canned VendorBillSource.
type fakeSource struct {
	key         string
	billingHost string
	bills       []billsrc.VendorDailyBill
	err         error
	gotFrom     time.Time
	gotTo       time.Time
	callCount   int
}

func (f *fakeSource) ProviderKey() string { return f.key }

// BillingHost defaults to "" so every provider row an existing test builds —
// which carries an empty baseUrl — keeps matching (SameBillingHost reads an
// empty baseUrl as "the adapter's own endpoint"). Tests that exercise the host
// guard set it explicitly.
func (f *fakeSource) BillingHost() string { return f.billingHost }
func (f *fakeSource) FetchDailyBill(_ context.Context, from, to time.Time) ([]billsrc.VendorDailyBill, error) {
	f.callCount++
	f.gotFrom, f.gotTo = from, to
	return f.bills, f.err
}

type fakeRegistry map[string]billsrc.VendorBillSource

func (r fakeRegistry) Resolve(k string) billsrc.VendorBillSource {
	if s, ok := r[k]; ok {
		return s
	}
	return nil
}

func makeJob(mock pgxmock.PgxPoolIface, reg sourceRegistry, now time.Time, lookback, lag int) *VendorBillReconcileJob {
	return &VendorBillReconcileJob{
		pool:            mock,
		registry:        reg,
		interval:        24 * time.Hour,
		logger:          testLogger(),
		lookbackDays:    lookback,
		finalizeLagDays: lag,
		now:             func() time.Time { return now },
		// A zero wait keeps awaitCorrection to the single watermark read every
		// test that is not about the wait itself queues. The wait has its own
		// tests, which build the job with an advancing clock; the sleep seam
		// here returns an error rather than nil-panicking so a future test that
		// sets a wait without a clock fails by name instead of by SIGSEGV.
		correctionWait: 0,
		correctionPoll: time.Millisecond,
		sleep: func(context.Context, time.Duration) error {
			return errors.New("test job has no advancing clock: build it with makeWaitingJob to exercise awaitCorrection")
		},
	}
}

// makeWaitingJob builds a job whose clock advances by `poll` on every sleep, so
// awaitCorrection's poll loop runs in real logical time without wall-clock
// delay. Returns the job; the caller queues one watermark read per expected
// poll.
func makeWaitingJob(mock pgxmock.PgxPoolIface, reg sourceRegistry, start time.Time, lookback, lag int, wait, poll time.Duration) *VendorBillReconcileJob {
	clock := start
	j := makeJob(mock, reg, start, lookback, lag)
	j.now = func() time.Time { return clock }
	j.correctionWait = wait
	j.correctionPoll = poll
	j.sleep = func(_ context.Context, d time.Duration) error {
		clock = clock.Add(d)
		return nil
	}
	return j
}

// expectProviders queues the two reads every Run() issues before it reaches a
// provider: the rollup-correction watermark that decides which days are
// comparable at all, then the enabled-provider list.
//
// The watermark is queued far in the future so the day gate never fires here.
// That gate has its own tests (TestReconcile_DefersDayBeyondCorrectionWatermark
// and TestReconcile_RunDefersOnlyTheDaysPastTheWatermark); pinning it in this
// shared helper would make every unrelated test in the file depend on the
// correction cursor as well as on the behaviour it actually covers.
func expectProviders(mock pgxmock.PgxPoolIface, rows ...[2]string) {
	expectCorrectionWatermark(mock, day(2999, 1, 1))
	expectProviderList(mock, rows...)
}

// expectProviderList queues only the enabled-provider read, for tests that pin
// their own correction watermark first.
func expectProviderList(mock pgxmock.PgxPoolIface, rows ...[2]string) {
	trip := make([][3]string, 0, len(rows))
	for _, r := range rows {
		// Empty baseUrl = "serve from the adapter's own endpoint", which is the
		// vendor's own host, so the billing-host guard passes it through. Tests
		// that exercise the guard use expectProviderListWithBaseURL.
		trip = append(trip, [3]string{r[0], r[1], ""})
	}
	expectProviderListWithBaseURL(mock, trip...)
}

// expectProviderListWithBaseURL is expectProviderList with each row's baseUrl
// spelled out, for the tests that assert the billing-host guard.
func expectProviderListWithBaseURL(mock pgxmock.PgxPoolIface, rows ...[3]string) {
	rr := pgxmock.NewRows([]string{"id", "adapter_type", "baseUrl"})
	for _, r := range rows {
		rr.AddRow(r[0], r[1], r[2])
	}
	mock.ExpectQuery(`SELECT id, adapter_type, COALESCE\("baseUrl", ''\) FROM "Provider"`).WillReturnRows(rr)
}

// expectCorrectionWatermark queues the rollup_watermark read, asserting the job
// asks for the correction cursor by the name the correction job publishes.
func expectCorrectionWatermark(mock pgxmock.PgxPoolIface, through time.Time) {
	mock.ExpectQuery(`FROM "rollup_watermark"`).
		WithArgs(defjobs_rollup.WatermarkCorrection).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(through))
}

// expectRollupSum queues one rollupSum read: the summed value plus the row
// count that tells "summed to zero" apart from "series never aggregated".
func expectRollupSum(mock pgxmock.PgxPoolIface, metricName, providerID string, v float64, rowCount int64) {
	mock.ExpectQuery(`FROM metric_rollup_1d`).
		WithArgs(metricName, metrics.BuildDimensionKey("routed_provider", providerID), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"sum", "n"}).AddRow(v, rowCount))
}

// expectOurBilled queues the billed_cost_usd read (the quota basis persisted as
// our_billed_usd).
func expectOurBilled(mock pgxmock.PgxPoolIface, providerID string, v float64) {
	expectRollupSum(mock, metrics.MetricBilledCostUSD, providerID, v, 1)
}

// expectDayReads queues the three rollup reads reconcileProvider issues for a
// comparable day, in the order it issues them: the reconciliation basis first
// (so a day it cannot compare pays for nothing else), then the internal-ops
// subset, then the customer-quota figure.
func expectDayReads(mock pgxmock.PgxPoolIface, providerID string, spend, internalOps, billed float64) {
	expectRollupSum(mock, metrics.MetricVendorSpendUSD, providerID, spend, 1)
	expectRollupSum(mock, metrics.MetricVendorSpendInternalUSD, providerID, internalOps, 1)
	expectOurBilled(mock, providerID, billed)
}

// expectNoBasisInsert queues the display-only placeholder the job writes for a
// day the vendor billed but we have no recorded vendor spend for. rowsAffected 0
// models the second run, where the row already exists and DO NOTHING applies.
func expectNoBasisInsert(mock pgxmock.PgxPoolIface, providerID string, d time.Time, vendor float64, scopeKind string, rowsAffected int64) {
	mock.ExpectExec(`ON CONFLICT \(provider_id, day\) DO NOTHING`).
		WithArgs(providerID, d, 0.0, 0.0, 0.0, vendor, nil, nil, scopeKind, coverageNoBasis).
		WillReturnResult(pgxmock.NewResult("INSERT", rowsAffected))
}

func TestReconcile_ComputesDiffScopedAndUpserts(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	src := &fakeSource{key: "openai", bills: []billsrc.VendorDailyBill{
		{Day: day(2026, 7, 17), AmountUSD: 10, ScopeKind: "project", ScopeID: "proj_gw"},
	}}
	j := makeJob(mock, fakeRegistry{"openai": src}, day(2026, 7, 19), 1, 2) // window = single day 07-17

	expectProviders(mock, [2]string{"prov1", "openai"})
	expectDayReads(mock, "prov1", 9.5, 0.5, 9)
	// diff = 10-9.5 = 0.5 ; pct = 0.5/10 = 0.05 ; scoped. The diff is measured
	// against the VENDOR-SPEND figure (9.5), not the quota figure (9), which is
	// still persisted alongside it.
	mock.ExpectExec(`INSERT INTO vendor_bill_reconciliation`).
		WithArgs("prov1", pgxmock.AnyArg(), 9.0, 9.5, 0.5, 10.0, 0.5, 0.05, "project", "scoped").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestReconcile_OrgOnlyCoverage(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	src := &fakeSource{key: "anthropic", bills: []billsrc.VendorDailyBill{
		{Day: day(2026, 7, 17), AmountUSD: 20, ScopeKind: "org"},
	}}
	j := makeJob(mock, fakeRegistry{"anthropic": src}, day(2026, 7, 19), 1, 2)

	expectProviders(mock, [2]string{"prov1", "anthropic"})
	expectDayReads(mock, "prov1", 20, 0, 20)
	// org scope => coverage org_only, alert-suppressed downstream
	mock.ExpectExec(`INSERT INTO vendor_bill_reconciliation`).
		WithArgs("prov1", pgxmock.AnyArg(), 20.0, 20.0, 0.0, 20.0, 0.0, 0.0, "org", "org_only").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestReconcile_FetchFailWritesFetchFailedNoVendor(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	src := &fakeSource{key: "openai", err: errors.New("401 unauthorized")}
	j := makeJob(mock, fakeRegistry{"openai": src}, day(2026, 7, 19), 1, 2)

	expectProviders(mock, [2]string{"prov1", "openai"})
	expectOurBilled(mock, "prov1", 5)
	// vendor/diff/pct all NULL, coverage fetch_failed — never a fabricated diff.
	mock.ExpectExec(`INSERT INTO vendor_bill_reconciliation`).
		WithArgs("prov1", pgxmock.AnyArg(), 5.0, 0.0, 0.0, nil, nil, nil, "org", "fetch_failed").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestReconcile_FetchFailInsertIsNonDestructive(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// A transient vendor error (500/429/network) must NOT erase a previously
	// reconciled row. The fetch_failed write is therefore INSERT ... ON CONFLICT
	// DO NOTHING: it only seeds a placeholder for a never-seen day and leaves any
	// existing good vendor_reported_usd untouched. Without this, one blip clobbers
	// the good number with NULL and — once the day ages out of the trailing
	// re-reconcile window — the data is lost silently (job_run stays success,
	// per-provider isolation swallows the error, and the drift alert never fires
	// on fetch_failed).
	src := &fakeSource{key: "openai", err: errors.New("500 server_error")}
	j := makeJob(mock, fakeRegistry{"openai": src}, day(2026, 7, 19), 1, 2)

	expectProviders(mock, [2]string{"prov1", "openai"})
	expectOurBilled(mock, "prov1", 5)
	mock.ExpectExec(`ON CONFLICT \(provider_id, day\) DO NOTHING`).
		WithArgs("prov1", pgxmock.AnyArg(), 5.0, 0.0, 0.0, nil, nil, nil, "org", "fetch_failed").
		WillReturnResult(pgxmock.NewResult("INSERT", 0))

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestReconcile_SuccessUpsertHealsViaDoUpdate(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// The success path MUST stay an overwriting upsert (ON CONFLICT DO UPDATE) so
	// a later good fetch heals a prior fetch_failed placeholder for the same day.
	// This guards the non-destructive fetch_failed change from over-reaching into
	// the success path and breaking self-healing.
	src := &fakeSource{key: "openai", bills: []billsrc.VendorDailyBill{
		{Day: day(2026, 7, 17), AmountUSD: 10, ScopeKind: "project"},
	}}
	j := makeJob(mock, fakeRegistry{"openai": src}, day(2026, 7, 19), 1, 2)

	expectProviders(mock, [2]string{"prov1", "openai"})
	expectDayReads(mock, "prov1", 9, 0, 9)
	mock.ExpectExec(`ON CONFLICT \(provider_id, day\) DO UPDATE`).
		WithArgs("prov1", pgxmock.AnyArg(), 9.0, 9.0, 0.0, 10.0, 1.0, 0.1, "project", "scoped").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// The upsert refreshes the numbers but must never touch an operator's review
// state: a self-healing re-run that silently un-reviewed a row would make the
// acknowledgement worthless, and the job re-writes every day in the trailing
// window on every pass. Adding a column to the DO UPDATE list is exactly how
// that would regress, so the assertion is on the SQL the job actually sends.
func TestReconcile_UpsertNeverOverwritesReviewState(t *testing.T) {
	var gotSQL string
	mock, _ := pgxmock.NewPool(pgxmock.QueryMatcherOption(
		pgxmock.QueryMatcherFunc(func(expectedSQL, actualSQL string) error {
			if strings.Contains(actualSQL, "ON CONFLICT (provider_id, day) DO UPDATE") {
				gotSQL = actualSQL
			}
			return pgxmock.QueryMatcherRegexp.Match(expectedSQL, actualSQL)
		})))
	defer mock.Close()
	j := makeJob(mock, fakeRegistry{}, day(2026, 8, 1), 1, 2)
	d := day(2026, 7, 30)

	expectDayReads(mock, "prov1", 9, 0, 9)
	mock.ExpectExec(`ON CONFLICT \(provider_id, day\) DO UPDATE`).
		WithArgs("prov1", d, 9.0, 9.0, 0.0, 10.0, 1.0, 0.1, "project", "scoped").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	reconcileOneDay(t, j, "prov1", []billsrc.VendorDailyBill{{Day: d, AmountUSD: 10, ScopeKind: "project"}}, d)

	_, updateClause, found := strings.Cut(gotSQL, "DO UPDATE SET")
	if !found {
		t.Fatalf("no DO UPDATE clause captured, got %q", gotSQL)
	}
	for _, col := range []string{"status", "reviewed_by", "note"} {
		if strings.Contains(updateClause, col+" ") || strings.Contains(updateClause, col+"=") {
			t.Errorf("DO UPDATE assigns %q — a re-run would discard the operator's review: %s", col, updateClause)
		}
	}
	// Sanity: the clause does refresh the money columns, so the loop above is
	// scanning a real assignment list rather than an empty string.
	for _, col := range []string{"our_billed_usd", "our_vendor_spend_usd", "our_internal_ops_usd", "vendor_reported_usd"} {
		if !strings.Contains(updateClause, col) {
			t.Errorf("DO UPDATE must refresh %q", col)
		}
	}
}

func TestReconcile_VendorAbsentDaySkipped(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// Fetch succeeds but returns no data for the day (not yet finalized).
	src := &fakeSource{key: "openai", bills: nil}
	j := makeJob(mock, fakeRegistry{"openai": src}, day(2026, 7, 19), 1, 2)

	expectProviders(mock, [2]string{"prov1", "openai"})
	// No ourBilled, no upsert — absence is not zero, we simply write nothing.

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestReconcile_UnknownProviderSkipped(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	src := &fakeSource{key: "openai", bills: []billsrc.VendorDailyBill{{Day: day(2026, 7, 17), AmountUSD: 1, ScopeKind: "project"}}}
	j := makeJob(mock, fakeRegistry{"openai": src}, day(2026, 7, 19), 1, 2)

	// gemini has no source; only openai is reconciled.
	expectProviders(mock, [2]string{"provG", "gemini"}, [2]string{"prov1", "openai"})
	expectDayReads(mock, "prov1", 1, 0, 1)
	mock.ExpectExec(`INSERT INTO vendor_bill_reconciliation`).
		WithArgs("prov1", pgxmock.AnyArg(), 1.0, 1.0, 0.0, 1.0, 0.0, 0.0, "project", "scoped").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
	if src.callCount != 1 {
		t.Fatalf("openai source should be called once, got %d", src.callCount)
	}
}

func TestReconcile_WindowBounds(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	src := &fakeSource{key: "openai", bills: nil} // absent → no upserts, we only check the window
	j := makeJob(mock, fakeRegistry{"openai": src}, day(2026, 7, 19), 4, 2)

	expectProviders(mock, [2]string{"prov1", "openai"})
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// N=4, lag=2, now=07-19 → to=07-17, from=07-14.
	if !src.gotTo.Equal(day(2026, 7, 17)) {
		t.Errorf("window end = %v, want 2026-07-17 (today-2)", src.gotTo)
	}
	if !src.gotFrom.Equal(day(2026, 7, 14)) {
		t.Errorf("window start = %v, want 2026-07-14 (4-day trailing)", src.gotFrom)
	}
}

func TestReconcile_ProviderErrorDoesNotAbortOthers(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bad := &fakeSource{key: "openai", err: errors.New("boom")}
	good := &fakeSource{key: "anthropic", bills: []billsrc.VendorDailyBill{{Day: day(2026, 7, 17), AmountUSD: 3, ScopeKind: "workspace"}}}
	j := makeJob(mock, fakeRegistry{"openai": bad, "anthropic": good}, day(2026, 7, 19), 1, 2)

	expectProviders(mock, [2]string{"pA", "openai"}, [2]string{"pB", "anthropic"})
	// openai fetch_failed path
	expectOurBilled(mock, "pA", 1)
	mock.ExpectExec(`INSERT INTO vendor_bill_reconciliation`).
		WithArgs("pA", pgxmock.AnyArg(), 1.0, 0.0, 0.0, nil, nil, nil, "org", "fetch_failed").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	// anthropic still reconciled
	expectDayReads(mock, "pB", 3, 0, 3)
	mock.ExpectExec(`INSERT INTO vendor_bill_reconciliation`).
		WithArgs("pB", pgxmock.AnyArg(), 3.0, 3.0, 0.0, 3.0, 0.0, 0.0, "workspace", "scoped").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// reconcileOneDay drives reconcileProvider directly for a single UTC day. Run()
// isolates per-provider errors by logging them, which would swallow pgxmock's
// "call was not expected" error — the very signal the write-suppression tests
// below exist to detect. These call the method directly so an unexpected write
// fails the test loudly.
func reconcileOneDay(t *testing.T, j *VendorBillReconcileJob, providerID string, bills []billsrc.VendorDailyBill, d time.Time) {
	t.Helper()
	src := &fakeSource{key: "openai", bills: bills}
	if err := j.reconcileProvider(context.Background(), providerID, src, d, d, d); err != nil {
		t.Fatalf("reconcileProvider: %v", err)
	}
}

func TestReconcileProvider_DiffsAgainstVendorSpendNotBilledCost(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	j := makeJob(mock, fakeRegistry{}, day(2026, 8, 1), 1, 2)
	d := day(2026, 7, 30)

	// billed_cost_usd omits the router call; vendor_spend_usd includes it. The
	// row must carry both, and the diff must come from the vendor-spend figure.
	expectDayReads(mock, "prov-openai", 41.56, 13.63, 27.93)
	mock.ExpectExec(`INSERT INTO vendor_bill_reconciliation`).
		WithArgs("prov-openai", d,
			27.93,            // our_billed_usd — unchanged, quota basis
			41.56,            // our_vendor_spend_usd — reconciliation basis
			13.63,            // our_internal_ops_usd
			41.56,            // vendor_reported_usd
			pgxmock.AnyArg(), // diff_usd
			pgxmock.AnyArg(), // diff_pct
			"api_key", coverageScoped).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	reconcileOneDay(t, j, "prov-openai", []billsrc.VendorDailyBill{{
		Day: d, AmountUSD: 41.56, ScopeKind: "api_key",
	}}, d)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileProvider_DiffIsZeroWhenVendorSpendMatchesTheBill(t *testing.T) {
	// The whole point: with the router cost recorded, a correct estimator
	// reconciles to zero rather than to the +32.8% seen on 2026-07-30. Diffing
	// the same day against billed_cost_usd (27.93) would report 0.33.
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	j := makeJob(mock, fakeRegistry{}, day(2026, 8, 1), 1, 2)
	d := day(2026, 7, 30)

	expectDayReads(mock, "prov-openai", 41.56, 13.63, 27.93)
	mock.ExpectExec(`INSERT INTO vendor_bill_reconciliation`).
		WithArgs("prov-openai", d, 27.93, 41.56, 13.63, 41.56,
			0.0, // diff_usd
			0.0, // diff_pct
			"api_key", coverageScoped).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	reconcileOneDay(t, j, "prov-openai", []billsrc.VendorDailyBill{{
		Day: d, AmountUSD: 41.56, ScopeKind: "api_key",
	}}, d)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileProvider_DayWithNoVendorSpendRowsIsNotCompared(t *testing.T) {
	// vendor_spend_usd rows exist only for buckets the aggregator has processed.
	// Every day predating this feature's deploy — any bucket outside
	// rollup-correction's window, and any day whose cost stamping produced
	// nothing (the rollup drops zero-valued rows) — has none. Absence must NOT be
	// read as zero spend: doing so would compute diff = vendor - 0 on each of
	// those days, rewrite the row at -100%, and fire the drift alert across the
	// whole trailing window the morning after deploy.
	//
	// So no comparison is computed and no existing row is modified — but the day
	// is NOT dropped on the floor either: a display-only no_basis placeholder is
	// inserted so a broken cost-stamping path shows up in the report and ages
	// into vendor.bill_sync_failed instead of vanishing.
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	j := makeJob(mock, fakeRegistry{}, day(2026, 8, 1), 1, 2)
	d := day(2026, 7, 30)

	expectRollupSum(mock, metrics.MetricVendorSpendUSD, "prov-openai", 0, 0) // no rows at all
	// No internal-ops read and no billed read: the day is not being compared, so
	// it must not pay for either query.
	expectNoBasisInsert(mock, "prov-openai", d, 41.56, "api_key", 1)

	reconcileOneDay(t, j, "prov-openai", []billsrc.VendorDailyBill{{
		Day: d, AmountUSD: 41.56, ScopeKind: "api_key",
	}}, d)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileProvider_VendorZeroDayIsWrittenAsAScopedZeroRow(t *testing.T) {
	// A provider-day where the vendor reported $0 and the gateway recorded no
	// vendor_spend_usd row is genuinely consistent — nothing was charged and
	// nothing was recorded. Unlike TestReconcileProvider_DayWithNoVendorSpendRowsIsNotCompared
	// (a real vendor amount with no basis to compare against, which IS a gap),
	// it must NOT get a no_basis placeholder: that would raise
	// vendor.bill_sync_failed 25 hours later for a day with nothing wrong.
	//
	// It must not be skipped either, which is what this used to do. An absent
	// row and a zero row are the same blank in the report, so skipping leaves an
	// operator unable to tell "no traffic that day" from "never reconciled" —
	// and leaves a fetch_failed placeholder on such a day unhealable forever
	// (see TestReconcileProvider_VendorZeroDayHealsAFetchFailedPlaceholder).
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	j := makeJob(mock, fakeRegistry{}, day(2026, 8, 1), 1, 2)
	d := day(2026, 7, 30)

	expectRollupSum(mock, metrics.MetricVendorSpendUSD, "prov-openai", 0, 0) // no rows at all
	// The quota figure is still read — it is a real column on a real row. The
	// internal-ops read is not: its subset of a zero basis can only be zero.
	expectOurBilled(mock, "prov-openai", 0)
	mock.ExpectExec(`INSERT INTO vendor_bill_reconciliation`).
		WithArgs("prov-openai", d, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, "api_key", coverageScoped).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	reconcileOneDay(t, j, "prov-openai", []billsrc.VendorDailyBill{{
		Day: d, AmountUSD: 0, ScopeKind: "api_key",
	}}, d)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestReconcileProvider_VendorZeroDayHealsAFetchFailedPlaceholder pins the
// durability property the zero row exists for. A transient 429 seeds a
// fetch_failed placeholder across the window; on a day the gateway sent no
// traffic, the vendor has no non-zero figure to report for that day EVER, so
// unless the zero itself is reconciled the placeholder can never be overwritten
// — it ages out of the trailing window frozen at fetch_failed, still raising
// vendor.bill_sync_failed. Prod OpenAI 2026-08-16 sat exactly there.
//
// The assertion is on the SQL: only DO UPDATE heals, and the insert-if-absent
// placeholder path deliberately cannot.
func TestReconcileProvider_VendorZeroDayHealsAFetchFailedPlaceholder(t *testing.T) {
	var gotSQL string
	mock, _ := pgxmock.NewPool(pgxmock.QueryMatcherOption(
		pgxmock.QueryMatcherFunc(func(expected, actual string) error {
			if strings.Contains(actual, "INSERT INTO vendor_bill_reconciliation") {
				gotSQL = actual
			}
			return pgxmock.QueryMatcherRegexp.Match(expected, actual)
		}),
	))
	defer mock.Close()
	j := makeJob(mock, fakeRegistry{}, day(2026, 8, 1), 1, 2)
	d := day(2026, 7, 30)

	expectRollupSum(mock, metrics.MetricVendorSpendUSD, "prov-openai", 0, 0)
	expectOurBilled(mock, "prov-openai", 0)
	mock.ExpectExec(`INSERT INTO vendor_bill_reconciliation`).
		WithArgs("prov-openai", d, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, "api_key", coverageScoped).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	reconcileOneDay(t, j, "prov-openai", []billsrc.VendorDailyBill{{
		Day: d, AmountUSD: 0, ScopeKind: "api_key",
	}}, d)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotSQL, "DO UPDATE") {
		t.Errorf("zero-day write must be an upsert that heals an existing placeholder, got SQL:\n%s", gotSQL)
	}
}

// TestReconcileProvider_VendorZeroDayKeepsOrgOnlyCoverage: an unpinned account
// reports org-wide totals, and a zero from an org-wide query says nothing about
// this gateway's slice. The row is still written — the operator still needs to
// see the day — but it keeps the org_only badge that holds it out of the drift
// alert, rather than being promoted to `scoped` by virtue of being zero.
func TestReconcileProvider_VendorZeroDayKeepsOrgOnlyCoverage(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	j := makeJob(mock, fakeRegistry{}, day(2026, 8, 1), 1, 2)
	d := day(2026, 7, 30)

	expectRollupSum(mock, metrics.MetricVendorSpendUSD, "prov-openai", 0, 0)
	expectOurBilled(mock, "prov-openai", 0)
	mock.ExpectExec(`INSERT INTO vendor_bill_reconciliation`).
		WithArgs("prov-openai", d, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, scopeOrgKind, coverageOrgOnly).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	reconcileOneDay(t, j, "prov-openai", []billsrc.VendorDailyBill{{
		Day: d, AmountUSD: 0, ScopeKind: scopeOrgKind,
	}}, d)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileProvider_NoBasisPlaceholderIsWrittenOnceAndNeverModifies(t *testing.T) {
	// The placeholder must be strictly additive. Two hazards it guards:
	//  1. A pre-cutover row already exists for the day, carrying the only record
	//     of that day's old-basis numbers. DO NOTHING is what stops the
	//     placeholder from replacing them with zeros.
	//  2. An existing placeholder must not be re-stamped. updated_at is the clock
	//     VendorBillSyncAlertsJob measures staleness against, so a DO UPDATE here
	//     would reset it every run and the alert could never fire.
	// A DO UPDATE variant would satisfy neither, so the assertion is on the SQL
	// the job actually sends, not only on the argument list.
	var gotSQL string
	mock, _ := pgxmock.NewPool(pgxmock.QueryMatcherOption(
		pgxmock.QueryMatcherFunc(func(expectedSQL, actualSQL string) error {
			if strings.Contains(actualSQL, "INSERT INTO vendor_bill_reconciliation") {
				gotSQL = actualSQL
			}
			return pgxmock.QueryMatcherRegexp.Match(expectedSQL, actualSQL)
		})))
	defer mock.Close()
	j := makeJob(mock, fakeRegistry{}, day(2026, 8, 1), 1, 2)
	d := day(2026, 7, 30)
	bills := []billsrc.VendorDailyBill{{Day: d, AmountUSD: 41.56, ScopeKind: "api_key"}}

	// First run: the row does not exist, so one row is inserted.
	expectRollupSum(mock, metrics.MetricVendorSpendUSD, "prov-openai", 0, 0)
	expectNoBasisInsert(mock, "prov-openai", d, 41.56, "api_key", 1)
	// Second run over the same day: the row exists, DO NOTHING applies, and the
	// job issues exactly the same statement — no UPDATE, no second row.
	expectRollupSum(mock, metrics.MetricVendorSpendUSD, "prov-openai", 0, 0)
	expectNoBasisInsert(mock, "prov-openai", d, 41.56, "api_key", 0)

	reconcileOneDay(t, j, "prov-openai", bills, d)
	reconcileOneDay(t, j, "prov-openai", bills, d)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotSQL, "ON CONFLICT (provider_id, day) DO NOTHING") {
		t.Errorf("placeholder must be non-destructive, got: %s", gotSQL)
	}
	if strings.Contains(gotSQL, "DO UPDATE") {
		t.Errorf("placeholder must never overwrite an existing row: %s", gotSQL)
	}
}

func TestReconcileProvider_ZeroVendorSpendIsWrittenWhenTheSeriesExists(t *testing.T) {
	// The mirror of the test above: a recorded series that genuinely sums to
	// zero IS comparable, so the row is written with a full -100% difference
	// (we spent nothing the vendor billed for). Skipping this case would hide a
	// real discrepancy behind the pre-cutover guard.
	//
	// CROSS-PACKAGE COUPLING, unguarded by any test in either package: the
	// report's isPreCutover heuristic (VendorBillReconciliationPage.tsx) and the
	// drift job's `comparable` check both read our_vendor_spend_usd == 0 as "no
	// basis was recorded". That is only sound because the rollup never persists a
	// zero-valued row — rollup_5m_vendor_spend.go drops zero-amount components
	// and rollup_5m.go drops zero-valued rows — so a persisted vendor_spend_usd
	// row is always strictly positive and this test's state cannot occur in
	// production. If that ever changes, a genuine zero-spend day would be
	// mislabelled "not comparable" and its drift alert suppressed. The job's own
	// contract below (recorded ⇒ compare) is deliberately independent of it.
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	j := makeJob(mock, fakeRegistry{}, day(2026, 8, 1), 1, 2)
	d := day(2026, 7, 30)

	expectDayReads(mock, "prov-openai", 0, 0, 0)
	mock.ExpectExec(`INSERT INTO vendor_bill_reconciliation`).
		WithArgs("prov-openai", d, 0.0, 0.0, 0.0, 12.0, 12.0, 1.0, "api_key", coverageScoped).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	reconcileOneDay(t, j, "prov-openai", []billsrc.VendorDailyBill{{
		Day: d, AmountUSD: 12, ScopeKind: "api_key",
	}}, d)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileProvider_AbsentInternalOpsSeriesIsARealZero(t *testing.T) {
	// A day of pure customer traffic emits no vendor_spend_internal_usd rows.
	// Unlike the vendor-spend series, that absence is a genuine zero — there was
	// no internal overhead — so the row is written with our_internal_ops_usd = 0
	// rather than skipped.
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	j := makeJob(mock, fakeRegistry{}, day(2026, 8, 1), 1, 2)
	d := day(2026, 7, 30)

	expectRollupSum(mock, metrics.MetricVendorSpendUSD, "prov-openai", 8, 1)
	expectRollupSum(mock, metrics.MetricVendorSpendInternalUSD, "prov-openai", 0, 0)
	expectOurBilled(mock, "prov-openai", 8)
	mock.ExpectExec(`INSERT INTO vendor_bill_reconciliation`).
		WithArgs("prov-openai", d, 8.0, 8.0, 0.0, 8.0, 0.0, 0.0, "api_key", coverageScoped).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	reconcileOneDay(t, j, "prov-openai", []billsrc.VendorDailyBill{{
		Day: d, AmountUSD: 8, ScopeKind: "api_key",
	}}, d)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileProvider_VendorSpendReadErrorAborts(t *testing.T) {
	// A failed rollup read must not fall through to a write with a zero basis.
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	j := makeJob(mock, fakeRegistry{}, day(2026, 8, 1), 1, 2)
	d := day(2026, 7, 30)

	mock.ExpectQuery(`FROM metric_rollup_1d`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("rollup read failed"))

	src := &fakeSource{key: "openai", bills: []billsrc.VendorDailyBill{{Day: d, AmountUSD: 8, ScopeKind: "api_key"}}}
	if err := j.reconcileProvider(context.Background(), "prov-openai", src, d, d, d); err == nil {
		t.Fatal("expected the rollup read error to propagate, got nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileProvider_InternalOpsReadErrorAborts(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	j := makeJob(mock, fakeRegistry{}, day(2026, 8, 1), 1, 2)
	d := day(2026, 7, 30)

	expectRollupSum(mock, metrics.MetricVendorSpendUSD, "prov-openai", 8, 1)
	mock.ExpectQuery(`FROM metric_rollup_1d`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("rollup read failed"))

	src := &fakeSource{key: "openai", bills: []billsrc.VendorDailyBill{{Day: d, AmountUSD: 8, ScopeKind: "api_key"}}}
	if err := j.reconcileProvider(context.Background(), "prov-openai", src, d, d, d); err == nil {
		t.Fatal("expected the internal-ops read error to propagate, got nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Every write and read failure inside the day loop must propagate. Run() logs
// per-provider errors and moves on to the next provider, which is the intended
// isolation; swallowing an error inside reconcileProvider instead would keep
// grinding through the remaining days against a broken pool and report success.
func TestReconcileProvider_WriteAndReadFailuresPropagate(t *testing.T) {
	d := day(2026, 7, 30)
	bills := []billsrc.VendorDailyBill{{Day: d, AmountUSD: 8, ScopeKind: "api_key"}}
	boom := errors.New("pool is gone")

	cases := []struct {
		name  string
		queue func(mock pgxmock.PgxPoolIface)
	}{
		{"no_basis placeholder insert fails", func(mock pgxmock.PgxPoolIface) {
			expectRollupSum(mock, metrics.MetricVendorSpendUSD, "prov1", 0, 0)
			mock.ExpectExec(`INSERT INTO vendor_bill_reconciliation`).WillReturnError(boom)
		}},
		{"billed_cost_usd read fails", func(mock pgxmock.PgxPoolIface) {
			expectRollupSum(mock, metrics.MetricVendorSpendUSD, "prov1", 8, 1)
			expectRollupSum(mock, metrics.MetricVendorSpendInternalUSD, "prov1", 0, 1)
			mock.ExpectQuery(`FROM metric_rollup_1d`).
				WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
				WillReturnError(boom)
		}},
		{"upsert fails", func(mock pgxmock.PgxPoolIface) {
			expectDayReads(mock, "prov1", 8, 0, 8)
			mock.ExpectExec(`INSERT INTO vendor_bill_reconciliation`).WillReturnError(boom)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mock, _ := pgxmock.NewPool()
			defer mock.Close()
			j := makeJob(mock, fakeRegistry{}, day(2026, 8, 1), 1, 2)
			c.queue(mock)

			src := &fakeSource{key: "openai", bills: bills}
			if err := j.reconcileProvider(context.Background(), "prov1", src, d, d, d); err == nil {
				t.Fatal("expected the failure to propagate, got nil")
			}
		})
	}
}

// pctDiff measures the estimate's error RELATIVE TO THE VENDOR BILL, so the
// vendor value is always the denominator. The symmetry cases below are the
// point: a 2x under-estimate and a 2x over-estimate must report the same
// magnitude in opposite directions. An earlier max()-based denominator reported
// the over-estimate as -50% against the under-estimate's +50%, understating
// every over-estimate.
func TestPctDiff(t *testing.T) {
	cases := []struct {
		name              string
		vendor, our, want float64
	}{
		{"slight under-estimate", 10, 9, 0.1},
		{"both zero", 0, 0, 0},
		{"2x under-estimate", 10, 5, 0.5},
		{"2x over-estimate is symmetric", 5, 10, -1},
		{"estimate missing entirely", 10, 0, 1},
		{"vendor billed nothing but we estimated spend", 0, 4, -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pctDiff(c.vendor, c.our); got != c.want {
				t.Errorf("pctDiff(%v,%v) = %v, want %v", c.vendor, c.our, got, c.want)
			}
		})
	}
}

// TestReconcileProvider_SubCentDisagreementZeroesTheAmountNotThePercentage
// pins the two figures deliberately answering at different resolutions, using
// the case reported in production on 2026-07-28: Anthropic, ours $0.4356,
// vendor $0.44.
//
// diff_usd is zero and stays zero — at the resolution the vendor reports, there
// is no amount in dispute. That is what the 2026-07-28 fix was for and it is
// unchanged.
//
// diff_pct is NOT zero: it reads the unrounded pair and reports the ~1%. This
// REVERSES the percentage half of that fix, on purpose. Zeroing it also zeroed
// every genuinely-drifting low-volume day, because a ratio taken from
// cent-rounded operands carries the rounding as error scaled by 1/vendor — at
// sub-dollar volume that swamps the true value and pins the output at 0.0%.
// Rounding noise on a single day is indistinguishable from real drift either
// way; what separates them is the SIGN holding across many days, and a column
// flattened to 0.0% cannot show a sign at all.
//
// The cost of this direction is that a sub-cent day now reports a non-zero
// percentage beside a zero amount. That pairing is intended, not a display bug:
// see the comment at the pctDiff call site.
func TestReconcileProvider_SubCentDisagreementZeroesTheAmountNotThePercentage(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	j := makeJob(mock, fakeRegistry{}, day(2026, 8, 1), 1, 2)
	d := day(2026, 7, 30)

	// Runtime float64s, not untyped constants: Go folds constant expressions in
	// arbitrary precision, which would not match pctDiff's float64 arithmetic.
	vendor, spend := 0.44, 0.4356

	expectDayReads(mock, "prov-anthropic", spend, 0.0, spend)
	mock.ExpectExec(`INSERT INTO vendor_bill_reconciliation`).
		WithArgs("prov-anthropic", d,
			spend, // our_billed_usd — unrounded, unchanged
			spend, // our_vendor_spend_usd — STORED UNROUNDED, the property this
			// test exists to pin: rounding must never leak into storage.
			0.0,                   // our_internal_ops_usd
			vendor,                // vendor_reported_usd — the vendor's own input value, as given
			0.0,                   // diff_usd — exactly zero at cent resolution
			(vendor-spend)/vendor, // diff_pct — the real ~1%, unrounded
			"api_key", coverageScoped).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	reconcileOneDay(t, j, "prov-anthropic", []billsrc.VendorDailyBill{{
		Day: d, AmountUSD: vendor, ScopeKind: "api_key",
	}}, d)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestReconcileProvider_GenuineDriftAboveACentSurvivesRounding is the
// counterpart to the sub-cent test above: cent-rounding must not mask a real
// disagreement. The raw figures carry their own sub-cent noise (20.004 and
// 14.999) that rounds away cleanly (20.00, 15.00), leaving a genuine $5.00 /
// 25% gap intact — proof the rounding quantises the comparison rather than
// hiding drift.
func TestReconcileProvider_GenuineDriftAboveACentSurvivesRounding(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	j := makeJob(mock, fakeRegistry{}, day(2026, 8, 1), 1, 2)
	d := day(2026, 7, 30)

	// Runtime float64s so the expectation goes through the same float64
	// arithmetic pctDiff does; untyped constants fold at arbitrary precision
	// and would differ in the last bit.
	vendor, spend := 20.004, 14.999

	expectDayReads(mock, "prov-openai", spend, 1.2345, 13.5)
	mock.ExpectExec(`INSERT INTO vendor_bill_reconciliation`).
		WithArgs("prov-openai", d,
			13.5,   // our_billed_usd — unrounded
			spend,  // our_vendor_spend_usd — STORED UNROUNDED
			1.2345, // our_internal_ops_usd — unrounded, untouched by this change
			vendor, // vendor_reported_usd — STORED UNROUNDED, as reported
			5.0,    // diff_usd = round(20.004)-round(14.999) = 20.00-15.00
			// diff_pct reads the UNROUNDED pair: a ratio must not inherit the
			// cent quantisation, whose error scales as 1/vendor.
			(vendor-spend)/vendor,
			"api_key", coverageScoped).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	reconcileOneDay(t, j, "prov-openai", []billsrc.VendorDailyBill{{
		Day: d, AmountUSD: vendor, ScopeKind: "api_key",
	}}, d)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestReconcileProvider_VendorRoundingToZeroDoesNotFlattenThePercentage pins
// the split resolutions on a sub-cent day. diff_usd quantises to cents and so
// reports -0.02 (the vendor's own reporting resolution). diff_pct does NOT: the
// vendor billed a real 0.004, so the estimate is 5x the bill and the percentage
// says so (-400%) instead of taking pctDiff's vendor==0 branch and flattening
// every such day to a uniform -100%.
//
// That branch now means what it says — the vendor genuinely reported zero —
// rather than "the vendor's amount happened to round to zero". The drift
// alert's dual threshold (|diff%| > 5% AND |diff$| > $1.00, see
// docs/operators/ops/runbooks/alerts.md) still keeps a sub-cent day like this
// one from alerting, since diff_usd here is a fraction of a cent.
func TestReconcileProvider_VendorRoundingToZeroDoesNotFlattenThePercentage(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	j := makeJob(mock, fakeRegistry{}, day(2026, 8, 1), 1, 2)
	d := day(2026, 7, 30)

	vendor, spend := 0.004, 0.02

	expectDayReads(mock, "prov-openai", spend, 0.0, spend)
	mock.ExpectExec(`INSERT INTO vendor_bill_reconciliation`).
		WithArgs("prov-openai", d,
			spend,  // our_billed_usd — unrounded
			spend,  // our_vendor_spend_usd — STORED UNROUNDED
			0.0,    // our_internal_ops_usd
			vendor, // vendor_reported_usd — STORED UNROUNDED, as reported
			-0.02,  // diff_usd = round(0.004)-round(0.02) = 0.00-0.02
			// diff_pct from the unrounded pair: (0.004-0.02)/0.004 = -4.0.
			(vendor-spend)/vendor,
			"api_key", coverageScoped).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	reconcileOneDay(t, j, "prov-openai", []billsrc.VendorDailyBill{{
		Day: d, AmountUSD: vendor, ScopeKind: "api_key",
	}}, d)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestReconcileProvider_SmallDayDriftSurvivesInThePercentage is the regression
// for the erased-signal defect. On a low-volume day both operands land in the
// same cent, so a diff_pct computed from the rounded pair reports exactly 0.0%
// — reading as "perfectly reconciled" on precisely the days where the dollar
// figure is too small to notice and the percentage is the only visible signal.
//
// Modelled on the observed 2026-08-05 shape: a real ~1.29% over-estimate that
// the rounded basis displayed as 0.0%.
func TestReconcileProvider_SmallDayDriftSurvivesInThePercentage(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	j := makeJob(mock, fakeRegistry{}, day(2026, 8, 1), 1, 2)
	d := day(2026, 8, 5)

	// Runtime float64s, not untyped constants — see the note in the sub-cent
	// test above. Both amounts round to $1.28.
	vendor, spend := 1.284, 1.2775

	expectDayReads(mock, "prov-openai", spend, 0.0, spend)
	mock.ExpectExec(`INSERT INTO vendor_bill_reconciliation`).
		WithArgs("prov-openai", d,
			spend, spend, 0.0, vendor,
			// diff_usd IS zero at cent resolution, and correctly so: the vendor
			// could not have billed the difference.
			0.0,
			// diff_pct must NOT be zero — the estimate really is ~0.5% off.
			(vendor-spend)/vendor,
			"api_key", coverageScoped).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	reconcileOneDay(t, j, "prov-openai", []billsrc.VendorDailyBill{{
		Day: d, AmountUSD: vendor, ScopeKind: "api_key",
	}}, d)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	// Guard the premise: if the rounded pair were used, this day would report
	// exactly 0.0% and the drift would be invisible.
	if pctDiff(roundToCents(vendor), roundToCents(spend)) != 0 {
		t.Fatal("premise broken: the rounded pair no longer collapses to 0% on this day")
	}
	if pctDiff(vendor, spend) == 0 {
		t.Fatal("the unrounded pair must expose the drift this day actually carries")
	}
}

// TestRoundToCents pins the quantisation itself: round-half-away-from-zero
// (math.Round), not truncation, so 0.005-style midpoints round up rather than
// being floored away.
func TestRoundToCents(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		want float64
	}{
		{"already exact", 41.56, 41.56},
		{"rounds up through sub-cent noise", 0.4356, 0.44},
		{"rounds down below half a cent", 0.004, 0.0},
		{"half-cent rounds away from zero", 0.005, 0.01},
		{"zero stays zero", 0, 0},
		{"large amount with trailing noise", 140.109999999, 140.11},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := roundToCents(c.in); got != c.want {
				t.Errorf("roundToCents(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestJobMetadata(t *testing.T) {
	j := makeJob(nil, fakeRegistry{}, day(2026, 7, 19), 4, 2)
	if j.ID() != "vendor-bill-reconcile" || j.Name() == "" || j.Description() == "" {
		t.Error("metadata incomplete")
	}
	if j.Interval() != 24*time.Hour {
		t.Errorf("interval = %v", j.Interval())
	}
	if !j.RunOnStart() {
		t.Error("daily job should RunOnStart")
	}
}

// --- rollup-correction gate -------------------------------------------------
//
// The reconciliation basis (metric_rollup_1d vendor_spend_usd) is only complete
// for days the rollup-correction pass has rebuilt from traffic_event. These
// tests pin the two ways a day past that cursor used to be mis-reported.

// TestReconcile_DefersDayBeyondCorrectionWatermark: a day the correction pass
// has not reached is not touched at all — no rollup read, no placeholder row.
// Before the gate this day was read anyway, found no vendor_spend_usd series
// (the correction pass writes it minutes later), and was stamped `no_basis`,
// which VendorBillSyncAlertsJob escalates to vendor.bill_sync_failed 25h later.
// The empty expectation queue is the assertion: pgxmock fails any DB call.
func TestReconcile_DefersDayBeyondCorrectionWatermark(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	d := day(2026, 8, 3)
	j := makeJob(mock, fakeRegistry{}, day(2026, 8, 5), 1, 2)
	src := &fakeSource{key: "openai", bills: []billsrc.VendorDailyBill{
		{Day: d, AmountUSD: 1.81, ScopeKind: "api_key"},
	}}

	// Corrected only through the previous day.
	if err := j.reconcileProvider(context.Background(), "prov1", src, d, d, d.AddDate(0, 0, -1)); err != nil {
		t.Fatalf("reconcileProvider: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a deferred day must issue no query and write no row: %v", err)
	}
}

// TestReconcile_ReconcilesDayAtTheWatermark: the day exactly at the cursor IS
// comparable — the watermark names the newest day the correction pass rebuilt,
// inclusive. An off-by-one here would defer every day forever at steady state,
// because the reconcile window's newest day is precisely the one the previous
// day's correction run finished.
func TestReconcile_ReconcilesDayAtTheWatermark(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	d := day(2026, 8, 3)
	j := makeJob(mock, fakeRegistry{}, day(2026, 8, 5), 1, 2)
	src := &fakeSource{key: "openai", bills: []billsrc.VendorDailyBill{
		{Day: d, AmountUSD: 10, ScopeKind: "api_key"},
	}}

	expectDayReads(mock, "prov1", 9.5, 0.5, 9)
	mock.ExpectExec(`INSERT INTO vendor_bill_reconciliation`).
		WithArgs("prov1", d, 9.0, 9.5, 0.5, 10.0, 0.5, 0.05, "api_key", "scoped").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	if err := j.reconcileProvider(context.Background(), "prov1", src, d, d, d); err != nil {
		t.Fatalf("reconcileProvider: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestReconcile_RunDefersOnlyTheDaysPastTheWatermark: within one run the cursor
// splits the window — older days reconcile normally, the newer day waits. A gate
// that dropped the whole window on a lagging cursor would stall reconciliation
// entirely whenever the correction job fell a day behind.
func TestReconcile_RunDefersOnlyTheDaysPastTheWatermark(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// now = 08-06, lag 2, lookback 2 → window = [08-03, 08-04].
	older, newer := day(2026, 8, 3), day(2026, 8, 4)
	src := &fakeSource{key: "openai", bills: []billsrc.VendorDailyBill{
		{Day: older, AmountUSD: 10, ScopeKind: "api_key"},
		{Day: newer, AmountUSD: 20, ScopeKind: "api_key"},
	}}
	j := makeJob(mock, fakeRegistry{"openai": src}, day(2026, 8, 6), 2, 2)

	expectCorrectionWatermark(mock, older)
	expectProviderList(mock, [2]string{"prov1", "openai"})
	// Only the older day is read and written; the newer one is deferred.
	expectDayReads(mock, "prov1", 9.5, 0.5, 9)
	mock.ExpectExec(`INSERT INTO vendor_bill_reconciliation`).
		WithArgs("prov1", older, 9.0, 9.5, 0.5, 10.0, 0.5, 0.05, "api_key", "scoped").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestReconcile_UnsetWatermarkFailsTheRun: before the correction job has ever
// published a cursor, no day in the window is comparable — so the run writes
// nothing AND says so. Returning nil here is what let stg report three
// consecutive successful runs (2026-08-06..08-08) while the report silently
// stopped advancing at 2026-08-03: the watermark row did not exist until the
// correction job's first success, and a run that defers its entire window has
// done none of its work.
func TestReconcile_UnsetWatermarkFailsTheRun(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	src := &fakeSource{key: "openai", bills: []billsrc.VendorDailyBill{
		{Day: day(2026, 8, 3), AmountUSD: 10, ScopeKind: "api_key"},
	}}
	j := makeJob(mock, fakeRegistry{"openai": src}, day(2026, 8, 5), 1, 2)

	mock.ExpectQuery(`FROM "rollup_watermark"`).
		WithArgs(defjobs_rollup.WatermarkCorrection).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}))

	err := j.Run(context.Background())
	if err == nil {
		t.Fatal("a run that can reconcile no day in its window must fail, not report success")
	}
	if !strings.Contains(err.Error(), "no day in the window can be reconciled") {
		t.Fatalf("error = %v, want it to say the window is entirely unreconcilable", err)
	}
	// The provider list is never even read: there is nothing any provider could
	// contribute, and the vendor cost APIs are rate-limited.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("nothing may be read or written before the first correction run: %v", err)
	}
}

// TestReconcile_WaitsForCorrectionToReachTheWindow is the regression test for
// the ordering race itself. rollup-correction and this job share a 24h interval
// and fire on the same scheduler tick; this job finishes in ~2s while the
// correction pass takes ~90s and only publishes its watermark at the end. A
// single read therefore observes a watermark that has not yet reached the
// window, and the days past it are deferred for no reason other than having
// asked too early. The job must wait for the value it depends on.
func TestReconcile_WaitsForCorrectionToReachTheWindow(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	src := &fakeSource{key: "openai", bills: []billsrc.VendorDailyBill{
		{Day: day(2026, 8, 3), AmountUSD: 10, ScopeKind: "api_key"},
	}}
	j := makeWaitingJob(mock, fakeRegistry{"openai": src}, day(2026, 8, 5), 1, 2,
		time.Minute, 15*time.Second)

	// Window is the single day 2026-08-03. The correction pass is mid-run: the
	// first two reads still show yesterday's cursor, the third shows it landing.
	expectCorrectionWatermark(mock, day(2026, 8, 2))
	expectCorrectionWatermark(mock, day(2026, 8, 2))
	expectCorrectionWatermark(mock, day(2026, 8, 3))
	expectProviderList(mock, [2]string{"prov1", "openai"})
	expectDayReads(mock, "prov1", 9, 0, 9)
	// diff = 10-9 = 1.00 against a recorded basis: a real comparison, which is
	// precisely what the single-read version of this job could not produce.
	mock.ExpectExec(`INSERT INTO vendor_bill_reconciliation`).
		WithArgs("prov1", day(2026, 8, 3), 9.0, 9.0, 0.0, 10.0, 1.0, 0.1, "api_key", coverageScoped).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The day reconciles because the job waited — not deferred, not a placeholder.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the day must reconcile once the correction pass reaches it: %v", err)
	}
}

// TestReconcile_WaitTimesOutButPartialWindowStillReconciles: the wait is
// bounded, so a correction pass that is failing outright cannot turn this job
// into one that never returns. When it times out, the days the watermark HAS
// reached are still reconciled and only the rest defer — a partially usable
// window is not a failed run.
func TestReconcile_WaitTimesOutButPartialWindowStillReconciles(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	src := &fakeSource{key: "openai", bills: []billsrc.VendorDailyBill{
		{Day: day(2026, 8, 2), AmountUSD: 10, ScopeKind: "api_key"},
		{Day: day(2026, 8, 3), AmountUSD: 11, ScopeKind: "api_key"},
	}}
	// Window 2026-08-02..08-03; the cursor never gets past 08-02.
	j := makeWaitingJob(mock, fakeRegistry{"openai": src}, day(2026, 8, 5), 2, 2,
		30*time.Second, 15*time.Second)

	expectCorrectionWatermark(mock, day(2026, 8, 2))
	expectCorrectionWatermark(mock, day(2026, 8, 2))
	expectCorrectionWatermark(mock, day(2026, 8, 2))
	expectProviderList(mock, [2]string{"prov1", "openai"})
	// 08-02 reconciles; 08-03 is past the cursor and defers, writing nothing.
	expectDayReads(mock, "prov1", 9, 0, 9)
	mock.ExpectExec(`INSERT INTO vendor_bill_reconciliation`).
		WithArgs("prov1", day(2026, 8, 2), 9.0, 9.0, 0.0, 10.0, 1.0, 0.1, "api_key", coverageScoped).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("a partially reconcilable window is not a failed run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the days the cursor reached must still reconcile after a timeout: %v", err)
	}
}

// TestSleepCtx covers the production delay seam awaitCorrection uses, which the
// job tests replace with a logical clock. Both outcomes matter: it must
// actually wait, and it must abandon the wait the moment the run's context is
// cancelled — a Hub shutdown during a 10-minute correction wait would otherwise
// hold the scheduler's drain open until the timer expired.
func TestSleepCtx(t *testing.T) {
	t.Run("returns after the delay", func(t *testing.T) {
		start := time.Now()
		if err := sleepCtx(context.Background(), 20*time.Millisecond); err != nil {
			t.Fatalf("sleepCtx: %v", err)
		}
		if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
			t.Fatalf("returned after %v, want at least the full 20ms delay", elapsed)
		}
	})

	t.Run("returns the cancellation cause without waiting out the delay", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		start := time.Now()
		err := sleepCtx(ctx, time.Hour)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("took %v: a cancelled context must not wait out the delay", elapsed)
		}
	})
}

// TestReconcile_WaitAbortsOnContextCancellation: a Hub shutdown mid-wait must
// end the run rather than hold the scheduler open for the full timeout.
func TestReconcile_WaitAbortsOnContextCancellation(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	j := makeJob(mock, fakeRegistry{}, day(2026, 8, 5), 1, 2)
	j.correctionWait = time.Minute
	j.sleep = func(ctx context.Context, _ time.Duration) error { return context.Canceled }

	expectCorrectionWatermark(mock, day(2026, 8, 1))

	err := j.Run(context.Background())
	if err == nil {
		t.Fatal("expected the cancelled wait to end the run")
	}
	if !strings.Contains(err.Error(), "waiting for rollup correction") {
		t.Fatalf("error = %v, want it to name the wait it was cancelled in", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("no provider may be read after the wait was cancelled: %v", err)
	}
}

// TestReconcile_WatermarkReadErrorAbortsRun: the cursor read failing is not a
// reason to fall back to reconciling blind — the run fails so job_run records
// it, and no provider is touched.
func TestReconcile_WatermarkReadErrorAbortsRun(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	j := makeJob(mock, fakeRegistry{}, day(2026, 8, 5), 1, 2)
	mock.ExpectQuery(`FROM "rollup_watermark"`).
		WithArgs(defjobs_rollup.WatermarkCorrection).
		WillReturnError(errors.New("watermark read failed"))

	err := j.Run(context.Background())
	if err == nil {
		t.Fatal("expected the watermark read failure to fail the run")
	}
	if !strings.Contains(err.Error(), "read correction watermark") {
		t.Fatalf("error = %v, want it to name the correction watermark read", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("no provider may be read after the cursor read failed: %v", err)
	}
}

// TestListCoveredProviders_ExcludesAProviderServedFromAnotherHost: a provider
// sharing a covered vendor's adapter type but served from its own domain — a
// self-hosted model on the OpenAI wire format — must not be reconciled against
// that vendor's bill. prod carries exactly this row today (local-inference,
// adapter_type openai, baseUrl http://localhost:9001/v1); enabling it under the
// type-only resolution would have given it the real OpenAI daily total as its
// vendor_reported_usd, double-counting the vendor's money under a provider that
// spent none of it and firing drift on a bill that was never issued.
func TestListCoveredProviders_ExcludesAProviderServedFromAnotherHost(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	src := &fakeSource{key: "openai", billingHost: "api.openai.com"}
	j := makeJob(mock, fakeRegistry{"openai": src}, day(2026, 8, 1), 1, 2)

	expectProviderListWithBaseURL(mock,
		[3]string{"prov-openai", "openai", "https://api.openai.com"},
		[3]string{"prov-local", "openai", "http://localhost:9001/v1"},
	)

	got, err := j.listCoveredProviders(context.Background())
	if err != nil {
		t.Fatalf("listCoveredProviders: %v", err)
	}
	if len(got) != 1 || got[0].id != "prov-openai" {
		t.Fatalf("covered = %+v, want only the provider actually served by the vendor", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestListCoveredProviders_EmptyBaseURLIsTheVendorsOwnHost: a row that names no
// base URL is asking for the adapter's default endpoint, which IS the vendor's
// host. Excluding it would silently drop a real provider from reconciliation.
func TestListCoveredProviders_EmptyBaseURLIsTheVendorsOwnHost(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	src := &fakeSource{key: "openai", billingHost: "api.openai.com"}
	j := makeJob(mock, fakeRegistry{"openai": src}, day(2026, 8, 1), 1, 2)

	expectProviderListWithBaseURL(mock, [3]string{"prov-openai", "openai", ""})

	got, err := j.listCoveredProviders(context.Background())
	if err != nil {
		t.Fatalf("listCoveredProviders: %v", err)
	}
	if len(got) != 1 || got[0].id != "prov-openai" {
		t.Fatalf("covered = %+v, want the provider kept", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

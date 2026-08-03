package vendorbill

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	billsrc "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/vendorbill"
	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// fakeSource is a canned VendorBillSource.
type fakeSource struct {
	key       string
	bills     []billsrc.VendorDailyBill
	err       error
	gotFrom   time.Time
	gotTo     time.Time
	callCount int
}

func (f *fakeSource) ProviderKey() string { return f.key }
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
	}
}

func expectProviders(mock pgxmock.PgxPoolIface, rows ...[2]string) {
	rr := pgxmock.NewRows([]string{"id", "adapter_type"})
	for _, r := range rows {
		rr.AddRow(r[0], r[1])
	}
	mock.ExpectQuery(`SELECT id, adapter_type FROM "Provider"`).WillReturnRows(rr)
}

func expectOurBilled(mock pgxmock.PgxPoolIface, providerID string, v float64) {
	mock.ExpectQuery(`FROM metric_rollup_1d`).
		WithArgs(metrics.MetricBilledCostUSD, metrics.BuildDimensionKey("routed_provider", providerID), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"sum"}).AddRow(v))
}

func TestReconcile_ComputesDiffScopedAndUpserts(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	src := &fakeSource{key: "openai", bills: []billsrc.VendorDailyBill{
		{Day: day(2026, 7, 17), AmountUSD: 10, ScopeKind: "project", ScopeID: "proj_gw"},
	}}
	j := makeJob(mock, fakeRegistry{"openai": src}, day(2026, 7, 19), 1, 2) // window = single day 07-17

	expectProviders(mock, [2]string{"prov1", "openai"})
	expectOurBilled(mock, "prov1", 9)
	// diff = 10-9 = 1 ; pct = 1/10 = 0.1 ; scoped
	mock.ExpectExec(`INSERT INTO vendor_bill_reconciliation`).
		WithArgs("prov1", pgxmock.AnyArg(), 9.0, 10.0, 1.0, 0.1, "project", "scoped").
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
	expectOurBilled(mock, "prov1", 20)
	// org scope => coverage org_only, alert-suppressed downstream
	mock.ExpectExec(`INSERT INTO vendor_bill_reconciliation`).
		WithArgs("prov1", pgxmock.AnyArg(), 20.0, 20.0, 0.0, 0.0, "org", "org_only").
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
		WithArgs("prov1", pgxmock.AnyArg(), 5.0, nil, nil, nil, "org", "fetch_failed").
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
		WithArgs("prov1", pgxmock.AnyArg(), 5.0, nil, nil, nil, "org", "fetch_failed").
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
	expectOurBilled(mock, "prov1", 9)
	mock.ExpectExec(`ON CONFLICT \(provider_id, day\) DO UPDATE`).
		WithArgs("prov1", pgxmock.AnyArg(), 9.0, 10.0, 1.0, 0.1, "project", "scoped").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
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
	expectOurBilled(mock, "prov1", 1)
	mock.ExpectExec(`INSERT INTO vendor_bill_reconciliation`).
		WithArgs("prov1", pgxmock.AnyArg(), 1.0, 1.0, 0.0, 0.0, "project", "scoped").
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
		WithArgs("pA", pgxmock.AnyArg(), 1.0, nil, nil, nil, "org", "fetch_failed").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	// anthropic still reconciled
	expectOurBilled(mock, "pB", 3)
	mock.ExpectExec(`INSERT INTO vendor_bill_reconciliation`).
		WithArgs("pB", pgxmock.AnyArg(), 3.0, 3.0, 0.0, 0.0, "workspace", "scoped").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
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

package vendorbill

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
)

type fakeRaiser struct {
	raises     []alerting.RaiseInput
	resolves   [][2]string
	raiseErr   error
	resolveErr error
}

func (f *fakeRaiser) Raise(_ context.Context, in alerting.RaiseInput) error {
	f.raises = append(f.raises, in)
	return f.raiseErr
}
func (f *fakeRaiser) Resolve(_ context.Context, ruleID, targetKey, _ string) error {
	f.resolves = append(f.resolves, [2]string{ruleID, targetKey})
	return f.resolveErr
}

type fakeRuleLoader struct {
	rule *alerting.AlertRule
	err  error
}

func (f *fakeRuleLoader) GetRule(_ context.Context, _ string) (*alerting.AlertRule, error) {
	return f.rule, f.err
}

func enabledRule() *alerting.AlertRule {
	return &alerting.AlertRule{
		ID:              driftRuleID,
		Enabled:         true,
		DefaultSeverity: alerting.SeverityHigh,
		Params:          map[string]any{"thresholdPct": 5.0, "thresholdUsd": 1.0},
	}
}

func makeDriftJob(mock pgxmock.PgxPoolIface, raiser *fakeRaiser, loader *fakeRuleLoader, now time.Time) *VendorBillDriftAlertsJob {
	return &VendorBillDriftAlertsJob{
		pool:       mock,
		raiser:     raiser,
		ruleLoader: loader,
		interval:   24 * time.Hour,
		logger:     testLogger(),
		now:        func() time.Time { return now },
	}
}

// expectScopedRow queues a single scoped reconciliation row for the query mock.
// `our` is the quota basis (our_billed_usd) and `spend` the reconciliation
// basis (our_vendor_spend_usd) that diff_usd / diff_pct are derived from; they
// are passed separately so a test can prove which one the alert quotes.
func expectScopedRow(mock pgxmock.PgxPoolIface, providerID string, d time.Time, our, spend, vendor, diffUSD, diffPct float64, display string) {
	mock.ExpectQuery(`FROM vendor_bill_reconciliation`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"provider_id", "day", "our_billed_usd", "our_vendor_spend_usd", "vendor_reported_usd", "diff_usd", "diff_pct", "display_name"}).
			AddRow(providerID, d, our, spend, vendor, diffUSD, diffPct, display))
}

func TestDrift_FiresWhenBothFloorsExceeded(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	raiser := &fakeRaiser{}
	// diff_pct 8% > 5% AND diff_usd $2 > $1 → fire.
	expectScopedRow(mock, "prov1", day(2026, 7, 17), 18, 23, 25, 2, 0.08, "OpenAI")
	j := makeDriftJob(mock, raiser, &fakeRuleLoader{rule: enabledRule()}, day(2026, 7, 19))

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(raiser.raises) != 1 {
		t.Fatalf("want 1 raise, got %d", len(raiser.raises))
	}
	r := raiser.raises[0]
	if r.RuleID != driftRuleID || r.TargetKey != "vendor-bill:prov1:2026-07-17" {
		t.Fatalf("wrong raise identity: %+v", r)
	}
	if r.Severity != alerting.SeverityHigh {
		t.Errorf("severity = %v, want high", r.Severity)
	}
	if !strings.Contains(r.Message, "OpenAI") || !strings.Contains(r.Message, "8.0%") {
		t.Errorf("message missing detail: %q", r.Message)
	}
	// The "ours" figure quoted next to the percentage must be the reconciliation
	// basis ($23, our_vendor_spend_usd), not the quota basis ($18) — quoting the
	// latter would print 25 vs 18 alongside a percentage derived from 25 vs 23.
	if !strings.Contains(r.Message, "ours $23.00") {
		t.Errorf("message must quote the vendor-spend basis: %q", r.Message)
	}
	if got := r.Details["ourVendorSpendUsd"]; got != 23.0 {
		t.Errorf("details ourVendorSpendUsd = %v, want 23", got)
	}
	// The shipped field keeps its original meaning: the customer-quota basis.
	if got := r.Details["ourBilledUsd"]; got != 18.0 {
		t.Errorf("details ourBilledUsd = %v, want 18 (quota basis, unchanged)", got)
	}
	if len(raiser.resolves) != 0 {
		t.Errorf("a breaching row must not resolve, got %v", raiser.resolves)
	}
}

func TestDrift_SuppressedWhenPctBelowFloor(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	raiser := &fakeRaiser{}
	// diff_usd $5 > $1 but diff_pct 2% < 5% → suppressed (dual floor), self-heal resolve.
	expectScopedRow(mock, "prov1", day(2026, 7, 17), 240, 245, 250, 5, 0.02, "OpenAI")
	j := makeDriftJob(mock, raiser, &fakeRuleLoader{rule: enabledRule()}, day(2026, 7, 19))

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(raiser.raises) != 0 {
		t.Fatalf("pct below floor must not fire, got %d raises", len(raiser.raises))
	}
	if len(raiser.resolves) != 1 || raiser.resolves[0][1] != "vendor-bill:prov1:2026-07-17" {
		t.Fatalf("non-breaching row must self-heal resolve, got %v", raiser.resolves)
	}
}

func TestDrift_SuppressedWhenUsdBelowFloor(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	raiser := &fakeRaiser{}
	// diff_pct 50% > 5% but diff_usd $0.50 < $1 → suppressed (dual floor).
	expectScopedRow(mock, "prov1", day(2026, 7, 17), 0.2, 0.5, 1.0, 0.5, 0.5, "DeepThink")
	j := makeDriftJob(mock, raiser, &fakeRuleLoader{rule: enabledRule()}, day(2026, 7, 19))

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(raiser.raises) != 0 {
		t.Fatalf("tiny absolute amount must not fire despite big %%, got %d raises", len(raiser.raises))
	}
}

// A row with a difference but no recorded vendor spend is a pre-cutover row: its
// diff_pct came from the old basis and does not follow from its (zero) spend, so
// alerting on it would print "vendor $25.00 vs ours $0.00 (28.0%)" and demand
// action on a number nobody can act on. The 7-day scan reaches further back than
// the 4-day rewrite window, so today-6 / today-7 always contain such rows, and a
// day the reconcile job skipped contains one permanently.
//
// It must land in the RESOLVE branch, not merely be skipped: an alert already
// raised on the old basis has to clear, and filtering the row out of the scan
// would leave it firing forever.
func TestDrift_PreCutoverRowResolvesInsteadOfReportingZeroSpend(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	raiser := &fakeRaiser{}
	// Both floors are breached on the persisted old-basis numbers (28% and $7),
	// so only the zero spend can stop this row from firing.
	expectScopedRow(mock, "prov1", day(2026, 7, 17), 18, 0, 25, 7, 0.28, "OpenAI")
	j := makeDriftJob(mock, raiser, &fakeRuleLoader{rule: enabledRule()}, day(2026, 7, 19))

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(raiser.raises) != 0 {
		t.Fatalf("a row with no recorded vendor spend must not fire, got %+v", raiser.raises)
	}
	if len(raiser.resolves) != 1 || raiser.resolves[0][1] != "vendor-bill:prov1:2026-07-17" {
		t.Fatalf("it must resolve so an alert raised on the old basis clears, got %v", raiser.resolves)
	}
}

func TestDrift_NegativeDriftFires(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	raiser := &fakeRaiser{}
	// We UNDER-charged: vendor > ours. |−8%| and |−$2| both breach.
	expectScopedRow(mock, "prov1", day(2026, 7, 17), 20, 25, 23, -2, -0.08, "OpenAI")
	j := makeDriftJob(mock, raiser, &fakeRuleLoader{rule: enabledRule()}, day(2026, 7, 19))

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(raiser.raises) != 1 {
		t.Fatalf("drift in either direction must fire, got %d", len(raiser.raises))
	}
}

func TestDrift_RuleDisabled_NoQuery(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	raiser := &fakeRaiser{}
	rule := enabledRule()
	rule.Enabled = false
	j := makeDriftJob(mock, raiser, &fakeRuleLoader{rule: rule}, day(2026, 7, 19))
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(raiser.raises) != 0 || len(raiser.resolves) != 0 {
		t.Fatal("disabled rule must not touch the raiser")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("disabled rule must not query: %v", err)
	}
}

func TestDrift_RuleNotFound_Skips(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	j := makeDriftJob(mock, &fakeRaiser{}, &fakeRuleLoader{err: alerting.ErrNotFound}, day(2026, 7, 19))
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("missing rule must be a clean skip, got %v", err)
	}
}

func TestDrift_RuleLoadError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	j := makeDriftJob(mock, &fakeRaiser{}, &fakeRuleLoader{err: errors.New("store down")}, day(2026, 7, 19))
	if err := j.Run(context.Background()); err == nil {
		t.Fatal("a non-NotFound rule-load error must propagate")
	}
}

func TestDrift_QueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`FROM vendor_bill_reconciliation`).WithArgs(pgxmock.AnyArg()).WillReturnError(errors.New("db down"))
	j := makeDriftJob(mock, &fakeRaiser{}, &fakeRuleLoader{rule: enabledRule()}, day(2026, 7, 19))
	if err := j.Run(context.Background()); err == nil {
		t.Fatal("query failure must propagate")
	}
}

func TestDrift_ScanError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`FROM vendor_bill_reconciliation`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"provider_id"}).AddRow("only-one-col"))
	j := makeDriftJob(mock, &fakeRaiser{}, &fakeRuleLoader{rule: enabledRule()}, day(2026, 7, 19))
	if err := j.Run(context.Background()); err == nil {
		t.Fatal("row scan mismatch must propagate")
	}
}

func TestDrift_RaiseErrorAggregated(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	raiser := &fakeRaiser{raiseErr: errors.New("raise failed")}
	expectScopedRow(mock, "prov1", day(2026, 7, 17), 18, 23, 25, 2, 0.08, "OpenAI")
	j := makeDriftJob(mock, raiser, &fakeRuleLoader{rule: enabledRule()}, day(2026, 7, 19))
	if err := j.Run(context.Background()); err == nil {
		t.Fatal("a raiser error must surface")
	}
}

func TestDrift_ResolveErrorAggregated(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	raiser := &fakeRaiser{resolveErr: errors.New("resolve failed")}
	expectScopedRow(mock, "prov1", day(2026, 7, 17), 240, 245, 250, 5, 0.02, "OpenAI") // non-breaching → resolve
	j := makeDriftJob(mock, raiser, &fakeRuleLoader{rule: enabledRule()}, day(2026, 7, 19))
	if err := j.Run(context.Background()); err == nil {
		t.Fatal("a resolve error must surface")
	}
}

func TestDrift_Defaults_And_Metadata(t *testing.T) {
	j := NewVendorBillDriftAlerts(nil, &fakeRaiser{}, &fakeRuleLoader{}, 0, testLogger())
	if j.Interval() != 24*time.Hour {
		t.Errorf("zero interval must default to 24h, got %v", j.Interval())
	}
	if j.ID() != driftJobID || j.Name() == "" || j.Description() == "" || !j.RunOnStart() {
		t.Error("metadata incomplete")
	}
	j2 := NewVendorBillDriftAlerts(nil, &fakeRaiser{}, &fakeRuleLoader{}, 2*time.Hour, testLogger())
	if j2.Interval() != 2*time.Hour {
		t.Errorf("explicit interval must be kept, got %v", j2.Interval())
	}
}

func TestFloatParam(t *testing.T) {
	if got := floatParam(nil, "x", 7); got != 7 {
		t.Errorf("nil params → default; got %v", got)
	}
	if got := floatParam(map[string]any{}, "x", 7); got != 7 {
		t.Errorf("missing key → default; got %v", got)
	}
	if got := floatParam(map[string]any{"x": "not-a-float"}, "x", 7); got != 7 {
		t.Errorf("wrong type → default; got %v", got)
	}
	if got := floatParam(map[string]any{"x": 3.5}, "x", 7); got != 3.5 {
		t.Errorf("present float → value; got %v", got)
	}
}

func TestDrift_FloatParamDefault(t *testing.T) {
	// Missing params fall back to defaults (5% / $1) — a $0.50 diff still suppressed.
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	raiser := &fakeRaiser{}
	rule := &alerting.AlertRule{ID: driftRuleID, Enabled: true, DefaultSeverity: alerting.SeverityHigh, Params: nil}
	expectScopedRow(mock, "prov1", day(2026, 7, 17), 9.0, 9.5, 10, 0.5, 0.05, "OpenAI") // 5% not > 5% → suppressed
	j := makeDriftJob(mock, raiser, &fakeRuleLoader{rule: rule}, day(2026, 7, 19))
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(raiser.raises) != 0 {
		t.Fatalf("exactly at the default floor must not fire, got %d", len(raiser.raises))
	}
}

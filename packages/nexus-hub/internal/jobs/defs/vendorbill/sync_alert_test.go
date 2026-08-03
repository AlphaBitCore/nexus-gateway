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

func makeSyncJob(mock pgxmock.PgxPoolIface, raiser *fakeRaiser, loader *fakeRuleLoader, now time.Time) *VendorBillSyncAlertsJob {
	return &VendorBillSyncAlertsJob{
		pool:       mock,
		raiser:     raiser,
		ruleLoader: loader,
		interval:   24 * time.Hour,
		logger:     testLogger(),
		now:        func() time.Time { return now },
	}
}

func enabledSyncRule() *alerting.AlertRule {
	return &alerting.AlertRule{
		ID:              syncRuleID,
		Enabled:         true,
		DefaultSeverity: alerting.SeverityMedium,
		Params:          map[string]any{"staleHours": 25.0},
	}
}

type syncRow struct {
	providerID  string
	display     string
	staleFailed int
	oldestDay   string // "YYYY-MM-DD" when staleFailed>0, else ""
}

func expectSyncScan(mock pgxmock.PgxPoolIface, rows ...syncRow) {
	rr := pgxmock.NewRows([]string{"provider_id", "display_name", "stale_failed", "oldest_failed_day"})
	for _, r := range rows {
		rr.AddRow(r.providerID, r.display, r.staleFailed, r.oldestDay)
	}
	mock.ExpectQuery(`FROM vendor_bill_reconciliation`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(rr)
}

// A provider whose vendor sync has been fetch_failed past the stale window
// raises one per-provider alert naming how long it has been broken.
func TestSync_FiresWhenStaleFetchFailed(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	raiser := &fakeRaiser{}
	expectSyncScan(mock, syncRow{"prov1", "OpenAI", 3, "2026-07-19"})
	j := makeSyncJob(mock, raiser, &fakeRuleLoader{rule: enabledSyncRule()}, day(2026, 7, 23))

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(raiser.raises) != 1 {
		t.Fatalf("want 1 raise, got %d", len(raiser.raises))
	}
	r := raiser.raises[0]
	if r.RuleID != syncRuleID || r.TargetKey != "vendor-bill-sync:prov1" {
		t.Fatalf("wrong raise identity: %+v", r)
	}
	if r.Severity != alerting.SeverityMedium {
		t.Errorf("severity = %v, want medium", r.Severity)
	}
	if !strings.Contains(r.Message, "OpenAI") || !strings.Contains(r.Message, "2026-07-19") || !strings.Contains(r.Message, "3") {
		t.Errorf("message missing provider/oldest-day/count: %q", r.Message)
	}
	if got := r.Details["oldestFailedDay"]; got != "2026-07-19" {
		t.Errorf("details oldestFailedDay = %v, want 2026-07-19", got)
	}
	if got := r.Details["failedDayCount"]; got != 3 {
		t.Errorf("details failedDayCount = %v, want 3", got)
	}
	if len(raiser.resolves) != 0 {
		t.Errorf("a firing provider must not also resolve, got %v", raiser.resolves)
	}
}

// A provider with no stale fetch_failed (healthy, or only a FRESH transient
// fetch_failed that the SQL stale filter excludes → stale_failed=0) resolves,
// never fires. This is the transient-blip guarantee.
func TestSync_ResolvesWhenNoStaleFailed(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	raiser := &fakeRaiser{}
	expectSyncScan(mock, syncRow{"prov1", "OpenAI", 0, ""})
	j := makeSyncJob(mock, raiser, &fakeRuleLoader{rule: enabledSyncRule()}, day(2026, 7, 23))

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(raiser.raises) != 0 {
		t.Fatalf("no stale failure must not fire, got %d raises", len(raiser.raises))
	}
	if len(raiser.resolves) != 1 || raiser.resolves[0][0] != syncRuleID || raiser.resolves[0][1] != "vendor-bill-sync:prov1" {
		t.Fatalf("a recovered/healthy provider must self-heal resolve, got %v", raiser.resolves)
	}
}

// The stale cutoff passed to the query is now-staleHours; the scan window is
// now-syncScanDays. This is where the persistent-vs-transient boundary lives.
func TestSync_StaleCutoffUsesStaleHoursParam(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	raiser := &fakeRaiser{}
	rule := enabledSyncRule()
	rule.Params = map[string]any{"staleHours": 48.0}
	// now=07-19 00:00 → staleCutoff = now-48h = 07-17 00:00; scanSince = now-7d = 07-12 00:00.
	mock.ExpectQuery(`FROM vendor_bill_reconciliation`).
		WithArgs(day(2026, 7, 17), day(2026, 7, 12)).
		WillReturnRows(pgxmock.NewRows([]string{"provider_id", "display_name", "stale_failed", "oldest_failed_day"}).
			AddRow("prov1", "OpenAI", 0, ""))
	j := makeSyncJob(mock, raiser, &fakeRuleLoader{rule: rule}, day(2026, 7, 19))

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("stale cutoff / scan window args wrong: %v", err)
	}
}

// Two providers in one poll: one broken (raise), one healthy (resolve).
func TestSync_PerProviderRaiseAndResolve(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	raiser := &fakeRaiser{}
	expectSyncScan(mock,
		syncRow{"pA", "OpenAI", 2, "2026-07-20"},
		syncRow{"pB", "Anthropic", 0, ""},
	)
	j := makeSyncJob(mock, raiser, &fakeRuleLoader{rule: enabledSyncRule()}, day(2026, 7, 23))

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(raiser.raises) != 1 || raiser.raises[0].TargetKey != "vendor-bill-sync:pA" {
		t.Fatalf("only the broken provider should raise, got %v", raiser.raises)
	}
	if len(raiser.resolves) != 1 || raiser.resolves[0][1] != "vendor-bill-sync:pB" {
		t.Fatalf("only the healthy provider should resolve, got %v", raiser.resolves)
	}
}

func TestSync_RuleMissingSkips(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	raiser := &fakeRaiser{}
	j := makeSyncJob(mock, raiser, &fakeRuleLoader{err: alerting.ErrNotFound}, day(2026, 7, 23))
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("missing rule must be a no-op, got %v", err)
	}
	if len(raiser.raises)+len(raiser.resolves) != 0 {
		t.Fatalf("missing rule must not touch the raiser")
	}
}

func TestSync_RuleDisabledSkips(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	raiser := &fakeRaiser{}
	rule := enabledSyncRule()
	rule.Enabled = false
	j := makeSyncJob(mock, raiser, &fakeRuleLoader{rule: rule}, day(2026, 7, 23))
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("disabled rule must be a no-op, got %v", err)
	}
	if len(raiser.raises)+len(raiser.resolves) != 0 {
		t.Fatalf("disabled rule must not touch the raiser")
	}
}

func TestSync_RuleLoadErrorFails(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	j := makeSyncJob(mock, &fakeRaiser{}, &fakeRuleLoader{err: errors.New("store down")}, day(2026, 7, 23))
	if err := j.Run(context.Background()); err == nil {
		t.Fatal("a non-notfound rule load error must fail the run")
	}
}

func TestSync_QueryErrorFails(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`FROM vendor_bill_reconciliation`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("db down"))
	j := makeSyncJob(mock, &fakeRaiser{}, &fakeRuleLoader{rule: enabledSyncRule()}, day(2026, 7, 23))
	if err := j.Run(context.Background()); err == nil {
		t.Fatal("a query error must fail the run")
	}
}

func TestSync_ScanErrorFails(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// Only two columns returned but Scan wants four → scan error.
	mock.ExpectQuery(`FROM vendor_bill_reconciliation`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"provider_id", "display_name"}).AddRow("prov1", "OpenAI"))
	j := makeSyncJob(mock, &fakeRaiser{}, &fakeRuleLoader{rule: enabledSyncRule()}, day(2026, 7, 23))
	if err := j.Run(context.Background()); err == nil {
		t.Fatal("a row scan error must fail the run")
	}
}

func TestSync_RaiseErrorSurfaced(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	raiser := &fakeRaiser{raiseErr: errors.New("raise failed")}
	expectSyncScan(mock, syncRow{"prov1", "OpenAI", 1, "2026-07-21"})
	j := makeSyncJob(mock, raiser, &fakeRuleLoader{rule: enabledSyncRule()}, day(2026, 7, 23))
	if err := j.Run(context.Background()); err == nil {
		t.Fatal("a raise error must be surfaced")
	}
}

func TestSync_ResolveErrorSurfaced(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	raiser := &fakeRaiser{resolveErr: errors.New("resolve failed")}
	expectSyncScan(mock, syncRow{"prov1", "OpenAI", 0, ""})
	j := makeSyncJob(mock, raiser, &fakeRuleLoader{rule: enabledSyncRule()}, day(2026, 7, 23))
	if err := j.Run(context.Background()); err == nil {
		t.Fatal("a resolve error must be surfaced")
	}
}

func TestSyncJobMetadata(t *testing.T) {
	j := makeSyncJob(nil, &fakeRaiser{}, &fakeRuleLoader{}, day(2026, 7, 23))
	if j.ID() != "vendor-bill-sync-alerts" || j.Name() == "" || j.Description() == "" {
		t.Error("metadata incomplete")
	}
	if j.Interval() != 24*time.Hour || !j.RunOnStart() {
		t.Error("interval/runonstart wrong")
	}
}

func TestNewVendorBillSyncAlerts_Defaults(t *testing.T) {
	j := NewVendorBillSyncAlerts(nil, &fakeRaiser{}, &fakeRuleLoader{}, 0, testLogger())
	if j.Interval() != 24*time.Hour {
		t.Errorf("zero interval must default to 24h, got %v", j.Interval())
	}
	if j.now == nil || j.now().IsZero() {
		t.Error("clock must be set and return a real time")
	}
	j2 := NewVendorBillSyncAlerts(nil, &fakeRaiser{}, &fakeRuleLoader{}, 6*time.Hour, testLogger())
	if j2.Interval() != 6*time.Hour {
		t.Errorf("explicit interval must be kept, got %v", j2.Interval())
	}
}

func TestSync_RowsIterErrorFails(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("rows iter err")
	// CloseError surfaces as rows.Err() after the (empty) loop.
	mock.ExpectQuery(`FROM vendor_bill_reconciliation`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"provider_id", "display_name", "stale_failed", "oldest_failed_day"}).
			CloseError(sentinel))
	j := makeSyncJob(mock, &fakeRaiser{}, &fakeRuleLoader{rule: enabledSyncRule()}, day(2026, 7, 23))
	if err := j.Run(context.Background()); err == nil {
		t.Fatal("a rows iteration error must fail the run")
	}
}

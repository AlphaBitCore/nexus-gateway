package vendorbill

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	billsrc "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/vendorbill"
)

func TestNewVendorBillReconcileJob_Defaults(t *testing.T) {
	j := NewVendorBillReconcileJob(nil, billsrc.NewRegistry(billsrc.Config{}), 0, testLogger())
	if j.Interval() != 24*time.Hour {
		t.Errorf("zero interval must default to 24h, got %v", j.Interval())
	}
	if j.lookbackDays != defaultLookbackDays || j.finalizeLagDays != defaultFinalizeLagDays {
		t.Errorf("defaults not applied: lookback=%d lag=%d", j.lookbackDays, j.finalizeLagDays)
	}
	if j.now == nil {
		t.Error("clock must be set")
	}

	// A positive interval is honored, not overridden.
	j2 := NewVendorBillReconcileJob(nil, billsrc.NewRegistry(billsrc.Config{}), 6*time.Hour, testLogger())
	if j2.Interval() != 6*time.Hour {
		t.Errorf("explicit interval must be kept, got %v", j2.Interval())
	}
}

func TestRun_ListProvidersQueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`SELECT id, adapter_type FROM "Provider"`).WillReturnError(errors.New("db down"))
	j := makeJob(mock, fakeRegistry{"openai": &fakeSource{key: "openai"}}, day(2026, 7, 19), 1, 2)
	if err := j.Run(context.Background()); err == nil {
		t.Fatal("a failed provider listing must fail the whole run")
	}
}

func TestListProviders_ScanError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// One column returned but Scan wants two → scan error surfaces.
	mock.ExpectQuery(`SELECT id, adapter_type FROM "Provider"`).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("only-one-col"))
	j := makeJob(mock, fakeRegistry{"openai": &fakeSource{key: "openai"}}, day(2026, 7, 19), 1, 2)
	if err := j.Run(context.Background()); err == nil {
		t.Fatal("a row scan error must fail the run")
	}
}

func TestReconcile_OurBilledError_SwallowedButNoUpsert(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	src := &fakeSource{key: "openai", bills: []billsrc.VendorDailyBill{{Day: day(2026, 7, 17), AmountUSD: 1, ScopeKind: "project"}}}
	j := makeJob(mock, fakeRegistry{"openai": src}, day(2026, 7, 19), 1, 2)

	expectProviders(mock, [2]string{"prov1", "openai"})
	mock.ExpectQuery(`FROM metric_rollup_1d`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("rollup read failed"))
	// No upsert expected — a read failure must not write a fabricated row.

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("per-provider error must be swallowed, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestReconcile_UpsertError_Swallowed(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	src := &fakeSource{key: "openai", bills: []billsrc.VendorDailyBill{{Day: day(2026, 7, 17), AmountUSD: 1, ScopeKind: "project"}}}
	j := makeJob(mock, fakeRegistry{"openai": src}, day(2026, 7, 19), 1, 2)

	expectProviders(mock, [2]string{"prov1", "openai"})
	expectOurBilled(mock, "prov1", 1)
	mock.ExpectExec(`INSERT INTO vendor_bill_reconciliation`).WillReturnError(errors.New("write failed"))

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("per-provider upsert error must be swallowed, got %v", err)
	}
}

func TestFetchFailed_OurBilledError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	src := &fakeSource{key: "openai", err: errors.New("401")}
	j := makeJob(mock, fakeRegistry{"openai": src}, day(2026, 7, 19), 1, 2)

	expectProviders(mock, [2]string{"prov1", "openai"})
	mock.ExpectQuery(`FROM metric_rollup_1d`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("rollup read failed"))
	// no upsert
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("swallowed, got %v", err)
	}
}

func TestFetchFailed_UpsertError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	src := &fakeSource{key: "openai", err: errors.New("401")}
	j := makeJob(mock, fakeRegistry{"openai": src}, day(2026, 7, 19), 1, 2)

	expectProviders(mock, [2]string{"prov1", "openai"})
	expectOurBilled(mock, "prov1", 5)
	mock.ExpectExec(`INSERT INTO vendor_bill_reconciliation`).WillReturnError(errors.New("write failed"))
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("swallowed, got %v", err)
	}
}

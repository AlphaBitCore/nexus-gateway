// Pgxmock-driven Run() coverage for QuotaAlertCheckJob. Covers the
// empty-DB happy path, the load-error arms, and resolveStale's three
// branches (target-removed / pct-below-floor / still-firing).

package quota

import (
	"context"
	"errors"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

func TestQuotaAlertCheck_Run_EmptyDB(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// ListActiveQuotaOverrides → 0 rows.
	mock.ExpectQuery(`FROM "QuotaOverride"`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "targetType", "targetId", "costLimitUsd"}))
	// ListEnabledQuotaPolicies → 0 rows.
	mock.ExpectQuery(`FROM "QuotaPolicy"`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "scope", "organizationId", "costLimitUsd", "alertThresholds"}))
	// resolveStale SELECT → no firing alerts.
	mock.ExpectQuery(`SELECT "targetKey"\s+FROM "Alert"`).WithArgs(quotaThresholdRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}))

	j := &QuotaAlertCheckJob{pool: mock, raiser: &fakeRaiser{}, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock: %v", err)
	}
}

func TestQuotaAlertCheck_Run_OverridesListError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("overrides boom")
	mock.ExpectQuery(`FROM "QuotaOverride"`).WillReturnError(sentinel)

	j := &QuotaAlertCheckJob{pool: mock, raiser: &fakeRaiser{}, logger: testLogger()}
	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestQuotaAlertCheck_Run_PoliciesListError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`FROM "QuotaOverride"`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "targetType", "targetId", "costLimitUsd"}))
	sentinel := errors.New("policies boom")
	mock.ExpectQuery(`FROM "QuotaPolicy"`).WillReturnError(sentinel)

	j := &QuotaAlertCheckJob{pool: mock, raiser: &fakeRaiser{}, logger: testLogger()}
	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestQuotaAlertCheck_ResolveStale_TargetRemovedAndPctFloor(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`SELECT "targetKey"`).WithArgs(quotaThresholdRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}).
			AddRow("not-evaluated-key").      // target-removed branch
			AddRow("evaluated-low-pct").      // floor branch
			AddRow("evaluated-still-firing")) // still-firing branch

	raiser := &fakeRaiser{}
	j := &QuotaAlertCheckJob{pool: mock, raiser: raiser, logger: testLogger()}

	evaluated := map[string]thresholdTarget{
		"evaluated-low-pct":      {minThreshold: 80, currentPct: 40}, // below floor (80 - hysteresisPoints)
		"evaluated-still-firing": {minThreshold: 80, currentPct: 90}, // above floor
	}
	if err := j.resolveStale(context.Background(), evaluated); err != nil {
		t.Fatalf("resolveStale: %v", err)
	}
	if len(raiser.resolves) != 2 {
		t.Errorf("resolves = %d, want 2 (target-removed + low-pct)", len(raiser.resolves))
	}
}

func TestQuotaAlertCheck_ResolveStale_QueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("stale query boom")
	mock.ExpectQuery(`SELECT "targetKey"`).WithArgs(quotaThresholdRuleID).WillReturnError(sentinel)

	j := &QuotaAlertCheckJob{pool: mock, raiser: &fakeRaiser{}, logger: testLogger()}
	if err := j.resolveStale(context.Background(), nil); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

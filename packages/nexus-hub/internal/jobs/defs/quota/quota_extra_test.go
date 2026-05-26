// Extra tests for jobs/defs/quota package covering loadRollupCosts,
// raiseForThresholds error branches (raise err accumulates), resolveStale
// warn branches, and the full Run() path with override cost limits.

package quota

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
)

// raiseForThresholds — Raise error is accumulated (not returned)

// TestRaiseForThresholds_RaiseErrorAccumulates checks that when Raise fails
// for a threshold, the error is appended to *errs and the count is not
// incremented, but processing continues for remaining thresholds.
func TestRaiseForThresholds_RaiseErrorAccumulates(t *testing.T) {
	sentinel := errors.New("raise boom")
	j := &QuotaAlertCheckJob{
		raiser: &raiseErrFakeRaiser{err: sentinel},
		logger: testLogger(),
	}
	var errs []error
	count := j.raiseForThresholds(context.Background(), raiseContext{
		targetKey:    "k",
		targetLabel:  "user:u1",
		thresholds:   []int{80, 95},
		pct:          96, // crosses both 80 and 95
		costLimitUsd: 100,
	}, &errs)
	if count != 0 {
		t.Errorf("count = %d, want 0 when all raises fail", count)
	}
	if len(errs) != 2 {
		t.Errorf("errs count = %d, want 2 (one per threshold)", len(errs))
	}
	for _, err := range errs {
		if !errors.Is(err, sentinel) {
			t.Errorf("err not wrapping sentinel: %v", err)
		}
	}
}

// raiseErrFakeRaiser always returns a configured error from Raise.
type raiseErrFakeRaiser struct {
	err error
}

func (r *raiseErrFakeRaiser) Raise(_ context.Context, _ alerting.RaiseInput) error {
	return r.err
}

func (r *raiseErrFakeRaiser) Resolve(_ context.Context, _, _, _ string) error {
	return nil
}

// resolveStale — Resolve warn paths (target-removed + auto)

func TestResolveStale_ResolveErrorLogsAndContinues(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// One target-removed key, one that will trigger the auto-resolve path.
	mock.ExpectQuery(`SELECT "targetKey"`).WithArgs(quotaThresholdRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}).
			AddRow("not-seen-key").
			AddRow("low-pct-key"))

	sentinel := errors.New("resolve err")
	raiser := &resolveErrFakeRaiser{err: sentinel}
	j := &QuotaAlertCheckJob{pool: mock, raiser: raiser, logger: testLogger()}

	evaluated := map[string]thresholdTarget{
		"low-pct-key": {minThreshold: 80, currentPct: 10}, // below floor → auto-resolve
	}
	// Resolve errors should NOT surface from resolveStale (they are just warned).
	if err := j.resolveStale(context.Background(), evaluated); err != nil {
		t.Fatalf("resolveStale must not return resolve warn errors; got %v", err)
	}
	// Two resolve calls should have been attempted.
	if raiser.calls != 2 {
		t.Errorf("resolve calls = %d, want 2", raiser.calls)
	}
}

type resolveErrFakeRaiser struct {
	err   error
	calls int
}

func (r *resolveErrFakeRaiser) Raise(_ context.Context, _ alerting.RaiseInput) error { return nil }
func (r *resolveErrFakeRaiser) Resolve(_ context.Context, _, _, _ string) error {
	r.calls++
	return r.err
}

// resolveStale — rows.Err path

func TestResolveStale_RowsErrPropagates(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("rows err")
	mock.ExpectQuery(`SELECT "targetKey"`).WithArgs(quotaThresholdRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}).
			CloseError(sentinel))

	j := &QuotaAlertCheckJob{pool: mock, raiser: &fakeRaiser{}, logger: testLogger()}
	if err := j.resolveStale(context.Background(), nil); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

// loadRollupCosts — cache hit + cache miss + query error paths

func TestLoadRollupCosts_CacheHit(t *testing.T) {
	// When the dim is already in the cache, no DB call is made.
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// No expectations — any DB call would cause mock to fail.

	j := &QuotaAlertCheckJob{pool: mock, logger: testLogger()}
	cache := map[string]map[string]float64{
		"user": {"u1": 50.0, "u2": 20.0},
	}
	start := time.Now().UTC().Add(-24 * time.Hour)
	end := time.Now().UTC()
	costs, err := j.loadRollupCosts(context.Background(), cache, "user", start, end)
	if err != nil {
		t.Fatalf("loadRollupCosts cache hit: %v", err)
	}
	if costs["u1"] != 50.0 {
		t.Errorf("u1 cost = %v, want 50.0", costs["u1"])
	}
}

func TestLoadRollupCosts_QueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("rollup query err")
	// The rollup query hits the 5m table for a ~24h range.
	// args: start, end, "billed_cost_usd", "user=%"
	start := time.Now().UTC().Add(-24 * time.Hour)
	end := time.Now().UTC()
	mock.ExpectQuery(`FROM "metric_rollup_`).
		WithArgs(start, end, "billed_cost_usd", "user=%").
		WillReturnError(sentinel)

	j := &QuotaAlertCheckJob{pool: mock, logger: testLogger()}
	cache := make(map[string]map[string]float64)
	_, err := j.loadRollupCosts(context.Background(), cache, "user", start, end)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestLoadRollupCosts_HappyPath_AggregatesEntityCosts(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	now := time.Now().UTC()
	start := now.Add(-24 * time.Hour)
	end := now

	// QueryRollup returns rows with dimensionKey = "user=<entityID>" and value.
	// Two rows for the same entity (u1) should be summed.
	// args: start, end, "billed_cost_usd", "user=%"
	mock.ExpectQuery(`FROM "metric_rollup_`).
		WithArgs(start, end, "billed_cost_usd", "user=%").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "bucketStart", "metricName", "dimensionKey", "subDimension",
			"value", "metadata", "updatedAt",
		}).
			AddRow("r1", start, "billed_cost_usd", "user=u1", "", 30.0, []byte("{}"), now).
			AddRow("r2", start, "billed_cost_usd", "user=u1", "", 20.0, []byte("{}"), now).
			AddRow("r3", start, "billed_cost_usd", "user=u2", "", 10.0, []byte("{}"), now).
			AddRow("r4", start, "billed_cost_usd", "vk=vk1", "", 5.0, []byte("{}"), now)) // wrong prefix, skipped

	j := &QuotaAlertCheckJob{pool: mock, logger: testLogger()}
	cache := make(map[string]map[string]float64)
	costs, err := j.loadRollupCosts(context.Background(), cache, "user", start, end)
	if err != nil {
		t.Fatalf("loadRollupCosts: %v", err)
	}
	if costs["u1"] != 50.0 {
		t.Errorf("u1 = %v, want 50.0 (two rows summed)", costs["u1"])
	}
	if costs["u2"] != 10.0 {
		t.Errorf("u2 = %v, want 10.0", costs["u2"])
	}
	if _, ok := costs["vk1"]; ok {
		t.Errorf("vk1 should not appear (wrong prefix for 'user' dimension)")
	}
	// Should be cached now.
	if _, ok := cache["user"]; !ok {
		t.Error("costs not cached after successful query")
	}
}

// Run() — full path with override having cost limit (exercises loadRollupCosts)

func TestRun_WithOverrideCostLimit_RaisesAlert(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	costLimit := 100.0
	now := time.Now().UTC()
	periodStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	// ListActiveQuotaOverrides → 1 override for user u1, costLimit=$100.
	mock.ExpectQuery(`FROM "QuotaOverride"`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "targetType", "targetId", "costLimitUsd"}).
			AddRow("override-1", "user", "u1", &costLimit))
	// ListEnabledQuotaPolicies → 0 rows.
	mock.ExpectQuery(`FROM "QuotaPolicy"`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "scope", "organizationId", "costLimitUsd", "alertThresholds"}))

	// loadRollupCosts → QueryRollup for "user" dim; use AnyArg for time values.
	mock.ExpectQuery(`FROM "metric_rollup_`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), "billed_cost_usd", "user=%").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "bucketStart", "metricName", "dimensionKey", "subDimension",
			"value", "metadata", "updatedAt",
		}).AddRow("r1", periodStart, "billed_cost_usd", "user=u1", "", 90.0, []byte("{}"), now))

	// resolveStale → SELECT firing alerts → empty.
	mock.ExpectQuery(`SELECT "targetKey"\s+FROM "Alert"`).WithArgs(quotaThresholdRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}))

	raiser := &fakeRaiser{}
	j := &QuotaAlertCheckJob{pool: mock, raiser: raiser, logger: testLogger()}

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 90% crosses the 80% threshold → one alert raised.
	if raiser.raiseCount() != 1 {
		t.Errorf("raises = %d, want 1 (crossed 80%% threshold)", raiser.raiseCount())
	}
	if raiser.raises[0].RuleID != quotaThresholdRuleID {
		t.Errorf("ruleID = %q, want %q", raiser.raises[0].RuleID, quotaThresholdRuleID)
	}
}

func TestRun_WithPolicyCostLimit_RaisesAlert(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	costLimit := 50.0
	thresholds, _ := json.Marshal([]int{80})
	now := time.Now().UTC()
	periodStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	// ListActiveQuotaOverrides → empty (no overrides for u2).
	mock.ExpectQuery(`FROM "QuotaOverride"`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "targetType", "targetId", "costLimitUsd"}))
	// ListEnabledQuotaPolicies → 1 policy for "user" scope.
	mock.ExpectQuery(`FROM "QuotaPolicy"`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "scope", "organizationId", "costLimitUsd", "alertThresholds"}).
			AddRow("policy-1", "user", (*string)(nil), &costLimit, json.RawMessage(thresholds)))

	// loadRollupCosts for "user" dim: u2 has $45 (90% of $50).
	mock.ExpectQuery(`FROM "metric_rollup_`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), "billed_cost_usd", "user=%").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "bucketStart", "metricName", "dimensionKey", "subDimension",
			"value", "metadata", "updatedAt",
		}).AddRow("r2", periodStart, "billed_cost_usd", "user=u2", "", 45.0, []byte("{}"), now))

	// resolveStale SELECT → empty.
	mock.ExpectQuery(`SELECT "targetKey"\s+FROM "Alert"`).WithArgs(quotaThresholdRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}))

	raiser := &fakeRaiser{}
	j := &QuotaAlertCheckJob{pool: mock, raiser: raiser, logger: testLogger()}

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 90% crosses 80% → one alert.
	if raiser.raiseCount() != 1 {
		t.Errorf("raises = %d, want 1", raiser.raiseCount())
	}
}

func TestRun_OverrideWithNilCostLimit_SkippedNoAlert(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// Override has nil cost limit → should be skipped.
	mock.ExpectQuery(`FROM "QuotaOverride"`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "targetType", "targetId", "costLimitUsd"}).
			AddRow("override-1", "user", "u1", (*float64)(nil)))
	// Policy has nil cost limit → also skipped.
	mock.ExpectQuery(`FROM "QuotaPolicy"`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "scope", "organizationId", "costLimitUsd", "alertThresholds"}).
			AddRow("policy-1", "user", (*string)(nil), (*float64)(nil), nil))
	// resolveStale → empty.
	mock.ExpectQuery(`SELECT "targetKey"\s+FROM "Alert"`).WithArgs(quotaThresholdRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}))

	raiser := &fakeRaiser{}
	j := &QuotaAlertCheckJob{pool: mock, raiser: raiser, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if raiser.raiseCount() != 0 {
		t.Errorf("raises = %d, want 0 (nil cost limits skipped)", raiser.raiseCount())
	}
}

func TestRun_OverrideWithUnknownScope_Skipped(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	costLimit := 100.0
	// Override has "unknown-scope" → scopeToDimension returns !ok → skipped.
	mock.ExpectQuery(`FROM "QuotaOverride"`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "targetType", "targetId", "costLimitUsd"}).
			AddRow("o1", "unknown-scope", "x", &costLimit))
	mock.ExpectQuery(`FROM "QuotaPolicy"`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "scope", "organizationId", "costLimitUsd", "alertThresholds"}))
	mock.ExpectQuery(`SELECT "targetKey"\s+FROM "Alert"`).WithArgs(quotaThresholdRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}))

	raiser := &fakeRaiser{}
	j := &QuotaAlertCheckJob{pool: mock, raiser: raiser, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if raiser.raiseCount() != 0 {
		t.Errorf("raises = %d, want 0 (unknown scope skipped)", raiser.raiseCount())
	}
}

func TestRun_RollupQueryError_AccumulatesInErrs(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	costLimit := 100.0
	mock.ExpectQuery(`FROM "QuotaOverride"`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "targetType", "targetId", "costLimitUsd"}).
			AddRow("o1", "user", "u1", &costLimit))
	mock.ExpectQuery(`FROM "QuotaPolicy"`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "scope", "organizationId", "costLimitUsd", "alertThresholds"}))

	sentinel := errors.New("rollup err")
	mock.ExpectQuery(`FROM "metric_rollup_`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), "billed_cost_usd", "user=%").
		WillReturnError(sentinel)

	// resolveStale → empty.
	mock.ExpectQuery(`SELECT "targetKey"\s+FROM "Alert"`).WithArgs(quotaThresholdRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}))

	j := &QuotaAlertCheckJob{pool: mock, raiser: &fakeRaiser{}, logger: testLogger()}
	err := j.Run(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel (rollup error accumulated and joined)", err)
	}
}

func TestRun_PolicyWithOrgFilter_MatchAndSkip(t *testing.T) {
	// Covers the orgFilter != "" && dim == "organization" branch:
	// entity org-a matches, org-b is filtered out.
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	costLimit := 100.0
	orgID := "org-a"
	thresholds, _ := json.Marshal([]int{80})
	now := time.Now().UTC()
	periodStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`FROM "QuotaOverride"`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "targetType", "targetId", "costLimitUsd"}))
	mock.ExpectQuery(`FROM "QuotaPolicy"`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "scope", "organizationId", "costLimitUsd", "alertThresholds"}).
			AddRow("p1", "organization", &orgID, &costLimit, json.RawMessage(thresholds)))

	// Rollup returns org-a at 90% and org-b at 90%, but org-b should be filtered.
	mock.ExpectQuery(`FROM "metric_rollup_`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), "billed_cost_usd", "organization=%").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "bucketStart", "metricName", "dimensionKey", "subDimension",
			"value", "metadata", "updatedAt",
		}).
			AddRow("r1", periodStart, "billed_cost_usd", "organization=org-a", "", 90.0, []byte("{}"), now).
			AddRow("r2", periodStart, "billed_cost_usd", "organization=org-b", "", 90.0, []byte("{}"), now))

	mock.ExpectQuery(`SELECT "targetKey"\s+FROM "Alert"`).WithArgs(quotaThresholdRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}))

	raiser := &fakeRaiser{}
	j := &QuotaAlertCheckJob{pool: mock, raiser: raiser, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Only org-a should have triggered an alert (org-b filtered).
	if raiser.raiseCount() != 1 {
		t.Errorf("raises = %d, want 1 (only org-a)", raiser.raiseCount())
	}
}

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

// TestLoadRollupCosts_TrailingWindowOnly covers the branch where the whole
// requested window sits inside the last two hours (start newer than the
// previous full hour): the 1h base read is skipped and only the 5m tail read
// fires. The end is set in the future to also exercise the clamp-to-now (F-0164).
func TestLoadRollupCosts_TrailingWindowOnly(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	now := time.Now().UTC()
	start := now.Add(-30 * time.Minute)
	end := now.Add(time.Hour) // future → clamped to now

	// Exactly one query (the 5m tail). A second expectation would fail if the
	// base read were not skipped.
	mock.ExpectQuery(`FROM "metric_rollup_`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), "billed_cost_usd", "user=%").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "bucketStart", "metricName", "dimensionKey", "subDimension",
			"value", "metadata", "updatedAt",
		}).AddRow("r1", start, "billed_cost_usd", "user=u1", "", 12.0, []byte("{}"), now))

	j := &QuotaAlertCheckJob{pool: mock, logger: testLogger()}
	costs, err := j.loadRollupCosts(context.Background(), map[string]map[string]float64{}, "user", start, end)
	if err != nil {
		t.Fatalf("loadRollupCosts: %v", err)
	}
	if costs["u1"] != 12.0 {
		t.Errorf("u1 = %v, want 12.0", costs["u1"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("base read must be skipped for a trailing-only window: %v", err)
	}
}

// TestLoadRollupCosts_PastWindowBaseOnly covers the branch where the window
// ends more than an hour in the past, so base1hEnd clamps to effectiveEnd and
// only the base read fires (no trailing 5m top-up).
func TestLoadRollupCosts_PastWindowBaseOnly(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	now := time.Now().UTC()
	start := now.Add(-5 * time.Hour)
	end := now.Add(-3 * time.Hour)

	mock.ExpectQuery(`FROM "metric_rollup_`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), "billed_cost_usd", "user=%").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "bucketStart", "metricName", "dimensionKey", "subDimension",
			"value", "metadata", "updatedAt",
		}).AddRow("r1", start, "billed_cost_usd", "user=u1", "", 8.0, []byte("{}"), now))

	j := &QuotaAlertCheckJob{pool: mock, logger: testLogger()}
	costs, err := j.loadRollupCosts(context.Background(), map[string]map[string]float64{}, "user", start, end)
	if err != nil {
		t.Fatalf("loadRollupCosts: %v", err)
	}
	if costs["u1"] != 8.0 {
		t.Errorf("u1 = %v, want 8.0", costs["u1"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("only the base read should fire for a fully-past window: %v", err)
	}
}

func TestLoadRollupCosts_QueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("rollup query err")
	// loadRollupCosts now splits into a 1h base read + a 5m tail read (F-0164);
	// the base read fires first, so its error short-circuits before the tail.
	// Times are runtime-derived (base end = prev full hour), so use AnyArg.
	start := time.Now().UTC().Add(-24 * time.Hour)
	end := time.Now().UTC()
	mock.ExpectQuery(`FROM "metric_rollup_`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), "billed_cost_usd", "user=%").
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

	mergeCols := []string{
		"id", "bucketStart", "metricName", "dimensionKey", "subDimension",
		"value", "metadata", "updatedAt",
	}
	// F-0164: loadRollupCosts reads the stable base from the 1h tier and tops up
	// the trailing window from the 5m tier. The two reads are additive — a
	// dimension's spend split across base and tail must sum. u1 = 25 (base) +
	// 25 (tail) = 50; u2 = 10 (base only). The wrong-prefix vk row is skipped.
	mock.ExpectQuery(`FROM "metric_rollup_`). // base (1h tier)
							WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), "billed_cost_usd", "user=%").
							WillReturnRows(pgxmock.NewRows(mergeCols).
								AddRow("r1", start, "billed_cost_usd", "user=u1", "", 25.0, []byte("{}"), now).
								AddRow("r3", start, "billed_cost_usd", "user=u2", "", 10.0, []byte("{}"), now).
								AddRow("r4", start, "billed_cost_usd", "vk=vk1", "", 5.0, []byte("{}"), now)) // wrong prefix, skipped
	mock.ExpectQuery(`FROM "metric_rollup_`). // tail (5m tier)
							WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), "billed_cost_usd", "user=%").
							WillReturnRows(pgxmock.NewRows(mergeCols).
								AddRow("r2", now, "billed_cost_usd", "user=u1", "", 25.0, []byte("{}"), now))

	j := &QuotaAlertCheckJob{pool: mock, logger: testLogger()}
	cache := make(map[string]map[string]float64)
	costs, err := j.loadRollupCosts(context.Background(), cache, "user", start, end)
	if err != nil {
		t.Fatalf("loadRollupCosts: %v", err)
	}
	if costs["u1"] != 50.0 {
		t.Errorf("u1 = %v, want 50.0 (base 25 + tail 25 stitched across tiers)", costs["u1"])
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

	// loadRollupCosts → 1h base read (the spend) + 5m tail read (empty) per the
	// F-0164 two-tier stitch; use AnyArg for time values.
	mock.ExpectQuery(`FROM "metric_rollup_`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), "billed_cost_usd", "user=%").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "bucketStart", "metricName", "dimensionKey", "subDimension",
			"value", "metadata", "updatedAt",
		}).AddRow("r1", periodStart, "billed_cost_usd", "user=u1", "", 90.0, []byte("{}"), now))
	mock.ExpectQuery(`FROM "metric_rollup_`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), "billed_cost_usd", "user=%").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "bucketStart", "metricName", "dimensionKey", "subDimension",
			"value", "metadata", "updatedAt",
		}))

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

	// loadRollupCosts for "user" dim: u2 has $45 (90% of $50). 1h base + 5m tail (empty).
	mock.ExpectQuery(`FROM "metric_rollup_`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), "billed_cost_usd", "user=%").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "bucketStart", "metricName", "dimensionKey", "subDimension",
			"value", "metadata", "updatedAt",
		}).AddRow("r2", periodStart, "billed_cost_usd", "user=u2", "", 45.0, []byte("{}"), now))
	mock.ExpectQuery(`FROM "metric_rollup_`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), "billed_cost_usd", "user=%").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "bucketStart", "metricName", "dimensionKey", "subDimension",
			"value", "metadata", "updatedAt",
		}))

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

// TestRun_ProjectOverride_ReadsProjectDimension is the F-0150 regression
// guard for Phase A. A project-scoped override must read spend from the
// `project=<uuid>` rollup dimension. Before the fix, scopeToDimension mapped
// project→organization, so costs[o.TargetID] looked a project UUID up in the
// org-keyed cost map → 0 → the threshold was never crossed and no alert fired.
func TestRun_ProjectOverride_ReadsProjectDimension(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	costLimit := 100.0
	projID := "proj-abc"
	now := time.Now().UTC()
	periodStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	// One project-scoped override, costLimit=$100.
	mock.ExpectQuery(`FROM "QuotaOverride"`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "targetType", "targetId", "costLimitUsd"}).
			AddRow("override-proj", "project", projID, &costLimit))
	mock.ExpectQuery(`FROM "QuotaPolicy"`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "scope", "organizationId", "costLimitUsd", "alertThresholds"}))

	// The rollup must be queried for the "project" dimension (prefix
	// "project=%"). Project proj-abc has spent $90 (90% of $100). An
	// organization= row carrying unrelated org-wide spend must be ignored —
	// it would never be returned by a "project=%" query in production, but
	// asserting the WithArgs prefix here proves we read the project dimension.
	mock.ExpectQuery(`FROM "metric_rollup_`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), "billed_cost_usd", "project=%").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "bucketStart", "metricName", "dimensionKey", "subDimension",
			"value", "metadata", "updatedAt",
		}).AddRow("r1", periodStart, "billed_cost_usd", "project="+projID, "", 90.0, []byte("{}"), now))
	mock.ExpectQuery(`FROM "metric_rollup_`). // 5m tail (empty)
							WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), "billed_cost_usd", "project=%").
							WillReturnRows(pgxmock.NewRows([]string{
			"id", "bucketStart", "metricName", "dimensionKey", "subDimension",
			"value", "metadata", "updatedAt",
		}))

	mock.ExpectQuery(`SELECT "targetKey"\s+FROM "Alert"`).WithArgs(quotaThresholdRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}))

	raiser := &fakeRaiser{}
	j := &QuotaAlertCheckJob{pool: mock, raiser: raiser, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if raiser.raiseCount() != 1 {
		t.Fatalf("raises = %d, want 1 (project spend $90 crosses 80%%)", raiser.raiseCount())
	}
	r := raiser.raises[0]
	if r.Details["targetType"] != "project" {
		t.Errorf("targetType = %v, want project", r.Details["targetType"])
	}
	if r.Details["targetId"] != projID {
		t.Errorf("targetId = %v, want %q", r.Details["targetId"], projID)
	}
	if r.Details["currentCostUsd"].(float64) != 90.0 {
		t.Errorf("currentCostUsd = %v, want 90.0 (the project's own spend)", r.Details["currentCostUsd"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}

// TestRun_ProjectPolicy_ComparesProjectSpendToProjectLimit is the F-0150
// regression guard for Phase B. A project-scoped policy must compare each
// project's own spend (from the project= dimension) against the project
// limit, and the resulting alert's targetType must be "project" (via
// dimensionToTargetType). Before the fix the policy fired org-keyed alerts
// comparing org-wide spend to the project limit.
func TestRun_ProjectPolicy_ComparesProjectSpendToProjectLimit(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	costLimit := 200.0
	thresholds, _ := json.Marshal([]int{80})
	now := time.Now().UTC()
	periodStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`FROM "QuotaOverride"`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "targetType", "targetId", "costLimitUsd"}))
	// Project-scoped policy, $200 limit, no org filter.
	mock.ExpectQuery(`FROM "QuotaPolicy"`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "scope", "organizationId", "costLimitUsd", "alertThresholds"}).
			AddRow("policy-proj", "project", (*string)(nil), &costLimit, json.RawMessage(thresholds)))

	// Two projects: p-hot at $180 (90% of $200 → crosses 80%), p-cold at
	// $20 (10% → no alert). Read from the project= dimension.
	mock.ExpectQuery(`FROM "metric_rollup_`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), "billed_cost_usd", "project=%").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "bucketStart", "metricName", "dimensionKey", "subDimension",
			"value", "metadata", "updatedAt",
		}).
			AddRow("r1", periodStart, "billed_cost_usd", "project=p-hot", "", 180.0, []byte("{}"), now).
			AddRow("r2", periodStart, "billed_cost_usd", "project=p-cold", "", 20.0, []byte("{}"), now))
	mock.ExpectQuery(`FROM "metric_rollup_`). // 5m tail (empty)
							WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), "billed_cost_usd", "project=%").
							WillReturnRows(pgxmock.NewRows([]string{
			"id", "bucketStart", "metricName", "dimensionKey", "subDimension",
			"value", "metadata", "updatedAt",
		}))

	mock.ExpectQuery(`SELECT "targetKey"\s+FROM "Alert"`).WithArgs(quotaThresholdRuleID).
		WillReturnRows(pgxmock.NewRows([]string{"targetKey"}))

	raiser := &fakeRaiser{}
	j := &QuotaAlertCheckJob{pool: mock, raiser: raiser, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if raiser.raiseCount() != 1 {
		t.Fatalf("raises = %d, want 1 (only p-hot crosses 80%%)", raiser.raiseCount())
	}
	r := raiser.raises[0]
	if r.Details["targetType"] != "project" {
		t.Errorf("targetType = %v, want project (via dimensionToTargetType)", r.Details["targetType"])
	}
	if r.Details["targetId"] != "p-hot" {
		t.Errorf("targetId = %v, want p-hot", r.Details["targetId"])
	}
	if r.Details["currentCostUsd"].(float64) != 180.0 {
		t.Errorf("currentCostUsd = %v, want 180.0 (p-hot's own project spend)", r.Details["currentCostUsd"])
	}
	if r.TargetKey != policyTargetKey("policy-proj", "project", "p-hot", now.Format("2006-01")) {
		t.Errorf("targetKey = %q, want project-keyed policy key", r.TargetKey)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
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
	mock.ExpectQuery(`FROM "metric_rollup_`). // 5m tail (empty)
							WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), "billed_cost_usd", "organization=%").
							WillReturnRows(pgxmock.NewRows([]string{
			"id", "bucketStart", "metricName", "dimensionKey", "subDimension",
			"value", "metadata", "updatedAt",
		}))

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

package quota

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
)

// newTestMetrics returns a *Metrics registered against a fresh registry so each
// test sees isolated counters and repeated construction never panics.
func newTestMetrics(t *testing.T) *Metrics {
	t.Helper()
	return NewMetrics("nexustest", prometheus.NewRegistry())
}

// TestEngine_Reconcile_IncrMultiFailure_EmitsCounter verifies F-0155/F-0176:
// when the post-success Reconcile cannot persist the increment (Redis down),
// the reconcile-failed counter is emitted so the otherwise-silent counter drift
// is alertable.
func TestEngine_Reconcile_IncrMultiFailure_EmitsCounter(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	mr.Close() // every Redis op now errors -> IncrMulti fails (and retry fails)

	metrics := newTestMetrics(t)
	engine := NewEngine(NewPolicyCache(nil, testLogger()), NewUsageCache(rdb, testLogger()), testLogger(), metrics)

	decision := &Decision{
		PeriodKey: CurrentPeriodKey("monthly"),
		Levels:    []CheckLevel{{TargetType: "virtual_key", TargetID: "vk-1"}},
	}
	// A whole-cent cost so a bucket forms and IncrMulti is actually attempted.
	engine.Reconcile(context.Background(), decision, ActualUsage{CostUSD: 0.05})

	if got := testutil.ToFloat64(metrics.reconcileFailedTotal); got != 1 {
		t.Errorf("quota_reconcile_failed_total: got %v, want 1", got)
	}
}

// TestEngine_Reconcile_Success_NoFailureCounter verifies the counter stays at 0
// when the increment lands — the metric only fires on real loss.
func TestEngine_Reconcile_Success_NoFailureCounter(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	metrics := newTestMetrics(t)
	engine := NewEngine(NewPolicyCache(nil, testLogger()), NewUsageCache(rdb, testLogger()), testLogger(), metrics)

	decision := &Decision{
		PeriodKey: CurrentPeriodKey("monthly"),
		Levels:    []CheckLevel{{TargetType: "virtual_key", TargetID: "vk-1"}},
	}
	engine.Reconcile(context.Background(), decision, ActualUsage{CostUSD: 0.05})

	if got := testutil.ToFloat64(metrics.reconcileFailedTotal); got != 0 {
		t.Errorf("reconcile-failed counter fired on success: got %v, want 0", got)
	}
	// And the counter actually advanced (5 cents).
	usage, _ := engine.UsageForTarget(context.Background(), "virtual_key", "vk-1", decision.PeriodKey)
	if usage != 5 {
		t.Errorf("usage after reconcile: got %d, want 5", usage)
	}
}

// TestEngine_Check_GetUsageError_EmitsFailOpenCounter verifies F-0156/F-0176:
// when the usage-cache read fails, Check fails open (allows) AND emits
// quota_check_failopen_total{reason="redis_error"} so the unmetered window is
// alertable instead of silent.
func TestEngine_Check_GetUsageError_EmitsFailOpenCounter(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	mr.Close() // GetUsage GET now errors

	policyCache := NewPolicyCache(nil, testLogger())
	policyCache.policiesByScope["virtual_key"] = []CachedPolicy{
		{ID: "p-1", Scope: "virtual_key", PeriodType: "monthly", CostLimitCents: 100, EnforcementMode: "reject", Priority: 100},
	}
	metrics := newTestMetrics(t)
	engine := NewEngine(policyCache, NewUsageCache(rdb, testLogger()), testLogger(), metrics)

	chain := []CheckLevel{{TargetType: "virtual_key", TargetID: "vk-1"}}
	decision := engine.Check(context.Background(), chain, CostEstimate{}, &vkauth.VKMeta{ID: "vk-1", OrganizationID: "org"})

	if !decision.Allowed || decision.Action != "allow" {
		t.Errorf("fail-open must allow: %+v", decision)
	}
	if got := testutil.ToFloat64(metrics.checkFailOpenTotal.WithLabelValues("redis_error")); got != 1 {
		t.Errorf("quota_check_failopen_total{reason=redis_error}: got %v, want 1", got)
	}
}

// TestEngine_Check_NilVKMeta_AllowsWithoutPanic verifies F-0171: Check with a
// nil vkMeta must not panic (it dereferences vkMeta.OrganizationID downstream)
// and returns Allow, mirroring VKLimit's nil guard. Also asserts the decision
// counter records the allow.
func TestEngine_Check_NilVKMeta_AllowsWithoutPanic(t *testing.T) {
	metrics := newTestMetrics(t)
	engine := NewEngine(NewPolicyCache(nil, testLogger()), NewUsageCache(nil, testLogger()), testLogger(), metrics)

	chain := []CheckLevel{{TargetType: "virtual_key", TargetID: "vk-1"}}
	decision := engine.Check(context.Background(), chain, CostEstimate{}, nil)

	if decision == nil || !decision.Allowed || decision.Action != "allow" {
		t.Fatalf("nil vkMeta must return Allow without panic; got %+v", decision)
	}
	if got := testutil.ToFloat64(metrics.decisionTotal.WithLabelValues("allow")); got != 1 {
		t.Errorf("quota_decision_total{action=allow}: got %v, want 1", got)
	}
}

// TestEngine_Check_DecisionCounter_RecordsReject verifies F-0176: a reject
// decision increments quota_decision_total{action="reject"}.
func TestEngine_Check_DecisionCounter_RecordsReject(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	policyCache := NewPolicyCache(nil, testLogger())
	policyCache.policiesByScope["virtual_key"] = []CachedPolicy{
		{ID: "p-1", Scope: "virtual_key", PeriodType: "monthly", CostLimitCents: 10, EnforcementMode: "reject", Priority: 100},
	}
	usageCache := NewUsageCache(rdb, testLogger())
	periodKey := CurrentPeriodKey("monthly")
	// Pre-seed usage at the limit so the estimate pushes it over.
	if err := usageCache.IncrMulti(context.Background(),
		[]UsageLevel{{TargetType: "virtual_key", TargetID: "vk-1"}}, periodKey, 10); err != nil {
		t.Fatalf("seed: %v", err)
	}

	metrics := newTestMetrics(t)
	engine := NewEngine(policyCache, usageCache, testLogger(), metrics)

	chain := []CheckLevel{{TargetType: "virtual_key", TargetID: "vk-1"}}
	estimate := CostEstimate{EstimatedInputTokens: 1_000_000, InputPricePM: 1} // $1 -> 100 cents
	decision := engine.Check(context.Background(), chain, estimate, &vkauth.VKMeta{ID: "vk-1", OrganizationID: "org"})

	if decision.Action != "reject" || decision.Allowed {
		t.Fatalf("expected reject; got %+v", decision)
	}
	if got := testutil.ToFloat64(metrics.decisionTotal.WithLabelValues("reject")); got != 1 {
		t.Errorf("quota_decision_total{action=reject}: got %v, want 1", got)
	}
}

// TestEngine_NilMetrics_NoPanic verifies the nil-*Metrics path is safe: an
// Engine constructed without metrics (sibling-package tests) must run Check and
// Reconcile without panicking.
func TestEngine_NilMetrics_NoPanic(t *testing.T) {
	engine := NewEngine(NewPolicyCache(nil, testLogger()), NewUsageCache(nil, testLogger()), testLogger(), nil)
	d := engine.Check(context.Background(), []CheckLevel{{TargetType: "virtual_key", TargetID: "v"}}, CostEstimate{}, nil)
	if d == nil || !d.Allowed {
		t.Fatalf("nil-metrics Check: %+v", d)
	}
	engine.Reconcile(context.Background(), &Decision{
		PeriodKey: CurrentPeriodKey("monthly"),
		Levels:    []CheckLevel{{TargetType: "virtual_key", TargetID: "v"}},
	}, ActualUsage{CostUSD: 0.05})
}

// TestPolicyCache_CostLimitRounds verifies F-0167: $0.29 must convert to 29
// cents, not 28 — math.Round, not truncation. Exercises both the policy and the
// override conversion sites.
func TestPolicyCache_CostLimitRounds(t *testing.T) {
	mock := newMockPool(t)
	c := NewPolicyCacheWithPgxPool(mock, testLogger())

	mock.ExpectQuery(`FROM "QuotaPolicy"`).WillReturnRows(
		pgxmock.NewRows([]string{
			"id", "scope", "organizationId", "vkType", "periodType",
			"costLimitUsd", "enforcementMode", "priority",
		}).AddRow("p1", "virtual_key", (*string)(nil), (*string)(nil), "monthly", floatPtr(0.29), "reject", 100))

	mock.ExpectQuery(`FROM "QuotaOverride"`).WillReturnRows(
		pgxmock.NewRows([]string{
			"id", "targetType", "targetId", "costLimitUsd", "enforcementMode", "periodType", "expiresAt",
		}).AddRow("o1", "virtual_key", "vk-1", floatPtr(0.29), (*string)(nil), (*string)(nil), (*time.Time)(nil)))

	mock.ExpectQuery(`FROM "Organization"`).WillReturnRows(
		pgxmock.NewRows([]string{"id", "parentId"}))

	if err := c.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}

	if p := c.FindPolicy("virtual_key", "", ""); p == nil || p.CostLimitCents != 29 {
		t.Errorf("policy $0.29 -> %v cents, want 29 (rounded, not truncated to 28)", p)
	}
	if o := c.GetOverride("virtual_key", "vk-1"); o == nil || o.CostLimitCents != 29 {
		t.Errorf("override $0.29 -> %v cents, want 29", o)
	}
}

// TestPolicyCache_ActivePeriodTypes verifies the distinct period-type
// enumeration used to drive multi-period Backfill (F-0158), including the
// empty/unknown -> monthly normalization.
func TestPolicyCache_ActivePeriodTypes(t *testing.T) {
	c := NewPolicyCache(nil, testLogger())
	c.policiesByScope["virtual_key"] = []CachedPolicy{
		{ID: "p1", PeriodType: "daily"},
		{ID: "p2", PeriodType: "weekly"},
		{ID: "p3", PeriodType: ""}, // -> monthly
	}
	c.overridesByKey["user:u1"] = &CachedOverride{ID: "o1", PeriodType: "daily"} // dup daily
	c.overridesByKey["user:u2"] = &CachedOverride{ID: "o2", PeriodType: "junk"}  // -> monthly

	got := c.ActivePeriodTypes()
	want := map[string]bool{"daily": true, "weekly": true, "monthly": true}
	if len(got) != len(want) {
		t.Fatalf("ActivePeriodTypes: got %v, want keys %v", got, want)
	}
	for _, pt := range got {
		if !want[pt] {
			t.Errorf("unexpected period type %q in %v", pt, got)
		}
	}
}

// TestUsageCache_Backfill_SeedsDailyAndWeekly verifies F-0158: Backfill seeds
// the CURRENT daily and weekly period keys, not just the monthly one — so a
// restart re-hydrates non-monthly counters instead of resetting them to 0.
func TestUsageCache_Backfill_SeedsDailyAndWeekly(t *testing.T) {
	mr, _ := miniredis.Run()
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	c := NewUsageCache(rdb, testLogger())
	mock := newMockPool(t)

	// Two period types -> the SQL runs once per dimension per period type
	// (4 dims x 2 periods = 8 queries). Each query returns one user row only;
	// the other three dimensions return empty rows.
	for _, pt := range []string{"daily", "weekly"} {
		_ = pt
		mock.ExpectQuery(`FROM "metric_rollup_1h"`).
			WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), "user=%").
			WillReturnRows(pgxmock.NewRows([]string{"dimensionKey", "total_cost"}).
				AddRow("user=alice", 2.00))
		for _, dim := range []string{"virtual_key=%", "project=%", "organization=%"} {
			mock.ExpectQuery(`FROM "metric_rollup_1h"`).
				WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), dim).
				WillReturnRows(pgxmock.NewRows([]string{"dimensionKey", "total_cost"}))
		}
	}

	if err := c.backfillWithPgxPool(context.Background(), mock, []string{"daily", "weekly"}, testLogger()); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	now := time.Now().UTC()
	dailyKey, _, _ := periodWindow("daily", now)
	weeklyKey, _, _ := periodWindow("weekly", now)
	monthlyKey, _, _ := periodWindow("monthly", now)

	if got, _ := c.GetUsage(context.Background(), "user", "alice", dailyKey); got != 200 {
		t.Errorf("daily key not seeded: got %d cents, want 200", got)
	}
	if got, _ := c.GetUsage(context.Background(), "user", "alice", weeklyKey); got != 200 {
		t.Errorf("weekly key not seeded: got %d cents, want 200", got)
	}
	// Monthly was NOT requested, so it must remain unseeded (0).
	if got, _ := c.GetUsage(context.Background(), "user", "alice", monthlyKey); got != 0 {
		t.Errorf("monthly key seeded despite not being requested: got %d", got)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// TestUsageCache_Backfill_EmptyPeriodTypes_DefaultsMonthly verifies the
// fallback: no period types supplied seeds monthly (preserves pre-F-0158
// behaviour for a quota-less deployment).
func TestUsageCache_Backfill_EmptyPeriodTypes_DefaultsMonthly(t *testing.T) {
	mr, _ := miniredis.Run()
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	c := NewUsageCache(rdb, testLogger())
	mock := newMockPool(t)

	mock.ExpectQuery(`FROM "metric_rollup_1h"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), "user=%").
		WillReturnRows(pgxmock.NewRows([]string{"dimensionKey", "total_cost"}).
			AddRow("user=bob", 1.00))
	for _, dim := range []string{"virtual_key=%", "project=%", "organization=%"} {
		mock.ExpectQuery(`FROM "metric_rollup_1h"`).
			WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), dim).
			WillReturnRows(pgxmock.NewRows([]string{"dimensionKey", "total_cost"}))
	}

	if err := c.backfillWithPgxPool(context.Background(), mock, nil, testLogger()); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	monthlyKey, _, _ := periodWindow("monthly", time.Now().UTC())
	if got, _ := c.GetUsage(context.Background(), "user", "bob", monthlyKey); got != 100 {
		t.Errorf("monthly default not seeded: got %d, want 100", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// TestPeriodWindow_KeysMatchCurrentPeriodKey asserts periodWindow's key output
// matches CurrentPeriodKey so a backfilled key collides with the live-traffic
// key and SETNX correctly no-ops over existing data.
func TestPeriodWindow_KeysMatchCurrentPeriodKey(t *testing.T) {
	now := time.Now().UTC()
	for _, pt := range []string{"daily", "weekly", "monthly"} {
		key, start, end := periodWindow(pt, now)
		if key != CurrentPeriodKey(pt) {
			t.Errorf("%s: periodWindow key %q != CurrentPeriodKey %q", pt, key, CurrentPeriodKey(pt))
		}
		if !now.Before(end) || now.Before(start) {
			t.Errorf("%s: now %v not within [%v,%v)", pt, now, start, end)
		}
	}
}

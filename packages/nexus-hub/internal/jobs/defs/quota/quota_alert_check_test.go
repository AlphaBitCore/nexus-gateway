package quota

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
	quotastore "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/quota/store"
)

// fakeRaiser records Raise/Resolve calls without touching a database. Shared
// by quota_alert_check and vk_expiry tests.
type fakeRaiser struct {
	mu       sync.Mutex
	raises   []alerting.RaiseInput
	resolves []resolveCall
}

type resolveCall struct {
	RuleID    string
	TargetKey string
	Reason    string
}

func (f *fakeRaiser) Raise(_ context.Context, in alerting.RaiseInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.raises = append(f.raises, in)
	return nil
}

func (f *fakeRaiser) Resolve(_ context.Context, ruleID, targetKey, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolves = append(f.resolves, resolveCall{RuleID: ruleID, TargetKey: targetKey, Reason: reason})
	return nil
}

func (f *fakeRaiser) raiseCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.raises)
}

// jobsTestPool mirrors alerting_test.testPool. It is duplicated here rather
// than shared through a testutil package to keep the dependency graph flat
// (the brief explicitly rules out a new testutil package).
//
// Foreign-data safety: every cleanup below is scoped to the
// `test.job.*` rule-id prefix and `override:test-job-*` /
// `policy:test-job-*` / `vk:test-job-*` targetKey prefixes. The job's
// resolveStale path is only exercised against a `fakeRaiser` (no DB
// writes), and the real-raiser test inserts under the test-owned
// targetKey only — no foreign Alert / AlertRule rows are modified.
func jobsTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping DB-backed integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("skip: DB unavailable (%v)", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Skipf("skip: DB ping failed (%v)", err)
	}
	return pool
}

// requireQuotaThresholdRuleSeeded skips the calling test if the
// `quota.threshold` AlertRule row is missing from the local DB. The
// rule is seeded by `tools/db-migrate/seed/seed.ts`; tests that depend
// on it (anything calling alerting.Raiser against the production
// ruleID) must not silently create foreign data when the seed is
// missing — per the tests-only-own-data rule, tests only modify rows
// they themselves seeded.
func requireQuotaThresholdRuleSeeded(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM "AlertRule" WHERE id = $1`,
		quotaThresholdRuleID,
	).Scan(&n); err != nil {
		t.Skipf("skip: cannot probe AlertRule seed (%v)", err)
	}
	if n == 0 {
		t.Skipf("skip: %q AlertRule not seeded in local DB; run `npx prisma db seed` to enable this test",
			quotaThresholdRuleID)
	}
}

// jobsTestCleanup removes test-scoped rows inserted by jobs tests. Uses a
// stable "test.job." rule-id prefix so it cannot accidentally reap the
// production seed rules or rows from the alerting package's own test suite
// (which uses "test.").
func jobsTestCleanup(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	_, _ = pool.Exec(ctx, `DELETE FROM "AlertDispatch" WHERE "alertId" IN (SELECT id FROM "Alert" WHERE "ruleId" LIKE 'test.job.%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM "Alert" WHERE "ruleId" LIKE 'test.job.%' OR "targetKey" LIKE 'override:test-job-%' OR "targetKey" LIKE 'policy:test-job-%' OR "targetKey" LIKE 'vk:test-job-%'`)
	_, _ = pool.Exec(ctx, `DELETE FROM "AlertRule" WHERE id LIKE 'test.job.%'`)
	pool.Close()
}

func TestQuotaAlertCheck_Identity(t *testing.T) {
	j := NewQuotaAlertCheck(nil, nil, time.Minute, testLogger())
	if j.ID() != "quota-alert-check" {
		t.Errorf("ID = %q, want quota-alert-check", j.ID())
	}
	if j.Name() == "" {
		t.Error("Name empty")
	}
	if j.Description() == "" {
		t.Error("Description empty")
	}
	if j.Interval() != time.Minute {
		t.Errorf("Interval = %v, want 1m", j.Interval())
	}
}

func TestQuotaAlertCheck_IntervalDefault(t *testing.T) {
	j := NewQuotaAlertCheck(nil, nil, 0, testLogger())
	if j.Interval() != time.Minute {
		t.Errorf("Interval = %v, want 1m default", j.Interval())
	}
}

func TestScopeToDimension(t *testing.T) {
	cases := []struct {
		scope string
		want  string
		ok    bool
	}{
		{"user", "user", true},
		{"vk", "virtual_key", true},
		{"virtual_key", "virtual_key", true},
		{"project", "project", true},
		{"organization", "organization", true},
		{"unknown", "", false},
	}
	for _, c := range cases {
		got, ok := scopeToDimension(c.scope)
		if got != c.want || ok != c.ok {
			t.Errorf("scopeToDimension(%q) = (%q, %v), want (%q, %v)", c.scope, got, ok, c.want, c.ok)
		}
	}
}

func TestDimensionToTargetType(t *testing.T) {
	cases := map[string]string{
		"user":         "user",
		"virtual_key":  "vk",
		"project":      "project",
		"organization": "organization",
		"unknown":      "unknown",
	}
	for dim, want := range cases {
		if got := dimensionToTargetType(dim); got != want {
			t.Errorf("dimensionToTargetType(%q) = %q, want %q", dim, got, want)
		}
	}
}

func TestParseAlertThresholds(t *testing.T) {
	t.Run("empty returns default", func(t *testing.T) {
		got := parseAlertThresholds(nil)
		if len(got) != 2 || got[0] != 80 || got[1] != 95 {
			t.Errorf("got %v, want default [80,95]", got)
		}
	})

	t.Run("invalid JSON returns default", func(t *testing.T) {
		got := parseAlertThresholds(json.RawMessage(`not-json`))
		if len(got) != 2 || got[0] != 80 || got[1] != 95 {
			t.Errorf("got %v, want default [80,95]", got)
		}
	})

	t.Run("empty array returns default", func(t *testing.T) {
		got := parseAlertThresholds(json.RawMessage(`[]`))
		if len(got) != 2 || got[0] != 80 || got[1] != 95 {
			t.Errorf("got %v, want default [80,95]", got)
		}
	})

	t.Run("unsorted custom thresholds are sorted", func(t *testing.T) {
		got := parseAlertThresholds(json.RawMessage(`[90, 50, 75]`))
		want := []int{50, 75, 90}
		for i, v := range want {
			if i >= len(got) || got[i] != v {
				t.Errorf("got %v, want %v", got, want)
				break
			}
		}
	})
}

func TestFindThresholdsForOverride(t *testing.T) {
	policies := []quotastore.QuotaPolicy{
		{ID: "p1", Scope: "user", AlertThresholds: json.RawMessage(`[50, 90]`)},
		{ID: "p2", Scope: "vk", AlertThresholds: json.RawMessage(`[75]`)},
	}

	t.Run("matches scope", func(t *testing.T) {
		o := quotastore.QuotaOverride{TargetType: "user"}
		got := findThresholdsForOverride(o, policies)
		if len(got) != 2 || got[0] != 50 || got[1] != 90 {
			t.Errorf("got %v, want [50,90]", got)
		}
	})

	t.Run("no match returns default", func(t *testing.T) {
		o := quotastore.QuotaOverride{TargetType: "project"}
		got := findThresholdsForOverride(o, policies)
		if len(got) != 2 || got[0] != 80 || got[1] != 95 {
			t.Errorf("got %v, want default [80,95]", got)
		}
	})
}

func TestSeverityForThreshold(t *testing.T) {
	cases := []struct {
		threshold int
		want      alerting.Severity
	}{
		{95, alerting.SeverityCritical},
		{99, alerting.SeverityCritical},
		{80, alerting.SeverityHigh},
		{94, alerting.SeverityHigh},
		{50, alerting.SeverityMedium},
		{10, alerting.SeverityMedium},
	}
	for _, c := range cases {
		if got := severityForThreshold(c.threshold); got != c.want {
			t.Errorf("severityForThreshold(%d) = %q, want %q", c.threshold, got, c.want)
		}
	}
}

// TestQuotaAlertCheck_RaisesAtThresholdCrossing verifies that raiseForThresholds
// emits exactly one Raise for each crossed band. No DB needed — the helper
// takes all inputs directly and calls through the injected alertRaiser.
func TestQuotaAlertCheck_RaisesAtThresholdCrossing(t *testing.T) {
	f := &fakeRaiser{}
	j := NewQuotaAlertCheck(nil, f, time.Minute, testLogger())

	// Single crossing: pct=82 vs [80,95] → Raise once for threshold=80.
	j.raiseForThresholds(context.Background(), raiseContext{
		targetKey:      "override:o1|period:2026-04",
		targetLabel:    "user:u1",
		targetType:     "user",
		targetID:       "u1",
		overrideID:     "o1",
		periodKey:      "2026-04",
		thresholds:     []int{80, 95},
		pct:            82,
		costLimitUsd:   100,
		currentCostUsd: 82,
	}, &[]error{})
	if got := f.raiseCount(); got != 1 {
		t.Fatalf("raises after pct=82: got %d, want 1", got)
	}
	if f.raises[0].RuleID != quotaThresholdRuleID {
		t.Errorf("ruleID = %q, want %q", f.raises[0].RuleID, quotaThresholdRuleID)
	}
	if f.raises[0].Severity != alerting.SeverityHigh {
		t.Errorf("severity @ threshold=80: got %q, want high", f.raises[0].Severity)
	}

	// Double crossing: pct=96 vs [80,95] → Raise for 80 AND 95.
	j.raiseForThresholds(context.Background(), raiseContext{
		targetKey:      "override:o1|period:2026-04",
		targetLabel:    "user:u1",
		targetType:     "user",
		targetID:       "u1",
		overrideID:     "o1",
		periodKey:      "2026-04",
		thresholds:     []int{80, 95},
		pct:            96,
		costLimitUsd:   100,
		currentCostUsd: 96,
	}, &[]error{})
	if got := f.raiseCount(); got != 3 {
		t.Fatalf("raises after pct=96: got %d, want 3 (1 prior + 2 new)", got)
	}
	// The two new raises should be for 80 and 95.
	thresholdsSeen := []int{
		f.raises[1].Details["threshold"].(int),
		f.raises[2].Details["threshold"].(int),
	}
	sort.Ints(thresholdsSeen)
	if thresholdsSeen[0] != 80 || thresholdsSeen[1] != 95 {
		t.Errorf("thresholds seen = %v, want [80,95]", thresholdsSeen)
	}
	// 95 crossing must be critical severity.
	for _, r := range f.raises[1:] {
		if r.Details["threshold"].(int) == 95 && r.Severity != alerting.SeverityCritical {
			t.Errorf("severity @ threshold=95: got %q, want critical", r.Severity)
		}
	}
}

// TestQuotaAlertCheck_NoRaiseBelowThreshold verifies the guard: when pct sits
// below every threshold, nothing is raised.
func TestQuotaAlertCheck_NoRaiseBelowThreshold(t *testing.T) {
	f := &fakeRaiser{}
	j := NewQuotaAlertCheck(nil, f, time.Minute, testLogger())
	j.raiseForThresholds(context.Background(), raiseContext{
		targetKey:      "override:o2|period:2026-04",
		thresholds:     []int{80, 95},
		pct:            50,
		costLimitUsd:   100,
		currentCostUsd: 50,
	}, &[]error{})
	if got := f.raiseCount(); got != 0 {
		t.Errorf("raises: got %d, want 0", got)
	}
}

func TestOverrideTargetKey(t *testing.T) {
	if got := overrideTargetKey("o1", "2026-04"); got != "override:o1|period:2026-04" {
		t.Errorf("got %q", got)
	}
}

func TestPolicyTargetKey(t *testing.T) {
	if got := policyTargetKey("p1", "user", "u1", "2026-04"); got != "policy:p1|entity:user:u1|period:2026-04" {
		t.Errorf("got %q", got)
	}
}

// TestQuotaAlertCheck_AutoResolveWithHysteresis drives resolveStale directly
// with a seeded FIRING alert row. pct=75 is below 80-2=78 → Resolve fires.
// pct=79 is above 78 → no Resolve. Requires the DB.
func TestQuotaAlertCheck_AutoResolveWithHysteresis(t *testing.T) {
	pool := jobsTestPool(t)
	requireQuotaThresholdRuleSeeded(t, pool)
	defer jobsTestCleanup(t, pool)

	ctx := context.Background()

	// Phase 1: hysteresis should resolve.
	targetKey := "override:test-job-hyst-low|period:2026-04"
	_, err := pool.Exec(ctx, `
		INSERT INTO "Alert" (id, "ruleId", "sourceType", "targetKey", "targetLabel",
		    severity, state, message, details, "firedAt", "lastSeenAt", "duplicateCount")
		VALUES (gen_random_uuid()::text, $1, 'quota', $2, 'user:u1',
		    'HIGH'::"AlertSeverity", 'FIRING'::"AlertState", 'test', '{}',
		    NOW(), NOW(), 1)`,
		quotaThresholdRuleID, targetKey,
	)
	if err != nil {
		t.Fatalf("seed firing alert: %v", err)
	}

	f := &fakeRaiser{}
	j := NewQuotaAlertCheck(pool, f, time.Minute, testLogger())
	evaluated := map[string]thresholdTarget{
		targetKey: {minThreshold: 80, currentPct: 75}, // floor=78, pct=75 → resolve
	}
	if err := j.resolveStale(ctx, evaluated); err != nil {
		t.Fatalf("resolveStale: %v", err)
	}
	// Filter to our own targetKey — resolveStale walks every FIRING
	// quota.threshold alert in the table, so foreign rows seeded by
	// other tests or dev usage would otherwise inflate the count. We
	// only modified our own seed; only our resolves should be asserted.
	var ourResolves []resolveCall
	for _, r := range f.resolves {
		if r.TargetKey == targetKey {
			ourResolves = append(ourResolves, r)
		}
	}
	if len(ourResolves) != 1 {
		t.Fatalf("resolves for %q: got %d, want 1 (all resolves=%+v)", targetKey, len(ourResolves), f.resolves)
	}
	if ourResolves[0].Reason != "auto" {
		t.Errorf("unexpected resolve reason: %+v", ourResolves[0])
	}

	// Phase 2: pct=79 is above floor=78, no resolve for this row.
	targetKey2 := "override:test-job-hyst-high|period:2026-04"
	_, err = pool.Exec(ctx, `
		INSERT INTO "Alert" (id, "ruleId", "sourceType", "targetKey", "targetLabel",
		    severity, state, message, details, "firedAt", "lastSeenAt", "duplicateCount")
		VALUES (gen_random_uuid()::text, $1, 'quota', $2, 'user:u2',
		    'HIGH'::"AlertSeverity", 'FIRING'::"AlertState", 'test', '{}',
		    NOW(), NOW(), 1)`,
		quotaThresholdRuleID, targetKey2,
	)
	if err != nil {
		t.Fatalf("seed firing alert 2: %v", err)
	}
	// Clear the prior firing row to isolate this assertion (resolveStale
	// inspects ALL firing rows on every call).
	_, _ = pool.Exec(ctx, `DELETE FROM "Alert" WHERE "targetKey" = $1`, targetKey)

	f2 := &fakeRaiser{}
	j2 := NewQuotaAlertCheck(pool, f2, time.Minute, testLogger())
	evaluated = map[string]thresholdTarget{
		targetKey2: {minThreshold: 80, currentPct: 79}, // floor=78, pct=79 → no resolve
	}
	if err := j2.resolveStale(ctx, evaluated); err != nil {
		t.Fatalf("resolveStale phase 2: %v", err)
	}
	for _, r := range f2.resolves {
		if r.TargetKey == targetKey2 {
			t.Errorf("unexpected resolve for targetKey2: %+v", r)
		}
	}
}

// TestQuotaAlertCheck_ResolveTargetRemoved: a firing alert whose targetKey is
// NOT in the evaluated set (e.g. override deleted between runs) resolves with
// reason "target-removed".
func TestQuotaAlertCheck_ResolveTargetRemoved(t *testing.T) {
	pool := jobsTestPool(t)
	requireQuotaThresholdRuleSeeded(t, pool)
	defer jobsTestCleanup(t, pool)

	ctx := context.Background()
	targetKey := "override:test-job-removed|period:2026-04"
	_, err := pool.Exec(ctx, `
		INSERT INTO "Alert" (id, "ruleId", "sourceType", "targetKey", "targetLabel",
		    severity, state, message, details, "firedAt", "lastSeenAt", "duplicateCount")
		VALUES (gen_random_uuid()::text, $1, 'quota', $2, 'user:gone',
		    'HIGH'::"AlertSeverity", 'FIRING'::"AlertState", 'test', '{}',
		    NOW(), NOW(), 1)`,
		quotaThresholdRuleID, targetKey,
	)
	if err != nil {
		t.Fatalf("seed firing alert: %v", err)
	}

	f := &fakeRaiser{}
	j := NewQuotaAlertCheck(pool, f, time.Minute, testLogger())
	if err := j.resolveStale(ctx, map[string]thresholdTarget{}); err != nil {
		t.Fatalf("resolveStale: %v", err)
	}
	found := false
	for _, r := range f.resolves {
		if r.TargetKey == targetKey && r.Reason == "target-removed" {
			found = true
		}
	}
	if !found {
		t.Errorf("no target-removed resolve for %s; resolves=%+v", targetKey, f.resolves)
	}
}

// TestQuotaAlertCheck_DedupSingleFiringRow runs a real Raiser against a real
// DB. Two back-to-back raiseForThresholds calls with the same (rule,target)
// must leave exactly one FIRING row. This is the DB-level dedup gate: if
// anything in the Raiser or Alert-table unique-constraint stack regresses,
// this test catches a second row appearing.
func TestQuotaAlertCheck_DedupSingleFiringRow(t *testing.T) {
	pool := jobsTestPool(t)
	requireQuotaThresholdRuleSeeded(t, pool)
	defer jobsTestCleanup(t, pool)

	ctx := context.Background()
	// quota.threshold is seeded by the migration — use it directly. If the
	// rule is missing from the local DB the Raiser returns an error and
	// this test fails with a clear message, surfacing the missing seed.
	store := alerting.NewStore(pool)
	raiser := alerting.NewRaiser(pool, store, nil, testLogger())

	j := NewQuotaAlertCheck(pool, raiser, time.Minute, testLogger())
	rc := raiseContext{
		targetKey:      "override:test-job-dedup|period:2026-04",
		targetLabel:    "user:test-dedup",
		targetType:     "user",
		targetID:       "test-dedup",
		overrideID:     "test-job-dedup",
		periodKey:      "2026-04",
		thresholds:     []int{80, 95},
		pct:            90,
		costLimitUsd:   100,
		currentCostUsd: 90,
	}

	for i := range 2 {
		if c := j.raiseForThresholds(ctx, rc, &[]error{}); c != 1 {
			t.Errorf("raiseForThresholds iter %d: got count %d, want 1 (pct=90 crosses only 80)", i, c)
		}
	}

	var count int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM "Alert"
		WHERE "ruleId" = $1 AND "targetKey" = $2 AND state = 'FIRING'::"AlertState"`,
		quotaThresholdRuleID, rc.targetKey,
	).Scan(&count); err != nil {
		t.Fatalf("count firing rows: %v", err)
	}
	if count != 1 {
		t.Errorf("firing rows for (rule=%s,target=%s): got %d, want 1", quotaThresholdRuleID, rc.targetKey, count)
	}

	// duplicateCount should have incremented on the second raise.
	var dup int
	if err := pool.QueryRow(ctx, `
		SELECT "duplicateCount" FROM "Alert"
		WHERE "ruleId" = $1 AND "targetKey" = $2`,
		quotaThresholdRuleID, rc.targetKey,
	).Scan(&dup); err != nil {
		t.Fatalf("scan duplicateCount: %v", err)
	}
	if dup < 2 {
		t.Errorf("duplicateCount: got %d, want >= 2", dup)
	}
}

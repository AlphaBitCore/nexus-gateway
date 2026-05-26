package expiry

import (
	"context"
	"testing"
	"time"

	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
)

func TestVKExpiry_Identity(t *testing.T) {
	j := NewVKExpiry(nil, nil, time.Hour, testLogger())
	if j.ID() != "vk-expiry" {
		t.Errorf("ID = %q, want vk-expiry", j.ID())
	}
	if j.Name() == "" {
		t.Error("Name empty")
	}
	if j.Description() == "" {
		t.Error("Description empty")
	}
	if j.Interval() != time.Hour {
		t.Errorf("Interval = %v, want 1h", j.Interval())
	}
}

func TestVKExpiry_IntervalDefault(t *testing.T) {
	j := NewVKExpiry(nil, nil, 0, testLogger())
	if j.Interval() != time.Hour {
		t.Errorf("Interval = %v, want 1h default", j.Interval())
	}
}

func TestSeverityForVKExpiry(t *testing.T) {
	cases := []struct {
		daysLeft int
		want     alerting.Severity
	}{
		{0, alerting.SeverityCritical},
		{1, alerting.SeverityCritical},
		{3, alerting.SeverityHigh},
		{7, alerting.SeverityHigh},
		{10, alerting.SeverityMedium},
		{15, alerting.SeverityMedium},
		{20, alerting.SeverityLow},
		{30, alerting.SeverityLow},
	}
	for _, c := range cases {
		if got := severityForVKExpiry(c.daysLeft); got != c.want {
			t.Errorf("severityForVKExpiry(%d) = %q, want %q", c.daysLeft, got, c.want)
		}
	}
}

func TestDaysUntil(t *testing.T) {
	now := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		expires time.Time
		want    int
		label   string
	}{
		{now.Add(-time.Hour), 0, "already expired"},
		{now.Add(30 * time.Minute), 1, "30m future → rounds up to 1"},
		{now.Add(36 * time.Hour), 2, "36h → 2"},
		{now.Add(7 * 24 * time.Hour), 7, "exactly 7d"},
		{now.Add(10 * 24 * time.Hour), 10, "10d"},
	}
	for _, c := range cases {
		if got := daysUntil(now, c.expires); got != c.want {
			t.Errorf("%s: daysUntil = %d, want %d", c.label, got, c.want)
		}
	}
}

// TestVKExpiry_ResolvesWhenNoLongerExpiring seeds a firing vk_expiring alert,
// runs resolveRenewed with an empty expiring set, and verifies the stored
// target is resolved with the "renewed-or-expired" reason.
func TestVKExpiry_ResolvesWhenNoLongerExpiring(t *testing.T) {
	pool := jobsTestPool(t)
	defer jobsTestCleanup(t, pool)

	ctx := context.Background()
	_, err := pool.Exec(ctx, `
		INSERT INTO "Alert" (id, "ruleId", "sourceType", "targetKey", "targetLabel",
		    severity, state, message, details, "firedAt", "lastSeenAt", "duplicateCount")
		VALUES (gen_random_uuid()::text, $1, 'quota', $2, 'abc',
		    'HIGH'::"AlertSeverity", 'FIRING'::"AlertState", 'test', '{}',
		    NOW(), NOW(), 1)`,
		vkExpiringRuleID, "vk:test-job-abc",
	)
	if err != nil {
		t.Fatalf("seed firing vk alert: %v", err)
	}

	f := &fakeRaiser{}
	j := NewVKExpiry(pool, f, time.Hour, testLogger())

	if err := j.resolveRenewed(ctx, map[string]bool{}); err != nil {
		t.Fatalf("resolveRenewed: %v", err)
	}

	var match int
	for _, r := range f.resolves {
		if r.RuleID == vkExpiringRuleID && r.TargetKey == "vk:test-job-abc" && r.Reason == "renewed-or-expired" {
			match++
		}
	}
	if match != 1 {
		t.Errorf("expected 1 renewed-or-expired resolve, got %d (all resolves=%+v)", match, f.resolves)
	}
}

// TestVKExpiry_SkipsWhenStillExpiring: if the VK is still in the expiring set,
// no resolve is emitted (the alert stays firing).
func TestVKExpiry_SkipsWhenStillExpiring(t *testing.T) {
	pool := jobsTestPool(t)
	defer jobsTestCleanup(t, pool)

	ctx := context.Background()
	_, err := pool.Exec(ctx, `
		INSERT INTO "Alert" (id, "ruleId", "sourceType", "targetKey", "targetLabel",
		    severity, state, message, details, "firedAt", "lastSeenAt", "duplicateCount")
		VALUES (gen_random_uuid()::text, $1, 'quota', $2, 'still-there',
		    'HIGH'::"AlertSeverity", 'FIRING'::"AlertState", 'test', '{}',
		    NOW(), NOW(), 1)`,
		vkExpiringRuleID, "vk:test-job-still",
	)
	if err != nil {
		t.Fatalf("seed firing vk alert: %v", err)
	}

	f := &fakeRaiser{}
	j := NewVKExpiry(pool, f, time.Hour, testLogger())

	if err := j.resolveRenewed(ctx, map[string]bool{"test-job-still": true}); err != nil {
		t.Fatalf("resolveRenewed: %v", err)
	}

	for _, r := range f.resolves {
		if r.TargetKey == "vk:test-job-still" {
			t.Errorf("unexpected resolve for still-expiring vk: %+v", r)
		}
	}
}

package health

import (
	"context"
	"testing"
	"time"

	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
)

// fakeRuleLoader records GetRule calls and returns scripted results.
type fakeRuleLoader struct {
	rule *alerting.AlertRule
	err  error
}

func (f *fakeRuleLoader) GetRule(_ context.Context, _ string) (*alerting.AlertRule, error) {
	return f.rule, f.err
}

// thingOfflineRule returns a minimal enabled thing.offline rule with the given params.
func thingOfflineRule(offlineAfterSec float64, excludeKinds []string) *alerting.AlertRule {
	kindsAny := make([]any, len(excludeKinds))
	for i, k := range excludeKinds {
		kindsAny[i] = k
	}
	return &alerting.AlertRule{
		ID:              thingOfflineRuleID,
		Enabled:         true,
		DefaultSeverity: alerting.SeverityHigh,
		Params: map[string]any{
			"offlineAfterSec": offlineAfterSec,
			"excludeKinds":    kindsAny,
		},
	}
}

func TestThingOfflineAlerts_Identity(t *testing.T) {
	j := NewThingOfflineAlerts(nil, nil, nil, 60*time.Second, testLogger())
	if j.ID() != thingOfflineJobID {
		t.Errorf("ID = %q, want %q", j.ID(), thingOfflineJobID)
	}
	if j.Name() == "" {
		t.Error("Name empty")
	}
	if j.Description() == "" {
		t.Error("Description empty")
	}
	if j.Interval() != 60*time.Second {
		t.Errorf("Interval = %v, want 60s", j.Interval())
	}
}

func TestThingOfflineAlerts_IntervalDefault(t *testing.T) {
	j := NewThingOfflineAlerts(nil, nil, nil, 0, testLogger())
	if j.Interval() != 60*time.Second {
		t.Errorf("Interval = %v, want 60s default", j.Interval())
	}
}

// TestThingOfflineAlerts_RuleDisabled: when the rule's Enabled flag is false,
// neither Raise nor Resolve should be called.
func TestThingOfflineAlerts_RuleDisabled(t *testing.T) {
	rule := thingOfflineRule(300, nil)
	rule.Enabled = false

	f := &fakeRaiser{}
	j := NewThingOfflineAlerts(nil, f, &fakeRuleLoader{rule: rule}, 60*time.Second, testLogger())

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run with disabled rule: %v", err)
	}
	if f.raiseCount() != 0 {
		t.Errorf("raises: got %d, want 0 (rule disabled)", f.raiseCount())
	}
	if f.resolveCount() != 0 {
		t.Errorf("resolves: got %d, want 0 (rule disabled)", f.resolveCount())
	}
}

// TestParseThingOfflineParams covers the param parsing helpers.
func TestParseThingOfflineParams(t *testing.T) {
	t.Run("valid params", func(t *testing.T) {
		params := map[string]any{
			"offlineAfterSec": float64(300),
			"excludeKinds":    []any{"nexus-hub", "control-plane"},
		}
		sec, kinds, err := parseThingOfflineParams(params)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if sec != 300 {
			t.Errorf("offlineAfterSec = %v, want 300", sec)
		}
		if len(kinds) != 2 || kinds[0] != "nexus-hub" || kinds[1] != "control-plane" {
			t.Errorf("excludeKinds = %v, want [nexus-hub control-plane]", kinds)
		}
	})

	t.Run("missing offlineAfterSec returns error", func(t *testing.T) {
		_, _, err := parseThingOfflineParams(map[string]any{})
		if err == nil {
			t.Error("expected error for missing offlineAfterSec")
		}
	})

	t.Run("offlineAfterSec=0 returns error", func(t *testing.T) {
		_, _, err := parseThingOfflineParams(map[string]any{"offlineAfterSec": float64(0)})
		if err == nil {
			t.Error("expected error for offlineAfterSec=0")
		}
	})

	t.Run("missing excludeKinds returns empty slice", func(t *testing.T) {
		_, kinds, err := parseThingOfflineParams(map[string]any{"offlineAfterSec": float64(60)})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(kinds) != 0 {
			t.Errorf("excludeKinds = %v, want empty", kinds)
		}
	})
}

// TestThingOfflineAlerts_RaisesStaleThingAlert seeds a thing with an old
// last_seen_at and verifies Raise is called with the correct ruleId,
// targetKey, and severity from the rule default.
func TestThingOfflineAlerts_RaisesStaleThingAlert(t *testing.T) {
	pool := jobsTestPool(t)
	defer jobsTestCleanup(t, pool)

	ctx := context.Background()

	// Insert a stale thing: last_seen_at = 10 minutes ago, threshold = 5 minutes.
	var thingID string
	err := pool.QueryRow(ctx, `
		INSERT INTO thing (id, name, type, status, last_seen_at, updated_at)
		VALUES (gen_random_uuid()::text, 'test-offline-node', 'agent', 'online',
		        NOW() - INTERVAL '10 minutes', NOW())
		RETURNING id`).Scan(&thingID)
	if err != nil {
		t.Fatalf("insert stale thing: %v", err)
	}
	defer pool.Exec(ctx, `DELETE FROM thing WHERE id = $1`, thingID) //nolint:errcheck

	f := &fakeRaiser{}
	rule := thingOfflineRule(300, nil) // 5-minute threshold
	j := NewThingOfflineAlerts(pool, f, &fakeRuleLoader{rule: rule}, 60*time.Second, testLogger())

	if err := j.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	found := false
	for _, r := range f.raises {
		if r.RuleID == thingOfflineRuleID && r.TargetKey == thingTargetKeyPrefix+thingID {
			found = true
			if r.Severity != alerting.SeverityHigh {
				t.Errorf("severity = %q, want high (rule default)", r.Severity)
			}
		}
	}
	if !found {
		t.Errorf("no Raise for stale thing %s; raises=%+v", thingID, f.raises)
	}
}

// TestThingOfflineAlerts_ResolvesRecoveredThing seeds a FIRING thing.offline
// alert and a thing that is now fresh. Run must call Raiser.Resolve with
// reason "back-online" for the previously-firing alert.
func TestThingOfflineAlerts_ResolvesRecoveredThing(t *testing.T) {
	pool := jobsTestPool(t)
	defer jobsTestCleanup(t, pool)

	ctx := context.Background()

	// Insert a thing that is now fresh (last_seen_at = 30 seconds ago, threshold = 5 minutes).
	var thingID string
	err := pool.QueryRow(ctx, `
		INSERT INTO thing (id, name, type, status, last_seen_at, updated_at)
		VALUES (gen_random_uuid()::text, 'test-recovered-node', 'agent', 'online',
		        NOW() - INTERVAL '30 seconds', NOW())
		RETURNING id`).Scan(&thingID)
	if err != nil {
		t.Fatalf("insert fresh thing: %v", err)
	}
	defer pool.Exec(ctx, `DELETE FROM thing WHERE id = $1`, thingID) //nolint:errcheck

	targetKey := thingTargetKeyPrefix + thingID

	// Seed a FIRING alert for this thing.
	_, err = pool.Exec(ctx, `
		INSERT INTO "Alert" (id, "ruleId", "sourceType", "targetKey", "targetLabel",
		    severity, state, message, details, "firedAt", "lastSeenAt", "duplicateCount")
		VALUES (gen_random_uuid()::text, $1, 'thing', $2, 'test-recovered-node (agent)',
		    'HIGH'::"AlertSeverity", 'FIRING'::"AlertState", 'test', '{}',
		    NOW(), NOW(), 1)`,
		thingOfflineRuleID, targetKey,
	)
	if err != nil {
		t.Fatalf("seed firing alert: %v", err)
	}
	defer pool.Exec(ctx, `DELETE FROM "Alert" WHERE "targetKey" = $1`, targetKey) //nolint:errcheck

	f := &fakeRaiser{}
	rule := thingOfflineRule(300, nil) // 5-minute threshold: fresh thing is within window
	j := NewThingOfflineAlerts(pool, f, &fakeRuleLoader{rule: rule}, 60*time.Second, testLogger())

	if err := j.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	found := false
	for _, r := range f.resolves {
		if r.RuleID == thingOfflineRuleID && r.TargetKey == targetKey && r.Reason == thingOfflineResolveReason {
			found = true
		}
	}
	if !found {
		t.Errorf("no back-online resolve for %s; resolves=%+v", targetKey, f.resolves)
	}
}

// TestThingOfflineAlerts_ExcludeKindsHonored seeds a stale thing of an
// excluded type and verifies Raise is NOT called for it.
func TestThingOfflineAlerts_ExcludeKindsHonored(t *testing.T) {
	pool := jobsTestPool(t)
	defer jobsTestCleanup(t, pool)

	ctx := context.Background()

	// Insert a stale nexus-hub thing (will be excluded).
	var hubID string
	err := pool.QueryRow(ctx, `
		INSERT INTO thing (id, name, type, status, last_seen_at, updated_at)
		VALUES (gen_random_uuid()::text, 'test-hub-excluded', 'nexus-hub', 'online',
		        NOW() - INTERVAL '10 minutes', NOW())
		RETURNING id`).Scan(&hubID)
	if err != nil {
		t.Fatalf("insert hub thing: %v", err)
	}
	defer pool.Exec(ctx, `DELETE FROM thing WHERE id = $1`, hubID) //nolint:errcheck

	f := &fakeRaiser{}
	// Exclude nexus-hub from offline checks.
	rule := thingOfflineRule(300, []string{"nexus-hub"})
	j := NewThingOfflineAlerts(pool, f, &fakeRuleLoader{rule: rule}, 60*time.Second, testLogger())

	if err := j.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, r := range f.raises {
		if r.TargetKey == thingTargetKeyPrefix+hubID {
			t.Errorf("unexpected Raise for excluded nexus-hub thing %s", hubID)
		}
	}
}

package health

import (
	"context"
	"testing"
	"time"

	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
)

// providerUnavailableRule returns a minimal enabled provider.unavailable rule.
func providerUnavailableRule(minDownSec, recoverySec float64) *alerting.AlertRule {
	return &alerting.AlertRule{
		ID:              providerUnavailableRuleID,
		Enabled:         true,
		DefaultSeverity: alerting.SeverityCritical,
		Params: map[string]any{
			"minDownSec":  minDownSec,
			"recoverySec": recoverySec,
		},
	}
}

func TestProviderUnavailableAlerts_Identity(t *testing.T) {
	j := NewProviderUnavailableAlerts(nil, nil, nil, 60*time.Second, testLogger())
	if j.ID() != providerUnavailableJobID {
		t.Errorf("ID = %q, want %q", j.ID(), providerUnavailableJobID)
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

func TestProviderUnavailableAlerts_IntervalDefault(t *testing.T) {
	j := NewProviderUnavailableAlerts(nil, nil, nil, 0, testLogger())
	if j.Interval() != 60*time.Second {
		t.Errorf("Interval = %v, want 60s default", j.Interval())
	}
}

// TestProviderUnavailableAlerts_RuleDisabled: when the rule's Enabled flag is
// false, neither Raise nor Resolve should be called.
func TestProviderUnavailableAlerts_RuleDisabled(t *testing.T) {
	// Pass 0/0 for minDownSec / recoverySec so the single-tick
	// fire/resolve assertion still works.
	rule := providerUnavailableRule(0, 0)
	rule.Enabled = false

	f := &fakeRaiser{}
	j := NewProviderUnavailableAlerts(nil, f, &fakeRuleLoader{rule: rule}, 60*time.Second, testLogger())

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

// TestParseProviderUnavailableParams covers the param-parsing helper.
func TestParseProviderUnavailableParams(t *testing.T) {
	t.Run("valid params", func(t *testing.T) {
		min, rec, err := parseProviderUnavailableParams(map[string]any{
			"minDownSec":  float64(120),
			"recoverySec": float64(60),
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if min != 120 {
			t.Errorf("minDownSec = %v, want 120", min)
		}
		if rec != 60 {
			t.Errorf("recoverySec = %v, want 60", rec)
		}
	})

	t.Run("missing minDownSec returns error", func(t *testing.T) {
		_, _, err := parseProviderUnavailableParams(map[string]any{"recoverySec": float64(60)})
		if err == nil {
			t.Error("expected error for missing minDownSec")
		}
	})

	t.Run("missing recoverySec returns error", func(t *testing.T) {
		_, _, err := parseProviderUnavailableParams(map[string]any{"minDownSec": float64(120)})
		if err == nil {
			t.Error("expected error for missing recoverySec")
		}
	})

	t.Run("wrong type returns error", func(t *testing.T) {
		_, _, err := parseProviderUnavailableParams(map[string]any{
			"minDownSec":  "notanumber",
			"recoverySec": float64(60),
		})
		if err == nil {
			t.Error("expected error for non-numeric minDownSec")
		}
	})
}

// --- DB-backed tests ---

// TestProviderUnavailableAlerts_RaisesUnavailableProvider seeds a Provider +
// ProviderHealth with status='unavailable' and verifies Raise is called with
// the correct ruleId, targetKey="provider:<id>", and severity=critical.
func TestProviderUnavailableAlerts_RaisesUnavailableProvider(t *testing.T) {
	pool := jobsTestPool(t)
	defer jobsTestCleanup(t, pool)

	ctx := context.Background()

	// Insert a Provider row.
	var providerID string
	err := pool.QueryRow(ctx, `
		INSERT INTO "Provider" (id, name, "displayName", adapter_type, "baseUrl", "pathPrefix", "createdAt", "updatedAt")
		VALUES (gen_random_uuid()::text, 'test-pua-openai', 'Test OpenAI', 'openai-compatible',
		        'https://api.openai.com', '/pua-openai', NOW(), NOW())
		RETURNING id`).Scan(&providerID)
	if err != nil {
		t.Fatalf("insert provider: %v", err)
	}
	defer pool.Exec(ctx, `DELETE FROM "Provider" WHERE id = $1`, providerID) //nolint:errcheck

	// Insert ProviderHealth with status='unavailable'.
	_, err = pool.Exec(ctx, `
		INSERT INTO "ProviderHealth" (id, "providerId", provider, status, "rollingErrorRate",
		    "windowStart", "updatedAt")
		VALUES (gen_random_uuid()::text, $1, 'openai', 'unavailable', 0.95,
		        NOW() - INTERVAL '5 minutes', NOW())`,
		providerID,
	)
	if err != nil {
		t.Fatalf("insert provider health: %v", err)
	}
	defer pool.Exec(ctx, `DELETE FROM "ProviderHealth" WHERE "providerId" = $1`, providerID) //nolint:errcheck

	f := &fakeRaiser{}
	// Pass 0/0 for minDownSec / recoverySec so the single-tick
	// fire/resolve assertion still works.
	rule := providerUnavailableRule(0, 0)
	j := NewProviderUnavailableAlerts(pool, f, &fakeRuleLoader{rule: rule}, 60*time.Second, testLogger())

	if err := j.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	found := false
	wantTargetKey := providerTargetKeyPrefix + providerID
	for _, r := range f.raises {
		if r.RuleID == providerUnavailableRuleID && r.TargetKey == wantTargetKey {
			found = true
			if r.Severity != alerting.SeverityCritical {
				t.Errorf("severity = %q, want critical (rule default)", r.Severity)
			}
		}
	}
	if !found {
		t.Errorf("no Raise for unavailable provider %s; raises=%+v", providerID, f.raises)
	}
}

// TestProviderUnavailableAlerts_ResolvesRecoveredProvider seeds a FIRING
// provider.unavailable alert for a provider that is now healthy, verifies
// Raiser.Resolve is called with reason="recovered".
func TestProviderUnavailableAlerts_ResolvesRecoveredProvider(t *testing.T) {
	pool := jobsTestPool(t)
	defer jobsTestCleanup(t, pool)

	ctx := context.Background()

	// Insert Provider.
	var providerID string
	err := pool.QueryRow(ctx, `
		INSERT INTO "Provider" (id, name, "displayName", adapter_type, "baseUrl", "pathPrefix", "createdAt", "updatedAt")
		VALUES (gen_random_uuid()::text, 'test-pua-recovered-provider', 'Test Recovered Provider', 'openai-compatible',
		        'https://api.openai.com', '/pua-recovered', NOW(), NOW())
		RETURNING id`).Scan(&providerID)
	if err != nil {
		t.Fatalf("insert provider: %v", err)
	}
	defer pool.Exec(ctx, `DELETE FROM "Provider" WHERE id = $1`, providerID) //nolint:errcheck

	// ProviderHealth status is now 'healthy' — provider recovered.
	_, err = pool.Exec(ctx, `
		INSERT INTO "ProviderHealth" (id, "providerId", provider, status, "rollingErrorRate",
		    "windowStart", "updatedAt")
		VALUES (gen_random_uuid()::text, $1, 'openai', 'healthy', 0.01,
		        NOW() - INTERVAL '5 minutes', NOW())`,
		providerID,
	)
	if err != nil {
		t.Fatalf("insert provider health: %v", err)
	}
	defer pool.Exec(ctx, `DELETE FROM "ProviderHealth" WHERE "providerId" = $1`, providerID) //nolint:errcheck

	targetKey := providerTargetKeyPrefix + providerID

	// Seed a FIRING alert for this provider.
	_, err = pool.Exec(ctx, `
		INSERT INTO "Alert" (id, "ruleId", "sourceType", "targetKey", "targetLabel",
		    severity, state, message, details, "firedAt", "lastSeenAt", "duplicateCount")
		VALUES (gen_random_uuid()::text, $1, 'provider', $2, 'Test Recovered Provider',
		    'CRITICAL'::"AlertSeverity", 'FIRING'::"AlertState", 'test', '{}',
		    NOW(), NOW(), 1)`,
		providerUnavailableRuleID, targetKey,
	)
	if err != nil {
		t.Fatalf("seed firing alert: %v", err)
	}
	defer pool.Exec(ctx, `DELETE FROM "Alert" WHERE "targetKey" = $1`, targetKey) //nolint:errcheck

	f := &fakeRaiser{}
	// Pass 0/0 for minDownSec / recoverySec so the single-tick
	// fire/resolve assertion still works.
	rule := providerUnavailableRule(0, 0)
	j := NewProviderUnavailableAlerts(pool, f, &fakeRuleLoader{rule: rule}, 60*time.Second, testLogger())

	if err := j.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	found := false
	for _, r := range f.resolves {
		if r.RuleID == providerUnavailableRuleID && r.TargetKey == targetKey && r.Reason == providerResolveReason {
			found = true
		}
	}
	if !found {
		t.Errorf("no recovered resolve for %s; resolves=%+v", targetKey, f.resolves)
	}
}

// TestProviderUnavailableAlerts_DegradedNotRaised seeds a provider with
// status='degraded' (not 'unavailable') and verifies Raise is NOT called.
func TestProviderUnavailableAlerts_DegradedNotRaised(t *testing.T) {
	pool := jobsTestPool(t)
	defer jobsTestCleanup(t, pool)

	ctx := context.Background()

	// Insert Provider.
	var providerID string
	err := pool.QueryRow(ctx, `
		INSERT INTO "Provider" (id, name, "displayName", adapter_type, "baseUrl", "pathPrefix", "createdAt", "updatedAt")
		VALUES (gen_random_uuid()::text, 'test-pua-degraded-provider', 'Test Degraded Provider', 'openai-compatible',
		        'https://api.openai.com', '/pua-degraded', NOW(), NOW())
		RETURNING id`).Scan(&providerID)
	if err != nil {
		t.Fatalf("insert provider: %v", err)
	}
	defer pool.Exec(ctx, `DELETE FROM "Provider" WHERE id = $1`, providerID) //nolint:errcheck

	// ProviderHealth status='degraded' — should NOT trigger provider.unavailable.
	_, err = pool.Exec(ctx, `
		INSERT INTO "ProviderHealth" (id, "providerId", provider, status, "rollingErrorRate",
		    "windowStart", "updatedAt")
		VALUES (gen_random_uuid()::text, $1, 'openai', 'degraded', 0.45,
		        NOW() - INTERVAL '5 minutes', NOW())`,
		providerID,
	)
	if err != nil {
		t.Fatalf("insert provider health: %v", err)
	}
	defer pool.Exec(ctx, `DELETE FROM "ProviderHealth" WHERE "providerId" = $1`, providerID) //nolint:errcheck

	f := &fakeRaiser{}
	// Pass 0/0 for minDownSec / recoverySec so the single-tick
	// fire/resolve assertion still works.
	rule := providerUnavailableRule(0, 0)
	j := NewProviderUnavailableAlerts(pool, f, &fakeRuleLoader{rule: rule}, 60*time.Second, testLogger())

	if err := j.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	wantTargetKey := providerTargetKeyPrefix + providerID
	for _, r := range f.raises {
		if r.TargetKey == wantTargetKey {
			t.Errorf("unexpected Raise for degraded provider %s", providerID)
		}
	}
}

package alerting_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
)

// testPool returns a pgx pool for alerting-store integration tests.
//
// Test-data isolation contract:
//   - AlertRule rows the tests seed all carry id LIKE 'test.%'.
//   - Alert rows reference one of the above test rules (ruleId LIKE 'test.%').
//   - AlertDispatch rows hang off those test Alerts (FK alertId).
//   - AlertChannel rows the tests seed all carry name LIKE 'test-%'.
//
// Every cleanup query below filters on these prefixes — no table-wide
// DELETE / TRUNCATE that could touch rows another developer or the seed
// migration created. Safe to run alongside live data, so no
// NEXUS_DESTRUCTIVE_TESTS gate.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("skip: DB unavailable (%v)", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Skipf("skip: DB ping failed (%v)", err)
	}

	// Pre-clean only test-prefixed rows so a previous run that aborted
	// mid-cleanup doesn't poison this run's assertions. Strictly scoped to
	// the test prefixes — no table-wide DELETE.
	scrubTestPrefixedRows(context.Background(), pool)

	return pool
}

// scrubTestPrefixedRows deletes ONLY rows under the test-data prefixes
// documented in testPool. Used by both the pre-test pool setup and the
// per-test cleanup so the two paths cannot drift.
func scrubTestPrefixedRows(ctx context.Context, pool *pgxpool.Pool) {
	// Order matters: dispatches → alerts → rules (FK chain), then channels
	// (independent of rules).
	_, _ = pool.Exec(ctx, `
		DELETE FROM "AlertDispatch"
		WHERE "alertId" IN (SELECT id FROM "Alert" WHERE "ruleId" LIKE 'test.%')
		   OR "channelId" IN (SELECT id FROM "AlertChannel" WHERE name LIKE 'test-%')
	`)
	_, _ = pool.Exec(ctx, `DELETE FROM "Alert" WHERE "ruleId" LIKE 'test.%'`)
	_, _ = pool.Exec(ctx, `DELETE FROM "AlertRule" WHERE id LIKE 'test.%'`)
	_, _ = pool.Exec(ctx, `DELETE FROM "AlertChannel" WHERE name LIKE 'test-%'`)
}

func cleanup(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	scrubTestPrefixedRows(context.Background(), pool)
	pool.Close()
}

// makeAlert returns a minimal Alert struct for insertion.
func makeAlert(ruleID, targetKey string) alerting.Alert {
	return alerting.Alert{
		RuleID:      ruleID,
		SourceType:  "test",
		TargetKey:   targetKey,
		TargetLabel: "Test Target",
		Severity:    alerting.SeverityMedium,
		State:       alerting.StateFiring,
		Message:     "test alert message",
		Details:     map[string]any{"k": "v"},
		FiredAt:     time.Now().UTC().Truncate(time.Millisecond),
		LastSeenAt:  time.Now().UTC().Truncate(time.Millisecond),
	}
}

func TestStore_InsertAlert(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	ctx := context.Background()

	// Need a rule first — insert directly via pool.
	_, err := pool.Exec(ctx, `
		INSERT INTO "AlertRule" (id, "displayName", "sourceType", "defaultSeverity", "requiresAck", enabled, params, "paramsSchema", "cooldownSec", "updatedAt")
		VALUES ('test.insert', 'Test Insert', 'test', 'MEDIUM'::"AlertSeverity", false, true, '{}', '{}', 60, NOW())
		ON CONFLICT (id) DO NOTHING`)
	if err != nil {
		t.Fatalf("seed rule: %v", err)
	}

	store := alerting.NewStore(pool)
	a := makeAlert("test.insert", "test:org:insert")

	id, err := store.InsertAlert(ctx, a)
	if err != nil {
		t.Fatalf("InsertAlert: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty id")
	}

	got, err := store.FindLatestByRuleTarget(ctx, "test.insert", "test:org:insert")
	if err != nil {
		t.Fatalf("FindLatestByRuleTarget: %v", err)
	}
	if got == nil {
		t.Fatal("expected alert row, got nil")
	}
	if got.ID != id {
		t.Errorf("id mismatch: want %s got %s", id, got.ID)
	}
	if got.State != alerting.StateFiring {
		t.Errorf("state: want firing got %s", got.State)
	}
}

func TestStore_UpdateFiringDuplicate(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	ctx := context.Background()
	_, _ = pool.Exec(ctx, `
		INSERT INTO "AlertRule" (id, "displayName", "sourceType", "defaultSeverity", "requiresAck", enabled, params, "paramsSchema", "cooldownSec", "updatedAt")
		VALUES ('test.dup', 'Test Dup', 'test', 'MEDIUM'::"AlertSeverity", false, true, '{}', '{}', 60, NOW())
		ON CONFLICT (id) DO NOTHING`)

	store := alerting.NewStore(pool)
	id, err := store.InsertAlert(ctx, makeAlert("test.dup", "test:org:dup"))
	if err != nil {
		t.Fatalf("InsertAlert: %v", err)
	}

	// Truncate to milliseconds: the DB column is timestamp(3), which stores only
	// 3 decimal digits of sub-second precision.
	now := time.Now().UTC().Add(time.Second).Truncate(time.Millisecond)
	if err := store.UpdateFiringDuplicate(ctx, id, now); err != nil {
		t.Fatalf("UpdateFiringDuplicate: %v", err)
	}

	got, err := store.FindLatestByRuleTarget(ctx, "test.dup", "test:org:dup")
	if err != nil {
		t.Fatalf("FindLatestByRuleTarget: %v", err)
	}
	if got.DuplicateCount != 1 {
		t.Errorf("duplicate_count: want 1 got %d", got.DuplicateCount)
	}
	gotLSA := got.LastSeenAt.UTC().Truncate(time.Millisecond)
	if !gotLSA.Equal(now) {
		t.Errorf("last_seen_at: want %v got %v", now, gotLSA)
	}
}

func TestStore_FindLatestByRuleTarget_ResolvedIgnored(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	ctx := context.Background()
	_, _ = pool.Exec(ctx, `
		INSERT INTO "AlertRule" (id, "displayName", "sourceType", "defaultSeverity", "requiresAck", enabled, params, "paramsSchema", "cooldownSec", "updatedAt")
		VALUES ('test.resolved', 'Test Resolved', 'test', 'MEDIUM'::"AlertSeverity", false, true, '{}', '{}', 60, NOW())
		ON CONFLICT (id) DO NOTHING`)

	store := alerting.NewStore(pool)
	id, err := store.InsertAlert(ctx, makeAlert("test.resolved", "test:org:resolved"))
	if err != nil {
		t.Fatalf("InsertAlert: %v", err)
	}

	if err := store.ResolveAlert(ctx, id, "system", "test cleanup"); err != nil {
		t.Fatalf("ResolveAlert: %v", err)
	}

	got, err := store.FindLatestByRuleTarget(ctx, "test.resolved", "test:org:resolved")
	if err != nil {
		t.Fatalf("FindLatestByRuleTarget: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil after resolve, got %+v", got)
	}
}

func TestStore_ListAlerts_Filters(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	ctx := context.Background()
	_, _ = pool.Exec(ctx, `
		INSERT INTO "AlertRule" (id, "displayName", "sourceType", "defaultSeverity", "requiresAck", enabled, params, "paramsSchema", "cooldownSec", "updatedAt")
		VALUES ('test.filter', 'Test Filter', 'test', 'MEDIUM'::"AlertSeverity", false, true, '{}', '{}', 60, NOW())
		ON CONFLICT (id) DO NOTHING`)

	store := alerting.NewStore(pool)

	// Insert 3 alerts: 2 firing, 1 resolved.
	id1, _ := store.InsertAlert(ctx, makeAlert("test.filter", "test:org:f1"))
	_, _ = store.InsertAlert(ctx, makeAlert("test.filter", "test:org:f2"))
	id3, _ := store.InsertAlert(ctx, makeAlert("test.filter", "test:org:f3"))
	_ = store.ResolveAlert(ctx, id3, "system", "done")

	t.Run("filter by state firing", func(t *testing.T) {
		rows, total, err := store.ListAlerts(ctx, alerting.ListFilter{State: []string{"firing"}, RuleID: []string{"test.filter"}, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if total != 2 {
			t.Errorf("want 2 firing, got total=%d", total)
		}
		if len(rows) != 2 {
			t.Errorf("want 2 rows, got %d", len(rows))
		}
	})

	t.Run("filter by state resolved", func(t *testing.T) {
		rows, total, err := store.ListAlerts(ctx, alerting.ListFilter{State: []string{"resolved"}, RuleID: []string{"test.filter"}, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if total != 1 {
			t.Errorf("want 1 resolved, got %d", total)
		}
		_ = rows
	})

	t.Run("pagination", func(t *testing.T) {
		rows, _, err := store.ListAlerts(ctx, alerting.ListFilter{RuleID: []string{"test.filter"}, Offset: 0, Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) > 2 {
			t.Errorf("want <= 2 rows, got %d", len(rows))
		}
	})

	_ = id1
}

// TestListAlerts_MultiStateFilter covers the multi-value state filter added
// for the admin UI inbox: a single ListAlerts call with State=[firing,
// acknowledged] must return rows in either state and exclude resolved ones.
// It also exercises the multi-value RuleID path via a single-element slice.
func TestListAlerts_MultiStateFilter(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	ctx := context.Background()
	_, _ = pool.Exec(ctx, `
		INSERT INTO "AlertRule" (id, "displayName", "sourceType", "defaultSeverity", "requiresAck", enabled, params, "paramsSchema", "cooldownSec", "updatedAt")
		VALUES ('test.multi', 'Test Multi', 'test', 'MEDIUM'::"AlertSeverity", false, true, '{}', '{}', 60, NOW())
		ON CONFLICT (id) DO NOTHING`)

	store := alerting.NewStore(pool)

	// Seed 3 alerts under rule "test.multi": one firing, one acknowledged,
	// one resolved. The multi-state filter must return the first two.
	_, _ = store.InsertAlert(ctx, makeAlert("test.multi", "test:org:m-firing"))
	ackID, _ := store.InsertAlert(ctx, makeAlert("test.multi", "test:org:m-ack"))
	resID, _ := store.InsertAlert(ctx, makeAlert("test.multi", "test:org:m-res"))
	if err := store.AcknowledgeAlert(ctx, ackID, "alice", ""); err != nil {
		t.Fatalf("AcknowledgeAlert: %v", err)
	}
	if err := store.ResolveAlert(ctx, resID, "alice", "done"); err != nil {
		t.Fatalf("ResolveAlert: %v", err)
	}

	rows, total, err := store.ListAlerts(ctx, alerting.ListFilter{
		State:  []string{"firing", "acknowledged"},
		RuleID: []string{"test.multi"},
		Limit:  10,
	})
	if err != nil {
		t.Fatalf("ListAlerts: %v", err)
	}
	if total != 2 {
		t.Errorf("total=%d want 2 (firing + acknowledged)", total)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows)=%d want 2", len(rows))
	}
	seen := map[alerting.State]int{}
	for _, r := range rows {
		seen[r.State]++
		if r.State == alerting.StateResolved {
			t.Errorf("resolved row leaked through multi-state filter: %+v", r)
		}
	}
	if seen[alerting.StateFiring] != 1 {
		t.Errorf("firing count=%d want 1", seen[alerting.StateFiring])
	}
	if seen[alerting.StateAcknowledged] != 1 {
		t.Errorf("acknowledged count=%d want 1", seen[alerting.StateAcknowledged])
	}
}

func TestStore_ChannelCRUD(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	ctx := context.Background()
	store := alerting.NewStore(pool)

	c := alerting.Channel{
		Name:        "test-chan-A",
		Type:        "webhook",
		Enabled:     true,
		Severities:  []alerting.Severity{alerting.SeverityCritical, alerting.SeverityHigh},
		SourceTypes: []string{"test"},
		Config:      map[string]any{"url": "http://example.com"},
	}

	id, err := store.InsertChannel(ctx, c)
	if err != nil {
		t.Fatalf("InsertChannel: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty id")
	}

	t.Run("get", func(t *testing.T) {
		got, err := store.GetChannel(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Name != c.Name {
			t.Errorf("name: want %s got %s", c.Name, got.Name)
		}
		if got.Type != c.Type {
			t.Errorf("type: want %s got %s", c.Type, got.Type)
		}
	})

	t.Run("list", func(t *testing.T) {
		list, err := store.ListChannels(ctx)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, ch := range list {
			if ch.ID == id {
				found = true
			}
		}
		if !found {
			t.Error("channel not found in list")
		}
	})

	t.Run("update", func(t *testing.T) {
		c.ID = id
		c.Name = "test-chan-A"
		c.Enabled = false
		if err := store.UpdateChannel(ctx, c); err != nil {
			t.Fatal(err)
		}
		got, _ := store.GetChannel(ctx, id)
		if got.Enabled {
			t.Error("expected disabled after update")
		}
	})

	t.Run("delete", func(t *testing.T) {
		if err := store.DeleteChannel(ctx, id); err != nil {
			t.Fatal(err)
		}
		_, err := store.GetChannel(ctx, id)
		if err == nil {
			t.Error("expected error after delete, got nil")
		}
	})
}

func TestStore_ListEnabledChannels(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	ctx := context.Background()
	store := alerting.NewStore(pool)

	// Insert one enabled, one disabled.
	_, _ = store.InsertChannel(ctx, alerting.Channel{
		Name: "test-enabled", Type: "webhook", Enabled: true,
		Severities: []alerting.Severity{alerting.SeverityCritical}, SourceTypes: []string{"test"},
		Config: map[string]any{"url": "http://a.example.com"},
	})
	disabledID, _ := store.InsertChannel(ctx, alerting.Channel{
		Name: "test-disabled", Type: "webhook", Enabled: false,
		Severities: []alerting.Severity{alerting.SeverityCritical}, SourceTypes: []string{"test"},
		Config: map[string]any{"url": "http://b.example.com"},
	})

	list, err := store.ListEnabledChannels(ctx)
	if err != nil {
		t.Fatalf("ListEnabledChannels: %v", err)
	}
	for _, ch := range list {
		if ch.ID == disabledID {
			t.Error("disabled channel must not appear in ListEnabledChannels")
		}
		if !ch.Enabled {
			t.Errorf("channel %s has enabled=false in ListEnabledChannels", ch.ID)
		}
	}
}

func TestStore_DispatchInsert_ListByAlert(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	ctx := context.Background()
	_, _ = pool.Exec(ctx, `
		INSERT INTO "AlertRule" (id, "displayName", "sourceType", "defaultSeverity", "requiresAck", enabled, params, "paramsSchema", "cooldownSec", "updatedAt")
		VALUES ('test.dispatch', 'Test Dispatch', 'test', 'MEDIUM'::"AlertSeverity", false, true, '{}', '{}', 60, NOW())
		ON CONFLICT (id) DO NOTHING`)

	store := alerting.NewStore(pool)
	alertID, err := store.InsertAlert(ctx, makeAlert("test.dispatch", "test:org:dispatch"))
	if err != nil {
		t.Fatalf("InsertAlert: %v", err)
	}

	chanID, _ := store.InsertChannel(ctx, alerting.Channel{
		Name: "test-dispatch-chan", Type: "webhook", Enabled: true,
		Severities: []alerting.Severity{alerting.SeverityMedium}, SourceTypes: []string{"test"},
		Config: map[string]any{"url": "http://dispatch.example.com"},
	})

	code := 200
	d1 := alerting.Dispatch{
		AlertID: alertID, ChannelID: chanID, ChannelName: "test-dispatch-chan",
		Success: true, StatusCode: &code, AttemptedAt: time.Now().UTC(),
	}
	d2 := alerting.Dispatch{
		AlertID: alertID, ChannelID: chanID, ChannelName: "test-dispatch-chan",
		Success: false, ErrorMsg: func() *string { s := "timeout"; return &s }(),
		AttemptedAt: time.Now().UTC(),
	}

	id1, err := store.InsertDispatch(ctx, d1)
	if err != nil {
		t.Fatalf("InsertDispatch 1: %v", err)
	}
	id2, err := store.InsertDispatch(ctx, d2)
	if err != nil {
		t.Fatalf("InsertDispatch 2: %v", err)
	}

	list, err := store.ListDispatchesByAlert(ctx, alertID)
	if err != nil {
		t.Fatalf("ListDispatchesByAlert: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("want 2 dispatches, got %d", len(list))
	}

	ids := map[string]bool{id1: true, id2: true}
	for _, d := range list {
		if !ids[d.ID] {
			t.Errorf("unexpected dispatch id %s", d.ID)
		}
	}
}

func TestStore_Rules(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	ctx := context.Background()
	store := alerting.NewStore(pool)

	// Seed a test-scoped rule so each subtest reads/updates a row this
	// test owns. The earlier "first seeded rule" pattern modified a
	// migration-seeded rule (cross-test data mutation) — forbidden by
	// the tests-only-own-data rule.
	const ruleID = "test.store-rules"
	if _, err := pool.Exec(ctx, `
		INSERT INTO "AlertRule" (id, "displayName", "sourceType", "defaultSeverity", "requiresAck", enabled, params, "paramsSchema", "cooldownSec", "updatedAt")
		VALUES ($1, 'Test Store Rules', 'test', 'MEDIUM'::"AlertSeverity", false, true, '{}', '{}', 60, NOW())
		ON CONFLICT (id) DO UPDATE SET enabled = true, "cooldownSec" = 60`, ruleID); err != nil {
		t.Fatalf("seed rule: %v", err)
	}

	t.Run("ListRules returns rows", func(t *testing.T) {
		rules, _, err := store.ListRules(ctx, alerting.ListRulesParams{Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		// The test-owned rule above guarantees a non-empty list even on
		// a stripped-down DB.
		found := false
		for _, r := range rules {
			if r.ID == ruleID {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("seeded test rule %q not in ListRules result", ruleID)
		}
	})

	t.Run("GetRule returns seeded rule", func(t *testing.T) {
		r, err := store.GetRule(ctx, ruleID)
		if err != nil {
			t.Fatal(err)
		}
		if r.ID != ruleID {
			t.Errorf("id mismatch: got %s want %s", r.ID, ruleID)
		}
	})

	t.Run("UpdateRule patches enabled+cooldown", func(t *testing.T) {
		rp, err := store.GetRule(ctx, ruleID)
		if err != nil {
			t.Fatalf("GetRule: %v", err)
		}
		r := *rp
		orig := r.Enabled
		r.Enabled = !orig
		r.CooldownSec += 10
		if err := store.UpdateRule(ctx, r); err != nil {
			t.Fatal(err)
		}
		got, _ := store.GetRule(ctx, r.ID)
		if got.Enabled == orig {
			t.Error("Enabled not updated")
		}
		if got.CooldownSec != r.CooldownSec {
			t.Errorf("CooldownSec = %d, want %d", got.CooldownSec, r.CooldownSec)
		}
		// No restore needed — cleanup() deletes the test-prefixed rule
		// at end of test, so no cross-test mutation is left behind.
	})
}

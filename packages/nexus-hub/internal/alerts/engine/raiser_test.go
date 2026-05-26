package alerting_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
)

// fakeDispatcher captures Dispatch calls from the Raiser. Dispatch runs in a
// goroutine, so access is guarded by a mutex.
type fakeDispatcher struct {
	mu    sync.Mutex
	calls []alerting.Alert
}

func (f *fakeDispatcher) Dispatch(_ context.Context, a alerting.Alert) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, a)
}

func (f *fakeDispatcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// waitFor polls fn until it returns true or the deadline expires.
func waitFor(t *testing.T, fn func() bool, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return fn()
}

// seedRaiserRule inserts a minimal enabled AlertRule for Raiser tests.
//
// cooldownSec=0 disables cooldown enforcement so post-ack / post-resolve
// reraises immediately create a fresh row — the semantics most tests need
// when they're not specifically exercising cooldown.
func seedRaiserRule(t *testing.T, pool *pgxpool.Pool, id string, cooldownSec int) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO "AlertRule" (id, "displayName", "sourceType", "defaultSeverity", "requiresAck", enabled, params, "paramsSchema", "cooldownSec", "updatedAt")
		VALUES ($1, 'Test Raiser Rule', 'test', 'MEDIUM'::"AlertSeverity", false, true, '{}', '{}', $2, NOW())
		ON CONFLICT (id) DO UPDATE SET "cooldownSec" = EXCLUDED."cooldownSec"`,
		id, cooldownSec,
	)
	if err != nil {
		t.Fatalf("seed rule %q: %v", id, err)
	}
}

// alertRow is a minimal projection of an "Alert" row used for assertions.
type alertRow struct {
	ID             string
	State          string
	DuplicateCount int
}

// dumpAlerts returns all alert rows for the given (ruleId, targetKey), ordered
// by firedAt ASC. Useful for asserting how many rows exist and their states.
func dumpAlerts(t *testing.T, pool *pgxpool.Pool, ruleID, targetKey string) []alertRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT id, state::text, "duplicateCount"
		FROM "Alert"
		WHERE "ruleId" = $1 AND "targetKey" = $2
		ORDER BY "firedAt" ASC`,
		ruleID, targetKey,
	)
	if err != nil {
		t.Fatalf("dumpAlerts query: %v", err)
	}
	defer rows.Close()

	var out []alertRow
	for rows.Next() {
		var r alertRow
		if err := rows.Scan(&r.ID, &r.State, &r.DuplicateCount); err != nil {
			t.Fatalf("dumpAlerts scan: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("dumpAlerts rows err: %v", err)
	}
	return out
}

func newTestRaiser(pool *pgxpool.Pool) (*alerting.Raiser, *alerting.Store, *fakeDispatcher) {
	store := alerting.NewStore(pool)
	d := &fakeDispatcher{}
	r := alerting.NewRaiser(pool, store, d, slog.Default())
	return r, store, d
}

func TestRaiser_NewAlertInsertsFiring(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	const ruleID = "test.raiser.new"
	const target = "test:org:new"
	seedRaiserRule(t, pool, ruleID, 0)

	ctx := context.Background()
	r, _, d := newTestRaiser(pool)

	in := alerting.RaiseInput{
		RuleID:      ruleID,
		TargetKey:   target,
		TargetLabel: "Org New",
		Severity:    alerting.SeverityHigh,
		Message:     "new alert",
		Details:     map[string]any{"k": "v"},
	}
	if err := r.Raise(ctx, in); err != nil {
		t.Fatalf("Raise: %v", err)
	}

	rows := dumpAlerts(t, pool, ruleID, target)
	if len(rows) != 1 {
		t.Fatalf("want 1 alert row, got %d", len(rows))
	}
	if rows[0].State != "FIRING" {
		t.Errorf("state: want FIRING got %s", rows[0].State)
	}
	if rows[0].DuplicateCount != 1 {
		t.Errorf("duplicateCount: want 1 got %d", rows[0].DuplicateCount)
	}

	if !waitFor(t, func() bool { return d.callCount() == 1 }, 2*time.Second) {
		t.Fatalf("dispatcher: want 1 call, got %d", d.callCount())
	}
}

func TestRaiser_RepeatFiringUpdatesDuplicate(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	const ruleID = "test.raiser.dup"
	const target = "test:org:dup"
	seedRaiserRule(t, pool, ruleID, 0)

	ctx := context.Background()
	r, _, d := newTestRaiser(pool)

	in := alerting.RaiseInput{
		RuleID:      ruleID,
		TargetKey:   target,
		TargetLabel: "Org Dup",
		Severity:    alerting.SeverityHigh,
		Message:     "m",
		Details:     map[string]any{"a": 1},
	}
	for i := range 3 {
		if err := r.Raise(ctx, in); err != nil {
			t.Fatalf("Raise %d: %v", i, err)
		}
	}

	rows := dumpAlerts(t, pool, ruleID, target)
	if len(rows) != 1 {
		t.Fatalf("want 1 alert row (dedup), got %d", len(rows))
	}
	if rows[0].State != "FIRING" {
		t.Errorf("state: want FIRING got %s", rows[0].State)
	}
	if rows[0].DuplicateCount != 3 {
		t.Errorf("duplicateCount: want 3 got %d", rows[0].DuplicateCount)
	}

	// Only the first (INSERT) path dispatches; subsequent duplicates do not.
	// Give the first dispatch goroutine time to land and ensure no more follow.
	if !waitFor(t, func() bool { return d.callCount() == 1 }, 2*time.Second) {
		t.Fatalf("dispatcher: want 1 call after first INSERT, got %d", d.callCount())
	}
	time.Sleep(50 * time.Millisecond)
	if got := d.callCount(); got != 1 {
		t.Errorf("dispatcher: want exactly 1 call total, got %d", got)
	}
}

func TestRaiser_AfterAcknowledgedCreatesSecondRow(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	const ruleID = "test.raiser.ack"
	const target = "test:org:ack"
	seedRaiserRule(t, pool, ruleID, 0)

	ctx := context.Background()
	r, store, d := newTestRaiser(pool)

	in := alerting.RaiseInput{
		RuleID:      ruleID,
		TargetKey:   target,
		TargetLabel: "Org Ack",
		Severity:    alerting.SeverityHigh,
		Message:     "m",
	}
	if err := r.Raise(ctx, in); err != nil {
		t.Fatalf("first Raise: %v", err)
	}

	// Find the firing alert's id and acknowledge it.
	latest, err := store.FindLatestByRuleTarget(ctx, ruleID, target)
	if err != nil {
		t.Fatalf("FindLatestByRuleTarget: %v", err)
	}
	if latest == nil {
		t.Fatal("expected firing alert to exist")
	}
	//nolint:staticcheck // SA5011: t.Fatal above terminates the test goroutine
	if err := store.AcknowledgeAlert(ctx, latest.ID, "alice", "ack reason"); err != nil {
		t.Fatalf("AcknowledgeAlert: %v", err)
	}

	// Second Raise after acknowledgement must create a new FIRING row.
	if err := r.Raise(ctx, in); err != nil {
		t.Fatalf("second Raise: %v", err)
	}

	rows := dumpAlerts(t, pool, ruleID, target)
	if len(rows) != 2 {
		t.Fatalf("want 2 alert rows (ack then fresh firing), got %d", len(rows))
	}
	// Oldest first: the acknowledged row, then the new firing row.
	if rows[0].State != "ACKNOWLEDGED" {
		t.Errorf("row[0] state: want ACKNOWLEDGED got %s", rows[0].State)
	}
	if rows[1].State != "FIRING" {
		t.Errorf("row[1] state: want FIRING got %s", rows[1].State)
	}

	if !waitFor(t, func() bool { return d.callCount() == 2 }, 2*time.Second) {
		t.Fatalf("dispatcher: want 2 calls, got %d", d.callCount())
	}
}

func TestRaiser_AfterResolvedCreatesSecondRow(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	const ruleID = "test.raiser.resolved"
	const target = "test:org:resolved"
	seedRaiserRule(t, pool, ruleID, 0)

	ctx := context.Background()
	r, store, d := newTestRaiser(pool)

	in := alerting.RaiseInput{
		RuleID:      ruleID,
		TargetKey:   target,
		TargetLabel: "Org Resolved",
		Severity:    alerting.SeverityHigh,
		Message:     "m",
	}
	if err := r.Raise(ctx, in); err != nil {
		t.Fatalf("first Raise: %v", err)
	}

	latest, err := store.FindLatestByRuleTarget(ctx, ruleID, target)
	if err != nil {
		t.Fatalf("FindLatestByRuleTarget: %v", err)
	}
	if latest == nil {
		t.Fatal("expected firing alert to exist")
	}
	//nolint:staticcheck // SA5011: t.Fatal above terminates the test goroutine
	if err := store.ResolveAlert(ctx, latest.ID, "system", "clear"); err != nil {
		t.Fatalf("ResolveAlert: %v", err)
	}

	if err := r.Raise(ctx, in); err != nil {
		t.Fatalf("second Raise: %v", err)
	}

	rows := dumpAlerts(t, pool, ruleID, target)
	if len(rows) != 2 {
		t.Fatalf("want 2 alert rows (resolved then fresh firing), got %d", len(rows))
	}
	if rows[0].State != "RESOLVED" {
		t.Errorf("row[0] state: want RESOLVED got %s", rows[0].State)
	}
	if rows[1].State != "FIRING" {
		t.Errorf("row[1] state: want FIRING got %s", rows[1].State)
	}

	if !waitFor(t, func() bool { return d.callCount() == 2 }, 2*time.Second) {
		t.Fatalf("dispatcher: want 2 calls, got %d", d.callCount())
	}
}

func TestRaiser_ResolveResolvesFiringAndAcked(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	const ruleID = "test.raiser.resolveboth"
	const target = "test:org:resolveboth"
	seedRaiserRule(t, pool, ruleID, 0)

	ctx := context.Background()
	r, store, _ := newTestRaiser(pool)

	in := alerting.RaiseInput{
		RuleID:      ruleID,
		TargetKey:   target,
		TargetLabel: "Org Both",
		Severity:    alerting.SeverityHigh,
		Message:     "m",
	}
	// 1) Raise → FIRING (first row)
	if err := r.Raise(ctx, in); err != nil {
		t.Fatalf("first Raise: %v", err)
	}
	first, err := store.FindLatestByRuleTarget(ctx, ruleID, target)
	if err != nil || first == nil {
		t.Fatalf("expected firing row, err=%v", err)
	}
	// 2) Ack → ACKNOWLEDGED
	if err := store.AcknowledgeAlert(ctx, first.ID, "alice", "looking"); err != nil {
		t.Fatalf("AcknowledgeAlert: %v", err)
	}
	// 3) Raise → second FIRING row
	if err := r.Raise(ctx, in); err != nil {
		t.Fatalf("second Raise: %v", err)
	}

	// Sanity — 2 rows in the right states (ack + firing).
	rows := dumpAlerts(t, pool, ruleID, target)
	if len(rows) != 2 {
		t.Fatalf("setup: want 2 rows, got %d", len(rows))
	}

	// 4) ResolveByRuleTarget should resolve BOTH rows (ACKNOWLEDGED + FIRING).
	if err := r.Resolve(ctx, ruleID, target, "auto-clear"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	rows = dumpAlerts(t, pool, ruleID, target)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows total, got %d", len(rows))
	}
	for i, r := range rows {
		if r.State != "RESOLVED" {
			t.Errorf("row[%d] state: want RESOLVED got %s", i, r.State)
		}
	}
}

func TestRaiser_ConcurrentRaiseNoDuplicates(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	const ruleID = "test.raiser.concurrent"
	const target = "test:org:concurrent"
	seedRaiserRule(t, pool, ruleID, 0)

	ctx := context.Background()
	r, _, d := newTestRaiser(pool)

	in := alerting.RaiseInput{
		RuleID:      ruleID,
		TargetKey:   target,
		TargetLabel: "Org Concurrent",
		Severity:    alerting.SeverityHigh,
		Message:     "m",
	}

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			if err := r.Raise(ctx, in); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("goroutine Raise: %v", err)
	}

	rows := dumpAlerts(t, pool, ruleID, target)
	if len(rows) != 1 {
		t.Fatalf("want exactly 1 row after %d concurrent Raises, got %d", n, len(rows))
	}
	if rows[0].State != "FIRING" {
		t.Errorf("state: want FIRING got %s", rows[0].State)
	}
	if rows[0].DuplicateCount != n {
		t.Errorf("duplicateCount: want %d got %d", n, rows[0].DuplicateCount)
	}

	// Exactly one dispatch (the winning INSERT); losers UPDATE and do not dispatch.
	if !waitFor(t, func() bool { return d.callCount() == 1 }, 2*time.Second) {
		t.Fatalf("dispatcher: want 1 call, got %d", d.callCount())
	}
	time.Sleep(50 * time.Millisecond)
	if got := d.callCount(); got != 1 {
		t.Errorf("dispatcher: want exactly 1 call total, got %d", got)
	}
}

//
// Cooldown silences repeat fires of the same (rule, target) across the full
// alert lifecycle — including post-ack and post-resolve — for cooldownSec
// after the most recent firedAt. Within the window, Raise bumps duplicateCount
// on the most-recent row regardless of state and does not dispatch.

func TestRaiser_CooldownDeduplicatesAfterAcknowledged(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	const ruleID = "test.raiser.cooldown.ack"
	const target = "test:org:cooldown-ack"
	// 1 hour cooldown — second Raise (microseconds later) is well within.
	seedRaiserRule(t, pool, ruleID, 3600)

	ctx := context.Background()
	r, store, d := newTestRaiser(pool)

	in := alerting.RaiseInput{
		RuleID:      ruleID,
		TargetKey:   target,
		TargetLabel: "Org Cooldown Ack",
		Severity:    alerting.SeverityHigh,
		Message:     "m",
	}
	if err := r.Raise(ctx, in); err != nil {
		t.Fatalf("first Raise: %v", err)
	}
	latest, err := store.FindLatestByRuleTarget(ctx, ruleID, target)
	if err != nil || latest == nil {
		t.Fatalf("expected firing alert, err=%v", err)
	}
	if err := store.AcknowledgeAlert(ctx, latest.ID, "alice", "looking"); err != nil {
		t.Fatalf("AcknowledgeAlert: %v", err)
	}
	if err := r.Raise(ctx, in); err != nil {
		t.Fatalf("second Raise (within cooldown): %v", err)
	}

	rows := dumpAlerts(t, pool, ruleID, target)
	if len(rows) != 1 {
		t.Fatalf("want 1 row (cooldown dedup post-ack), got %d", len(rows))
	}
	if rows[0].State != "ACKNOWLEDGED" {
		t.Errorf("state: want ACKNOWLEDGED (unchanged), got %s", rows[0].State)
	}
	if rows[0].DuplicateCount != 2 {
		t.Errorf("duplicateCount: want 2 (initial + cooldown bump), got %d", rows[0].DuplicateCount)
	}

	// Only the first INSERT dispatches; cooldown-suppressed Raise must not.
	if !waitFor(t, func() bool { return d.callCount() == 1 }, 2*time.Second) {
		t.Fatalf("dispatcher: want 1 call (first only), got %d", d.callCount())
	}
	time.Sleep(50 * time.Millisecond)
	if got := d.callCount(); got != 1 {
		t.Errorf("dispatcher: want exactly 1 call total, got %d", got)
	}
}

func TestRaiser_CooldownDeduplicatesAfterResolved(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	const ruleID = "test.raiser.cooldown.resolved"
	const target = "test:org:cooldown-resolved"
	seedRaiserRule(t, pool, ruleID, 3600)

	ctx := context.Background()
	r, store, d := newTestRaiser(pool)

	in := alerting.RaiseInput{
		RuleID:      ruleID,
		TargetKey:   target,
		TargetLabel: "Org Cooldown Resolved",
		Severity:    alerting.SeverityHigh,
		Message:     "m",
	}
	if err := r.Raise(ctx, in); err != nil {
		t.Fatalf("first Raise: %v", err)
	}
	latest, err := store.FindLatestByRuleTarget(ctx, ruleID, target)
	if err != nil || latest == nil {
		t.Fatalf("expected firing alert, err=%v", err)
	}
	if err := store.ResolveAlert(ctx, latest.ID, "system", "auto-clear"); err != nil {
		t.Fatalf("ResolveAlert: %v", err)
	}
	if err := r.Raise(ctx, in); err != nil {
		t.Fatalf("second Raise (within cooldown): %v", err)
	}

	rows := dumpAlerts(t, pool, ruleID, target)
	if len(rows) != 1 {
		t.Fatalf("want 1 row (cooldown dedup post-resolve), got %d", len(rows))
	}
	if rows[0].State != "RESOLVED" {
		t.Errorf("state: want RESOLVED (unchanged), got %s", rows[0].State)
	}
	if rows[0].DuplicateCount != 2 {
		t.Errorf("duplicateCount: want 2, got %d", rows[0].DuplicateCount)
	}

	if !waitFor(t, func() bool { return d.callCount() == 1 }, 2*time.Second) {
		t.Fatalf("dispatcher: want 1 call, got %d", d.callCount())
	}
	time.Sleep(50 * time.Millisecond)
	if got := d.callCount(); got != 1 {
		t.Errorf("dispatcher: want exactly 1 call total, got %d", got)
	}
}

func TestRaiser_CooldownExpiredAllowsFreshFire(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	const ruleID = "test.raiser.cooldown.expired"
	const target = "test:org:cooldown-expired"
	// 60s cooldown; we'll fire the second Raise 61s later via explicit FiredAt.
	seedRaiserRule(t, pool, ruleID, 60)

	ctx := context.Background()
	r, store, d := newTestRaiser(pool)

	base := time.Now().UTC().Add(-2 * time.Hour) // arbitrary anchor
	first := alerting.RaiseInput{
		RuleID:      ruleID,
		TargetKey:   target,
		TargetLabel: "Org Cooldown Expired",
		Severity:    alerting.SeverityHigh,
		Message:     "m",
		FiredAt:     base,
	}
	if err := r.Raise(ctx, first); err != nil {
		t.Fatalf("first Raise: %v", err)
	}
	latest, err := store.FindLatestByRuleTarget(ctx, ruleID, target)
	if err != nil || latest == nil {
		t.Fatalf("expected firing alert, err=%v", err)
	}
	if err := store.ResolveAlert(ctx, latest.ID, "system", "clear"); err != nil {
		t.Fatalf("ResolveAlert: %v", err)
	}

	second := first
	second.FiredAt = base.Add(61 * time.Second) // past 60s cooldown
	if err := r.Raise(ctx, second); err != nil {
		t.Fatalf("second Raise (cooldown expired): %v", err)
	}

	rows := dumpAlerts(t, pool, ruleID, target)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows (resolved + fresh firing past cooldown), got %d", len(rows))
	}
	if rows[0].State != "RESOLVED" {
		t.Errorf("row[0] state: want RESOLVED got %s", rows[0].State)
	}
	if rows[1].State != "FIRING" {
		t.Errorf("row[1] state: want FIRING got %s", rows[1].State)
	}

	// Two dispatches: one per INSERT.
	if !waitFor(t, func() bool { return d.callCount() == 2 }, 2*time.Second) {
		t.Fatalf("dispatcher: want 2 calls, got %d", d.callCount())
	}
}

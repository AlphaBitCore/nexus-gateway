package expiry

import (
	"bytes"
	"context"
	"errors"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/fleet/manager"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// overrideExpiryTestPool returns a pgx pool against the dev DB. Skips
// when the DB is unreachable. NOTE: The job's production lister
// (ListExpiredOverrides) returns EVERY expired override in the table,
// so a naive test would have ClearOverride touch foreign rows. Each
// DB-touching test below wraps the real store with prefixFilteringLister
// so the job only ever sees rows whose thing_id matches the test prefix.
func overrideExpiryTestPool(t *testing.T) *pgxpool.Pool {
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

// prefixFilteringLister wraps a real overrideExpiryLister and drops any
// returned row whose ThingID does not start with `prefix`. Used by the
// DB-touching tests below so the job's clear loop can never touch rows
// the test did not seed.
type prefixFilteringLister struct {
	inner  overrideExpiryLister
	prefix string
}

func (p prefixFilteringLister) ListExpiredOverrides(ctx context.Context, before time.Time) ([]store.ThingConfigOverride, error) {
	all, err := p.inner.ListExpiredOverrides(ctx, before)
	if err != nil {
		return nil, err
	}
	out := make([]store.ThingConfigOverride, 0, len(all))
	for _, o := range all {
		if strings.HasPrefix(o.ThingID, p.prefix) {
			out = append(out, o)
		}
	}
	return out, nil
}

const overrideExpiryPrefix = "override-expiry-test-"

// seedThingForExpiry inserts a minimal thing row so the FK on
// thing_config_override holds.
func seedThingForExpiry(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id, ttype string) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO thing (id, type, name, version, address, enrolled_by, auth_type, conn_protocol,
		                   status, metadata, desired, reported, desired_ver, reported_ver, last_seen_at, enrolled_at, updated_at)
		VALUES ($1, $2, $3, '1.0.0', '127.0.0.1', 'tester', 'bearer', 'http',
		        'online', '{}', '{}', '{}', 0, 0, NOW(), NOW(), NOW())
		ON CONFLICT (id) DO UPDATE SET type = EXCLUDED.type
	`, id, ttype, id)
	if err != nil {
		t.Fatalf("seed thing %s: %v", id, err)
	}
}

// seedTemplateForExpiry writes a thing_config_template row so the
// recompute-on-clear path inside Manager.ClearOverride finds a base state.
func seedTemplateForExpiry(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ttype, key string, version int64, state map[string]any) {
	t.Helper()
	stateJSON, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal template state: %v", err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO thing_config_template (type, config_key, state, version, updated_at, updated_by)
		VALUES ($1, $2, $3::jsonb, $4, NOW(), 'tester')
		ON CONFLICT (type, config_key) DO UPDATE SET
			state = EXCLUDED.state,
			version = EXCLUDED.version,
			updated_at = NOW()
	`, ttype, key, stateJSON, version)
	if err != nil {
		t.Fatalf("seed template %s/%s: %v", ttype, key, err)
	}
}

// seedExpiredOverride INSERTs a thing_config_override row with set_at well in
// the past and an expires_at also in the past. We can't go through
// UpsertOverride because that helper sets set_at=NOW(), which breaks the
// chk_tco_expires_set CHECK (expires_at > set_at) when expires_at must be in
// the past.
func seedExpiredOverride(t *testing.T, ctx context.Context, pool *pgxpool.Pool, thingID, key string, expiresAt time.Time) {
	t.Helper()
	setAt := expiresAt.Add(-time.Hour)
	_, err := pool.Exec(ctx, `
		INSERT INTO thing_config_override (
			thing_id, config_key, state, template_ver_at_set,
			set_by, set_at, reason, expires_at, emergency_override
		) VALUES ($1, $2, $3::jsonb, 1, 'alice', $4, NULL, $5, false)
	`, thingID, key, []byte(`{"weight":99}`), setAt, expiresAt)
	if err != nil {
		t.Fatalf("seed expired override %s/%s: %v", thingID, key, err)
	}
}

// seedFreshOverride INSERTs a thing_config_override row with expires_at in
// the future. The expiry job must NOT touch this row.
func seedFreshOverride(t *testing.T, ctx context.Context, pool *pgxpool.Pool, thingID, key string) {
	t.Helper()
	now := time.Now().UTC()
	_, err := pool.Exec(ctx, `
		INSERT INTO thing_config_override (
			thing_id, config_key, state, template_ver_at_set,
			set_by, set_at, reason, expires_at, emergency_override
		) VALUES ($1, $2, $3::jsonb, 1, 'alice', $4, NULL, $5, false)
	`, thingID, key, []byte(`{"weight":50}`), now.Add(-time.Minute), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("seed fresh override %s/%s: %v", thingID, key, err)
	}
}

func cleanupExpiryPrefix(ctx context.Context, pool *pgxpool.Pool, prefix string) {
	_, _ = pool.Exec(ctx, `DELETE FROM thing_config_override WHERE thing_id LIKE $1`, prefix+"%")
	_, _ = pool.Exec(ctx, `DELETE FROM "AdminAuditLog" WHERE "entityId" LIKE $1`, prefix+"%")
	_, _ = pool.Exec(ctx, `DELETE FROM thing WHERE id LIKE $1`, prefix+"%")
	_, _ = pool.Exec(ctx, `DELETE FROM thing_config_template WHERE config_key LIKE $1`, prefix+"%")
}

// expiryWS is a minimal WSPool: every Thing reports as connected so
// Manager.ClearOverride's post-commit RePushConfigKey takes the in-memory
// branch instead of attempting MQ publish.
type expiryWS struct{}

func (expiryWS) Send(thingID string, msg []byte) bool       { return true }
func (expiryWS) Broadcast(thingType string, msg []byte) int { return 0 }
func (expiryWS) IsConnected(thingID string) bool            { return true }

func newExpiryTestManager(pool *pgxpool.Pool, logger *slog.Logger) *manager.Manager {
	return manager.New(store.New(pool), nil, nil, expiryWS{}, "hub-test", logger)
}

func countAuditRowsForExpiry(t *testing.T, ctx context.Context, pool *pgxpool.Pool, entityID, action, actorID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM "AdminAuditLog"
		WHERE "entityId" = $1 AND action = $2 AND "actorId" = $3
	`, entityID, action, actorID).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	return n
}

func overrideRowExists(t *testing.T, ctx context.Context, pool *pgxpool.Pool, thingID, key string) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM thing_config_override WHERE thing_id = $1 AND config_key = $2
	`, thingID, key).Scan(&n); err != nil {
		t.Fatalf("count override rows: %v", err)
	}
	return n > 0
}

func TestOverrideExpiry_Identity(t *testing.T) {
	j := NewOverrideExpiry(nil, nil, time.Minute, nil, testLogger())
	if j.ID() != "override-expiry" {
		t.Errorf("ID = %q, want override-expiry", j.ID())
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
	if !j.RunOnStart() {
		t.Error("RunOnStart = false, want true")
	}
}

func TestOverrideExpiry_NoExpiredRows(t *testing.T) {
	pool := overrideExpiryTestPool(t)
	defer pool.Close()
	ctx := context.Background()

	prefix := overrideExpiryPrefix + "noop-"
	cleanupExpiryPrefix(ctx, pool, prefix)
	defer cleanupExpiryPrefix(ctx, pool, prefix)

	thingID := prefix + "agent-1"
	key := prefix + "routing"
	seedThingForExpiry(t, ctx, pool, thingID, "agent")
	seedTemplateForExpiry(t, ctx, pool, "agent", key, 2, map[string]any{"weight": 1})
	seedFreshOverride(t, ctx, pool, thingID, key)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := newExpiryTestManager(pool, logger)
	j := NewOverrideExpiry(store.New(pool), mgr, time.Minute, nil, logger)
	// Filter the production lister so the job only ever sees rows from
	// THIS test's prefix — fresh-row, expired-row, anything inserted by
	// other test runs or real dev usage outside the prefix is invisible.
	j.st = prefixFilteringLister{inner: j.st, prefix: prefix}

	if err := j.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !overrideRowExists(t, ctx, pool, thingID, key) {
		t.Error("fresh override row was deleted; want preserved")
	}
	if n := countAuditRowsForExpiry(t, ctx, pool, thingID, "thing_override_cleared", "system:override-expiry-job"); n != 0 {
		t.Errorf("audit rows = %d, want 0 (no-op)", n)
	}
}

func TestOverrideExpiry_ClearsExpiredKeepsFresh(t *testing.T) {
	pool := overrideExpiryTestPool(t)
	defer pool.Close()
	ctx := context.Background()

	prefix := overrideExpiryPrefix + "mixed-"
	cleanupExpiryPrefix(ctx, pool, prefix)
	defer cleanupExpiryPrefix(ctx, pool, prefix)

	thingID := prefix + "agent-2"
	expiredKey := prefix + "policy"
	freshKey := prefix + "routing"

	seedThingForExpiry(t, ctx, pool, thingID, "agent")
	seedTemplateForExpiry(t, ctx, pool, "agent", expiredKey, 2, map[string]any{"level": "low"})
	seedTemplateForExpiry(t, ctx, pool, "agent", freshKey, 3, map[string]any{"weight": 1})

	now := time.Now().UTC()
	seedExpiredOverride(t, ctx, pool, thingID, expiredKey, now.Add(-time.Minute))
	seedFreshOverride(t, ctx, pool, thingID, freshKey)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := newExpiryTestManager(pool, logger)
	j := NewOverrideExpiry(store.New(pool), mgr, time.Minute, nil, logger)
	j.st = prefixFilteringLister{inner: j.st, prefix: prefix}

	if err := j.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Expired row gone, fresh row preserved.
	if overrideRowExists(t, ctx, pool, thingID, expiredKey) {
		t.Error("expired override still present; want deleted")
	}
	if !overrideRowExists(t, ctx, pool, thingID, freshKey) {
		t.Error("fresh override was deleted; want preserved")
	}

	// Audit row written under the system actor.
	if n := countAuditRowsForExpiry(t, ctx, pool, thingID, "thing_override_cleared", "system:override-expiry-job"); n != 1 {
		t.Errorf("audit rows = %d, want exactly 1", n)
	}
}

func TestOverrideExpiry_ClearsMultipleExpired(t *testing.T) {
	pool := overrideExpiryTestPool(t)
	defer pool.Close()
	ctx := context.Background()

	prefix := overrideExpiryPrefix + "multi-"
	cleanupExpiryPrefix(ctx, pool, prefix)
	defer cleanupExpiryPrefix(ctx, pool, prefix)

	thingA := prefix + "agent-a"
	thingB := prefix + "agent-b"
	keyA := prefix + "policy-a"
	keyB := prefix + "routing-b"

	seedThingForExpiry(t, ctx, pool, thingA, "agent")
	seedThingForExpiry(t, ctx, pool, thingB, "agent")
	seedTemplateForExpiry(t, ctx, pool, "agent", keyA, 1, map[string]any{"x": 1})
	seedTemplateForExpiry(t, ctx, pool, "agent", keyB, 1, map[string]any{"y": 2})

	now := time.Now().UTC()
	seedExpiredOverride(t, ctx, pool, thingA, keyA, now.Add(-2*time.Minute))
	seedExpiredOverride(t, ctx, pool, thingB, keyB, now.Add(-30*time.Second))

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := newExpiryTestManager(pool, logger)
	j := NewOverrideExpiry(store.New(pool), mgr, time.Minute, nil, logger)
	j.st = prefixFilteringLister{inner: j.st, prefix: prefix}

	if err := j.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if overrideRowExists(t, ctx, pool, thingA, keyA) {
		t.Error("thingA expired override still present")
	}
	if overrideRowExists(t, ctx, pool, thingB, keyB) {
		t.Error("thingB expired override still present")
	}
	if n := countAuditRowsForExpiry(t, ctx, pool, thingA, "thing_override_cleared", "system:override-expiry-job"); n != 1 {
		t.Errorf("thingA audit rows = %d, want 1", n)
	}
	if n := countAuditRowsForExpiry(t, ctx, pool, thingB, "thing_override_cleared", "system:override-expiry-job"); n != 1 {
		t.Errorf("thingB audit rows = %d, want 1", n)
	}
}

// Test 5: per-row failure escalation — rowFailureCounter increments
// per failure, and the 5th consecutive failure for the same
// (thing_id, config_key) escalates the log to Error so it shows up
// under alert filters. A poisoned row that only warns forever would
// never trip on-call filters.
//
// We don't need Postgres for this — the test injects a stubbed lister
// + clearer through the unexported overrideExpiry{Lister,Clearer}
// interfaces. The same path runs in production via *store.Store +
// *manager.Manager.

// fakeExpiryLister returns a fixed expired-overrides slice on every
// ListExpiredOverrides call. Tests vary it between ticks to simulate
// admin extensions and direct deletes.
type fakeExpiryLister struct {
	mu    sync.Mutex
	rows  []store.ThingConfigOverride
	calls int32
}

func (f *fakeExpiryLister) set(rows []store.ThingConfigOverride) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = rows
}

func (f *fakeExpiryLister) ListExpiredOverrides(_ context.Context, _ time.Time) ([]store.ThingConfigOverride, error) {
	atomic.AddInt32(&f.calls, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.ThingConfigOverride, len(f.rows))
	copy(out, f.rows)
	return out, nil
}

// failingClearer returns a configurable error from ClearOverride. nil
// err triggers the success branch — switching mid-test lets the same
// instance simulate "row finally clears on tick N".
type failingClearer struct {
	mu   sync.Mutex
	err  error
	hits int32
}

func (c *failingClearer) setErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
}

func (c *failingClearer) ClearOverride(_ context.Context, _ string, _ string, _ string) error {
	atomic.AddInt32(&c.hits, 1)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// captureLogs returns a slog.Logger that writes every record into buf,
// plus the buf so the test can inspect captured records line-by-line.
func captureLogs() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	h := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), &buf
}

// newExpiryJobWithFakes wires an OverrideExpiry instance that uses the
// supplied fakes instead of *store.Store / *manager.Manager. We rely on
// the fact that OverrideExpiry's struct fields are typed against the
// narrow interfaces, so the same Run code path runs against the test
// doubles.
func newExpiryJobWithFakes(t *testing.T, lister overrideExpiryLister, clearer overrideExpiryClearer, reg *opsmetrics.Registry, logger *slog.Logger) *OverrideExpiry {
	t.Helper()
	j := NewOverrideExpiry(nil, nil, time.Minute, reg, logger)
	j.st = lister
	j.mgr = clearer
	return j
}

func TestOverrideExpiry_PerRowFailureEscalates(t *testing.T) {
	logger, buf := captureLogs()

	promReg := prometheus.NewRegistry()
	reg := opsmetrics.NewRegistry(promReg)
	rowKey := "thing-x:policy"

	lister := &fakeExpiryLister{}
	lister.set([]store.ThingConfigOverride{
		{ThingID: "thing-x", ConfigKey: "policy"},
	})
	clearer := &failingClearer{}
	clearer.setErr(errors.New("simulated clear failure"))

	j := newExpiryJobWithFakes(t, lister, clearer, reg, logger)

	ctx := context.Background()
	// Run 5 ticks with the clearer always failing.
	for i := range overrideExpiryEscalateAfter {
		if err := j.Run(ctx); err != nil {
			t.Fatalf("Run %d: %v", i+1, err)
		}
	}

	// Every tick incremented hits + the failure map.
	if got := atomic.LoadInt32(&clearer.hits); got != int32(overrideExpiryEscalateAfter) {
		t.Errorf("clearer hits = %d; want %d", got, overrideExpiryEscalateAfter)
	}
	j.failuresMu.Lock()
	if j.failures[rowKey] != overrideExpiryEscalateAfter {
		t.Errorf("failure count for %s = %d; want %d",
			rowKey, j.failures[rowKey], overrideExpiryEscalateAfter)
	}
	j.failuresMu.Unlock()

	// Inspect captured slog records: the first 4 must be at warn, the
	// 5th at error.
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var failedLines []map[string]any
	for _, ln := range lines {
		if ln == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(ln), &rec); err != nil {
			t.Fatalf("decode log line %q: %v", ln, err)
		}
		if rec["event"] == "override_expiry_clear_failed" {
			failedLines = append(failedLines, rec)
		}
	}
	if len(failedLines) != overrideExpiryEscalateAfter {
		t.Fatalf("captured %d failure log lines; want %d (full buf=%s)",
			len(failedLines), overrideExpiryEscalateAfter, buf.String())
	}
	for i, rec := range failedLines {
		levelStr, _ := rec["level"].(string)
		wantLevel := "WARN"
		if i == overrideExpiryEscalateAfter-1 {
			wantLevel = "ERROR"
		}
		if levelStr != wantLevel {
			t.Errorf("failure log #%d level = %q; want %q (rec=%v)", i+1, levelStr, wantLevel, rec)
		}
		// consecutive_failures must climb monotonically 1..5.
		var fails float64
		if v, ok := rec["consecutive_failures"].(float64); ok {
			fails = v
		}
		if fails != float64(i+1) {
			t.Errorf("failure log #%d consecutive_failures = %v; want %d", i+1, rec["consecutive_failures"], i+1)
		}
	}

	// Counter increments must equal the failure count. Read from the
	// prometheus.Registry the opsmetrics.Registry was built on.
	if got := readCounter(t, promReg, "nexus_override_expiry_row_failures_total"); got != overrideExpiryEscalateAfter {
		t.Errorf("override_expiry.row_failures_total = %d; want %d",
			got, overrideExpiryEscalateAfter)
	}

	// Now flip the clearer to succeed and tick once: failure-map entry
	// must drop and a 6th failure log must NOT appear.
	clearer.setErr(nil)
	bufLenBeforeSuccess := buf.Len()
	if err := j.Run(ctx); err != nil {
		t.Fatalf("Run on success: %v", err)
	}
	j.failuresMu.Lock()
	if _, present := j.failures[rowKey]; present {
		t.Errorf("failure map still has %s after success; want reset", rowKey)
	}
	j.failuresMu.Unlock()
	successPart := buf.String()[bufLenBeforeSuccess:]
	if strings.Contains(successPart, "override_expiry_clear_failed") {
		t.Errorf("post-success log slice still contains failure event: %s", successPart)
	}
	if !strings.Contains(successPart, "override_expiry_cleared") {
		t.Errorf("post-success log slice missing cleared event: %s", successPart)
	}
}

// TestOverrideExpiry_StaleFailureMapEntryReaped covers the case where
// admin extends or deletes the row mid-flight: after one Run sees the
// row missing, the failure-map entry must be reaped so the consecutive
// counter does not survive forever.
func TestOverrideExpiry_StaleFailureMapEntryReaped(t *testing.T) {
	logger, _ := captureLogs()
	reg := opsmetrics.NewRegistry(prometheus.NewRegistry())

	lister := &fakeExpiryLister{}
	lister.set([]store.ThingConfigOverride{
		{ThingID: "thing-y", ConfigKey: "routing"},
	})
	clearer := &failingClearer{}
	clearer.setErr(errors.New("transient"))

	j := newExpiryJobWithFakes(t, lister, clearer, reg, logger)
	ctx := context.Background()

	// Tick once → failure recorded.
	if err := j.Run(ctx); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	j.failuresMu.Lock()
	if j.failures["thing-y:routing"] != 1 {
		t.Fatalf("after tick 1 failure map = %v; want 1", j.failures)
	}
	j.failuresMu.Unlock()

	// Admin extends TTL: row is no longer expired.
	lister.set(nil)
	if err := j.Run(ctx); err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	j.failuresMu.Lock()
	if len(j.failures) != 0 {
		t.Errorf("after stale reap failure map = %v; want empty", j.failures)
	}
	j.failuresMu.Unlock()
}

// readCounter returns the current value of the named counter pulled out
// of the supplied prometheus.Registry. Counter is single-cell (no
// labels), so we sum across whatever happens to be emitted.
func readCounter(t *testing.T, reg *prometheus.Registry, name string) int {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		var sum float64
		for _, m := range mf.GetMetric() {
			if c := m.GetCounter(); c != nil {
				sum += c.GetValue()
			}
		}
		return int(sum)
	}
	return 0
}

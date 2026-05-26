package alerting_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine/senders"
)

// These tests exercise the full alert pipeline end-to-end:
//   Raiser -> Store -> Dispatcher -> senders.Registry -> Webhook -> remote HTTP
//
// They rely on a real Postgres (TEST_DATABASE_URL) and httptest.NewServer as
// the webhook target. If the DB is unavailable, testPool() skips cleanly. The
// shared cleanup() helper reaps rows whose ruleId/name starts with "test." /
// "test-", so every rule id and channel name below uses those prefixes.

// integrationSenderRegAdapter bridges *senders.Registry into
// alerting.SenderRegistry — mirrors cmd/nexus-hub/main.go's senderRegAdapter so
// the test uses the same wiring the production binary does.
type integrationSenderRegAdapter struct{ r *senders.Registry }

func (a integrationSenderRegAdapter) Get(channelType string) (alerting.Sender, error) {
	s, err := a.r.Get(channelType)
	if err != nil {
		return nil, err
	}
	return integrationSenderShim{s}, nil
}

type integrationSenderShim struct{ s senders.Sender }

func (w integrationSenderShim) Send(ctx context.Context, ch alerting.Channel, a alerting.Alert) (int, error) {
	return w.s.Send(ctx, ch, a)
}

// recordingServer wraps an httptest.Server that captures every POST it
// receives. The status code written back is configurable per-instance.
type recordingServer struct {
	srv    *httptest.Server
	status int

	mu       sync.Mutex
	requests []alerting.Alert
}

func newRecordingServer(status int) *recordingServer {
	rs := &recordingServer{status: status}
	rs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var a alerting.Alert
		if err := json.Unmarshal(body, &a); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		rs.mu.Lock()
		rs.requests = append(rs.requests, a)
		rs.mu.Unlock()
		w.WriteHeader(rs.status)
	}))
	return rs
}

func (rs *recordingServer) URL() string { return rs.srv.URL }
func (rs *recordingServer) Close()      { rs.srv.Close() }

func (rs *recordingServer) count() int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return len(rs.requests)
}

func (rs *recordingServer) snapshot() []alerting.Alert {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	out := make([]alerting.Alert, len(rs.requests))
	copy(out, rs.requests)
	return out
}

// integrationLogger returns a discard-logger so tests stay quiet.
func integrationLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// insertIntegrationRule writes an AlertRule directly via the pool. Using pool
// SQL (rather than the store) matches the existing store_test.go / raiser_test.go
// pattern — the Store has no InsertRule method.
//
// The DB enum values are uppercase ("HIGH", "CRITICAL", ...) while the Go
// Severity constants are lowercase, so we upper-case before inserting.
func insertIntegrationRule(t *testing.T, pool *pgxpool.Pool, id, sourceType string, severity alerting.Severity, cooldownSec int) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO "AlertRule" (id, "displayName", "sourceType", "defaultSeverity", "requiresAck", enabled, params, "paramsSchema", "cooldownSec", "updatedAt")
		VALUES ($1, $2, $3, $4::"AlertSeverity", false, true, '{}', '{}', $5, NOW())
		ON CONFLICT (id) DO UPDATE SET "cooldownSec" = EXCLUDED."cooldownSec"`,
		id, "Integration "+id, sourceType, strings.ToUpper(string(severity)), cooldownSec,
	)
	if err != nil {
		t.Fatalf("insert rule %q: %v", id, err)
	}
}

// buildPipeline wires store + dispatcher + raiser with a real senders.Registry.
// Matches production wiring from cmd/nexus-hub/main.go.
func buildPipeline(pool *pgxpool.Pool) (*alerting.Raiser, *alerting.Store, *senders.Registry) {
	store := alerting.NewStore(pool)
	reg := senders.NewRegistry()
	reg.Register("webhook", senders.NewWebhook(nil))
	adapter := integrationSenderRegAdapter{r: reg}
	dispatcher := alerting.NewDispatcher(store, adapter, integrationLogger())
	raiser := alerting.NewRaiser(pool, store, dispatcher, integrationLogger())
	return raiser, store, reg
}

// countDispatches returns the number of AlertDispatch rows for the given alert.
func countDispatches(t *testing.T, pool *pgxpool.Pool, alertID string) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM "AlertDispatch" WHERE "alertId" = $1`, alertID,
	).Scan(&n)
	if err != nil {
		t.Fatalf("countDispatches: %v", err)
	}
	return n
}

// countDispatchesForRule counts AlertDispatch rows whose alert belongs to the
// given rule (covers both success and failure rows, across possibly multiple
// alert rows if the rule has been re-fired).
func countDispatchesForRule(t *testing.T, pool *pgxpool.Pool, ruleID string) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM "AlertDispatch" d
		JOIN "Alert" a ON a.id = d."alertId"
		WHERE a."ruleId" = $1`, ruleID,
	).Scan(&n)
	if err != nil {
		t.Fatalf("countDispatchesForRule: %v", err)
	}
	return n
}

// getSingleDispatch returns the first (should be only) dispatch row for alertID.
func getSingleDispatch(t *testing.T, pool *pgxpool.Pool, alertID string) (success bool, statusCode *int, errorMsg *string) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`SELECT success, "statusCode", "errorMsg" FROM "AlertDispatch" WHERE "alertId" = $1 LIMIT 1`,
		alertID,
	).Scan(&success, &statusCode, &errorMsg)
	if err != nil {
		t.Fatalf("getSingleDispatch: %v", err)
	}
	return
}

// getAlertState returns the DB state column for an alert ID.
func getAlertState(t *testing.T, pool *pgxpool.Pool, alertID string) string {
	t.Helper()
	var s string
	err := pool.QueryRow(context.Background(),
		`SELECT state::text FROM "Alert" WHERE id = $1`, alertID,
	).Scan(&s)
	if err != nil {
		t.Fatalf("getAlertState: %v", err)
	}
	return s
}

// TestIntegration_RaiseDispatchAckResolve exercises the happy path:
// Raise (INSERT + dispatch) -> Raise (dedup, no dispatch) -> Ack -> Raise
// (fresh FIRING row + dispatch) -> Resolve (all rows RESOLVED).
func TestIntegration_RaiseDispatchAckResolve(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	ctx := context.Background()
	const ruleID = "test.e2e1"
	const targetKey = "device:abc"

	server := newRecordingServer(http.StatusOK)
	defer server.Close()

	// cooldownSec=0 — this test exercises the post-ack fresh-fire path; with
	// cooldown enforcement the third Raise would dedup into the ACKed row
	// instead of inserting a new FIRING row. Cooldown semantics are covered
	// by raiser_test.go TestRaiser_Cooldown* tests.
	insertIntegrationRule(t, pool, ruleID, "test", alerting.SeverityHigh, 0)

	raiser, store, _ := buildPipeline(pool)

	// Insert channel via pool SQL — its severities/sourceTypes filters must
	// match a high-severity test alert.
	cfg := mustJSON(t, map[string]any{"url": server.URL()})
	_, err := pool.Exec(ctx, `
		INSERT INTO "AlertChannel" (id, name, type, enabled, severities, "sourceTypes", config, "updatedAt")
		VALUES (gen_random_uuid()::text, 'test-webhook', 'webhook', true, ARRAY['HIGH','CRITICAL'], ARRAY['test'], $1, NOW())`,
		cfg,
	)
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}

	// --- First Raise — new FIRING row + one dispatch. -------------------
	now := time.Now().UTC()
	if err := raiser.Raise(ctx, alerting.RaiseInput{
		RuleID:      ruleID,
		TargetKey:   targetKey,
		TargetLabel: "Device abc",
		Severity:    alerting.SeverityHigh,
		Message:     "m1",
		FiredAt:     now,
	}); err != nil {
		t.Fatalf("first Raise: %v", err)
	}

	// Dispatch runs in a goroutine; wait for the webhook to land.
	if !waitFor(t, func() bool { return server.count() == 1 }, 2*time.Second) {
		t.Fatalf("webhook: want 1 request after first Raise, got %d", server.count())
	}

	rows := dumpAlerts(t, pool, ruleID, targetKey)
	if len(rows) != 1 {
		t.Fatalf("want 1 alert row after first Raise, got %d", len(rows))
	}
	if rows[0].State != "FIRING" {
		t.Errorf("state after first Raise: want FIRING got %s", rows[0].State)
	}
	if rows[0].DuplicateCount != 1 {
		// Raiser stamps duplicateCount=1 on INSERT (one observation so far).
		t.Errorf("duplicateCount after first Raise: want 1 got %d", rows[0].DuplicateCount)
	}
	firstAlertID := rows[0].ID

	// Wait until the dispatch row is persisted — the webhook handler returns
	// before DispatcherImpl writes the row, so the HTTP callback isn't a
	// synchronisation point for the DB write.
	if !waitFor(t, func() bool { return countDispatches(t, pool, firstAlertID) == 1 }, 2*time.Second) {
		t.Fatalf("dispatch rows for first alert: want 1, got %d", countDispatches(t, pool, firstAlertID))
	}
	success, statusCode, errorMsg := getSingleDispatch(t, pool, firstAlertID)
	if !success {
		t.Errorf("first dispatch: want success=true, got false (errorMsg=%v)", errorMsg)
	}
	if statusCode == nil || *statusCode != http.StatusOK {
		t.Errorf("first dispatch: want statusCode=200, got %v", statusCode)
	}

	// Verify the webhook payload — camelCase keys, lowercase severity.
	received := server.snapshot()[0]
	if received.RuleID != ruleID {
		t.Errorf("webhook body ruleId: want %q got %q", ruleID, received.RuleID)
	}
	if string(received.Severity) != "high" {
		t.Errorf("webhook body severity: want %q got %q", "high", received.Severity)
	}

	// --- Second Raise — duplicate, must NOT dispatch. --------------------
	if err := raiser.Raise(ctx, alerting.RaiseInput{
		RuleID:    ruleID,
		TargetKey: targetKey,
		Severity:  alerting.SeverityHigh,
		Message:   "m1 dup",
		FiredAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("second Raise: %v", err)
	}

	// Give any stray dispatch a chance to land.
	time.Sleep(100 * time.Millisecond)
	if server.count() != 1 {
		t.Errorf("webhook after dedup Raise: want 1 (no new request), got %d", server.count())
	}

	rows = dumpAlerts(t, pool, ruleID, targetKey)
	if len(rows) != 1 {
		t.Fatalf("want 1 alert row after dedup Raise, got %d", len(rows))
	}
	if rows[0].DuplicateCount != 2 {
		// Start=1 on INSERT + one duplicate = 2.
		t.Errorf("duplicateCount after dedup: want 2 got %d", rows[0].DuplicateCount)
	}
	if got := countDispatches(t, pool, firstAlertID); got != 1 {
		t.Errorf("dispatch rows after dedup: want 1 (unchanged), got %d", got)
	}

	// --- Acknowledge. ---------------------------------------------------
	if err := store.AcknowledgeAlert(ctx, firstAlertID, "alice", ""); err != nil {
		t.Fatalf("AcknowledgeAlert: %v", err)
	}
	if s := getAlertState(t, pool, firstAlertID); s != "ACKNOWLEDGED" {
		t.Errorf("state after Ack: want ACKNOWLEDGED, got %s", s)
	}
	var ackBy string
	if err := pool.QueryRow(ctx,
		`SELECT "acknowledgedBy" FROM "Alert" WHERE id = $1`, firstAlertID,
	).Scan(&ackBy); err != nil {
		t.Fatalf("read acknowledgedBy: %v", err)
	}
	if ackBy != "alice" {
		t.Errorf("acknowledgedBy: want alice, got %q", ackBy)
	}

	// --- Third Raise — latest row is ACKNOWLEDGED, so Raiser inserts a
	// fresh FIRING row and dispatches again. ----------------------------
	if err := raiser.Raise(ctx, alerting.RaiseInput{
		RuleID:      ruleID,
		TargetKey:   targetKey,
		TargetLabel: "Device abc",
		Severity:    alerting.SeverityHigh,
		Message:     "m1 post-ack",
		FiredAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("third Raise: %v", err)
	}

	if !waitFor(t, func() bool { return server.count() == 2 }, 2*time.Second) {
		t.Fatalf("webhook after post-ack Raise: want 2 requests, got %d", server.count())
	}
	rows = dumpAlerts(t, pool, ruleID, targetKey)
	if len(rows) != 2 {
		t.Fatalf("want 2 alert rows after post-ack Raise, got %d", len(rows))
	}
	if rows[0].State != "ACKNOWLEDGED" {
		t.Errorf("row[0] state: want ACKNOWLEDGED, got %s", rows[0].State)
	}
	if rows[1].State != "FIRING" {
		t.Errorf("row[1] state: want FIRING, got %s", rows[1].State)
	}

	// --- Resolve both rows via Raiser.Resolve. --------------------------
	if err := raiser.Resolve(ctx, ruleID, targetKey, "auto"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	rows = dumpAlerts(t, pool, ruleID, targetKey)
	if len(rows) != 2 {
		t.Fatalf("want 2 alert rows after Resolve, got %d", len(rows))
	}
	for i, r := range rows {
		if r.State != "RESOLVED" {
			t.Errorf("row[%d] state after Resolve: want RESOLVED, got %s", i, r.State)
		}
	}
	// Confirm resolvedReason was persisted.
	reasonRows, err := pool.Query(ctx,
		`SELECT "resolvedReason" FROM "Alert" WHERE "ruleId" = $1 AND "targetKey" = $2`,
		ruleID, targetKey,
	)
	if err != nil {
		t.Fatalf("query resolvedReason: %v", err)
	}
	defer reasonRows.Close()
	for reasonRows.Next() {
		var reason *string
		if err := reasonRows.Scan(&reason); err != nil {
			t.Fatalf("scan reason: %v", err)
		}
		if reason == nil || *reason != "auto" {
			t.Errorf("resolvedReason: want %q, got %v", "auto", reason)
		}
	}
}

// TestIntegration_DispatchRoutingFiltersCorrectly verifies severity and
// sourceType filters gate channels correctly when fanning out to multiple
// configured channels.
//
// Two channels are configured:
//   - A: severities=[CRITICAL], sourceTypes=[test] — should only fire for CRITICAL.
//   - B: severities=[], sourceTypes=[] — match-all, always fires.
//
// We raise twice against different targetKeys (so Raiser dedup doesn't collapse
// them): first medium → only B, then critical → A + B.
func TestIntegration_DispatchRoutingFiltersCorrectly(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	ctx := context.Background()
	const ruleID = "test.filter1"
	insertIntegrationRule(t, pool, ruleID, "test", alerting.SeverityMedium, 0)

	criticalServer := newRecordingServer(http.StatusOK)
	defer criticalServer.Close()
	allServer := newRecordingServer(http.StatusOK)
	defer allServer.Close()

	// Channel A — only CRITICAL/test.
	cfgA := mustJSON(t, map[string]any{"url": criticalServer.URL()})
	if _, err := pool.Exec(ctx, `
		INSERT INTO "AlertChannel" (id, name, type, enabled, severities, "sourceTypes", config, "updatedAt")
		VALUES (gen_random_uuid()::text, 'test-critical-only', 'webhook', true, ARRAY['CRITICAL'], ARRAY['test'], $1, NOW())`,
		cfgA,
	); err != nil {
		t.Fatalf("insert channel A: %v", err)
	}
	// Channel B — match-all.
	cfgB := mustJSON(t, map[string]any{"url": allServer.URL()})
	if _, err := pool.Exec(ctx, `
		INSERT INTO "AlertChannel" (id, name, type, enabled, severities, "sourceTypes", config, "updatedAt")
		VALUES (gen_random_uuid()::text, 'test-all', 'webhook', true, ARRAY[]::TEXT[], ARRAY[]::TEXT[], $1, NOW())`,
		cfgB,
	); err != nil {
		t.Fatalf("insert channel B: %v", err)
	}

	raiser, _, _ := buildPipeline(pool)

	// --- Medium severity: only channel B (match-all) should fire. -------
	if err := raiser.Raise(ctx, alerting.RaiseInput{
		RuleID:      ruleID,
		TargetKey:   "device:med",
		TargetLabel: "Device Medium",
		Severity:    alerting.SeverityMedium,
		Message:     "medium fire",
	}); err != nil {
		t.Fatalf("medium Raise: %v", err)
	}

	if !waitFor(t, func() bool { return allServer.count() == 1 }, 2*time.Second) {
		t.Fatalf("allServer after medium: want 1, got %d", allServer.count())
	}
	time.Sleep(100 * time.Millisecond) // give criticalServer a chance to (incorrectly) fire
	if criticalServer.count() != 0 {
		t.Errorf("criticalServer after medium: want 0 (filtered out), got %d", criticalServer.count())
	}

	// Wait for the single dispatch row to land.
	if !waitFor(t, func() bool { return countDispatchesForRule(t, pool, ruleID) == 1 }, 2*time.Second) {
		t.Fatalf("dispatch rows after medium: want 1, got %d", countDispatchesForRule(t, pool, ruleID))
	}

	// --- Critical severity on a different target (to avoid Raiser dedup).
	// Both channels should fire.
	if err := raiser.Raise(ctx, alerting.RaiseInput{
		RuleID:      ruleID,
		TargetKey:   "device:crit",
		TargetLabel: "Device Critical",
		Severity:    alerting.SeverityCritical,
		Message:     "critical fire",
	}); err != nil {
		t.Fatalf("critical Raise: %v", err)
	}

	if !waitFor(t, func() bool { return criticalServer.count() == 1 }, 2*time.Second) {
		t.Fatalf("criticalServer after critical: want 1, got %d", criticalServer.count())
	}
	if !waitFor(t, func() bool { return allServer.count() == 2 }, 2*time.Second) {
		t.Fatalf("allServer after critical: want 2 (cumulative), got %d", allServer.count())
	}

	// Total dispatch rows across both alerts: 1 (medium→B) + 2 (critical→A+B) = 3.
	if !waitFor(t, func() bool { return countDispatchesForRule(t, pool, ruleID) == 3 }, 2*time.Second) {
		t.Fatalf("total dispatch rows: want 3, got %d", countDispatchesForRule(t, pool, ruleID))
	}
}

// TestIntegration_SenderFailureWritesFailedDispatch verifies that when the
// webhook returns an error status (HTTP 500), the dispatcher still writes an
// AlertDispatch row with success=false, statusCode=500, and a non-empty
// errorMsg referencing the status.
func TestIntegration_SenderFailureWritesFailedDispatch(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	ctx := context.Background()
	const ruleID = "test.fail1"
	insertIntegrationRule(t, pool, ruleID, "test", alerting.SeverityHigh, 0)

	failServer := newRecordingServer(http.StatusInternalServerError)
	defer failServer.Close()

	cfg := mustJSON(t, map[string]any{"url": failServer.URL()})
	if _, err := pool.Exec(ctx, `
		INSERT INTO "AlertChannel" (id, name, type, enabled, severities, "sourceTypes", config, "updatedAt")
		VALUES (gen_random_uuid()::text, 'test-fail', 'webhook', true, ARRAY[]::TEXT[], ARRAY[]::TEXT[], $1, NOW())`,
		cfg,
	); err != nil {
		t.Fatalf("insert channel: %v", err)
	}

	raiser, _, _ := buildPipeline(pool)

	if err := raiser.Raise(ctx, alerting.RaiseInput{
		RuleID:      ruleID,
		TargetKey:   "device:fail",
		TargetLabel: "Device Fail",
		Severity:    alerting.SeverityHigh,
		Message:     "should fail",
	}); err != nil {
		t.Fatalf("Raise: %v", err)
	}

	// Webhook was called even though it returned 500.
	if !waitFor(t, func() bool { return failServer.count() == 1 }, 2*time.Second) {
		t.Fatalf("failServer: want 1 request, got %d", failServer.count())
	}

	rows := dumpAlerts(t, pool, ruleID, "device:fail")
	if len(rows) != 1 {
		t.Fatalf("want 1 alert row, got %d", len(rows))
	}
	if rows[0].State != "FIRING" {
		t.Errorf("state: want FIRING, got %s", rows[0].State)
	}
	alertID := rows[0].ID

	// Dispatch row is written asynchronously — poll.
	if !waitFor(t, func() bool { return countDispatches(t, pool, alertID) == 1 }, 2*time.Second) {
		t.Fatalf("dispatch rows: want 1, got %d", countDispatches(t, pool, alertID))
	}
	success, statusCode, errorMsg := getSingleDispatch(t, pool, alertID)
	if success {
		t.Error("dispatch success: want false, got true")
	}
	if statusCode == nil || *statusCode != http.StatusInternalServerError {
		t.Errorf("dispatch statusCode: want 500, got %v", statusCode)
	}
	if errorMsg == nil {
		t.Fatal("dispatch errorMsg: want non-nil, got nil")
	}
	// senders/webhook.go returns `fmt.Errorf("webhook: status %d", ...)` — the
	// dispatcher stores err.Error() verbatim, so "500" is a stable substring.
	if !strings.Contains(*errorMsg, "500") { //nolint:staticcheck // SA5011: t.Fatal above terminates the test goroutine
		t.Errorf("dispatch errorMsg %q: want substring %q", *errorMsg, "500")
	}
}

// mustJSON marshals v to JSON or fails the test.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}

// compile-time assertion that the adapter type satisfies the interface.
var _ alerting.SenderRegistry = integrationSenderRegAdapter{}

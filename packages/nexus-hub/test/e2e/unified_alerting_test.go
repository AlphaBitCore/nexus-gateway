//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	alertclient "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/client"
	hubAlerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/testharness"
)

// e2e prefix used to scope all test rows so cleanup does not touch seed data.
const (
	e2eRuleIDPrefix  = "test.e2e-alerting."
	e2eChannelPrefix = "test-e2e-alerting-"
)

// cleanupAlertingRows deletes Alert / AlertDispatch / AlertChannel rows
// created by these tests, keyed by the shared e2e prefix.
func cleanupAlertingRows(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	_, _ = pool.Exec(ctx, `
		DELETE FROM "AlertDispatch" WHERE "alertId" IN (
			SELECT id FROM "Alert" WHERE "ruleId" LIKE $1
		)`, e2eRuleIDPrefix+"%")
	_, _ = pool.Exec(ctx, `
		DELETE FROM "AlertDispatch" WHERE "alertId" IN (
			SELECT id FROM "Alert" WHERE "ruleId" = 'system.channel_test'
			AND "targetKey" LIKE 'channel:%'
			AND id IN (
				SELECT "alertId" FROM "AlertDispatch" d
				JOIN "AlertChannel" c ON c.id = d."channelId"
				WHERE c.name LIKE $1
			)
		)`, e2eChannelPrefix+"%")
	_, _ = pool.Exec(ctx, `DELETE FROM "Alert" WHERE "ruleId" LIKE $1`, e2eRuleIDPrefix+"%")
	// Remove system.channel_test alerts whose channel belongs to us.
	_, _ = pool.Exec(ctx, `
		DELETE FROM "AlertDispatch"
		WHERE "channelId" IN (
			SELECT id FROM "AlertChannel" WHERE name LIKE $1
		)`, e2eChannelPrefix+"%")
	_, _ = pool.Exec(ctx, `
		DELETE FROM "Alert"
		WHERE "ruleId" = 'system.channel_test'
		AND "targetKey" IN (
			SELECT 'channel:' || id FROM "AlertChannel" WHERE name LIKE $1
		)`, e2eChannelPrefix+"%")
	_, _ = pool.Exec(ctx, `DELETE FROM "AlertRule" WHERE id LIKE $1`, e2eRuleIDPrefix+"%")
	_, _ = pool.Exec(ctx, `DELETE FROM "AlertChannel" WHERE name LIKE $1`, e2eChannelPrefix+"%")
}

// insertE2ERule inserts an AlertRule via pool SQL. The rule ID must use the
// e2e prefix. DB enum values are uppercase.
func insertE2ERule(t *testing.T, pool *pgxpool.Pool, id, sourceType string, severity hubAlerting.Severity) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO "AlertRule" (id, "displayName", "sourceType", "defaultSeverity", "requiresAck",
		                        enabled, params, "paramsSchema", "cooldownSec", "updatedAt")
		VALUES ($1, $2, $3, $4::"AlertSeverity", false, true, '{}', '{}', 0, NOW())
		ON CONFLICT (id) DO NOTHING`,
		id, "E2E "+id, sourceType, alertSeverityDBEnum(severity),
	)
	if err != nil {
		t.Fatalf("insertE2ERule %q: %v", id, err)
	}
}

// alertSeverityDBEnum returns the uppercase DB enum value for a severity.
func alertSeverityDBEnum(s hubAlerting.Severity) string {
	switch s {
	case hubAlerting.SeverityCritical:
		return "CRITICAL"
	case hubAlerting.SeverityHigh:
		return "HIGH"
	case hubAlerting.SeverityMedium:
		return "MEDIUM"
	case hubAlerting.SeverityLow:
		return "LOW"
	default:
		return "INFO"
	}
}

// insertE2EChannel inserts an AlertChannel via pool SQL with a webhook URL.
// Returns the inserted channel's id.
func insertE2EChannel(t *testing.T, pool *pgxpool.Pool, name, webhookURL string) string {
	t.Helper()
	cfg, _ := json.Marshal(map[string]any{"url": webhookURL})
	var id string
	err := pool.QueryRow(context.Background(), `
		INSERT INTO "AlertChannel" (id, name, type, enabled, severities, "sourceTypes", config, "updatedAt")
		VALUES (gen_random_uuid()::text, $1, 'webhook', true, ARRAY[]::TEXT[], ARRAY[]::TEXT[], $2, NOW())
		RETURNING id`,
		name, cfg,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insertE2EChannel %q: %v", name, err)
	}
	return id
}

// alertSink is a thread-safe recording HTTP server that captures Alert payloads.
type alertSink struct {
	srv     *httptest.Server
	mu      sync.Mutex
	alerts  []hubAlerting.Alert
	counter atomic.Int64
}

func newAlertSink() *alertSink {
	s := &alertSink{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		var a hubAlerting.Alert
		if err := json.Unmarshal(body, &a); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.alerts = append(s.alerts, a)
		s.mu.Unlock()
		s.counter.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	return s
}

func (s *alertSink) URL() string { return s.srv.URL }
func (s *alertSink) Close()      { s.srv.Close() }
func (s *alertSink) count() int  { return int(s.counter.Load()) }
func (s *alertSink) snapshot() []hubAlerting.Alert {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]hubAlerting.Alert, len(s.alerts))
	copy(out, s.alerts)
	return out
}

// inlineEvaluator is a minimal evaluator that mirrors the compliance-proxy
// alerting.Evaluator contract. It tracks whether a check is firing and
// sends Fire / Resolve transitions via alertclient. Run is synchronous —
// tests call it directly for deterministic timing.
type inlineEvaluator struct {
	ac       *alertclient.Client
	ruleID   string
	target   string
	label    string
	severity string
	logger   *slog.Logger

	mu     sync.Mutex
	firing bool
}

func newInlineEvaluator(ac *alertclient.Client, ruleID, target, label, severity string, logger *slog.Logger) *inlineEvaluator {
	return &inlineEvaluator{
		ac: ac, ruleID: ruleID, target: target, label: label, severity: severity, logger: logger,
	}
}

// Run executes a single evaluation tick. isFiring controls the reported state.
func (e *inlineEvaluator) Run(ctx context.Context, isFiring bool) {
	e.mu.Lock()
	wasFiring := e.firing
	e.mu.Unlock()

	if isFiring {
		if err := e.ac.Fire(ctx, alertclient.AlertEnvelope{
			RuleID:      e.ruleID,
			TargetKey:   e.target,
			TargetLabel: e.label,
			Severity:    e.severity,
			Message:     "e2e test check is firing",
			FiredAt:     time.Now().UTC(),
		}); err != nil {
			e.logger.Warn("inline evaluator fire failed", "ruleId", e.ruleID, "err", err)
		}
		e.mu.Lock()
		e.firing = true
		e.mu.Unlock()
	} else if wasFiring {
		if err := e.ac.Resolve(ctx, e.ruleID, e.target, "auto"); err != nil {
			e.logger.Warn("inline evaluator resolve failed", "ruleId", e.ruleID, "err", err)
		}
		e.mu.Lock()
		e.firing = false
		e.mu.Unlock()
	}
}

// getAlertByRule queries the first Alert row for the given ruleID from the DB.
func getAlertByRule(t *testing.T, pool *pgxpool.Pool, ruleID string) (id, state string, ok bool) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`SELECT id, state::text FROM "Alert" WHERE "ruleId" = $1 ORDER BY "firedAt" DESC LIMIT 1`,
		ruleID,
	).Scan(&id, &state)
	if err != nil {
		return "", "", false
	}
	return id, state, true
}

// countSuccessDispatches returns the number of successful AlertDispatch rows for an alertID.
func countSuccessDispatches(t *testing.T, pool *pgxpool.Pool, alertID string) int {
	t.Helper()
	var n int
	_ = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM "AlertDispatch" WHERE "alertId" = $1 AND success = true`, alertID,
	).Scan(&n)
	return n
}

// adminPost sends an authenticated POST to the harness admin API.
func adminPost(t *testing.T, ctx context.Context, hubURL, path, token string, body any) *http.Response {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("adminPost marshal: %v", err)
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hubURL+path, bodyReader)
	if err != nil {
		t.Fatalf("adminPost new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Nexus-Actor-User-Id", "e2e-harness")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("adminPost do: %v", err)
	}
	return resp
}

// discardBody reads and closes a response body to allow connection reuse.
func discardBody(resp *http.Response) {
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// TestE2E_HappyPath exercises:
//  1. Fire an alert via alertclient → Hub raises a FIRING row + dispatches.
//  2. Ack the alert via admin API → state becomes ACKNOWLEDGED.
//  3. Resolve via alertclient → state becomes RESOLVED.
func TestE2E_HappyPath(t *testing.T) {
	if os.Getenv("RUN_E2E") == "" {
		t.Skip("set RUN_E2E=1 to run; requires Postgres at DATABASE_URL")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hub := testharness.NewForTest(t, testharness.WithAlerting())
	ts := httptest.NewServer(hub.Handler())
	t.Cleanup(ts.Close)

	pool := hub.Pool()
	t.Cleanup(func() { cleanupAlertingRows(t, pool) })

	const (
		ruleID    = e2eRuleIDPrefix + "happy-path"
		targetKey = "e2e-device:happy"
	)

	sink := newAlertSink()
	defer sink.Close()

	insertE2ERule(t, pool, ruleID, "e2e", hubAlerting.SeverityHigh)
	insertE2EChannel(t, pool, e2eChannelPrefix+"happy-path", sink.URL())

	// Build alertclient pointed at the harness Hub, authenticated as a
	// device that can reach /api/v1/alerts/*.
	ac, err := alertclient.New(alertclient.Config{
		HubBaseURL: ts.URL,
		AuthHeader: "Bearer " + hub.ServiceToken(),
		SpoolDir:   t.TempDir(),
		Logger:     newTestLogger(),
	})
	if err != nil {
		t.Fatalf("alertclient.New: %v", err)
	}

	logger := newTestLogger()
	eval := newInlineEvaluator(ac, ruleID, targetKey, "E2E Device", "high", logger)

	// --- Step 1: Fire. ---
	eval.Run(ctx, true)

	// Wait for the webhook sink to receive the dispatch.
	waitUntil(t, 5*time.Second, "webhook receives alert", func() bool {
		return sink.count() >= 1
	})

	// Hub DB must have exactly one FIRING row.
	var alertID, alertState string
	waitUntil(t, 5*time.Second, "FIRING row appears in DB", func() bool {
		var ok bool
		alertID, alertState, ok = getAlertByRule(t, pool, ruleID)
		return ok && alertState == "FIRING"
	})

	// Dispatch row must be successful.
	waitUntil(t, 5*time.Second, "dispatch row written", func() bool {
		return countSuccessDispatches(t, pool, alertID) >= 1
	})

	// --- Step 2: Ack via admin API. ---
	resp := adminPost(t, ctx, ts.URL, fmt.Sprintf("/api/v1/admin/alerts/%s/ack", alertID), hub.ServiceToken(), nil)
	discardBody(resp)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("ack: got HTTP %d, want 204", resp.StatusCode)
	}

	var stateAfterAck string
	err = pool.QueryRow(ctx, `SELECT state::text FROM "Alert" WHERE id = $1`, alertID).Scan(&stateAfterAck)
	if err != nil {
		t.Fatalf("query state after ack: %v", err)
	}
	if stateAfterAck != "ACKNOWLEDGED" {
		t.Errorf("state after ack = %q, want ACKNOWLEDGED", stateAfterAck)
	}

	// --- Step 3: Resolve — flip firing back to false. ---
	eval.Run(ctx, false)

	waitUntil(t, 5*time.Second, "RESOLVED in DB", func() bool {
		_, state, ok := getAlertByRule(t, pool, ruleID)
		return ok && state == "RESOLVED"
	})

	snap := sink.snapshot()
	if len(snap) < 1 {
		t.Errorf("sink received %d alerts, want >= 1", len(snap))
	}
}

// TestE2E_HubDownSpoolReplay verifies that when Hub is unreachable the
// alertclient spools the envelope and ReplayPending delivers it once the
// Hub is accessible.
func TestE2E_HubDownSpoolReplay(t *testing.T) {
	if os.Getenv("RUN_E2E") == "" {
		t.Skip("set RUN_E2E=1 to run; requires Postgres at DATABASE_URL")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hub := testharness.NewForTest(t, testharness.WithAlerting())
	ts := httptest.NewServer(hub.Handler())
	t.Cleanup(ts.Close)

	pool := hub.Pool()
	t.Cleanup(func() { cleanupAlertingRows(t, pool) })

	const (
		ruleID    = e2eRuleIDPrefix + "spool-replay"
		targetKey = "e2e-device:spool"
	)

	sink := newAlertSink()
	defer sink.Close()

	insertE2ERule(t, pool, ruleID, "e2e", hubAlerting.SeverityHigh)
	insertE2EChannel(t, pool, e2eChannelPrefix+"spool-replay", sink.URL())

	// Point the client at a guaranteed-unreachable address so all Fire calls spool.
	ac, err := alertclient.New(alertclient.Config{
		HubBaseURL: "http://127.0.0.1:1", // port 1 is reserved; connection will be refused
		AuthHeader: "Bearer " + hub.ServiceToken(),
		SpoolDir:   t.TempDir(),
		Logger:     newTestLogger(),
	})
	if err != nil {
		t.Fatalf("alertclient.New: %v", err)
	}

	logger := newTestLogger()
	eval := newInlineEvaluator(ac, ruleID, targetKey, "E2E Device Spool", "high", logger)

	// Fire a few ticks while Hub is "down" — all should land in the spool.
	for i := 0; i < 3; i++ {
		eval.Run(ctx, true)
	}

	if ac.PendingCount() < 1 {
		t.Fatalf("expected pending spool entries after unreachable Hub, got %d", ac.PendingCount())
	}

	// Restore connectivity by pointing the client at the real harness URL.
	ac.SetHubBaseURL(ts.URL)
	replayed, err := ac.ReplayPending(ctx)
	if err != nil {
		t.Fatalf("ReplayPending: %v", err)
	}
	if replayed < 1 {
		t.Errorf("ReplayPending replayed %d, want >= 1", replayed)
	}
	if ac.PendingCount() != 0 {
		t.Errorf("PendingCount after replay = %d, want 0", ac.PendingCount())
	}

	// Hub DB must have a FIRING row for the rule.
	waitUntil(t, 5*time.Second, "FIRING row after replay", func() bool {
		_, state, ok := getAlertByRule(t, pool, ruleID)
		return ok && state == "FIRING"
	})
}

// TestE2E_ChannelTestEndpoint exercises the channel test endpoint:
//  1. Create a webhook channel via POST /api/v1/admin/alerts/channels.
//  2. POST /api/v1/admin/alerts/channels/{id}/test.
//  3. Sink receives exactly one POST with ruleId == "system.channel_test".
//  4. AlertDispatch has a success row; Alert row is RESOLVED.
func TestE2E_ChannelTestEndpoint(t *testing.T) {
	if os.Getenv("RUN_E2E") == "" {
		t.Skip("set RUN_E2E=1 to run; requires Postgres at DATABASE_URL")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hub := testharness.NewForTest(t, testharness.WithAlerting())
	ts := httptest.NewServer(hub.Handler())
	t.Cleanup(ts.Close)

	pool := hub.Pool()
	t.Cleanup(func() { cleanupAlertingRows(t, pool) })

	sink := newAlertSink()
	defer sink.Close()

	// Create channel via admin HTTP API.
	channelName := e2eChannelPrefix + "channel-test"
	createBody := map[string]any{
		"name":        channelName,
		"type":        "webhook",
		"enabled":     true,
		"severities":  []string{},
		"sourceTypes": []string{},
		"config":      map[string]any{"url": sink.URL()},
	}
	createResp := adminPost(t, ctx, ts.URL, "/api/v1/admin/alerts/channels", hub.ServiceToken(), createBody)
	defer discardBody(createResp)
	if createResp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(createResp.Body)
		t.Fatalf("create channel: got HTTP %d, body: %s", createResp.StatusCode, body)
	}
	var created hubAlerting.Channel
	if err := json.NewDecoder(createResp.Body).Decode(&created); err != nil {
		t.Fatalf("decode created channel: %v", err)
	}
	if created.ID == "" {
		t.Fatal("created channel has empty id")
	}

	// Trigger the channel test endpoint.
	testResp := adminPost(t, ctx, ts.URL,
		fmt.Sprintf("/api/v1/admin/alerts/channels/%s/test", created.ID),
		hub.ServiceToken(), nil)
	defer discardBody(testResp)
	if testResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(testResp.Body)
		t.Fatalf("channel test: got HTTP %d, body: %s", testResp.StatusCode, body)
	}

	// Sink must have received exactly one webhook POST.
	waitUntil(t, 5*time.Second, "sink receives channel test webhook", func() bool {
		return sink.count() >= 1
	})
	snap := sink.snapshot()
	if len(snap) != 1 {
		t.Errorf("sink count = %d, want 1", len(snap))
	}
	if snap[0].RuleID != "system.channel_test" {
		t.Errorf("webhook ruleId = %q, want system.channel_test", snap[0].RuleID)
	}

	// AlertDispatch must have a success row for the synthetic alert.
	var dispatchCount int
	waitUntil(t, 5*time.Second, "dispatch row written", func() bool {
		_ = pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM "AlertDispatch" d
			JOIN "Alert" a ON a.id = d."alertId"
			WHERE a."ruleId" = 'system.channel_test'
			  AND d."channelId" = $1
			  AND d.success = true`, created.ID,
		).Scan(&dispatchCount)
		return dispatchCount >= 1
	})

	// The synthetic alert must be in RESOLVED state.
	waitUntil(t, 5*time.Second, "synthetic alert resolved", func() bool {
		var state string
		err := pool.QueryRow(ctx, `
			SELECT state::text FROM "Alert"
			WHERE "ruleId" = 'system.channel_test' AND "targetKey" = $1`,
			"channel:"+created.ID,
		).Scan(&state)
		return err == nil && state == "RESOLVED"
	})
}

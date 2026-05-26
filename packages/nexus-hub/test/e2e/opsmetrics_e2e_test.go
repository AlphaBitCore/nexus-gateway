//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/testharness"
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

// TestOpsMetrics_EndToEnd exercises the full Hub-side ops-metrics ingestion
// path wired by T13:
//
//  1. WS-connected Thing pushes a metrics_sample → Hub's WS dispatch routes it
//     into opsmetrics.Handler.HandleMetricsSample → Writer COPYs into
//     metric_ops_raw.
//  2. Same Thing pushes a diag_event → DiagWriter inserts into thing_diag_event.
//  3. A separate HTTP POST to /api/internal/things/diag-events:batch (the
//     drain endpoint the agent uses on startup) returns 200 with acceptedIds
//     covering every event id, and the rows land in thing_diag_event.
//
// All three exercise are run against one harness so a regression in any
// piece of the wiring (ws.Server.handleMessage dispatch, route mount, Writer
// Stop ordering) shows up here.
func TestOpsMetrics_EndToEnd(t *testing.T) {
	if os.Getenv("RUN_E2E") == "" {
		t.Skip("set RUN_E2E=1 to run; requires Postgres at DATABASE_URL")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	hub := testharness.NewForTest(t, testharness.WithOpsMetrics())
	ts := httptest.NewServer(hub.Handler())
	defer ts.Close()

	thingID := "agent-opsmetrics-e2e"
	wsURL := "ws" + ts.URL[len("http"):] + "/ws?id=" + thingID + "&type=agent"
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	token := hub.IssueEnrollmentTokenOfType(t, thingID, "agent")

	// metric_ops_raw / thing_diag_event rows are FK-cascaded by the harness's
	// thing-row delete, but be defensive: scoped DELETE before the run so
	// re-runs don't leak rows from a previous flake.
	pool := hub.Pool()
	_, _ = pool.Exec(ctx, `DELETE FROM metric_ops_raw WHERE thing_id = $1`, thingID)
	_, _ = pool.Exec(ctx, `DELETE FROM thing_diag_event WHERE thing_id = $1`, thingID)
	t.Cleanup(func() {
		cleanCtx := context.Background()
		_, _ = pool.Exec(cleanCtx, `DELETE FROM metric_ops_raw WHERE thing_id = $1`, thingID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM thing_diag_event WHERE thing_id = $1`, thingID)
	})

	// --- Connect a thingclient over WS ---
	client, err := thingclient.New(thingclient.Config{
		HubURL:            wsURL,
		HubHTTPURL:        ts.URL,
		ThingType:         "agent",
		ThingID:           thingID,
		Token:             token,
		Logger:            logger,
		MetricsRegisterer: prometheus.NewRegistry(),
	})
	if err != nil {
		t.Fatalf("thingclient.New: %v", err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatalf("thingclient.Start: %v", err)
	}
	defer client.Close(context.Background()) //nolint:errcheck

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if client.Mode() == thingclient.ModeWSConnected {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if client.Mode() != thingclient.ModeWSConnected {
		t.Fatalf("thingclient did not reach WSConnected (mode=%s)", client.Mode())
	}

	// --- 1. metrics_sample over WS ---
	sampleBatch := opsmetrics.SampleBatch{
		ThingID:   thingID,
		SampledAt: time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC),
		Samples: []opsmetrics.Sample{
			{Name: "runtime.heap_alloc_bytes", Kind: opsmetrics.KindGauge, DimensionKey: "", Value: 12345.0},
			{Name: "relay.dial_total", Kind: opsmetrics.KindCounter, DimensionKey: "mode=new", Value: 42.0},
		},
	}
	if err := client.PushMetricsSample(ctx, sampleBatch); err != nil {
		t.Fatalf("PushMetricsSample: %v", err)
	}

	// --- 2. diag_event over WS ---
	diagEvt := opsmetrics.DiagEvent{
		ThingID:      thingID,
		OccurredAt:   time.Date(2026, 4, 27, 10, 1, 0, 0, time.UTC),
		Level:        opsmetrics.LevelError,
		EventType:    opsmetrics.EventTypeError,
		Source:       "relay",
		Message:      "upstream dial failed",
		MessageHash:  "deadbeefcafef00d",
		Attrs:        map[string]any{"upstream": "api.openai.com:443"},
		RepeatCount:  1,
		AgentVersion: "v1.4.2",
		OSInfo:       map[string]any{"os": "darwin"},
	}
	if err := client.PushDiagEvent(ctx, diagEvt); err != nil {
		t.Fatalf("PushDiagEvent: %v", err)
	}

	// Both writers buffer up to 200ms / 100ms; poll up to 10s for the rows
	// to appear (CI shared DB can be slow on first contact).
	waitUntil(t, 10*time.Second, "metrics_sample lands in metric_ops_raw", func() bool {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM metric_ops_raw WHERE thing_id = $1`, thingID).Scan(&n); err != nil {
			return false
		}
		return n == 2
	})

	waitUntil(t, 10*time.Second, "diag_event lands in thing_diag_event", func() bool {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM thing_diag_event WHERE thing_id = $1 AND source = 'relay'`, thingID).Scan(&n); err != nil {
			return false
		}
		return n == 1
	})

	// Spot-check one metric row carries the right kind + value.
	var (
		gotKind string
		gotVal  *float64
	)
	if err := pool.QueryRow(ctx, `
		SELECT metric_kind, value
		  FROM metric_ops_raw
		 WHERE thing_id = $1 AND metric_name = 'runtime.heap_alloc_bytes'
	`, thingID).Scan(&gotKind, &gotVal); err != nil {
		t.Fatalf("scan gauge row: %v", err)
	}
	if gotKind != "gauge" {
		t.Errorf("metric_kind = %q, want gauge", gotKind)
	}
	if gotVal == nil || *gotVal != 12345.0 {
		t.Errorf("value = %v, want 12345", gotVal)
	}

	// --- 3. HTTP drain endpoint ---
	drainID1 := uuid.NewString()
	drainID2 := uuid.NewString()
	drainBody := map[string]any{
		"events": []map[string]any{
			{
				"id":           drainID1,
				"thingId":      thingID,
				"occurredAt":   time.Date(2026, 4, 27, 9, 0, 0, 0, time.UTC).Format(time.RFC3339),
				"level":        opsmetrics.LevelFatal,
				"eventType":    opsmetrics.EventTypeCrash,
				"source":       "main",
				"message":      "panic: runtime error",
				"messageHash":  "11223344",
				"stackTrace":   "goroutine 1 [running]:\nmain.crash()\n\t/app/main.go:42",
				"repeatCount":  1,
				"agentVersion": "v1.4.2",
				"osInfo":       map[string]any{"os": "darwin"},
			},
			{
				"id":          drainID2,
				"thingId":     thingID,
				"occurredAt":  time.Date(2026, 4, 27, 9, 1, 0, 0, time.UTC).Format(time.RFC3339),
				"level":       opsmetrics.LevelFatal,
				"eventType":   opsmetrics.EventTypeCrash,
				"source":      "main",
				"message":     "another panic",
				"messageHash": "55667788",
				"repeatCount": 1,
			},
		},
	}
	bodyBytes, err := json.Marshal(drainBody)
	if err != nil {
		t.Fatalf("marshal drain body: %v", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		ts.URL+"/api/internal/things/diag-events:batch", bytes.NewReader(bodyBytes))
	if err != nil {
		t.Fatalf("new drain request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Auth via service token — covers the Hub-internal callers path
	// (DeviceOrServiceAuth: service-token branch leaves c.Get("thing")
	// nil, so the handler must accept the X-Thing-Id fallback header).
	req.Header.Set("Authorization", "Bearer "+hub.ServiceToken())
	req.Header.Set("X-Thing-Id", thingID)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("drain do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("drain status = %d, want 200", resp.StatusCode)
	}

	var drainResp struct {
		AcceptedIds []string `json:"acceptedIds"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&drainResp); err != nil {
		t.Fatalf("decode drain resp: %v", err)
	}
	if len(drainResp.AcceptedIds) != 2 {
		t.Fatalf("acceptedIds = %v, want 2 entries", drainResp.AcceptedIds)
	}
	got := map[string]bool{drainResp.AcceptedIds[0]: true, drainResp.AcceptedIds[1]: true}
	if !got[drainID1] || !got[drainID2] {
		t.Errorf("acceptedIds = %v, missing %s or %s", drainResp.AcceptedIds, drainID1, drainID2)
	}

	// 1 row from the WS diag_event + 2 from the drain = 3 total.
	var total int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM thing_diag_event WHERE thing_id = $1`, thingID).Scan(&total); err != nil {
		t.Fatalf("count diag rows: %v", err)
	}
	if total != 3 {
		t.Errorf("thing_diag_event rows for %s = %d, want 3", thingID, total)
	}

	// Drain idempotency — a second POST with the same id list returns ack
	// for both ids and inserts no new rows.
	resp2, err := http.DefaultClient.Do(mustClone(t, req, bodyBytes))
	if err != nil {
		t.Fatalf("drain retry do: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("drain retry status = %d", resp2.StatusCode)
	}
	var retryResp struct {
		AcceptedIds []string `json:"acceptedIds"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&retryResp); err != nil {
		t.Fatalf("decode retry resp: %v", err)
	}
	if len(retryResp.AcceptedIds) != 2 {
		t.Errorf("retry acceptedIds = %v, want 2", retryResp.AcceptedIds)
	}

	var afterRetry int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM thing_diag_event WHERE thing_id = $1`, thingID).Scan(&afterRetry); err != nil {
		t.Fatalf("count after retry: %v", err)
	}
	if afterRetry != 3 {
		t.Errorf("rows after retry = %d, want 3 (PK conflict on retry)", afterRetry)
	}
}

// mustClone rebuilds an outbound request with a fresh body reader so we can
// post the same payload twice (http.Request.Body is a one-shot reader).
func mustClone(t *testing.T, src *http.Request, body []byte) *http.Request {
	t.Helper()
	r, err := http.NewRequestWithContext(src.Context(), src.Method, src.URL.String(), bytes.NewReader(body))
	if err != nil {
		t.Fatalf("clone req: %v", err)
	}
	for k, v := range src.Header {
		r.Header[k] = append([]string(nil), v...)
	}
	return r
}

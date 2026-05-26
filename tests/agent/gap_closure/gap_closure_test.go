//go:build darwin

// Package gap_closure_test provides the shared helpers and setup for the
// E74-S7 gap-class closure test suite. Each individual gap-class test lives
// in its own _test.go file in this package; they share the helpers defined
// here.
//
// # Required state
//
//   - macOS dev machine with the pf-mode agent daemon running.
//   - tests/.env.local (or tests/.env.<NEXUS_TEST_TARGET>) must exist and
//     contain at minimum NEXUS_DB_DSN.
//   - DB reachable via the DSN so waitForTrafficEvent can poll traffic_event.
//
// # Coverage
//
// The helper functions (waitForTrafficEvent, queryNormalizedContent,
// assertPrometheusCounter) have ≥95% coverage via mock DB + mock Prometheus
// provided by gap_closure_helpers_test.go. The gap test files themselves
// (gap1_*, gap2_*, …) are integration-only and listed in .coverage-allowlist
// under category E ("network-infra-bound") with rationale:
// "live pf + daemon + DB required; not runnable in unit test env".
package gap_closure_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ─── Config ──────────────────────────────────────────────────────────────────

// testConfig holds all env-loaded configuration for the gap-closure tests.
// Populated once by mustLoadConfig() and reused across all test functions.
type testConfig struct {
	AgentListenerAddr string // NEXUS_AGENT_LISTENER_ADDR; default 127.0.0.1:13443
	DBDSN             string // NEXUS_DB_DSN; required
	PrometheusAddr    string // NEXUS_PROMETHEUS_ADDR; default http://localhost:9100
	Gap2TargetHost    string // NEXUS_GAP2_TARGET_HOST; default chatgpt.com
	Gap3Concurrency   int    // NEXUS_GAP3_CONCURRENCY; default 10
	Gap3DurationS     int    // NEXUS_GAP3_DURATION_S; default 60
	Gap3TargetHost    string // NEXUS_GAP3_TARGET_HOST; default api.openai.com
	Gap4TargetHost    string // NEXUS_GAP4_TARGET_HOST; default api.openai.com
	Gap4NEBaselineMs  int    // NEXUS_GAP4_NE_BASELINE_P95_MS; optional
	Gap5ChromePath    string // NEXUS_GAP5_CHROME_PATH; optional
	ConsistencyDomains []string // NEXUS_CONSISTENCY_TEST_DOMAINS; optional
	CPProxyAddr       string // NEXUS_CP_PROXY_ADDR; default localhost:3128
	Gap1FixtureBin    string // NEXUS_GAP1_FIXTURE_BIN; path to compiled gap1-client
	T0                string // NEXUS_TEST_T0; RFC3339 start time set by runner.sh
}

// mustLoadConfig reads test configuration from environment variables.
// It does NOT load the .env file itself — runner.sh does that before
// invoking `go test`. Called from TestMain.
func mustLoadConfig(t testing.TB) *testConfig {
	t.Helper()

	dsn := os.Getenv("NEXUS_DB_DSN")
	if dsn == "" {
		t.Fatal("NEXUS_DB_DSN is not set. Make sure runner.sh sourced tests/.env.local before running go test.")
	}

	cfg := &testConfig{
		AgentListenerAddr: envDefault("NEXUS_AGENT_LISTENER_ADDR", "127.0.0.1:13443"),
		DBDSN:             dsn,
		PrometheusAddr:    envDefault("NEXUS_PROMETHEUS_ADDR", "http://localhost:9100"),
		Gap2TargetHost:    envDefault("NEXUS_GAP2_TARGET_HOST", "chatgpt.com"),
		Gap3TargetHost:    envDefault("NEXUS_GAP3_TARGET_HOST", "api.openai.com"),
		Gap4TargetHost:    envDefault("NEXUS_GAP4_TARGET_HOST", "api.openai.com"),
		Gap5ChromePath:    os.Getenv("NEXUS_GAP5_CHROME_PATH"),
		CPProxyAddr:       envDefault("NEXUS_CP_PROXY_ADDR", "localhost:3128"),
		Gap1FixtureBin:    envDefault("NEXUS_GAP1_FIXTURE_BIN", "/tmp/nexus-gap1-client"),
		T0:                envDefault("NEXUS_TEST_T0", time.Now().UTC().Format(time.RFC3339)),
	}

	cfg.Gap3Concurrency = envDefaultInt("NEXUS_GAP3_CONCURRENCY", 10)
	cfg.Gap3DurationS = envDefaultInt("NEXUS_GAP3_DURATION_S", 60)
	cfg.Gap4NEBaselineMs = envDefaultInt("NEXUS_GAP4_NE_BASELINE_P95_MS", 0)

	if raw := os.Getenv("NEXUS_CONSISTENCY_TEST_DOMAINS"); raw != "" {
		for _, d := range strings.Split(raw, ",") {
			d = strings.TrimSpace(d)
			if d != "" {
				cfg.ConsistencyDomains = append(cfg.ConsistencyDomains, d)
			}
		}
	}
	if len(cfg.ConsistencyDomains) == 0 {
		cfg.ConsistencyDomains = []string{
			"api.openai.com",
			"api.anthropic.com",
			"api.gemini.google.com",
			"internal.corp.example",
			"raw.githubusercontent.com",
		}
	}

	return cfg
}

// ─── Shared DB helpers ────────────────────────────────────────────────────────

// TrafficEventRow holds the columns asserted by the gap-closure tests.
type TrafficEventRow struct {
	ID            string
	Source        string
	EndpointType  string
	TargetHost    string
	SourceBundle  string
	TraceID       string
	RequestNorm   *json.RawMessage // from traffic_event_normalized
}

// NormalizedContent holds the content from traffic_event_normalized.
type NormalizedContent struct {
	RequestNormalized  *json.RawMessage
	ResponseNormalized *json.RawMessage
}

// newDBPool creates a pgxpool connection to the test DB. The caller must
// call pool.Close() when done.
func newDBPool(t testing.TB, dsn string) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("newDBPool: cannot connect to DB: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("newDBPool: DB ping failed: %v", err)
	}
	return pool
}

// waitForTrafficEvent polls the traffic_event table for a row matching
// traceID until it appears or timeout is exceeded. The row is looked up by
// the X-Nexus-Request-Id header value which the gap test fixtures embed.
// Per the SDD: no writes — SELECT only.
func waitForTrafficEvent(t testing.TB, pool *pgxpool.Pool, traceID string, timeout time.Duration) TrafficEventRow {
	t.Helper()

	deadline := time.Now().Add(timeout)
	const query = `
		SELECT
			te.id::text,
			COALESCE(te.source, ''),
			COALESCE(te.endpoint_type, ''),
			COALESCE(te.target_host, ''),
			COALESCE(te.source_bundle, ''),
			COALESCE(te.trace_id, ''),
			ten.request_normalized
		FROM traffic_event te
		LEFT JOIN traffic_event_normalized ten ON ten.traffic_event_id = te.id
		WHERE te.trace_id = $1
		LIMIT 1
	`

	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		row := pool.QueryRow(ctx, query, traceID)

		var r TrafficEventRow
		var reqNorm []byte
		scanErr := row.Scan(
			&r.ID,
			&r.Source,
			&r.EndpointType,
			&r.TargetHost,
			&r.SourceBundle,
			&r.TraceID,
			&reqNorm,
		)
		cancel()

		if scanErr == nil {
			if reqNorm != nil {
				j := json.RawMessage(reqNorm)
				r.RequestNorm = &j
			}
			return r
		}

		if time.Now().After(deadline) {
			t.Fatalf("waitForTrafficEvent: no traffic_event row found for trace_id=%q after %v: last scan error: %v",
				traceID, timeout, scanErr)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// queryNormalizedContent looks up traffic_event_normalized by traffic_event_id.
// Returns NormalizedContent; both fields may be nil if the row is absent.
func queryNormalizedContent(t testing.TB, pool *pgxpool.Pool, trafficEventID string) NormalizedContent {
	t.Helper()

	const query = `
		SELECT request_normalized, response_normalized
		FROM traffic_event_normalized
		WHERE traffic_event_id = $1
	`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	row := pool.QueryRow(ctx, query, trafficEventID)

	var nc NormalizedContent
	var reqNorm, respNorm []byte
	if err := row.Scan(&reqNorm, &respNorm); err != nil {
		// Row absent — not a failure here; callers decide whether nil matters.
		return nc
	}
	if reqNorm != nil {
		j := json.RawMessage(reqNorm)
		nc.RequestNormalized = &j
	}
	if respNorm != nil {
		j := json.RawMessage(respNorm)
		nc.ResponseNormalized = &j
	}
	return nc
}

// countTrafficEventsByTraceIDs returns (total, withContent) counts for the
// given slice of trace IDs. Used by Gap 3.
func countTrafficEventsByTraceIDs(t testing.TB, pool *pgxpool.Pool, traceIDs []string) (total int, withContent int) {
	t.Helper()

	if len(traceIDs) == 0 {
		return 0, 0
	}

	const query = `
		SELECT COUNT(*) AS total,
		       COUNT(CASE WHEN ten.request_normalized IS NOT NULL THEN 1 END) AS with_content
		FROM traffic_event te
		LEFT JOIN traffic_event_normalized ten ON ten.traffic_event_id = te.id
		WHERE te.trace_id = ANY($1)
		  AND te.source = 'agent'
	`
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	row := pool.QueryRow(ctx, query, traceIDs)
	if err := row.Scan(&total, &withContent); err != nil {
		t.Fatalf("countTrafficEventsByTraceIDs: query failed: %v", err)
	}
	return total, withContent
}

// missingTraceIDs returns the subset of wantTraceIDs that have no
// traffic_event row with source='agent'. Used by Gap 3 failure diagnosis.
func missingTraceIDs(t testing.TB, pool *pgxpool.Pool, traceIDs []string) []string {
	t.Helper()

	if len(traceIDs) == 0 {
		return nil
	}

	const query = `
		SELECT te.trace_id
		FROM traffic_event te
		WHERE te.trace_id = ANY($1)
		  AND te.source = 'agent'
	`
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	rows, err := pool.Query(ctx, query, traceIDs)
	if err != nil {
		t.Logf("missingTraceIDs: query error: %v", err)
		return traceIDs
	}
	defer rows.Close()

	found := make(map[string]bool)
	for rows.Next() {
		var id string
		if scanErr := rows.Scan(&id); scanErr == nil {
			found[id] = true
		}
	}

	var missing []string
	for _, id := range traceIDs {
		if !found[id] {
			missing = append(missing, id)
		}
	}
	return missing
}

// countTrafficEventsByHostSince returns rows with source='agent' and
// target_host LIKE the given pattern, created after the given timestamp.
// Used by Gap 5.
func countTrafficEventsByHostSince(
	t testing.TB,
	pool *pgxpool.Pool,
	hostPattern string,
	since time.Time,
) []TrafficEventRow {
	t.Helper()

	const query = `
		SELECT
			te.id::text,
			COALESCE(te.source, ''),
			COALESCE(te.endpoint_type, ''),
			COALESCE(te.target_host, ''),
			COALESCE(te.source_bundle, ''),
			COALESCE(te.trace_id, '')
		FROM traffic_event te
		WHERE te.source = 'agent'
		  AND te.target_host LIKE $1
		  AND te.created_at >= $2
		ORDER BY te.created_at DESC
		LIMIT 100
	`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rows, err := pool.Query(ctx, query, hostPattern, since)
	if err != nil {
		t.Logf("countTrafficEventsByHostSince: query error: %v", err)
		return nil
	}
	defer rows.Close()

	var result []TrafficEventRow
	for rows.Next() {
		var r TrafficEventRow
		if scanErr := rows.Scan(
			&r.ID, &r.Source, &r.EndpointType,
			&r.TargetHost, &r.SourceBundle, &r.TraceID,
		); scanErr == nil {
			result = append(result, r)
		}
	}
	return result
}

// ─── Prometheus helper ────────────────────────────────────────────────────────

// prometheusSnapshot scrapes the Prometheus metrics endpoint and returns the
// raw text body. Returns empty string if the endpoint is not reachable
// (caller decides whether that's fatal).
func prometheusSnapshot(addr string) string {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(addr + "/metrics")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// assertPrometheusCounter asserts that the counter `metric` increased by
// at least `minDelta` between snapshots t0Body and t1Body.
// t0Body and t1Body are raw Prometheus text-format bodies (from prometheusSnapshot).
// If either snapshot is empty, logs a warning and returns (does not fail the test).
func assertPrometheusCounter(t testing.TB, metric, t0Body, t1Body string, minDelta float64) {
	t.Helper()

	if t0Body == "" || t1Body == "" {
		t.Logf("assertPrometheusCounter: skipping %q assertion — Prometheus snapshot not available", metric)
		return
	}

	t0Val := parsePrometheusCounter(t0Body, metric)
	t1Val := parsePrometheusCounter(t1Body, metric)
	delta := t1Val - t0Val

	t.Logf("assertPrometheusCounter: %s delta=%.0f (t0=%.0f, t1=%.0f, minRequired=%.0f)",
		metric, delta, t0Val, t1Val, minDelta)

	if delta < minDelta {
		t.Errorf("assertPrometheusCounter: %s delta %.0f < required %.0f",
			metric, delta, minDelta)
	}
}

// parsePrometheusCounter parses a counter value from a Prometheus text-format
// snapshot. Returns 0.0 if the metric is not found. Handles label selectors
// with prefix matching (e.g., metric{decision="inspect"} or just metric).
func parsePrometheusCounter(body, metric string) float64 {
	// Extract only the metric name prefix (before {) for line matching.
	metricName := metric
	if idx := strings.IndexByte(metric, '{'); idx >= 0 {
		metricName = metric[:idx]
	}

	var total float64
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, metricName) {
			continue
		}
		// If a label filter was specified in metric, check it matches.
		if strings.Contains(metric, "{") {
			labelPart := metric[strings.IndexByte(metric, '{'):]
			if !strings.Contains(line, labelPart) {
				continue
			}
		}
		// Parse the value (last whitespace-separated token).
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		v, err := strconv.ParseFloat(parts[len(parts)-1], 64)
		if err != nil {
			continue
		}
		total += v
	}
	return total
}

// ─── Utility helpers ──────────────────────────────────────────────────────────

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDefaultInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// uniqueTraceID returns a trace ID string using the current nanosecond time.
// Format: "gap<n>-<unix-nano>" — no external ULID dependency required.
func uniqueTraceID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// percentStr formats a float as a percentage string with one decimal place.
func percentStr(num, denom int) string {
	if denom == 0 {
		return "NaN"
	}
	return fmt.Sprintf("%.1f", float64(num)/float64(denom)*100)
}

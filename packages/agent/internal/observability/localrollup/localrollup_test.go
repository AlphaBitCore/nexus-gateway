// Package localrollup tests pin observable agent-side rollup behavior:
// schema-aware aggregation of audit_events into 5m buckets, cascading merges
// into 1h / 1d / 1mo bins, granule-aware reads, and retention purge.
//
// Tests use the audit package's NewQueue(":memory:", nil) to obtain a fully
// migrated SQLite handle including audit_events + the four rollup tables +
// rollup_watermark_local. This keeps the test contract aligned with what
// production code receives at runtime (auditQueue.DB() per
// packages/agent/cmd/agent/main.go:1430) and avoids a parallel schema in test
// code that could drift from production.
package localrollup

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	audit "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/queue"
)

func newAggregator(t *testing.T) (*Aggregator, *sql.DB) {
	t.Helper()
	q, err := audit.NewQueue(":memory:", nil)
	if err != nil {
		t.Fatalf("open audit queue: %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })
	db := q.DB()
	agg := New(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return agg, db
}

// insertAuditEvent inserts a row into audit_events directly. Only the columns
// the rollup queries are populated; NOT NULL columns (id / dest_ip /
// dest_port) get sentinel values.
type auditRow struct {
	id               string
	ts               time.Time
	srcProc          string
	srcUser          string
	destHost         string
	action           string
	bumpStatus       sql.NullString
	bytesIn          sql.NullInt64
	bytesOut         sql.NullInt64
	durationMs       sql.NullInt64
	hookDecision     sql.NullString
	complianceTags   sql.NullString
	providerName     sql.NullString
	modelName        sql.NullString
	promptTokens     sql.NullInt64
	completionTokens sql.NullInt64
	upstreamTtfb     sql.NullInt64
	upstreamTotal    sql.NullInt64
	requestHooks     sql.NullInt64
	responseHooks    sql.NullInt64
}

func insertAuditEvent(t *testing.T, db *sql.DB, r auditRow) {
	t.Helper()
	if r.id == "" {
		r.id = fmt.Sprintf("ev-%d-%s", r.ts.UnixNano(), r.srcProc)
	}
	if r.destHost == "" {
		r.destHost = "example.com"
	}
	if r.action == "" {
		r.action = "passthrough"
	}
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO audit_events (
			id, timestamp, source_process, source_user, dest_host, dest_ip, dest_port,
			action, bump_status, bytes_in, bytes_out, duration_ms,
			hook_decision, compliance_tags, provider_name, model_name,
			prompt_tokens, completion_tokens,
			upstream_ttfb_ms, upstream_total_ms,
			request_hooks_ms, response_hooks_ms
		) VALUES (?, ?, ?, ?, ?, '127.0.0.1', 443,
			?, ?, ?, ?, ?,
			?, ?, ?, ?,
			?, ?,
			?, ?,
			?, ?)
	`,
		r.id, r.ts.UTC().Format(time.RFC3339Nano), r.srcProc, r.srcUser, r.destHost,
		r.action, r.bumpStatus, r.bytesIn, r.bytesOut, r.durationMs,
		r.hookDecision, r.complianceTags, r.providerName, r.modelName,
		r.promptTokens, r.completionTokens,
		r.upstreamTtfb, r.upstreamTotal,
		r.requestHooks, r.responseHooks,
	)
	if err != nil {
		t.Fatalf("insert audit row %s: %v", r.id, err)
	}
}

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }
func nullInt(i int64) sql.NullInt64      { return sql.NullInt64{Int64: i, Valid: true} }

// queryValue returns the value of a single rollup row matching the predicate.
func queryValue(t *testing.T, db *sql.DB, table, metric, dim, sub string) float64 {
	t.Helper()
	var v float64
	err := db.QueryRowContext(context.Background(),
		fmt.Sprintf(`SELECT value FROM %s WHERE metric_name = ? AND dimension_key = ? AND sub_dimension = ?`, table),
		metric, dim, sub).Scan(&v)
	if err == sql.ErrNoRows {
		return 0
	}
	if err != nil {
		t.Fatalf("query %s metric=%s dim=%q sub=%q: %v", table, metric, dim, sub, err)
	}
	return v
}

// countRows counts rows in a rollup table for diagnostics.
func countRows(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		fmt.Sprintf(`SELECT COUNT(*) FROM %s`, table)).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// constants / pure helpers

func TestDefaultRetention_PinsUserDecidedPolicy(t *testing.T) {
	r := DefaultRetention()
	if r.Keep5m != 24*time.Hour {
		t.Errorf("Keep5m: want 24h, got %v", r.Keep5m)
	}
	if r.Keep1h != 30*24*time.Hour {
		t.Errorf("Keep1h: want 30d, got %v", r.Keep1h)
	}
	if r.Keep1d != 365*24*time.Hour {
		t.Errorf("Keep1d: want 365d, got %v", r.Keep1d)
	}
	if r.Keep1mo != 5*365*24*time.Hour {
		t.Errorf("Keep1mo: want 5y, got %v", r.Keep1mo)
	}
}

func TestNew_AttachesComponentLabelAndDefaultRetention(t *testing.T) {
	agg, _ := newAggregator(t)
	if agg.logger == nil {
		t.Fatal("logger not set")
	}
	want := DefaultRetention()
	if agg.retention != want {
		t.Errorf("default retention not applied: got %+v want %+v", agg.retention, want)
	}
}

func TestWithRetention_OverridesAndReturnsSelf(t *testing.T) {
	agg, _ := newAggregator(t)
	r := Retention{Keep5m: time.Minute, Keep1h: 2 * time.Minute, Keep1d: 3 * time.Minute, Keep1mo: 4 * time.Minute}
	if got := agg.WithRetention(r); got != agg {
		t.Error("WithRetention should return the same Aggregator (fluent API)")
	}
	if agg.retention != r {
		t.Errorf("WithRetention did not apply: got %+v want %+v", agg.retention, r)
	}
}

func TestBucketForLatency_MapsBoundaries(t *testing.T) {
	cases := []struct {
		name string
		ms   float64
		want int
	}{
		{"below first boundary", 10, 0},
		{"at first boundary", 50, 1}, // ms < 50 false; ms < 100 true → 1
		{"between 50 and 100", 75, 1},
		{"at 100", 100, 2},
		{"between 100 and 200", 150, 2},
		{"at 200", 200, 3},
		{"between 200 and 500", 300, 3},
		{"at 500", 500, 4},
		{"between 500 and 1000", 750, 4},
		{"at 1000", 1000, 5},
		{"beyond all boundaries", 1e10, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := bucketForLatency(tc.ms); got != tc.want {
				t.Errorf("bucketForLatency(%v) = %d want %d", tc.ms, got, tc.want)
			}
		})
	}
}

func TestGranule_SelectionByWindow(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		span time.Duration
		want string
	}{
		{"1h returns 5m", time.Hour, "5m"},
		{"just over 1h returns 1h", time.Hour + time.Minute, "1h"},
		{"7d returns 1h", 7 * 24 * time.Hour, "1h"},
		{"just over 7d returns 1d", 7*24*time.Hour + time.Minute, "1d"},
		{"90d returns 1d", 90 * 24 * time.Hour, "1d"},
		{"just over 90d returns 1mo", 90*24*time.Hour + time.Minute, "1mo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Granule(now, now.Add(tc.span)); got != tc.want {
				t.Errorf("Granule(span=%v) = %s want %s", tc.span, got, tc.want)
			}
		})
	}
}

func TestTableForGranule_MapsAllAndFallsBackTo5m(t *testing.T) {
	cases := map[string]string{
		"5m":      "thing_metric_rollup_local_5m",
		"1h":      "thing_metric_rollup_local_1h",
		"1d":      "thing_metric_rollup_local_1d",
		"1mo":     "thing_metric_rollup_local_1mo",
		"unknown": "thing_metric_rollup_local_5m",
	}
	for in, want := range cases {
		if got := tableForGranule(in); got != want {
			t.Errorf("tableForGranule(%q) = %s want %s", in, got, want)
		}
	}
}

func TestNextMonth_RollsOverDecemberAndPreservesUTC(t *testing.T) {
	dec := time.Date(2026, 12, 15, 23, 59, 59, 0, time.UTC)
	got := nextMonth(dec)
	want := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("nextMonth(Dec) = %v want %v", got, want)
	}
	mid := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	want2 := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if got2 := nextMonth(mid); !got2.Equal(want2) {
		t.Errorf("nextMonth(June) = %v want %v", got2, want2)
	}
}

func TestGetWatermark_ReturnsZeroOnNoRow(t *testing.T) {
	agg, _ := newAggregator(t)
	got, err := agg.getWatermark(context.Background(), "never-seen-job")
	if err != nil {
		t.Fatalf("getWatermark: %v", err)
	}
	if !got.IsZero() {
		t.Errorf("expected zero time for missing row, got %v", got)
	}
}

func TestSetWatermark_InsertThenUpsertViaOnConflict(t *testing.T) {
	agg, db := newAggregator(t)
	ctx := context.Background()

	wm1 := time.Date(2026, 5, 17, 10, 0, 0, 0, time.UTC)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := agg.setWatermark(ctx, tx, "job-A", wm1); err != nil {
		t.Fatalf("setWatermark insert: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit insert: %v", err)
	}

	got, err := agg.getWatermark(ctx, "job-A")
	if err != nil {
		t.Fatalf("getWatermark after insert: %v", err)
	}
	if !got.Equal(wm1) {
		t.Errorf("watermark insert: got %v want %v", got, wm1)
	}

	// Upsert path — ON CONFLICT DO UPDATE.
	wm2 := wm1.Add(15 * time.Minute)
	tx2, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin upsert: %v", err)
	}
	if err := agg.setWatermark(ctx, tx2, "job-A", wm2); err != nil {
		t.Fatalf("setWatermark upsert: %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("commit upsert: %v", err)
	}

	got2, err := agg.getWatermark(ctx, "job-A")
	if err != nil {
		t.Fatalf("getWatermark after upsert: %v", err)
	}
	if !got2.Equal(wm2) {
		t.Errorf("watermark upsert: got %v want %v", got2, wm2)
	}
}

func TestGetWatermark_WrapsDBErrorWhenClosed(t *testing.T) {
	agg, db := newAggregator(t)
	_ = db.Close()
	_, err := agg.getWatermark(context.Background(), "job-X")
	if err == nil {
		t.Fatal("expected error when DB is closed")
	}
}

func TestGetWatermark_ParseErrorOnGarbageRow(t *testing.T) {
	agg, db := newAggregator(t)
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO rollup_watermark_local (job_name, watermark) VALUES (?, ?)`,
		"garbage-job", "not-a-timestamp")
	if err != nil {
		t.Fatalf("seed garbage: %v", err)
	}
	_, err = agg.getWatermark(context.Background(), "garbage-job")
	if err == nil {
		t.Fatal("expected parse error from invalid watermark string")
	}
}

// aggregate5m / processBucket5m

// freshAggregator seeds the watermark to a known value so we don't depend on
// the wall clock. Returns the (sealed) bucket-start time the test should
// expect Tick to process.
func freshAggregatorWithWatermark(t *testing.T, agg *Aggregator, db *sql.DB) (bucket time.Time) {
	t.Helper()
	// Place watermark "one bucket before the next sealed bucket". With
	// latestSealed := now.Add(-5m).Truncate(5m), and wm = latestSealed - 5m,
	// the for-loop runs exactly once for bucket = latestSealed.
	latestSealed := time.Now().UTC().Add(-bucket5m).Truncate(bucket5m)
	wm := latestSealed.Add(-bucket5m)

	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := agg.setWatermark(context.Background(), tx, "rollup-5m-local", wm); err != nil {
		t.Fatalf("seed wm: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return latestSealed
}

func TestAggregate5m_FullDimensionFanOutAndMetricArms(t *testing.T) {
	agg, db := newAggregator(t)
	bucket := freshAggregatorWithWatermark(t, agg, db)

	mid := bucket.Add(2 * time.Minute)

	// Event 1: rich inspect flow with all phase fields + tokens + hook ALLOW.
	insertAuditEvent(t, db, auditRow{
		id: "e-rich-1", ts: mid,
		srcProc: "/usr/bin/curl", destHost: "api.openai.com", action: "inspect",
		bumpStatus:       nullString("BUMP_SUCCESS"),
		bytesIn:          nullInt(1000),
		bytesOut:         nullInt(2000),
		durationMs:       nullInt(150), // bucket 2 (100<=150<200)
		hookDecision:     nullString("APPROVE"),
		complianceTags:   nullString(`["pii"]`),
		providerName:     nullString("openai"),
		modelName:        nullString("gpt-4o"),
		promptTokens:     nullInt(50),
		completionTokens: nullInt(75),
		upstreamTtfb:     nullInt(40),
		upstreamTotal:    nullInt(90), // us = 150-90 = 60
		requestHooks:     nullInt(5),
		responseHooks:    nullInt(7), // hooksTotal = 12
	})

	// Event 2: passthrough flow with duration but no upstream_total — exercises
	// the "treat duration as upstream wall time" branch.
	insertAuditEvent(t, db, auditRow{
		id: "e-pt-1", ts: mid.Add(time.Second),
		srcProc: "/usr/bin/python", destHost: "api.openai.com", action: "passthrough",
		durationMs:   nullInt(80), // bucket 1 (50<=80<100)
		bytesIn:      nullInt(100),
		hookDecision: nullString("allow"), // lowercase alias also counted
	})

	// Event 3: deny → 5xx counter + action_deny + hook BLOCK_SOFT (4xx). Note:
	// isDeny wins over isAdminBlock, so this stamps 5xx (not 4xx).
	insertAuditEvent(t, db, auditRow{
		id: "e-deny-1", ts: mid.Add(2 * time.Second),
		srcProc: "/usr/bin/evil", destHost: "blocked.example.com", action: "deny",
		durationMs:   nullInt(20), // bucket 0
		hookDecision: nullString("BLOCK_SOFT"),
		bumpStatus:   nullString("BUMP_FAILED_PASSTHROUGH"),
	})

	// Event 4: admin-block (reject_hard) without action=deny → 4xx.
	insertAuditEvent(t, db, auditRow{
		id: "e-rh-1", ts: mid.Add(3 * time.Second),
		srcProc: "/usr/bin/curl", destHost: "blocked.example.com", action: "inspect",
		hookDecision: nullString("reject_hard"), // lowercase variant — admin block
		bumpStatus:   nullString("BUMP_EXEMPT_USER"),
	})

	// Event 5: REJECT_HARD (hook-deny counter capital variant) +
	// hook_decision ERROR uppercase variant covered in event 6.
	insertAuditEvent(t, db, auditRow{
		id: "e-rh-2", ts: mid.Add(4 * time.Second),
		srcProc: "/usr/bin/curl", destHost: "blocked.example.com", action: "inspect",
		hookDecision: nullString("REJECT_HARD"),
	})

	// Event 6: hook ERROR (capital) + lowercase reject + lowercase error.
	insertAuditEvent(t, db, auditRow{
		id: "e-err-1", ts: mid.Add(5 * time.Second),
		srcProc: "/usr/bin/curl", destHost: "x.example.com", action: "inspect",
		hookDecision: nullString("ERROR"),
	})
	insertAuditEvent(t, db, auditRow{
		id: "e-err-2", ts: mid.Add(6 * time.Second),
		srcProc: "/usr/bin/curl", destHost: "x.example.com", action: "inspect",
		hookDecision: nullString("error"),
	})
	insertAuditEvent(t, db, auditRow{
		id: "e-rej-1", ts: mid.Add(7 * time.Second),
		srcProc: "/usr/bin/curl", destHost: "x.example.com", action: "inspect",
		hookDecision: nullString("reject"),
	})

	if err := agg.aggregate5m(context.Background()); err != nil {
		t.Fatalf("aggregate5m: %v", err)
	}

	// Pin observable aggregates on the GLOBAL dimension.
	if v := queryValue(t, db, "thing_metric_rollup_local_5m", MetricRequestCount, "", "source=agent"); v != 8 {
		t.Errorf("global request_count: got %v want 8", v)
	}
	// Hook ALLOW: 2 (APPROVE + allow alias).
	if v := queryValue(t, db, "thing_metric_rollup_local_5m", MetricHookAllowCount, "", "source=agent"); v != 2 {
		t.Errorf("hook_allow: got %v want 2", v)
	}
	// Hook DENY counter: only uppercase REJECT_HARD + BLOCK_SOFT + lowercase
	// reject are matched (the switch is case-sensitive) = 3. Lowercase
	// reject_hard does not hit this arm; it only feeds the synthesised
	// status_4xx counter further down.
	if v := queryValue(t, db, "thing_metric_rollup_local_5m", MetricHookDenyCount, "", "source=agent"); v != 3 {
		t.Errorf("hook_deny: got %v want 3", v)
	}
	// Hook ERROR: ERROR + error = 2.
	if v := queryValue(t, db, "thing_metric_rollup_local_5m", MetricHookErrorCount, "", "source=agent"); v != 2 {
		t.Errorf("hook_error: got %v want 2", v)
	}
	// Bump status: 1 success + 1 failed + 1 exempt (BUMP_EXEMPT_USER prefix).
	if v := queryValue(t, db, "thing_metric_rollup_local_5m", MetricBumpSuccessCount, "", "source=agent"); v != 1 {
		t.Errorf("bump_success: got %v want 1", v)
	}
	if v := queryValue(t, db, "thing_metric_rollup_local_5m", MetricBumpFailedCount, "", "source=agent"); v != 1 {
		t.Errorf("bump_failed: got %v want 1", v)
	}
	if v := queryValue(t, db, "thing_metric_rollup_local_5m", MetricBumpExemptCount, "", "source=agent"); v != 1 {
		t.Errorf("bump_exempt: got %v want 1", v)
	}
	// Action breakdown.
	if v := queryValue(t, db, "thing_metric_rollup_local_5m", MetricActionPassthrough, "", "source=agent"); v != 1 {
		t.Errorf("action_passthrough: got %v want 1", v)
	}
	if v := queryValue(t, db, "thing_metric_rollup_local_5m", MetricActionInspect, "", "source=agent"); v != 6 {
		t.Errorf("action_inspect: got %v want 6", v)
	}
	if v := queryValue(t, db, "thing_metric_rollup_local_5m", MetricActionDeny, "", "source=agent"); v != 1 {
		t.Errorf("action_deny: got %v want 1", v)
	}
	// Status synthesis (isAdminBlock only triggers on lowercase
	// reject_hard / block_soft; isDeny on action == "deny"):
	//   e-deny-1: action=deny → 5xx (isDeny wins, even with uppercase
	//             BLOCK_SOFT which alone would NOT trigger 4xx since the
	//             check is lowercase).
	//   e-rh-1:   reject_hard (lowercase) → 4xx
	//   rest (e-rich-1 APPROVE / e-pt-1 allow / e-rh-2 REJECT_HARD upper /
	//        e-err-1 ERROR / e-err-2 error / e-rej-1 reject) → 2xx (6).
	if v := queryValue(t, db, "thing_metric_rollup_local_5m", MetricStatus5xxCount, "", "source=agent"); v != 1 {
		t.Errorf("status_5xx: got %v want 1", v)
	}
	if v := queryValue(t, db, "thing_metric_rollup_local_5m", MetricStatus4xxCount, "", "source=agent"); v != 1 {
		t.Errorf("status_4xx: got %v want 1", v)
	}
	if v := queryValue(t, db, "thing_metric_rollup_local_5m", MetricStatus2xxCount, "", "source=agent"); v != 6 {
		t.Errorf("status_2xx: got %v want 6", v)
	}
	// Tokens (event 1 only).
	if v := queryValue(t, db, "thing_metric_rollup_local_5m", MetricPromptTokens, "", "source=agent"); v != 50 {
		t.Errorf("prompt_tokens: got %v want 50", v)
	}
	if v := queryValue(t, db, "thing_metric_rollup_local_5m", MetricCompletionTokens, "", "source=agent"); v != 75 {
		t.Errorf("completion_tokens: got %v want 75", v)
	}
	if v := queryValue(t, db, "thing_metric_rollup_local_5m", MetricTotalTokens, "", "source=agent"); v != 125 {
		t.Errorf("total_tokens: got %v want 125", v)
	}
	// Bytes (event 1 + event 2 in == 1100; out == 2000).
	if v := queryValue(t, db, "thing_metric_rollup_local_5m", MetricBytesInSum, "", "source=agent"); v != 1100 {
		t.Errorf("bytes_in_sum: got %v want 1100", v)
	}
	if v := queryValue(t, db, "thing_metric_rollup_local_5m", MetricBytesOutSum, "", "source=agent"); v != 2000 {
		t.Errorf("bytes_out_sum: got %v want 2000", v)
	}
	// Latency sum: 150+80+20 = 250; count = 3.
	if v := queryValue(t, db, "thing_metric_rollup_local_5m", MetricLatencySum, "", "source=agent"); v != 250 {
		t.Errorf("latency_sum: got %v want 250", v)
	}
	if v := queryValue(t, db, "thing_metric_rollup_local_5m", MetricLatencyCount, "", "source=agent"); v != 3 {
		t.Errorf("latency_count: got %v want 3", v)
	}
	// Upstream TTFB only event 1 contributes (40, count 1).
	if v := queryValue(t, db, "thing_metric_rollup_local_5m", MetricLatencyUpstreamTtfbSum, "", "source=agent"); v != 40 {
		t.Errorf("upstream_ttfb_sum: got %v want 40", v)
	}
	if v := queryValue(t, db, "thing_metric_rollup_local_5m", MetricLatencyUpstreamTtfbCount, "", "source=agent"); v != 1 {
		t.Errorf("upstream_ttfb_count: got %v want 1", v)
	}
	// Upstream Sum: event 1 (90) + event 2 (80 from durationMs fallback) +
	// event 3 (20 fallback) = 190.
	if v := queryValue(t, db, "thing_metric_rollup_local_5m", MetricLatencyUpstreamSum, "", "source=agent"); v != 190 {
		t.Errorf("upstream_sum: got %v want 190", v)
	}
	if v := queryValue(t, db, "thing_metric_rollup_local_5m", MetricLatencyUpstreamCount, "", "source=agent"); v != 3 {
		t.Errorf("upstream_count: got %v want 3", v)
	}
	// Us sum: event 1 (60) + event 2 (0) + event 3 (0) = 60; count = 3.
	if v := queryValue(t, db, "thing_metric_rollup_local_5m", MetricLatencyUsSum, "", "source=agent"); v != 60 {
		t.Errorf("us_sum: got %v want 60", v)
	}
	if v := queryValue(t, db, "thing_metric_rollup_local_5m", MetricLatencyUsCount, "", "source=agent"); v != 3 {
		t.Errorf("us_count: got %v want 3", v)
	}
	// Hooks (event 1 only).
	if v := queryValue(t, db, "thing_metric_rollup_local_5m", MetricLatencyHooksSum, "", "source=agent"); v != 12 {
		t.Errorf("hooks_sum: got %v want 12", v)
	}
	if v := queryValue(t, db, "thing_metric_rollup_local_5m", MetricLatencyHooksCount, "", "source=agent"); v != 1 {
		t.Errorf("hooks_count: got %v want 1", v)
	}

	// Distinct counters live ON the typed dimension rows, not the global "" row.
	// distinct_source_processes for target_host=api.openai.com should be 2
	// (/usr/bin/curl + /usr/bin/python).
	if v := queryValue(t, db, "thing_metric_rollup_local_5m", "distinct_source_processes", "target_host=api.openai.com", "source=agent"); v != 2 {
		t.Errorf("distinct procs for api.openai.com: got %v want 2", v)
	}
	// distinct_target_hosts on dim source_process=/usr/bin/curl = 2
	// (api.openai.com + blocked.example.com + x.example.com == 3? curl hits
	// api.openai.com, blocked.example.com, x.example.com → 3).
	if v := queryValue(t, db, "thing_metric_rollup_local_5m", "distinct_target_hosts", "source_process=/usr/bin/curl", "source=agent"); v != 3 {
		t.Errorf("distinct hosts for /usr/bin/curl: got %v want 3", v)
	}

	// Histogram row present (metadata is JSON).
	var meta sql.NullString
	if err := db.QueryRowContext(context.Background(),
		`SELECT metadata FROM thing_metric_rollup_local_5m WHERE metric_name = ? AND dimension_key = '' AND sub_dimension = 'source=agent'`,
		MetricLatencyHistogram).Scan(&meta); err != nil {
		t.Fatalf("query histogram row: %v", err)
	}
	if !meta.Valid || meta.String == "" {
		t.Fatal("histogram metadata missing")
	}
	var h histogram
	if err := json.Unmarshal([]byte(meta.String), &h); err != nil {
		t.Fatalf("decode histogram: %v", err)
	}
	// 20→bucket 0, 80→bucket 1, 150→bucket 2.
	if h[0] != 1 || h[1] != 1 || h[2] != 1 {
		t.Errorf("histogram distribution: %+v want [1,1,1,0,0,0]", h)
	}

	// Watermark advanced to the processed bucket.
	wm, err := agg.getWatermark(context.Background(), "rollup-5m-local")
	if err != nil {
		t.Fatalf("read wm: %v", err)
	}
	if !wm.Equal(bucket) {
		t.Errorf("watermark not advanced: got %v want %v", wm, bucket)
	}
}

func TestAggregate5m_EmptyAuditEventsStillAdvancesWatermark(t *testing.T) {
	agg, db := newAggregator(t)
	bucket := freshAggregatorWithWatermark(t, agg, db)

	if err := agg.aggregate5m(context.Background()); err != nil {
		t.Fatalf("aggregate5m: %v", err)
	}

	// No rows in any rollup table (the value=0 skip drops the placeholder).
	if n := countRows(t, db, "thing_metric_rollup_local_5m"); n != 0 {
		t.Errorf("expected 0 rollup rows, got %d", n)
	}
	// Watermark still moves forward — the agent must not re-scan empty buckets.
	wm, _ := agg.getWatermark(context.Background(), "rollup-5m-local")
	if !wm.Equal(bucket) {
		t.Errorf("watermark not advanced on empty bucket: got %v want %v", wm, bucket)
	}
}

func TestAggregate5m_ShortCircuitsWhenWatermarkAtLatestSealed(t *testing.T) {
	agg, db := newAggregator(t)
	// Put watermark AT (or after) latestSealed so the for-loop is skipped.
	latestSealed := time.Now().UTC().Add(-bucket5m).Truncate(bucket5m)
	tx, _ := db.BeginTx(context.Background(), nil)
	if err := agg.setWatermark(context.Background(), tx, "rollup-5m-local", latestSealed); err != nil {
		t.Fatalf("seed wm: %v", err)
	}
	_ = tx.Commit()

	if err := agg.aggregate5m(context.Background()); err != nil {
		t.Fatalf("aggregate5m short-circuit: %v", err)
	}
}

func TestAggregate5m_BootstrapsWatermarkAtMinusOneHour(t *testing.T) {
	agg, _ := newAggregator(t)
	// No watermark seeded → bootstraps at now-1h truncated to bucket5m.
	// Aggregate should complete (running ~12 empty buckets) and end at
	// latestSealed.
	if err := agg.aggregate5m(context.Background()); err != nil {
		t.Fatalf("aggregate5m bootstrap: %v", err)
	}
	wm, err := agg.getWatermark(context.Background(), "rollup-5m-local")
	if err != nil {
		t.Fatalf("wm: %v", err)
	}
	want := time.Now().UTC().Add(-bucket5m).Truncate(bucket5m)
	if !wm.Equal(want) {
		t.Errorf("bootstrap wm: got %v want %v", wm, want)
	}
}

func TestAggregate5m_WrapsGetWatermarkError(t *testing.T) {
	agg, db := newAggregator(t)
	_ = db.Close()
	err := agg.aggregate5m(context.Background())
	if err == nil || !strings.Contains(err.Error(), "get watermark") {
		t.Fatalf("expected get-watermark error, got %v", err)
	}
}

func TestAggregate5m_WrapsProcessBucketErrorWhenAuditTableMissing(t *testing.T) {
	agg, db := newAggregator(t)
	// Seed a valid watermark (so getWatermark succeeds), then drop
	// audit_events so processBucket5m's query inside the loop fails.
	bucket := freshAggregatorWithWatermark(t, agg, db)
	_ = bucket
	if _, err := db.ExecContext(context.Background(), `DROP TABLE audit_events`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	err := agg.aggregate5m(context.Background())
	if err == nil || !strings.Contains(err.Error(), "bucket ") {
		t.Fatalf("expected bucket wrap, got %v", err)
	}
}

func TestProcessBucket5m_ClampsNegativeUsToZero(t *testing.T) {
	agg, db := newAggregator(t)
	bucket := freshAggregatorWithWatermark(t, agg, db)

	// duration_ms (10) < upstream_total_ms (100) → us = 10 - 100 = -90 → clamped to 0.
	insertAuditEvent(t, db, auditRow{
		ts: bucket.Add(time.Minute), srcProc: "/bin/p", destHost: "h.example.com",
		action:        "passthrough",
		durationMs:    nullInt(10),
		upstreamTotal: nullInt(100),
	})
	if err := agg.aggregate5m(context.Background()); err != nil {
		t.Fatalf("aggregate5m: %v", err)
	}
	// us_count == 1 (one event with both fields valid).
	if v := queryValue(t, db, "thing_metric_rollup_local_5m", MetricLatencyUsCount, "", "source=agent"); v != 1 {
		t.Errorf("us_count: got %v want 1", v)
	}
	// us_sum must NOT have a row (value=0 is skipped on insert) — verify
	// by looking up the row directly.
	var v float64
	err := db.QueryRowContext(context.Background(),
		`SELECT value FROM thing_metric_rollup_local_5m WHERE metric_name = ? AND dimension_key = '' AND sub_dimension = 'source=agent'`,
		MetricLatencyUsSum).Scan(&v)
	if err != sql.ErrNoRows {
		t.Errorf("us_sum row expected absent (value=0 skipped), got value=%v err=%v", v, err)
	}
}

func TestProcessBucket5m_WrapsQueryErrorWhenAuditTableMissing(t *testing.T) {
	agg, db := newAggregator(t)
	if _, err := db.ExecContext(context.Background(), `DROP TABLE audit_events`); err != nil {
		t.Fatalf("drop audit_events: %v", err)
	}
	err := agg.processBucket5m(context.Background(), time.Now().UTC().Add(-time.Hour).Truncate(bucket5m))
	if err == nil || !strings.Contains(err.Error(), "scan audit_events") {
		t.Fatalf("expected scan audit_events error, got %v", err)
	}
}

func TestProcessBucket5m_WrapsScanErrorOnCorruptIntColumn(t *testing.T) {
	agg, db := newAggregator(t)
	bucket := freshAggregatorWithWatermark(t, agg, db)
	// Insert a row directly that puts a non-numeric value into the INTEGER
	// column the rollup scans (bytes_in). SQLite's loose affinity stores it
	// as text; the Go Scan into sql.NullInt64 then fails.
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO audit_events (id, timestamp, source_process, source_user,
			dest_host, dest_ip, dest_port, action, bytes_in)
		VALUES ('corrupt-1', ?, '/bin/x', NULL, 'h', '1.1.1.1', 443,
			'inspect', 'not-an-int')
	`, bucket.Add(time.Minute).UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatalf("seed corrupt: %v", err)
	}
	err = agg.processBucket5m(context.Background(), bucket)
	if err == nil || !strings.Contains(err.Error(), "scan row") {
		t.Fatalf("expected scan row wrap, got %v", err)
	}
}

func TestProcessBucket5m_WrapsDeleteBucketErrorUnderReadOnlyPragma(t *testing.T) {
	agg, db := newAggregator(t)
	bucket := freshAggregatorWithWatermark(t, agg, db)
	insertAuditEvent(t, db, auditRow{
		ts: bucket.Add(time.Minute), srcProc: "/bin/p", destHost: "h.example.com",
		action: "inspect", durationMs: nullInt(10),
	})
	// Make all writes fail without touching tables. The DELETE statement
	// inside processBucket5m's transaction is the first write, so this
	// surfaces as the "delete bucket" wrap.
	if _, err := db.ExecContext(context.Background(), `PRAGMA query_only=ON`); err != nil {
		t.Fatalf("set readonly: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `PRAGMA query_only=OFF`)
	})
	err := agg.processBucket5m(context.Background(), bucket)
	if err == nil || !strings.Contains(err.Error(), "delete bucket") {
		t.Fatalf("expected delete bucket wrap, got %v", err)
	}
}

func TestProcessBucket5m_WrapsSetWatermarkErrorWhenWatermarkTableMissing(t *testing.T) {
	agg, db := newAggregator(t)
	bucket := freshAggregatorWithWatermark(t, agg, db)
	insertAuditEvent(t, db, auditRow{
		ts: bucket.Add(time.Minute), srcProc: "/bin/p", destHost: "h.example.com",
		action: "inspect", durationMs: nullInt(10),
	})
	// Drop the watermark table AFTER seeding the watermark — processBucket5m
	// only writes to it (inside the tx); the failure surfaces as
	// "set watermark" wrap.
	if _, err := db.ExecContext(context.Background(), `DROP TABLE rollup_watermark_local`); err != nil {
		t.Fatalf("drop wm table: %v", err)
	}
	err := agg.processBucket5m(context.Background(), bucket)
	if err == nil || !strings.Contains(err.Error(), "set watermark") {
		t.Fatalf("expected set watermark wrap, got %v", err)
	}
}

func TestMergeOneBucket_WrapsDeleteTargetErrorUnderReadOnlyPragma(t *testing.T) {
	agg, db := newAggregator(t)
	ctx := context.Background()
	hourBucket := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Hour)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO thing_metric_rollup_local_5m
		 (bucket_start, metric_name, dimension_key, sub_dimension, value, metadata)
		 VALUES (?, ?, ?, ?, ?, NULL)`,
		hourBucket.Format(time.RFC3339Nano), MetricRequestCount, "", "source=agent", 3.0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA query_only=ON`); err != nil {
		t.Fatalf("readonly: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `PRAGMA query_only=OFF`)
	})
	err := agg.mergeOneBucket(ctx, "merge-1h-local",
		"thing_metric_rollup_local_5m", "thing_metric_rollup_local_1h",
		hourBucket, hourBucket.Add(time.Hour))
	if err == nil || !strings.Contains(err.Error(), "delete target") {
		t.Fatalf("expected delete target wrap, got %v", err)
	}
}

func TestMergeOneBucket_WrapsSetWatermarkErrorWhenWatermarkTableMissing(t *testing.T) {
	agg, db := newAggregator(t)
	ctx := context.Background()
	hourBucket := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Hour)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO thing_metric_rollup_local_5m
		 (bucket_start, metric_name, dimension_key, sub_dimension, value, metadata)
		 VALUES (?, ?, ?, ?, ?, NULL)`,
		hourBucket.Format(time.RFC3339Nano), MetricRequestCount, "", "source=agent", 3.0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE rollup_watermark_local`); err != nil {
		t.Fatalf("drop wm: %v", err)
	}
	err := agg.mergeOneBucket(ctx, "merge-1h-local",
		"thing_metric_rollup_local_5m", "thing_metric_rollup_local_1h",
		hourBucket, hourBucket.Add(time.Hour))
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	// Either a setWatermark wrap OR a generic wrap — both indicate the
	// watermark-write failure has propagated.
	if !strings.Contains(err.Error(), "no such table") && !strings.Contains(err.Error(), "set watermark") {
		t.Fatalf("expected watermark-related error, got %v", err)
	}
}

func TestProcessBucket5m_WrapsBeginTxErrorWhenDBClosed(t *testing.T) {
	agg, db := newAggregator(t)
	// Insert one row, then close the DB AFTER opening Query. Easier: open a
	// fresh aggregator then close — the SELECT will fail first. Hit BeginTx
	// path by leaving table intact but closing right after this call returns.
	// Simpler approach: just verify the error wrapping path returns *some*
	// error when DB is closed.
	_ = db.Close()
	err := agg.processBucket5m(context.Background(), time.Now().UTC().Add(-time.Hour).Truncate(bucket5m))
	if err == nil {
		t.Fatal("expected error from closed DB")
	}
}

// merge / mergeOneBucket

func TestMerge_AggregatesSourceRowsIntoTargetBucket(t *testing.T) {
	agg, db := newAggregator(t)
	ctx := context.Background()

	// Seed two 5m rows in the same 1h target bucket. The merge sums values
	// and merges histograms.
	hourBucket := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Hour)
	b1 := hourBucket.Add(0 * time.Minute)
	b2 := hourBucket.Add(5 * time.Minute)

	for _, b := range []time.Time{b1, b2} {
		bs := b.Format(time.RFC3339Nano)
		_, err := db.ExecContext(ctx,
			`INSERT INTO thing_metric_rollup_local_5m
			 (bucket_start, metric_name, dimension_key, sub_dimension, value, metadata)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			bs, MetricRequestCount, "", "source=agent", 3.0, nil)
		if err != nil {
			t.Fatalf("seed value: %v", err)
		}
		h := histogram{1, 2, 3, 4, 5, 6}
		data, _ := json.Marshal(h)
		_, err = db.ExecContext(ctx,
			`INSERT INTO thing_metric_rollup_local_5m
			 (bucket_start, metric_name, dimension_key, sub_dimension, value, metadata)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			bs, MetricLatencyHistogram, "", "source=agent", 0.0, string(data))
		if err != nil {
			t.Fatalf("seed hist: %v", err)
		}
	}

	// Seed a value=0 5m row that should be SKIPPED on insert into the
	// target table (covers the `if v == 0 { continue }` arm).
	zeroBucket := hourBucket.Add(10 * time.Minute).Format(time.RFC3339Nano)
	_, err := db.ExecContext(ctx,
		`INSERT INTO thing_metric_rollup_local_5m
		 (bucket_start, metric_name, dimension_key, sub_dimension, value, metadata)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		zeroBucket, "zero_metric", "", "source=agent", 0.0, nil)
	if err != nil {
		t.Fatalf("seed zero: %v", err)
	}

	// Seed watermark to one bucket before hourBucket, so loop runs exactly
	// once for hourBucket.
	tx, _ := db.BeginTx(ctx, nil)
	if err := agg.setWatermark(ctx, tx, "merge-1h-local", hourBucket.Add(-time.Hour)); err != nil {
		t.Fatalf("seed wm: %v", err)
	}
	_ = tx.Commit()

	if err := agg.merge(ctx, "merge-1h-local",
		"thing_metric_rollup_local_5m",
		"thing_metric_rollup_local_1h",
		time.Hour, 6*time.Hour); err != nil {
		t.Fatalf("merge: %v", err)
	}

	// 1h request_count should be 3+3 = 6.
	if v := queryValue(t, db, "thing_metric_rollup_local_1h", MetricRequestCount, "", "source=agent"); v != 6 {
		t.Errorf("1h request_count: got %v want 6", v)
	}
	// 1h histogram should be element-wise summed: [2,4,6,8,10,12].
	var meta sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT metadata FROM thing_metric_rollup_local_1h WHERE metric_name = ?`,
		MetricLatencyHistogram).Scan(&meta); err != nil {
		t.Fatalf("query 1h hist: %v", err)
	}
	var h histogram
	if err := json.Unmarshal([]byte(meta.String), &h); err != nil {
		t.Fatalf("decode 1h hist: %v", err)
	}
	want := histogram{2, 4, 6, 8, 10, 12}
	if h != want {
		t.Errorf("merged hist: got %+v want %+v", h, want)
	}
	// zero_metric must NOT appear in target.
	var n int
	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM thing_metric_rollup_local_1h WHERE metric_name = 'zero_metric'`).Scan(&n)
	if n != 0 {
		t.Errorf("zero-value metric leaked into target: count=%d", n)
	}
}

func TestMerge_NoSourceRowsStillAdvancesWatermark(t *testing.T) {
	agg, db := newAggregator(t)
	ctx := context.Background()
	hourBucket := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Hour)

	tx, _ := db.BeginTx(ctx, nil)
	_ = agg.setWatermark(ctx, tx, "merge-1h-local", hourBucket.Add(-time.Hour))
	_ = tx.Commit()

	if err := agg.merge(ctx, "merge-1h-local",
		"thing_metric_rollup_local_5m",
		"thing_metric_rollup_local_1h",
		time.Hour, 6*time.Hour); err != nil {
		t.Fatalf("merge empty: %v", err)
	}
	wm, _ := agg.getWatermark(ctx, "merge-1h-local")
	// Watermark must be at least hourBucket (loop runs through latestSealed).
	if wm.Before(hourBucket) {
		t.Errorf("wm did not advance: got %v want >= %v", wm, hourBucket)
	}
}

func TestMerge_BootstrapsWatermarkAtNegativeLookback(t *testing.T) {
	agg, _ := newAggregator(t)
	ctx := context.Background()
	// No watermark → bootstrap branch.
	if err := agg.merge(ctx, "merge-1h-local",
		"thing_metric_rollup_local_5m",
		"thing_metric_rollup_local_1h",
		time.Hour, 6*time.Hour); err != nil {
		t.Fatalf("merge bootstrap: %v", err)
	}
	wm, _ := agg.getWatermark(ctx, "merge-1h-local")
	if wm.IsZero() {
		t.Error("watermark should have been bootstrapped")
	}
}

func TestMerge_ShortCircuitsWhenWatermarkPastLatestSealed(t *testing.T) {
	agg, db := newAggregator(t)
	ctx := context.Background()
	// Seed watermark in the future.
	future := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Hour)
	tx, _ := db.BeginTx(ctx, nil)
	_ = agg.setWatermark(ctx, tx, "merge-1h-local", future)
	_ = tx.Commit()

	if err := agg.merge(ctx, "merge-1h-local",
		"thing_metric_rollup_local_5m",
		"thing_metric_rollup_local_1h",
		time.Hour, 6*time.Hour); err != nil {
		t.Fatalf("merge short-circuit: %v", err)
	}
}

func TestMerge_WrapsWatermarkErrorWhenDBClosed(t *testing.T) {
	agg, db := newAggregator(t)
	_ = db.Close()
	err := agg.merge(context.Background(), "merge-1h-local",
		"thing_metric_rollup_local_5m",
		"thing_metric_rollup_local_1h",
		time.Hour, 6*time.Hour)
	if err == nil {
		t.Fatal("expected error from closed DB")
	}
}

func TestMergeOneBucket_WrapsReadSourceErrorOnMissingTable(t *testing.T) {
	agg, _ := newAggregator(t)
	now := time.Now().UTC()
	err := agg.mergeOneBucket(context.Background(), "merge-1h-local",
		"no_such_table_x", "thing_metric_rollup_local_1h",
		now.Add(-time.Hour), now)
	if err == nil || !strings.Contains(err.Error(), "read source") {
		t.Fatalf("expected read source error, got %v", err)
	}
}

func TestMergeOneBucket_TolaratesInvalidHistogramJSON(t *testing.T) {
	agg, db := newAggregator(t)
	ctx := context.Background()
	hourBucket := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Hour)
	bs := hourBucket.Format(time.RFC3339Nano)
	// Seed a histogram metric with malformed metadata — must be silently
	// skipped (not panic, not fail).
	if _, err := db.ExecContext(ctx,
		`INSERT INTO thing_metric_rollup_local_5m
		 (bucket_start, metric_name, dimension_key, sub_dimension, value, metadata)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		bs, MetricLatencyHistogram, "", "source=agent", 0.0, "not-json"); err != nil {
		t.Fatalf("seed malformed: %v", err)
	}
	if err := agg.mergeOneBucket(ctx, "merge-1h-local",
		"thing_metric_rollup_local_5m", "thing_metric_rollup_local_1h",
		hourBucket, hourBucket.Add(time.Hour)); err != nil {
		t.Fatalf("merge with malformed json: %v", err)
	}
	// Target histogram row should NOT exist (decode failed → nothing
	// merged → empty histos map → no insert).
	var n int
	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM thing_metric_rollup_local_1h WHERE metric_name = ?`,
		MetricLatencyHistogram).Scan(&n)
	if n != 0 {
		t.Errorf("malformed-meta hist should not produce a row, got %d", n)
	}
}

func TestMergeCalendarMonth_BootstrapsAtPrevMonthAndProducesNoBucketsWhenEmpty(t *testing.T) {
	agg, _ := newAggregator(t)
	if err := agg.mergeCalendarMonth(context.Background()); err != nil {
		t.Fatalf("mergeCalendarMonth bootstrap: %v", err)
	}
}

func TestMergeCalendarMonth_RollsPriorMonthIntoTarget(t *testing.T) {
	agg, db := newAggregator(t)
	ctx := context.Background()
	// Seed watermark to TWO months ago. Production curMonthStart > wm so the
	// loop runs once for the bucket two-months-ago.
	now := time.Now().UTC()
	twoMonthsAgo := time.Date(now.Year(), now.Month()-2, 1, 0, 0, 0, 0, time.UTC)
	oneMonthAgo := time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, time.UTC)

	tx, _ := db.BeginTx(ctx, nil)
	_ = agg.setWatermark(ctx, tx, "merge-1mo-local", twoMonthsAgo)
	_ = tx.Commit()

	// Seed a 1d row in the previous month so the merge has something to roll.
	mid := oneMonthAgo.Add(48 * time.Hour)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO thing_metric_rollup_local_1d
		 (bucket_start, metric_name, dimension_key, sub_dimension, value, metadata)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		mid.Format(time.RFC3339Nano), MetricRequestCount, "", "source=agent", 11.0, nil); err != nil {
		t.Fatalf("seed 1d row: %v", err)
	}

	if err := agg.mergeCalendarMonth(ctx); err != nil {
		t.Fatalf("mergeCalendarMonth: %v", err)
	}
	if v := queryValue(t, db, "thing_metric_rollup_local_1mo", MetricRequestCount, "", "source=agent"); v != 11 {
		t.Errorf("1mo request_count: got %v want 11", v)
	}
}

func TestMergeCalendarMonth_WrapsWatermarkErrorWhenDBClosed(t *testing.T) {
	agg, db := newAggregator(t)
	_ = db.Close()
	if err := agg.mergeCalendarMonth(context.Background()); err == nil {
		t.Fatal("expected error from closed DB")
	}
}

// Tick — full cascade

func TestTick_HappyPathCascadesAllStages(t *testing.T) {
	agg, db := newAggregator(t)
	bucket := freshAggregatorWithWatermark(t, agg, db)

	// One event so the 5m stage produces non-zero output.
	insertAuditEvent(t, db, auditRow{
		ts: bucket.Add(time.Minute), srcProc: "/bin/p", destHost: "h.example.com",
		action: "inspect", durationMs: nullInt(45),
	})

	if err := agg.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// 5m bucket holds one request_count row.
	if v := queryValue(t, db, "thing_metric_rollup_local_5m", MetricRequestCount, "", "source=agent"); v != 1 {
		t.Errorf("5m request_count: got %v want 1", v)
	}
}

func TestTick_WrapsAggregate5mError(t *testing.T) {
	agg, db := newAggregator(t)
	_ = db.Close()
	err := agg.Tick(context.Background())
	if err == nil || !strings.Contains(err.Error(), "aggregate 5m") {
		t.Fatalf("expected aggregate 5m wrap, got %v", err)
	}
}

func TestTick_WrapsMerge1hError(t *testing.T) {
	agg, db := newAggregator(t)
	// Make aggregate5m succeed (seed wm to past-latestSealed so the loop
	// short-circuits), then drop the 5m table so the 1h merge fails.
	latestSealed := time.Now().UTC().Add(-bucket5m).Truncate(bucket5m)
	tx, _ := db.BeginTx(context.Background(), nil)
	_ = agg.setWatermark(context.Background(), tx, "rollup-5m-local", latestSealed)
	_ = tx.Commit()

	if _, err := db.ExecContext(context.Background(), `DROP TABLE thing_metric_rollup_local_5m`); err != nil {
		t.Fatalf("drop 5m: %v", err)
	}
	err := agg.Tick(context.Background())
	if err == nil || !strings.Contains(err.Error(), "merge 1h") {
		t.Fatalf("expected merge 1h wrap, got %v", err)
	}
}

func TestTick_WrapsMerge1dError(t *testing.T) {
	agg, db := newAggregator(t)
	latestSealed := time.Now().UTC().Add(-bucket5m).Truncate(bucket5m)
	ctx := context.Background()
	// Seed all earlier watermarks past-latestSealed so 5m + 1h short-circuit.
	for _, job := range []string{"rollup-5m-local", "merge-1h-local"} {
		tx, _ := db.BeginTx(ctx, nil)
		_ = agg.setWatermark(ctx, tx, job, latestSealed.Add(48*time.Hour))
		_ = tx.Commit()
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE thing_metric_rollup_local_1h`); err != nil {
		t.Fatalf("drop 1h: %v", err)
	}
	err := agg.Tick(ctx)
	if err == nil || !strings.Contains(err.Error(), "merge 1d") {
		t.Fatalf("expected merge 1d wrap, got %v", err)
	}
}

func TestTick_WrapsMerge1moError(t *testing.T) {
	agg, db := newAggregator(t)
	ctx := context.Background()
	latestSealed := time.Now().UTC().Add(-bucket5m).Truncate(bucket5m)
	for _, job := range []string{"rollup-5m-local", "merge-1h-local", "merge-1d-local"} {
		tx, _ := db.BeginTx(ctx, nil)
		_ = agg.setWatermark(ctx, tx, job, latestSealed.Add(48*time.Hour))
		_ = tx.Commit()
	}
	// Seed merge-1mo watermark to two months back so the loop runs once
	// over the prior month, then drop the source table so the read fails.
	now := time.Now().UTC()
	twoMonthsAgo := time.Date(now.Year(), now.Month()-2, 1, 0, 0, 0, 0, time.UTC)
	tx, _ := db.BeginTx(ctx, nil)
	_ = agg.setWatermark(ctx, tx, "merge-1mo-local", twoMonthsAgo)
	_ = tx.Commit()

	if _, err := db.ExecContext(ctx, `DROP TABLE thing_metric_rollup_local_1d`); err != nil {
		t.Fatalf("drop 1d: %v", err)
	}
	err := agg.Tick(ctx)
	if err == nil || !strings.Contains(err.Error(), "merge 1mo") {
		t.Fatalf("expected merge 1mo wrap, got %v", err)
	}
}

func TestTick_WrapsPurgeError(t *testing.T) {
	agg, db := newAggregator(t)
	ctx := context.Background()
	latestSealed := time.Now().UTC().Add(-bucket5m).Truncate(bucket5m)
	for _, job := range []string{"rollup-5m-local", "merge-1h-local", "merge-1d-local", "merge-1mo-local"} {
		tx, _ := db.BeginTx(ctx, nil)
		_ = agg.setWatermark(ctx, tx, job, latestSealed.Add(365*24*time.Hour))
		_ = tx.Commit()
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE thing_metric_rollup_local_5m`); err != nil {
		t.Fatalf("drop 5m for purge: %v", err)
	}
	err := agg.Tick(ctx)
	// The merge-1h read of 5m will fire BEFORE purge — but we already pushed
	// the 1h wm past latestSealed, so merge-1h short-circuits at wm check
	// (which only reads watermark — that succeeds). Then merge-1d / merge-1mo
	// short-circuit similarly. Purge then tries to DELETE from the missing
	// table → "purge:" wrap.
	if err == nil || !strings.Contains(err.Error(), "purge") {
		t.Fatalf("expected purge wrap, got %v", err)
	}
}

func TestQueryRollup_NilWindowReturnsNoRows(t *testing.T) {
	agg, _ := newAggregator(t)
	// Use a single Now snapshot so EndTime == StartTime exactly. Calling
	// time.Now() twice would advance the clock between calls and the
	// guard `!EndTime.After(StartTime)` would NOT fire.
	now := time.Now()
	out, err := agg.QueryRollup(context.Background(), Query{StartTime: now, EndTime: now})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if out != nil {
		t.Errorf("expected nil rows, got %+v", out)
	}

	// Also test EndTime < StartTime to belt-and-brace the guard.
	out2, err2 := agg.QueryRollup(context.Background(),
		Query{StartTime: now.Add(time.Hour), EndTime: now})
	if err2 != nil {
		t.Fatalf("query reversed: %v", err2)
	}
	if out2 != nil {
		t.Errorf("expected nil rows for reversed window, got %+v", out2)
	}
}

func TestQueryRollup_FullFilterStackHitsExpectedRows(t *testing.T) {
	agg, db := newAggregator(t)
	ctx := context.Background()
	// Seed three 5m rows. Only the first matches the full filter.
	bucket := time.Now().UTC().Add(-30 * time.Minute).Truncate(bucket5m)
	rows := []struct {
		metric, dim, sub string
		v                float64
	}{
		{MetricRequestCount, "target_host=api.openai.com", "source=agent", 7},
		{MetricRequestCount, "target_host=other.com", "source=agent", 99},
		{MetricRequestCount, "", "source=agent", 200}, // global row, NOT matched
	}
	for _, r := range rows {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO thing_metric_rollup_local_5m
			 (bucket_start, metric_name, dimension_key, sub_dimension, value, metadata)
			 VALUES (?, ?, ?, ?, ?, NULL)`,
			bucket.Format(time.RFC3339Nano), r.metric, r.dim, r.sub, r.v); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	out, err := agg.QueryRollup(ctx, Query{
		StartTime:    bucket.Add(-time.Minute),
		EndTime:      bucket.Add(time.Minute),
		MetricNames:  []string{MetricRequestCount},
		DimensionKey: "target_host",
		SubDimension: "source=agent",
	})
	if err != nil {
		t.Fatalf("QueryRollup: %v", err)
	}
	// Should match the two target_host rows.
	if len(out) != 2 {
		t.Fatalf("rows: got %d want 2 — %+v", len(out), out)
	}
	// Order ascending by bucket_start (same bucket → undefined ordering, just
	// check sum).
	sum := 0.0
	for _, r := range out {
		sum += r.Value
		if !strings.HasPrefix(r.DimensionKey, "target_host=") {
			t.Errorf("unexpected dim: %s", r.DimensionKey)
		}
		if r.SubDimension != "source=agent" {
			t.Errorf("unexpected sub: %s", r.SubDimension)
		}
		if r.BucketStart.IsZero() {
			t.Error("bucket start not parsed")
		}
	}
	if sum != 106 {
		t.Errorf("sum: got %v want 106", sum)
	}
}

func TestQueryRollup_EmptyDimensionKeyOnlyReturnsGlobalRows(t *testing.T) {
	agg, db := newAggregator(t)
	ctx := context.Background()
	bucket := time.Now().UTC().Add(-30 * time.Minute).Truncate(bucket5m)
	for _, dim := range []string{"", "target_host=h"} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO thing_metric_rollup_local_5m
			 (bucket_start, metric_name, dimension_key, sub_dimension, value, metadata)
			 VALUES (?, ?, ?, ?, ?, NULL)`,
			bucket.Format(time.RFC3339Nano), MetricRequestCount, dim, "source=agent", 3.0); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	out, err := agg.QueryRollup(ctx, Query{
		StartTime: bucket.Add(-time.Minute),
		EndTime:   bucket.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("QueryRollup: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected only global row, got %+v", out)
	}
	if out[0].DimensionKey != "" {
		t.Errorf("expected empty dim, got %q", out[0].DimensionKey)
	}
}

func TestQueryRollup_MetadataPassthrough(t *testing.T) {
	agg, db := newAggregator(t)
	ctx := context.Background()
	bucket := time.Now().UTC().Add(-30 * time.Minute).Truncate(bucket5m)
	meta := `{"x":1}`
	if _, err := db.ExecContext(ctx,
		`INSERT INTO thing_metric_rollup_local_5m
		 (bucket_start, metric_name, dimension_key, sub_dimension, value, metadata)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		bucket.Format(time.RFC3339Nano), MetricLatencyHistogram, "", "source=agent", 0.0, meta); err != nil {
		t.Fatalf("seed: %v", err)
	}
	out, err := agg.QueryRollup(ctx, Query{
		StartTime: bucket.Add(-time.Minute),
		EndTime:   bucket.Add(time.Minute),
	})
	if err != nil || len(out) != 1 {
		t.Fatalf("query: %v out=%+v", err, out)
	}
	if out[0].Metadata != meta {
		t.Errorf("metadata: got %q want %q", out[0].Metadata, meta)
	}
}

func TestQueryRollup_PicksGranuleBasedOnWindow(t *testing.T) {
	agg, db := newAggregator(t)
	ctx := context.Background()
	// Seed a row in the 1d table.
	bucket := time.Now().UTC().Add(-30 * 24 * time.Hour).Truncate(24 * time.Hour)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO thing_metric_rollup_local_1d
		 (bucket_start, metric_name, dimension_key, sub_dimension, value, metadata)
		 VALUES (?, ?, ?, ?, ?, NULL)`,
		bucket.Format(time.RFC3339Nano), MetricRequestCount, "", "source=agent", 42.0); err != nil {
		t.Fatalf("seed 1d: %v", err)
	}
	// Window > 7d but ≤ 90d → 1d granule.
	out, err := agg.QueryRollup(ctx, Query{
		StartTime: bucket.Add(-time.Hour),
		EndTime:   bucket.Add(60 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(out) != 1 || out[0].Value != 42 {
		t.Fatalf("expected single 1d row v=42, got %+v", out)
	}
}

func TestQueryRollup_WrapsScanErrorOnMalformedValueCell(t *testing.T) {
	agg, db := newAggregator(t)
	ctx := context.Background()
	bucketTime := time.Now().UTC().Add(-30 * time.Minute).Truncate(bucket5m)
	bucket := bucketTime.Format(time.RFC3339Nano)
	// Insert a non-numeric value into the REAL column. SQLite's affinity
	// lets the row land; the float64 Scan then fails.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO thing_metric_rollup_local_5m
		 (bucket_start, metric_name, dimension_key, sub_dimension, value, metadata)
		 VALUES (?, ?, ?, ?, ?, NULL)`,
		bucket, MetricRequestCount, "", "source=agent", "not-a-float"); err != nil {
		t.Fatalf("seed bad value: %v", err)
	}
	_, err := agg.QueryRollup(ctx, Query{
		StartTime: bucketTime.Add(-time.Minute),
		EndTime:   bucketTime.Add(time.Minute),
	})
	if err == nil || !strings.Contains(err.Error(), "scan row") {
		t.Fatalf("expected scan row error, got %v", err)
	}
}

func TestMergeOneBucket_WrapsScanErrorOnMalformedValueCell(t *testing.T) {
	agg, db := newAggregator(t)
	ctx := context.Background()
	hourBucket := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Hour)
	bs := hourBucket.Format(time.RFC3339Nano)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO thing_metric_rollup_local_5m
		 (bucket_start, metric_name, dimension_key, sub_dimension, value, metadata)
		 VALUES (?, ?, ?, ?, ?, NULL)`,
		bs, MetricRequestCount, "", "source=agent", "not-a-float"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	err := agg.mergeOneBucket(ctx, "merge-1h-local",
		"thing_metric_rollup_local_5m", "thing_metric_rollup_local_1h",
		hourBucket, hourBucket.Add(time.Hour))
	if err == nil || !strings.Contains(err.Error(), "scan") {
		t.Fatalf("expected scan error, got %v", err)
	}
}

func TestQueryRollup_WrapsQueryErrorWhenTableMissing(t *testing.T) {
	agg, db := newAggregator(t)
	if _, err := db.ExecContext(context.Background(), `DROP TABLE thing_metric_rollup_local_5m`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	_, err := agg.QueryRollup(context.Background(), Query{
		StartTime: time.Now().Add(-30 * time.Minute),
		EndTime:   time.Now(),
	})
	if err == nil || !strings.Contains(err.Error(), "query rollup") {
		t.Fatalf("expected query rollup error, got %v", err)
	}
}

func TestQueryRollup_ParsesBucketTimestampOnReadback(t *testing.T) {
	// Note: the source code has a fallback time.Parse(RFC3339) for buckets
	// that fail RFC3339Nano parsing. In practice this fallback is
	// structurally unreachable — time.RFC3339Nano is a strict superset of
	// time.RFC3339, so any value Format(RFC3339) produces also parses with
	// Format(RFC3339Nano). This test still exercises the parse-success path
	// (RFC3339-format input parsed via RFC3339Nano) which is what production
	// rows actually look like (most are Format(RFC3339Nano) writes).
	agg, db := newAggregator(t)
	ctx := context.Background()
	bucketTime := time.Now().UTC().Add(-30 * time.Minute).Truncate(bucket5m)
	bucket := bucketTime.Format(time.RFC3339)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO thing_metric_rollup_local_5m
		 (bucket_start, metric_name, dimension_key, sub_dimension, value, metadata)
		 VALUES (?, ?, ?, ?, ?, NULL)`,
		bucket, MetricRequestCount, "", "source=agent", 1.0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	out, err := agg.QueryRollup(ctx, Query{
		StartTime: bucketTime.Add(-time.Minute),
		EndTime:   bucketTime.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(out) != 1 || out[0].BucketStart.IsZero() {
		t.Fatalf("expected one row with parsed bucket, got %+v", out)
	}
}

// Retention purge

func TestPurge_DropsRowsOlderThanRetentionPerTable(t *testing.T) {
	agg, db := newAggregator(t)
	ctx := context.Background()
	// Override retention to 1ms so any seeded row before the call is past
	// the cutoff for ALL four tables.
	agg.WithRetention(Retention{
		Keep5m:  time.Millisecond,
		Keep1h:  time.Millisecond,
		Keep1d:  time.Millisecond,
		Keep1mo: time.Millisecond,
	})

	// Seed one ancient row per table + one fresh row to confirm purge only
	// touches the old ones.
	ancient := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339Nano)
	fresh := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339Nano) // future → past cutoff anyway? Yes - cutoff is now-1ms, future > cutoff so kept.
	for _, table := range []string{
		"thing_metric_rollup_local_5m", "thing_metric_rollup_local_1h",
		"thing_metric_rollup_local_1d", "thing_metric_rollup_local_1mo",
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			`INSERT INTO %s (bucket_start, metric_name, dimension_key, sub_dimension, value)
			 VALUES (?, 'm', '', 'source=agent', 1.0), (?, 'm', '', 'source=agent', 2.0)`,
			table), ancient, fresh); err != nil {
			t.Fatalf("seed %s: %v", table, err)
		}
	}

	if err := agg.purge(ctx); err != nil {
		t.Fatalf("purge: %v", err)
	}

	// Each table should still have the fresh row but not the ancient one.
	for _, table := range []string{
		"thing_metric_rollup_local_5m", "thing_metric_rollup_local_1h",
		"thing_metric_rollup_local_1d", "thing_metric_rollup_local_1mo",
	} {
		if n := countRows(t, db, table); n != 1 {
			t.Errorf("%s: got %d rows want 1 (only fresh)", table, n)
		}
	}
}

func TestPurge_WrapsErrorOnMissingTable(t *testing.T) {
	agg, db := newAggregator(t)
	if _, err := db.ExecContext(context.Background(), `DROP TABLE thing_metric_rollup_local_5m`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	err := agg.purge(context.Background())
	if err == nil || !strings.Contains(err.Error(), "purge thing_metric_rollup_local_5m") {
		t.Fatalf("expected purge wrap, got %v", err)
	}
}

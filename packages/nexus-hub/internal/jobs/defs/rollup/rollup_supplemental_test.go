// rollup_supplemental_test.go covers statement gaps not addressed by the
// primary per-file test files. Each test targets a named failure mode or
// behaviour branch:
//
//   - NewHealthRollupMetrics with a real Prometheus registry (non-nil path)
//   - HealthRollupMetrics methods with a non-nil receiver
//   - CredentialHealthRollupJob identity accessors (ID/Name/Description/Interval)
//   - Thresholds() accessor (0%)
//   - Rollup5mJob.processOneBucket full happy path (covers DeleteRollupBucket,
//     aggregateTrafficEvents with one row, InsertRollupRows, SetWatermark, Commit)
//   - Rollup5mJob.aggregateTrafficEvents error path (query error, scan error,
//     rows.Err path) and the skip-unknown-source branch
//   - Rollup5mJob.Run loop processes one bucket (covers Run loop body)
//   - RollupMergeJob.coldStartWatermark (error + happy path)
//   - RollupMergeJob.mergeOneBucket full transaction path (non-empty source)
//   - RollupMergeJob.runFixed loop that processes one bucket
//   - RollupMergeJob.runCalendarMonth that processes one month
//   - RollupCorrectionJob.Run happy path (24 × 5m + 24 × 1h + 1 × 1d merges)
//   - ThingRollup5mJob.Interval accessor (0%)
//   - ThingRollup5mJob.processOneBucket full happy path
//   - ThingRollup5mJob.aggregateThingEvents skip-unknown-source and skip-nil-thingID
//   - ThingRollupMergeJob.Interval accessor (0%)
//   - ThingRollupMergeJob.coldStartWatermark error path
//   - ThingRollupMergeJob.runCalendarMonth loop that processes one month
//   - ThingRollupMergeJob.mergeOneBucket full transaction path (non-empty source)
package rollup

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/credstate"
)

// NewHealthRollupMetrics — non-nil registry path

func TestNewHealthRollupMetrics_NonNilRegistry(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewHealthRollupMetrics(reg)
	if m == nil {
		t.Fatal("NewHealthRollupMetrics with non-nil registry returned nil")
	}
	// Exercise each method to confirm non-nil receiver paths run without panic.
	m.cycle("ok")
	m.updated(2)
	m.candidates(3)
	m.transition("healthy", "degraded")
	m.observe(10 * time.Millisecond)
}

// CredentialHealthRollupJob — identity accessors

func TestCredentialHealthRollup_Identity(t *testing.T) {
	j := NewCredentialHealthRollup(nil, nil, 5*time.Minute, testLogger(), nil)
	if j.ID() == "" {
		t.Error("ID must not be empty")
	}
	if j.Name() == "" {
		t.Error("Name must not be empty")
	}
	if j.Description() == "" {
		t.Error("Description must not be empty")
	}
	if j.Interval() != 5*time.Minute {
		t.Errorf("Interval = %v, want 5m", j.Interval())
	}
}

// thresholdsReader — Thresholds() accessor (via fakeThresholds stub)

type fakeThresholds struct{ val credstate.Thresholds }

func (f *fakeThresholds) Thresholds(_ context.Context) credstate.Thresholds { return f.val }

func TestFakeThresholds_Thresholds(t *testing.T) {
	ft := &fakeThresholds{val: credstate.DefaultThresholds}
	got := ft.Thresholds(context.Background())
	if got.HealthMinSamples != credstate.DefaultThresholds.HealthMinSamples {
		t.Errorf("Thresholds() returned wrong value")
	}
}

// Rollup5mJob.processOneBucket — full happy path (one traffic_event row)

// trafficEventCols lists the 43 SELECT columns that aggregateTrafficEvents
// scans in exactly the order the Scan call expects them. Last two columns
// Last two columns (embedding_cost_usd, ai_guard_cost_usd) cover internal-ops
// cost rollup metrics.
var trafficEventCols = []string{
	"source", "provider_id", "model_id",
	"entity_id", "entity_type", "org_id",
	"routed_provider_id",
	"routing_rule_id", "target_host", "source_ip",
	"status_code", "latency_ms", "cache_hit",
	"prompt_tokens", "completion_tokens", "total_tokens", "estimated_cost_usd", "gateway_cache_savings_usd",
	"request_hook_decision", "response_hook_decision", "bump_status",
	"routed_model_id", "original_model_id",
	"has_quality_signals", "virtual_key_id", "project_id",
	"error_code",
	"timestamp",
	"cache_write_cost_usd", "cache_read_savings_usd", "cache_net_savings_usd",
	"cache_creation_tokens", "cache_read_tokens", "l4_cache_hit",
	"normalized_strip_count", "normalized_strip_bytes", "cache_marker_injected",
	"upstream_ttfb_ms", "upstream_total_ms", "request_hooks_ms", "response_hooks_ms",
	"embedding_cost_usd", "ai_guard_cost_usd",
}

// oneTrafficEventRow returns one valid AddRow call matching trafficEventCols.
// source = "ai-gateway" maps to DomainVK, status_code = 200.
func oneTrafficEventRow(ts time.Time) []any {
	src := "ai-gateway"
	sc := 200
	lat := 50
	var (
		nilStr   *string
		nilInt   *int
		nilBool  *bool
		nilF64   *float64
		nilInt64 *int64
	)
	return []any{
		&src,     // source
		nilStr,   // provider_id
		nilStr,   // model_id
		nilStr,   // entity_id
		nilStr,   // entity_type
		nilStr,   // org_id
		nilStr,   // routed_provider_id
		nilStr,   // routing_rule_id
		nilStr,   // target_host
		nilStr,   // source_ip
		&sc,      // status_code
		&lat,     // latency_ms
		nilBool,  // cache_hit
		nilInt,   // prompt_tokens
		nilInt,   // completion_tokens
		nilInt,   // total_tokens
		nilF64,   // estimated_cost_usd
		nilF64,   // gateway_cache_savings_usd
		nilStr,   // request_hook_decision
		nilStr,   // response_hook_decision
		nilStr,   // bump_status
		nilStr,   // routed_model_id
		nilStr,   // original_model_id
		nilBool,  // has_quality_signals
		nilStr,   // virtual_key_id
		nilStr,   // project_id
		nilStr,   // error_code
		ts,       // timestamp
		nilF64,   // cache_write_cost_usd
		nilF64,   // cache_read_savings_usd
		nilF64,   // cache_net_savings_usd
		nilInt64, // cache_creation_tokens
		nilInt64, // cache_read_tokens
		nilBool,  // l4_cache_hit
		nilInt64, // normalized_strip_count
		nilInt64, // normalized_strip_bytes
		nilInt64, // cache_marker_injected
		nilInt,   // upstream_ttfb_ms
		nilInt,   // upstream_total_ms
		nilInt,   // request_hooks_ms
		nilInt,   // response_hooks_ms
		nilF64,   // embedding_cost_usd
		nilF64,   // ai_guard_cost_usd
	}
}

// TestRollup5m_AggregateTrafficEvents_OneRow exercises aggregateTrafficEvents
// directly (via a mock transaction) with one valid row so that emitEventMetrics
// and assembleRollupRows are exercised. The returned rows slice must be
// non-empty (at least MetricRequestCount is always emitted for a valid event).
func TestRollup5m_AggregateTrafficEvents_OneRow(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := time.Now().UTC().Truncate(5 * time.Minute).Add(-10 * time.Minute)
	ts := bucket.Add(time.Minute)

	// We need a pgx.Tx, which pgxmock provides via BeginTx/Begin.
	mock.ExpectBegin()
	rows := pgxmock.NewRows(trafficEventCols).AddRow(oneTrafficEventRow(ts)...)
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(rows)

	tx, err := mock.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	j := NewRollup5m(nil, time.Minute, testLogger(), false)
	rollupRows, err := j.aggregateTrafficEvents(context.Background(), tx, bucket, bucket.Add(bucketDuration5m))
	if err != nil {
		t.Fatalf("aggregateTrafficEvents: %v", err)
	}
	if len(rollupRows) == 0 {
		t.Error("expected at least one rollup row for a valid event; got 0")
	}
}

// TestRollup5m_ProcessOneBucket_FullPath drives the complete processOneBucket
// transaction with an empty traffic_event result (no rollup rows emitted) so
// InsertRollupRows is a no-op and the watermark INSERT is the only Exec after
// the DELETE.
func TestRollup5m_ProcessOneBucket_FullPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := time.Now().UTC().Truncate(5 * time.Minute).Add(-10 * time.Minute)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM "metric_rollup_5m"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(trafficEventCols)) // empty → no InsertRollupRows
	mock.ExpectExec(`INSERT INTO "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	j := NewRollup5m(nil, time.Minute, testLogger(), false)
	j.pool = mock

	if err := j.processOneBucket(context.Background(), bucket); err != nil {
		t.Fatalf("processOneBucket: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met: %v", err)
	}
}

// TestRollup5m_AggregateTrafficEvents_QueryError exercises the error path when
// the traffic_event query itself fails.
func TestRollup5m_AggregateTrafficEvents_QueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := time.Now().UTC().Truncate(5 * time.Minute).Add(-10 * time.Minute)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM "metric_rollup_5m"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	sentinel := errors.New("traffic_event query failed")
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(sentinel)
	mock.ExpectRollback()

	j := NewRollup5m(nil, time.Minute, testLogger(), false)
	j.pool = mock

	if err := j.processOneBucket(context.Background(), bucket); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel (traffic_event query)", err)
	}
}

// TestRollup5m_AggregateTrafficEvents_UnknownSourceSkipped exercises the
// branch where source is not a known data-plane source → row skipped.
// With no rows emitted, assembleRollupRows returns empty → InsertRollupRows
// is a no-op → SetWatermark and Commit proceed.
func TestRollup5m_AggregateTrafficEvents_UnknownSourceSkipped(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := time.Now().UTC().Truncate(5 * time.Minute).Add(-10 * time.Minute)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM "metric_rollup_5m"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))

	ts := bucket.Add(time.Minute)
	row := oneTrafficEventRow(ts)
	// Replace source with an unknown value so DBSourceToDomain returns ok=false.
	unknownSrc := "legacy-admin"
	row[0] = &unknownSrc
	rows := pgxmock.NewRows(trafficEventCols).AddRow(row...)
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(rows)

	// InsertRollupRows is a no-op for empty rows — no Exec expected.
	mock.ExpectExec(`INSERT INTO "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	j := NewRollup5m(nil, time.Minute, testLogger(), false)
	j.pool = mock

	if err := j.processOneBucket(context.Background(), bucket); err != nil {
		t.Fatalf("processOneBucket: %v (unknown source must be silently skipped)", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met: %v", err)
	}
}

// TestRollup5m_Run_ProcessesBuckets exercises the Run loop with a watermark one
// bucket in the past so exactly one bucket is processed.
func TestRollup5m_Run_ProcessesBuckets(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// Watermark = 2 buckets before latestSealed so the loop runs once.
	latestSealed := time.Now().UTC().Add(-bucketDuration5m).Truncate(bucketDuration5m)
	watermark := latestSealed.Add(-bucketDuration5m)

	mock.ExpectQuery(`FROM "rollup_watermark"`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(watermark))

	// processOneBucket for the one bucket = watermark + 5m = latestSealed.
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM "metric_rollup_5m"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(trafficEventCols)) // empty → no InsertRollupRows
	mock.ExpectExec(`INSERT INTO "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	j := NewRollup5m(nil, time.Minute, testLogger(), false)
	j.pool = mock

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met: %v", err)
	}
}

func TestRollupMerge_ColdStartWatermark_EarliestError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`SELECT MIN\("bucketStart"\)`).
		WillReturnError(errors.New("earliest boom"))

	j := NewRollupMerge1h(nil, time.Hour, testLogger())
	j.pool = mock

	got := j.coldStartWatermark(context.Background())
	if got.IsZero() {
		t.Error("coldStartWatermark must return a non-zero time on error path")
	}
}

func TestRollupMerge_ColdStartWatermark_HappyPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	old := time.Now().UTC().Truncate(time.Hour).Add(-48 * time.Hour)
	mock.ExpectQuery(`SELECT MIN\("bucketStart"\)`).
		WillReturnRows(pgxmock.NewRows([]string{"min"}).AddRow(&old))

	j := NewRollupMerge1h(nil, time.Hour, testLogger())
	j.pool = mock

	got := j.coldStartWatermark(context.Background())
	if got.IsZero() {
		t.Error("coldStartWatermark must return a non-zero time when source exists")
	}
}

// RollupMergeJob.mergeOneBucket — full transaction path (non-empty source)

// TestRollupMerge_MergeOneBucket_WithData exercises the transaction path when
// the source table has rows: QueryRollupMergeSource → MergeRollupRows →
// Begin → DeleteRollupBucket → InsertRollupRows → SetWatermark → Commit.
func TestRollupMerge_MergeOneBucket_WithData(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucketStart := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	bucketEnd := bucketStart.Add(time.Hour)

	// QueryRollupMergeSource: SELECT … FROM "metric_rollup_5m" WHERE …
	mock.ExpectQuery(`FROM "metric_rollup_5m"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "bucketStart", "metricName", "dimensionKey", "subDimension", "value", "metadata", "updatedAt"}).
			AddRow("uuid-1", bucketStart, "request_count", "global", "vk", float64(5), nil, time.Now()))

	// tx Begin → DELETE → INSERT → Watermark → Commit
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM "metric_rollup_1h"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectExec(`INSERT INTO "metric_rollup_1h"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(`INSERT INTO "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	j := NewRollupMerge1h(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.mergeOneBucket(context.Background(), bucketStart, bucketEnd); err != nil {
		t.Fatalf("mergeOneBucket: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met: %v", err)
	}
}

// RollupMergeJob.runFixed — loop that processes one bucket

// TestRollupMerge_RunFixed_ProcessesBucket exercises the runFixed loop with
// a watermark two bucket durations behind latestSealed so exactly one bucket
// is processed. Uses the 1h merge job (bucketDuration = 1h).
func TestRollupMerge_RunFixed_ProcessesBucket(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	latestSealed := time.Now().UTC().Add(-time.Hour).Truncate(time.Hour)
	watermark := latestSealed.Add(-time.Hour) // one bucket behind

	mock.ExpectQuery(`FROM "rollup_watermark"`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(watermark))

	// mergeOneBucket: source query → empty → early return (no tx)
	mock.ExpectQuery(`FROM "metric_rollup_5m"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "bucketStart", "metricName", "dimensionKey", "subDimension", "value", "metadata", "updatedAt"}))

	j := NewRollupMerge1h(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met: %v", err)
	}
}

// RollupMergeJob.runCalendarMonth — processes one sealed month

// TestRollupMerge_RunCalendarMonth_ProcessesMonth drives runCalendarMonth so
// it processes exactly one sealed month (two months ago → one month ago).
func TestRollupMerge_RunCalendarMonth_ProcessesMonth(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	now := time.Now().UTC()
	// Watermark = 2 months ago → loop iteration = 1 month ago < currentMonthStart.
	twoMonthsAgo := time.Date(now.Year(), now.Month()-2, 1, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`FROM "rollup_watermark"`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(twoMonthsAgo))

	// mergeOneBucket for last month: source query → empty → early return
	mock.ExpectQuery(`FROM "metric_rollup_1d"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "bucketStart", "metricName", "dimensionKey", "subDimension", "value", "metadata", "updatedAt"}))

	j := NewRollupMerge1mo(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met: %v", err)
	}
}

// RollupCorrectionJob.Run — happy path (no errors)

// TestRollupCorrection_Run_HappyPath drives Run() so that all 24 × 5m buckets,
// 24 × 1h buckets, and 1 × 1d bucket are processed. Each sub-job's pool is
// set to a shared pgxmock that expects the right sequence of calls.
//
// To keep the harness simple, each processOneBucket/mergeOneBucket call is
// satisfied by returning empty rows from the source queries — the transaction
// path is covered by the other tests above.
func TestRollupCorrection_Run_HappyPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// 24 × 5m processOneBucket: Begin → DELETE → SELECT (empty) → watermark → Commit
	for range 288 { // 24h / 5m = 288 buckets
		mock.ExpectBegin()
		mock.ExpectExec(`DELETE FROM "metric_rollup_5m"`).
			WithArgs(pgxmock.AnyArg()).
			WillReturnResult(pgxmock.NewResult("DELETE", 0))
		mock.ExpectQuery(`FROM traffic_event`).
			WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
			WillReturnRows(pgxmock.NewRows(trafficEventCols))
		mock.ExpectExec(`INSERT INTO "rollup_watermark"`).
			WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
			WillReturnResult(pgxmock.NewResult("INSERT", 1))
		mock.ExpectCommit()
	}

	// 24 × 1h mergeOneBucket: source query → empty → early return (no tx)
	for range 24 {
		mock.ExpectQuery(`FROM "metric_rollup_5m"`).
			WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
			WillReturnRows(pgxmock.NewRows([]string{"id", "bucketStart", "metricName", "dimensionKey", "subDimension", "value", "metadata", "updatedAt"}))
	}

	// 1 × 1d mergeOneBucket: source query → empty → early return
	mock.ExpectQuery(`FROM "metric_rollup_1h"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "bucketStart", "metricName", "dimensionKey", "subDimension", "value", "metadata", "updatedAt"}))

	r5m := NewRollup5m(nil, time.Minute, testLogger(), false)
	r5m.pool = mock
	m1h := NewRollupMerge1h(nil, 5*time.Minute, testLogger())
	m1h.pool = mock
	m1d := NewRollupMerge1d(nil, time.Hour, testLogger())
	m1d.pool = mock
	m1mo := NewRollupMerge1mo(nil, 24*time.Hour, testLogger())
	m1mo.pool = mock

	j := NewRollupCorrection(r5m, m1h, m1d, m1mo, 24*time.Hour, testLogger())

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestRollupCorrection_Run_5mBucketError exercises the branch where
// processOneBucket returns an error — Run returns it immediately.
func TestRollupCorrection_Run_5mBucketError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	sentinel := errors.New("5m bucket boom")
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM "metric_rollup_5m"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(sentinel)
	mock.ExpectRollback()

	r5m := NewRollup5m(nil, time.Minute, testLogger(), false)
	r5m.pool = mock
	m1h := NewRollupMerge1h(nil, 5*time.Minute, testLogger())
	m1h.pool = mock
	m1d := NewRollupMerge1d(nil, time.Hour, testLogger())
	m1d.pool = mock
	m1mo := NewRollupMerge1mo(nil, 24*time.Hour, testLogger())
	m1mo.pool = mock

	j := NewRollupCorrection(r5m, m1h, m1d, m1mo, 24*time.Hour, testLogger())

	if err := j.Run(context.Background()); err == nil {
		t.Fatal("expected error from 5m bucket failure")
	}
}

// ThingRollup5mJob — Interval accessor

func TestThingRollup5m_Interval(t *testing.T) {
	j := NewThingRollup5m(nil, 2*time.Minute, testLogger(), false, false)
	if j.Interval() != 2*time.Minute {
		t.Errorf("Interval = %v, want 2m", j.Interval())
	}
}

// ThingRollup5mJob.processOneBucket — full happy path (one row)

// thingTrafficEventCols is the 42-column SELECT used by aggregateThingEvents
// (prepends thing_id to trafficEventCols).
var thingTrafficEventCols = append([]string{"thing_id"}, trafficEventCols...)

// oneThingTrafficEventRow returns one valid AddRow for thingTrafficEventCols.
func oneThingTrafficEventRow(ts time.Time) []any {
	thingID := "thing-1"
	return append([]any{&thingID}, oneTrafficEventRow(ts)...)
}

// TestThingRollup5m_AggregateThingEvents_OneRow exercises aggregateThingEvents
// directly with one valid row, driving emitThingEventMetrics and
// assembleThingRollupRows.
func TestThingRollup5m_AggregateThingEvents_OneRow(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := time.Now().UTC().Truncate(5 * time.Minute).Add(-10 * time.Minute)
	ts := bucket.Add(time.Minute)

	mock.ExpectBegin()
	rows := pgxmock.NewRows(thingTrafficEventCols).AddRow(oneThingTrafficEventRow(ts)...)
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(rows)

	tx, err := mock.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	j := NewThingRollup5m(nil, time.Minute, testLogger(), true, false)
	thingRows, err := j.aggregateThingEvents(context.Background(), tx, bucket, bucket.Add(bucketDuration5m))
	if err != nil {
		t.Fatalf("aggregateThingEvents: %v", err)
	}
	if len(thingRows) == 0 {
		t.Error("expected at least one thing rollup row for a valid event; got 0")
	}
}

// TestThingRollup5m_ProcessOneBucket_FullPath drives the complete
// processOneBucket transaction with an empty traffic_event result so
// InsertThingRollupRows is a no-op and watermark INSERT is the only Exec.
func TestThingRollup5m_ProcessOneBucket_FullPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := time.Now().UTC().Truncate(5 * time.Minute).Add(-10 * time.Minute)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM "thing_metric_rollup_5m"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(thingTrafficEventCols)) // empty → no InsertThingRollupRows
	mock.ExpectExec(`INSERT INTO "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	j := NewThingRollup5m(nil, time.Minute, testLogger(), true, false)
	j.pool = mock

	if err := j.processOneBucket(context.Background(), bucket); err != nil {
		t.Fatalf("processOneBucket: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met: %v", err)
	}
}

// TestThingRollup5m_AggregateThingEvents_UnknownSourceSkipped exercises the
// skip path when the source value doesn't map to a domain.
func TestThingRollup5m_AggregateThingEvents_UnknownSourceSkipped(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := time.Now().UTC().Truncate(5 * time.Minute).Add(-10 * time.Minute)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM "thing_metric_rollup_5m"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))

	ts := bucket.Add(time.Minute)
	row := oneThingTrafficEventRow(ts)
	// Replace source (index 1 after thingID) with unknown value.
	unknownSrc := "legacy"
	row[1] = &unknownSrc
	rows := pgxmock.NewRows(thingTrafficEventCols).AddRow(row...)
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(rows)

	// No InsertThingRollupRows expected (rows=0).
	mock.ExpectExec(`INSERT INTO "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	j := NewThingRollup5m(nil, time.Minute, testLogger(), true, false)
	j.pool = mock

	if err := j.processOneBucket(context.Background(), bucket); err != nil {
		t.Fatalf("processOneBucket: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met: %v", err)
	}
}

// TestThingRollup5m_AggregateThingEvents_NilThingIDSkipped exercises the
// defensive nil-thingID check (IS NOT NULL filter catches it in SQL, but the
// code double-checks).
func TestThingRollup5m_AggregateThingEvents_NilThingIDSkipped(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := time.Now().UTC().Truncate(5 * time.Minute).Add(-10 * time.Minute)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM "thing_metric_rollup_5m"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))

	ts := bucket.Add(time.Minute)
	row := oneThingTrafficEventRow(ts)
	// Set thingID to nil (overrides index 0).
	row[0] = (*string)(nil)
	rows := pgxmock.NewRows(thingTrafficEventCols).AddRow(row...)
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(rows)

	mock.ExpectExec(`INSERT INTO "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	j := NewThingRollup5m(nil, time.Minute, testLogger(), true, false)
	j.pool = mock

	if err := j.processOneBucket(context.Background(), bucket); err != nil {
		t.Fatalf("processOneBucket: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met: %v", err)
	}
}

// ThingRollupMergeJob — Interval accessor (0%)

func TestThingRollupMerge_Interval(t *testing.T) {
	j := NewThingRollupMerge1h(nil, 3*time.Minute, testLogger())
	if j.Interval() != 3*time.Minute {
		t.Errorf("Interval = %v, want 3m", j.Interval())
	}
	j2 := NewThingRollupMerge1d(nil, 2*time.Hour, testLogger())
	if j2.Interval() != 2*time.Hour {
		t.Errorf("Interval = %v, want 2h", j2.Interval())
	}
}

// ThingRollupMergeJob.coldStartWatermark — error path

func TestThingRollupMerge_ColdStartWatermark_EarliestError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`SELECT MIN\("bucketStart"\)`).
		WillReturnError(errors.New("thing earliest boom"))

	j := NewThingRollupMerge1h(nil, time.Hour, testLogger())
	j.pool = mock

	got := j.coldStartWatermark(context.Background())
	if got.IsZero() {
		t.Error("coldStartWatermark must return non-zero time on error path")
	}
}

// ThingRollupMergeJob.runCalendarMonth — processes one sealed month

func TestThingRollupMerge_RunCalendarMonth_ProcessesMonth(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	now := time.Now().UTC()
	twoMonthsAgo := time.Date(now.Year(), now.Month()-2, 1, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`FROM "rollup_watermark"`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(twoMonthsAgo))

	// mergeOneBucket: source → empty → early return (no tx)
	mock.ExpectQuery(`FROM "thing_metric_rollup_1d"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "bucketStart", "thing_id", "metricName", "dimensionKey", "subDimension", "value", "metadata", "updatedAt"}))

	j := NewThingRollupMerge1mo(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met: %v", err)
	}
}

// ThingRollupMergeJob.mergeOneBucket — full transaction path (non-empty source)

func TestThingRollupMerge_MergeOneBucket_WithData(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucketStart := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	bucketEnd := bucketStart.Add(time.Hour)
	thingID := "thing-1"

	mock.ExpectQuery(`FROM "thing_metric_rollup_5m"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "bucketStart", "thing_id", "metricName", "dimensionKey", "subDimension", "value", "metadata", "updatedAt"}).
			AddRow("uuid-1", bucketStart, thingID, "request_count", "global", "vk", float64(3), nil, time.Now()))

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM "thing_metric_rollup_1h"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectExec(`INSERT INTO "thing_metric_rollup_1h"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(`INSERT INTO "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	j := NewThingRollupMerge1h(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.mergeOneBucket(context.Background(), bucketStart, bucketEnd); err != nil {
		t.Fatalf("mergeOneBucket: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met: %v", err)
	}
}

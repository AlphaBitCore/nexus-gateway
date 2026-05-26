// rollup_supplemental3_test.go covers the remaining statement gaps after
// rollup_supplemental2_test.go. Each test targets a specific uncovered branch:
//
//   - NewHealthRollupMetrics(nil) → returns nil (nil-registerer guard)
//   - NewCredentialHealthRollup with zero interval → defaults to 5 min
//   - CredentialHealthRollupJob.collect: scan error path
//   - CredentialHealthRollupJob.priorStatus: scan error + rows.Err paths
//   - RollupMergeJob.runFixed: processOneBucket error + count>0 log path
//   - RollupMergeJob.runCalendarMonth: mergeOneBucket error path
//   - RollupMergeJob.mergeOneBucket: InsertRollupRows error + SetWatermark error
//   - ThingRollup5mJob.Run: processOneBucket error path
//   - ThingRollup5mJob.processOneBucket: InsertThingRollupRows error + SetWatermark error
//   - ThingRollup5mJob.aggregateThingEvents: enableAgentRollup=false (agentClause branch)
//   - ThingRollupMergeJob.runFixed: processOneBucket error + count>0 log path
//   - ThingRollupMergeJob.runCalendarMonth: mergeOneBucket error path
//   - ThingRollupMergeJob.mergeOneBucket: InsertThingRollupRows error + SetWatermark error
//   - emitEventMetrics: upstream-total>lat (us=0 clamp), APPROVE hook, BUMP_FAILED_PASSTHROUGH
//   - emitThingEventMetrics: same three branches
package rollup

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
	"github.com/prometheus/client_golang/prometheus"
)

// NewHealthRollupMetrics(nil) — returns nil (nil-registerer guard)

func TestNewHealthRollupMetrics_NilRegistry(t *testing.T) {
	m := NewHealthRollupMetrics(nil)
	if m != nil {
		t.Error("NewHealthRollupMetrics(nil) must return nil")
	}
	// Also exercise the nil-receiver no-op methods to confirm they don't panic.
	m.cycle("ok")
	m.updated(1)
	m.candidates(1)
	m.transition("a", "b")
	m.observe(time.Millisecond)
}

// TestNewHealthRollupMetrics_NilReceiverMethods confirms nil-pointer receiver
// guards on each helper method (already exercised above; re-stated explicitly).
func TestNewHealthRollupMetrics_UpdatedZero(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewHealthRollupMetrics(reg)
	// updated with n=0 → guard fires, Add is never called (no panic).
	m.updated(0)
	// candidates with n=0 → same guard.
	m.candidates(0)
}

// NewCredentialHealthRollup — zero interval defaults to 5 min

func TestNewCredentialHealthRollup_ZeroInterval(t *testing.T) {
	j := NewCredentialHealthRollup(nil, nil, 0, testLogger(), nil)
	if j.Interval() != 5*time.Minute {
		t.Errorf("Interval = %v, want 5m (default)", j.Interval())
	}
}

// CredentialHealthRollupJob.collect — scan error

// TestCredentialHealthRollup_Collect_ScanError exercises the rows.Scan error
// branch in collect() by providing a type mismatch in the returned row.
func TestCredentialHealthRollup_Collect_ScanError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	collectCols := []string{
		"credential_id",
		"short_samples", "short_success", "short_auth", "short_rate",
		"short_5xx", "short_timeout", "short_client",
		"short_last",
		"long_samples", "long_success",
	}
	// Return an int for credential_id → scan fails.
	mock.ExpectQuery(`FROM\s+traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(collectCols).AddRow(
			42, // wrong type
			0, 0, 0, 0, 0, 0, 0,
			(*time.Time)(nil),
			0, 0,
		))

	j := NewCredentialHealthRollup(nil, nil, 5*time.Minute, testLogger(), nil)
	j.pool = mock

	if err := j.Run(context.Background()); err == nil {
		t.Error("expected error from collect scan failure; got nil")
	}
}

// CredentialHealthRollupJob.priorStatus — scan error

// TestCredentialHealthRollup_PriorStatus_ScanError exercises the scan error
// path in priorStatus() where the Credential SELECT returns a bad row type.
func TestCredentialHealthRollup_PriorStatus_ScanError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	nowT := time.Now().UTC()
	collectCols := []string{
		"credential_id",
		"short_samples", "short_success", "short_auth", "short_rate",
		"short_5xx", "short_timeout", "short_client",
		"short_last",
		"long_samples", "long_success",
	}
	mock.ExpectQuery(`FROM\s+traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(collectCols).AddRow(
			"cred-1", 5, 5, 0, 0, 0, 0, 0, &nowT, 5, 5,
		))

	// Return an int for the id column → scan error in priorStatus.
	mock.ExpectQuery(`FROM\s+"Credential"\s+WHERE\s+id\s+=\s+ANY`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "healthStatus", "healthStatusChangedAt"}).
			AddRow(42, "healthy", (*time.Time)(nil)))

	j := NewCredentialHealthRollup(nil, nil, 5*time.Minute, testLogger(), nil)
	j.pool = mock

	if err := j.Run(context.Background()); err == nil {
		t.Error("expected scan error from priorStatus; got nil")
	}
}

// RollupMergeJob.runFixed — processOneBucket error + count>0 log path

// TestRollupMerge_RunFixed_ProcessOneBucketError exercises the error return
// inside the runFixed loop when mergeOneBucket returns an error.
func TestRollupMerge_RunFixed_ProcessOneBucketError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	latestSealed := time.Now().UTC().Add(-time.Hour).Truncate(time.Hour)
	watermark := latestSealed.Add(-time.Hour)

	mock.ExpectQuery(`FROM "rollup_watermark"`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(watermark))

	// mergeOneBucket: source query errors → mergeOneBucket returns error → runFixed returns error.
	sentinel := errors.New("source query boom")
	mock.ExpectQuery(`FROM "metric_rollup_5m"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(sentinel)

	j := NewRollupMerge1h(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel (runFixed mergeOneBucket error)", err)
	}
}

// TestRollupMerge_RunFixed_CountGTZeroLog exercises the count>0 logger.Info path
// in runFixed. The watermark is placed so two buckets are processed and the
// source table is non-empty on the first call (empty on second). Because the
// first bucket has data (one row), the transaction runs and inserts it, advancing
// count to 1 → logger.Info fires.
// Note: we reuse the mergeOneBucket happy path (WithData) for the first bucket,
// then return empty source for the second bucket.
func TestRollupMerge_RunFixed_CountGTZeroLog(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	latestSealed := time.Now().UTC().Add(-time.Hour).Truncate(time.Hour)
	watermark := latestSealed.Add(-2 * time.Hour) // two buckets behind

	mock.ExpectQuery(`FROM "rollup_watermark"`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(watermark))

	// First bucket: source returns one row → full transaction path.
	firstBucket := latestSealed.Add(-time.Hour)
	mock.ExpectQuery(`FROM "metric_rollup_5m"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "bucketStart", "metricName", "dimensionKey", "subDimension", "value", "metadata", "updatedAt"}).
			AddRow("uuid-1", firstBucket, "request_count", "global", "vk", float64(2), nil, time.Now()))
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

	// Second bucket: source returns empty → early return.
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

// RollupMergeJob.runCalendarMonth — mergeOneBucket error path

func TestRollupMerge_RunCalendarMonth_BucketError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	now := time.Now().UTC()
	twoMonthsAgo := time.Date(now.Year(), now.Month()-2, 1, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`FROM "rollup_watermark"`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(twoMonthsAgo))

	sentinel := errors.New("calendar month boom")
	mock.ExpectQuery(`FROM "metric_rollup_1d"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(sentinel)

	j := NewRollupMerge1mo(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel (runCalendarMonth error)", err)
	}
}

// RollupMergeJob.mergeOneBucket — InsertRollupRows error + SetWatermark error

func TestRollupMerge_MergeOneBucket_InsertError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucketStart := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	bucketEnd := bucketStart.Add(time.Hour)

	mock.ExpectQuery(`FROM "metric_rollup_5m"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "bucketStart", "metricName", "dimensionKey", "subDimension", "value", "metadata", "updatedAt"}).
			AddRow("uuid-1", bucketStart, "request_count", "global", "vk", float64(5), nil, time.Now()))

	sentinel := errors.New("insert rollup boom")
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM "metric_rollup_1h"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectExec(`INSERT INTO "metric_rollup_1h"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(sentinel)
	mock.ExpectRollback()

	j := NewRollupMerge1h(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.mergeOneBucket(context.Background(), bucketStart, bucketEnd); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want insert sentinel", err)
	}
}

func TestRollupMerge_MergeOneBucket_SetWatermarkError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucketStart := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	bucketEnd := bucketStart.Add(time.Hour)

	mock.ExpectQuery(`FROM "metric_rollup_5m"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "bucketStart", "metricName", "dimensionKey", "subDimension", "value", "metadata", "updatedAt"}).
			AddRow("uuid-1", bucketStart, "request_count", "global", "vk", float64(5), nil, time.Now()))

	sentinel := errors.New("set watermark boom")
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM "metric_rollup_1h"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectExec(`INSERT INTO "metric_rollup_1h"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(`INSERT INTO "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(sentinel)
	mock.ExpectRollback()

	j := NewRollupMerge1h(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.mergeOneBucket(context.Background(), bucketStart, bucketEnd); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want watermark sentinel", err)
	}
}

// ThingRollup5mJob.Run — processOneBucket error path

// TestThingRollup5m_Run_BucketError exercises the Run loop error return when
// processOneBucket fails.
func TestThingRollup5m_Run_BucketError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	latestSealed := time.Now().UTC().Add(-bucketDuration5m).Truncate(bucketDuration5m)
	watermark := latestSealed.Add(-bucketDuration5m)

	mock.ExpectQuery(`FROM "rollup_watermark"`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(watermark))

	// processOneBucket: Begin succeeds, DELETE fails.
	sentinel := errors.New("thing bucket boom")
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM "thing_metric_rollup_5m"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(sentinel)
	mock.ExpectRollback()

	j := NewThingRollup5m(nil, time.Minute, testLogger(), true, false)
	j.pool = mock

	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel (thing Run bucket error)", err)
	}
}

// ThingRollup5mJob.processOneBucket — InsertThingRollupRows error + SetWatermark error

// TestThingRollup5m_ProcessOneBucket_InsertError exercises the InsertThingRollupRows
// error path inside the processOneBucket transaction. Requires a non-empty
// aggregateThingEvents result, which is achieved by returning one valid thing row.
func TestThingRollup5m_ProcessOneBucket_InsertError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := time.Now().UTC().Truncate(5 * time.Minute).Add(-10 * time.Minute)
	ts := bucket.Add(time.Minute)

	sentinel := errors.New("insert thing rollup boom")
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM "thing_metric_rollup_5m"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(thingTrafficEventCols).AddRow(oneThingTrafficEventRow(ts)...))
	mock.ExpectExec(`INSERT INTO "thing_metric_rollup_5m"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(sentinel)
	mock.ExpectRollback()

	j := NewThingRollup5m(nil, time.Minute, testLogger(), true, false)
	j.pool = mock

	if err := j.processOneBucket(context.Background(), bucket); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want insert sentinel", err)
	}
}

// TestThingRollup5m_ProcessOneBucket_SetWatermarkError exercises the SetWatermark
// error path in processOneBucket (empty traffic_event → no insert → watermark fails).
func TestThingRollup5m_ProcessOneBucket_SetWatermarkError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := time.Now().UTC().Truncate(5 * time.Minute).Add(-10 * time.Minute)

	sentinel := errors.New("set thing watermark boom")
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM "thing_metric_rollup_5m"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(thingTrafficEventCols)) // empty → no INSERT
	mock.ExpectExec(`INSERT INTO "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(sentinel)
	mock.ExpectRollback()

	j := NewThingRollup5m(nil, time.Minute, testLogger(), true, false)
	j.pool = mock

	if err := j.processOneBucket(context.Background(), bucket); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want watermark sentinel", err)
	}
}

// ThingRollup5mJob.aggregateThingEvents — enableAgentRollup=false (agentClause branch)

// TestThingRollup5m_AggregateThingEvents_AgentRollupDisabled exercises the
// agentClause assignment branch when enableAgentRollup is false.
// The SQL gains " AND source != 'agent'" but the mock matches any SQL containing
// FROM traffic_event, so it passes through normally.
func TestThingRollup5m_AggregateThingEvents_AgentRollupDisabled(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := time.Now().UTC().Truncate(5 * time.Minute).Add(-10 * time.Minute)
	ts := bucket.Add(time.Minute)

	mock.ExpectBegin()
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(thingTrafficEventCols).AddRow(oneThingTrafficEventRow(ts)...))

	tx, err := mock.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	// enableAgentRollup=false → agentClause = " AND source != 'agent'"
	j := NewThingRollup5m(nil, time.Minute, testLogger(), false, false)
	rows, err := j.aggregateThingEvents(context.Background(), tx, bucket, bucket.Add(bucketDuration5m))
	if err != nil {
		t.Fatalf("aggregateThingEvents: %v", err)
	}
	if len(rows) == 0 {
		t.Error("expected rows; got 0")
	}
}

// ThingRollupMergeJob.runFixed — error + count>0 log

func TestThingRollupMerge_RunFixed_BucketError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	latestSealed := time.Now().UTC().Add(-time.Hour).Truncate(time.Hour)
	watermark := latestSealed.Add(-time.Hour)

	mock.ExpectQuery(`FROM "rollup_watermark"`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(watermark))

	sentinel := errors.New("thing merge source boom")
	mock.ExpectQuery(`FROM "thing_metric_rollup_5m"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(sentinel)

	j := NewThingRollupMerge1h(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel (thing runFixed error)", err)
	}
}

func TestThingRollupMerge_RunFixed_CountGTZeroLog(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	latestSealed := time.Now().UTC().Add(-time.Hour).Truncate(time.Hour)
	watermark := latestSealed.Add(-2 * time.Hour) // two buckets behind

	mock.ExpectQuery(`FROM "rollup_watermark"`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(watermark))

	firstBucket := latestSealed.Add(-time.Hour)
	thingID := "thing-merge-1"
	// First bucket: one row → full transaction path.
	mock.ExpectQuery(`FROM "thing_metric_rollup_5m"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "bucketStart", "thing_id", "metricName", "dimensionKey", "subDimension", "value", "metadata", "updatedAt"}).
			AddRow("uuid-1", firstBucket, thingID, "request_count", "global", "vk", float64(2), nil, time.Now()))
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

	// Second bucket: empty → early return.
	mock.ExpectQuery(`FROM "thing_metric_rollup_5m"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "bucketStart", "thing_id", "metricName", "dimensionKey", "subDimension", "value", "metadata", "updatedAt"}))

	j := NewThingRollupMerge1h(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met: %v", err)
	}
}

// ThingRollupMergeJob.runCalendarMonth — mergeOneBucket error

func TestThingRollupMerge_RunCalendarMonth_BucketError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	now := time.Now().UTC()
	twoMonthsAgo := time.Date(now.Year(), now.Month()-2, 1, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`FROM "rollup_watermark"`).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(twoMonthsAgo))

	sentinel := errors.New("thing calendar month boom")
	mock.ExpectQuery(`FROM "thing_metric_rollup_1d"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(sentinel)

	j := NewThingRollupMerge1mo(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel (thing runCalendarMonth error)", err)
	}
}

// ThingRollupMergeJob.mergeOneBucket — InsertThingRollupRows error + SetWatermark error

func TestThingRollupMerge_MergeOneBucket_InsertError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucketStart := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	bucketEnd := bucketStart.Add(time.Hour)
	thingID := "thing-insert-err"

	sentinel := errors.New("insert thing merge boom")
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
		WillReturnError(sentinel)
	mock.ExpectRollback()

	j := NewThingRollupMerge1h(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.mergeOneBucket(context.Background(), bucketStart, bucketEnd); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want insert sentinel", err)
	}
}

func TestThingRollupMerge_MergeOneBucket_SetWatermarkError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucketStart := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	bucketEnd := bucketStart.Add(time.Hour)
	thingID := "thing-wm-err"

	sentinel := errors.New("set thing merge watermark boom")
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
		WillReturnError(sentinel)
	mock.ExpectRollback()

	j := NewThingRollupMerge1h(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.mergeOneBucket(context.Background(), bucketStart, bucketEnd); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want watermark sentinel", err)
	}
}

// emitEventMetrics — APPROVE hook, upstream-total>lat (us=0 clamp), BUMP_FAILED

// approveHookBumpFailedRow returns a row with hookDecision=APPROVE,
// bumpStatus=BUMP_FAILED_PASSTHROUGH, and upstreamTotalMs > latencyMs (us<0
// → clamped to 0) so the us=0 clamp and MetricHookAllowCount branches fire.
func approveHookBumpFailedRow(ts time.Time) []any {
	src := "ai-gateway"
	sc := 200
	lat := 50
	hook := "APPROVE"
	bump := "BUMP_FAILED_PASSTHROUGH"
	upTotal := 80 // > lat → us = lat - upTotal = 50 - 80 = -30 → clamped to 0
	return []any{
		&src, (*string)(nil), (*string)(nil),
		(*string)(nil), (*string)(nil), (*string)(nil),
		(*string)(nil), (*string)(nil), (*string)(nil), (*string)(nil),
		&sc, &lat, (*bool)(nil),
		(*int)(nil), (*int)(nil), (*int)(nil), (*float64)(nil), (*float64)(nil),
		&hook, (*string)(nil), &bump,
		(*string)(nil), (*string)(nil),
		(*bool)(nil), (*string)(nil), (*string)(nil),
		(*string)(nil),
		ts,
		(*float64)(nil), (*float64)(nil), (*float64)(nil),
		(*int64)(nil), (*int64)(nil), (*bool)(nil),
		(*int64)(nil), (*int64)(nil), (*int64)(nil),
		(*int)(nil), &upTotal, (*int)(nil), (*int)(nil),
		(*float64)(nil), (*float64)(nil),
	}
}

// TestRollup5m_EmitEventMetrics_ApproveHookBumpFailed exercises the
// MetricHookAllowCount, us=0 clamp (upstream_total > lat), and
// MetricBumpFailedCount branches in emitEventMetrics.
func TestRollup5m_EmitEventMetrics_ApproveHookBumpFailed(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := time.Now().UTC().Truncate(5 * time.Minute).Add(-10 * time.Minute)
	ts := bucket.Add(time.Minute)

	mock.ExpectBegin()
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(trafficEventCols).AddRow(approveHookBumpFailedRow(ts)...))

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
		t.Error("expected rollup rows; got 0")
	}
}

// approveHookBumpFailedThingRow prepends a thingID to approveHookBumpFailedRow.
func approveHookBumpFailedThingRow(ts time.Time) []any {
	thingID := "thing-approve-1"
	return append([]any{&thingID}, approveHookBumpFailedRow(ts)...)
}

// TestThingRollup5m_EmitThingEventMetrics_ApproveHookBumpFailed exercises the
// same three branches (MetricHookAllowCount, us=0 clamp, MetricBumpFailedCount)
// in emitThingEventMetrics.
func TestThingRollup5m_EmitThingEventMetrics_ApproveHookBumpFailed(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := time.Now().UTC().Truncate(5 * time.Minute).Add(-10 * time.Minute)
	ts := bucket.Add(time.Minute)

	mock.ExpectBegin()
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(thingTrafficEventCols).AddRow(approveHookBumpFailedThingRow(ts)...))

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
		t.Error("expected thing rollup rows; got 0")
	}
}

// Rollup5mJob.processOneBucket — SetWatermark error path

// TestRollup5m_ProcessOneBucket_SetWatermarkError exercises the SetWatermark
// error path in processOneBucket (empty traffic_event → no INSERT rows → watermark fails).
func TestRollup5m_ProcessOneBucket_SetWatermarkError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucket := time.Now().UTC().Truncate(5 * time.Minute).Add(-10 * time.Minute)

	sentinel := errors.New("set watermark boom")
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM "metric_rollup_5m"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(trafficEventCols)) // empty → no InsertRollupRows
	mock.ExpectExec(`INSERT INTO "rollup_watermark"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(sentinel)
	mock.ExpectRollback()

	j := NewRollup5m(nil, time.Minute, testLogger(), false)
	j.pool = mock

	if err := j.processOneBucket(context.Background(), bucket); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want watermark sentinel", err)
	}
}

// NewThingRollup5m — zero interval defaults to 1 min

func TestNewThingRollup5m_ZeroInterval(t *testing.T) {
	j := NewThingRollup5m(nil, 0, testLogger(), false, false)
	if j.Interval() != time.Minute {
		t.Errorf("Interval = %v, want 1m (default)", j.Interval())
	}
}

// NewThingRollupMerge constructors — zero interval defaults

func TestNewThingRollupMerge1h_ZeroInterval(t *testing.T) {
	j := NewThingRollupMerge1h(nil, 0, testLogger())
	if j.Interval() != 5*time.Minute {
		t.Errorf("Interval = %v, want 5m (default)", j.Interval())
	}
}

func TestNewThingRollupMerge1d_ZeroInterval(t *testing.T) {
	j := NewThingRollupMerge1d(nil, 0, testLogger())
	if j.Interval() != time.Hour {
		t.Errorf("Interval = %v, want 1h (default)", j.Interval())
	}
}

func TestNewThingRollupMerge1mo_ZeroInterval(t *testing.T) {
	j := NewThingRollupMerge1mo(nil, 0, testLogger())
	if j.Interval() != 24*time.Hour {
		t.Errorf("Interval = %v, want 24h (default)", j.Interval())
	}
}

// Rollup5mJob.coldStartWatermark — ok path (earliest source exists)

func TestRollup5m_ColdStartWatermark_HappyPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	old := time.Now().UTC().Add(-3 * time.Hour)
	mock.ExpectQuery(`FROM traffic_event`).
		WillReturnRows(pgxmock.NewRows([]string{"timestamp"}).AddRow(&old))

	j := NewRollup5m(nil, time.Minute, testLogger(), false)
	j.pool = mock

	got := j.coldStartWatermark(context.Background())
	if got.IsZero() {
		t.Error("coldStartWatermark must return non-zero time when source exists")
	}
}

// ThingRollup5mJob.coldStartWatermark — ok path (earliest source exists)

func TestThingRollup5m_ColdStartWatermark_HappyPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	old := time.Now().UTC().Add(-3 * time.Hour)
	mock.ExpectQuery(`FROM traffic_event`).
		WillReturnRows(pgxmock.NewRows([]string{"timestamp"}).AddRow(&old))

	j := NewThingRollup5m(nil, time.Minute, testLogger(), true, false)
	j.pool = mock

	got := j.coldStartWatermark(context.Background())
	if got.IsZero() {
		t.Error("coldStartWatermark must return non-zero time when source exists")
	}
}

// ThingRollupMergeJob.coldStartWatermark — ok path (earliest source exists)

func TestThingRollupMerge_ColdStartWatermark_HappyPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	old := time.Now().UTC().Truncate(time.Hour).Add(-48 * time.Hour)
	mock.ExpectQuery(`SELECT MIN\("bucketStart"\)`).
		WillReturnRows(pgxmock.NewRows([]string{"min"}).AddRow(&old))

	j := NewThingRollupMerge1h(nil, time.Hour, testLogger())
	j.pool = mock

	got := j.coldStartWatermark(context.Background())
	if got.IsZero() {
		t.Error("coldStartWatermark must return non-zero time when source exists")
	}
}

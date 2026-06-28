// rollup_correction_extra_test.go covers the audit-wave changes to the
// correction + merge pipeline:
//
//   - gauge-snapshot metrics are dropped from the merge source so the
//     SUM cascade never folds N hourly snapshots into a coarser bucket.
//   - the per-Thing correction job (ThingRollupCorrectionJob) mirrors
//     the fleet correction so late per-Thing events are re-aggregated.
//   - the correction window spans multiple trailing days, not just T-1.
//   - the correction backfill suppresses the watermark write (asserted
//     by the absence of a rollup_watermark INSERT).
package rollup

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

// excludeGaugeRows unit behaviour.

func TestExcludeGaugeRows_DropsGaugeMetrics(t *testing.T) {
	in := []metrics.RollupRow{
		{MetricName: metrics.MetricRequestCount, Value: 5},
		{MetricName: metrics.MetricDeviceFleetStatus, DimensionKey: "status=online", Value: 12},
		{MetricName: metrics.MetricBilledCostUSD, Value: 1.5},
		{MetricName: metrics.MetricDeviceFleetOS, DimensionKey: "os=darwin", Value: 7},
	}
	out := excludeGaugeRows(in)
	if len(out) != 2 {
		t.Fatalf("len(out) = %d, want 2 (both gauge rows dropped)", len(out))
	}
	for _, r := range out {
		if r.MetricName == metrics.MetricDeviceFleetStatus || r.MetricName == metrics.MetricDeviceFleetOS {
			t.Errorf("gauge metric %q survived the filter", r.MetricName)
		}
	}
}

func TestExcludeGaugeRows_NoGaugeReturnsInput(t *testing.T) {
	in := []metrics.RollupRow{
		{MetricName: metrics.MetricRequestCount, Value: 5},
		{MetricName: metrics.MetricBilledCostUSD, Value: 1.5},
	}
	out := excludeGaugeRows(in)
	if len(out) != 2 {
		t.Fatalf("len(out) = %d, want 2 (nothing dropped)", len(out))
	}
}

// A merge bucket whose source holds ONLY gauge rows produces no target
// write — the gauge rows are excluded, the slice empties, and mergeBucket
// early-returns before opening a transaction. Asserts the gauges never reach a
// coarser tier.
func TestRollupMerge_MergeBucket_GaugeOnlySource_NoWrite(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	bucketStart := time.Now().UTC().Truncate(24 * time.Hour).Add(-48 * time.Hour)
	bucketEnd := bucketStart.Add(24 * time.Hour)

	// merge-1d reads metric_rollup_1h; return only device_fleet gauge rows.
	mock.ExpectQuery(`FROM "metric_rollup_1h"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "bucketStart", "metricName", "dimensionKey", "subDimension", "value", "metadata", "updatedAt"}).
			AddRow("g1", bucketStart, metrics.MetricDeviceFleetStatus, "status=online", "", float64(10), nil, time.Now()).
			AddRow("g2", bucketStart, metrics.MetricDeviceFleetOS, "os=darwin", "", float64(4), nil, time.Now()))
	// No Begin/DELETE/INSERT/Commit expected — gauge-only source filters to empty.

	j := NewRollupMerge1d(nil, time.Hour, testLogger())
	j.pool = mock

	if err := j.mergeOneBucket(context.Background(), bucketStart, bucketEnd); err != nil {
		t.Fatalf("mergeOneBucket: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("gauge-only source must produce no merge write: %v", err)
	}
}

// The correction window spans multiple trailing days. lookbackDays=2
// drives two full days of 5m/1h/1d re-aggregation (oldest day first), each with
// the watermark write suppressed (no rollup_watermark INSERT).
func TestRollupCorrection_Run_MultiDayLookback(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mergeCols := []string{"id", "bucketStart", "metricName", "dimensionKey", "subDimension", "value", "metadata", "updatedAt"}
	// Two days, oldest first. Each day: 288 × 5m (Begin/DELETE/SELECT/Commit, NO
	// watermark INSERT), 24 × 1h merge (empty), 1 × 1d merge (empty).
	for range 2 {
		for range 288 {
			mock.ExpectBegin()
			mock.ExpectExec(`DELETE FROM "metric_rollup_5m"`).
				WithArgs(pgxmock.AnyArg()).
				WillReturnResult(pgxmock.NewResult("DELETE", 0))
			mock.ExpectQuery(`FROM traffic_event`).
				WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
				WillReturnRows(pgxmock.NewRows(trafficEventCols))
			mock.ExpectCommit()
		}
		for range 24 {
			mock.ExpectQuery(`FROM "metric_rollup_5m"`).
				WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
				WillReturnRows(pgxmock.NewRows(mergeCols))
		}
		mock.ExpectQuery(`FROM "metric_rollup_1h"`).
			WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
			WillReturnRows(pgxmock.NewRows(mergeCols))
	}

	r5m := NewRollup5m(nil, time.Minute, testLogger(), false)
	r5m.pool = mock
	m1h := NewRollupMerge1h(nil, 5*time.Minute, testLogger())
	m1h.pool = mock
	m1d := NewRollupMerge1d(nil, time.Hour, testLogger())
	m1d.pool = mock
	m1mo := NewRollupMerge1mo(nil, 24*time.Hour, testLogger())
	m1mo.pool = mock

	j := NewRollupCorrection(r5m, m1h, m1d, m1mo, 2, 24*time.Hour, testLogger())
	// Mid-month so both lookback days stay inside the unsealed current month
	// (no 1mo re-merge).
	j.nowFn = func() time.Time { return time.Date(2026, 5, 15, 9, 0, 0, 0, time.UTC) }

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met (2-day lookback): %v", err)
	}
}

// ThingRollupCorrectionJob identity + interval default.

func TestThingRollupCorrection_Identity(t *testing.T) {
	r5m := NewThingRollup5m(nil, 0, testLogger(), false, false)
	m1h := NewThingRollupMerge1h(nil, 0, testLogger())
	m1d := NewThingRollupMerge1d(nil, 0, testLogger())
	m1mo := NewThingRollupMerge1mo(nil, 0, testLogger())

	j := NewThingRollupCorrection(r5m, m1h, m1d, m1mo, 5, 12*time.Hour, testLogger())
	if j.ID() != "thing-rollup-correction" {
		t.Errorf("ID = %q, want thing-rollup-correction", j.ID())
	}
	if j.Name() == "" {
		t.Error("Name empty")
	}
	if j.Description() == "" {
		t.Error("Description empty")
	}
	if j.Interval() != 12*time.Hour {
		t.Errorf("Interval = %v, want 12h", j.Interval())
	}
	if j.lookbackDays != 5 {
		t.Errorf("lookbackDays = %d, want 5", j.lookbackDays)
	}

	def := NewThingRollupCorrection(nil, nil, nil, nil, 0, 0, testLogger())
	if def.Interval() != 24*time.Hour {
		t.Errorf("default Interval = %v, want 24h", def.Interval())
	}
	if def.lookbackDays != correctionLookbackDays {
		t.Errorf("default lookbackDays = %d, want %d", def.lookbackDays, correctionLookbackDays)
	}
}

// ThingRollupCorrection.Run drives one trailing day of the
// per-Thing pipeline with NO thing watermark INSERT.
func TestThingRollupCorrection_Run_HappyPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mergeCols := []string{"id", "bucketStart", "thing_id", "metricName", "dimensionKey", "subDimension", "value", "metadata", "updatedAt"}
	// 288 × 5m processBucket(writeWatermark=false): Begin → DELETE → SELECT(empty) → Commit.
	for range 288 {
		mock.ExpectBegin()
		mock.ExpectExec(`DELETE FROM "thing_metric_rollup_5m"`).
			WithArgs(pgxmock.AnyArg()).
			WillReturnResult(pgxmock.NewResult("DELETE", 0))
		mock.ExpectQuery(`FROM traffic_event`).
			WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
			WillReturnRows(pgxmock.NewRows(thingTrafficEventCols))
		mock.ExpectCommit()
	}
	for range 24 {
		mock.ExpectQuery(`FROM "thing_metric_rollup_5m"`).
			WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
			WillReturnRows(pgxmock.NewRows(mergeCols))
	}
	mock.ExpectQuery(`FROM "thing_metric_rollup_1h"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(mergeCols))

	r5m := NewThingRollup5m(nil, time.Minute, testLogger(), true, false)
	r5m.pool = mock
	m1h := NewThingRollupMerge1h(nil, 5*time.Minute, testLogger())
	m1h.pool = mock
	m1d := NewThingRollupMerge1d(nil, time.Hour, testLogger())
	m1d.pool = mock
	m1mo := NewThingRollupMerge1mo(nil, 24*time.Hour, testLogger())
	m1mo.pool = mock

	j := NewThingRollupCorrection(r5m, m1h, m1d, m1mo, 1, 24*time.Hour, testLogger())
	j.nowFn = func() time.Time { return time.Date(2026, 5, 15, 9, 0, 0, 0, time.UTC) }

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met: %v", err)
	}
}

// A 5m bucket failure in the per-Thing correction surfaces as an error.
func TestThingRollupCorrection_Run_5mBucketError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	sentinel := errors.New("thing 5m boom")
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM "thing_metric_rollup_5m"`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(sentinel)
	mock.ExpectRollback()

	r5m := NewThingRollup5m(nil, time.Minute, testLogger(), true, false)
	r5m.pool = mock
	m1h := NewThingRollupMerge1h(nil, 5*time.Minute, testLogger())
	m1h.pool = mock
	m1d := NewThingRollupMerge1d(nil, time.Hour, testLogger())
	m1d.pool = mock
	m1mo := NewThingRollupMerge1mo(nil, 24*time.Hour, testLogger())
	m1mo.pool = mock

	j := NewThingRollupCorrection(r5m, m1h, m1d, m1mo, 1, 24*time.Hour, testLogger())
	j.nowFn = func() time.Time { return time.Date(2026, 5, 15, 9, 0, 0, 0, time.UTC) }

	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

// A failure during the sealed-month 1mo re-merge surfaces as an error
// (the monthly re-merge runs after all day layers succeed).
func TestRollupCorrection_Run_MonthlyRemergeError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mergeCols := []string{"id", "bucketStart", "metricName", "dimensionKey", "subDimension", "value", "metadata", "updatedAt"}
	// One sealed-month day (2026-05-31 with now=2026-06-01), all day layers empty.
	for range 288 {
		mock.ExpectBegin()
		mock.ExpectExec(`DELETE FROM "metric_rollup_5m"`).
			WithArgs(pgxmock.AnyArg()).
			WillReturnResult(pgxmock.NewResult("DELETE", 0))
		mock.ExpectQuery(`FROM traffic_event`).
			WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
			WillReturnRows(pgxmock.NewRows(trafficEventCols))
		mock.ExpectCommit()
	}
	for range 24 {
		mock.ExpectQuery(`FROM "metric_rollup_5m"`).
			WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
			WillReturnRows(pgxmock.NewRows(mergeCols))
	}
	mock.ExpectQuery(`FROM "metric_rollup_1h"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(mergeCols))
	// 1mo re-merge of sealed May: source read fails.
	sentinel := errors.New("1mo boom")
	mock.ExpectQuery(`FROM "metric_rollup_1d"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(sentinel)

	r5m := NewRollup5m(nil, time.Minute, testLogger(), false)
	r5m.pool = mock
	m1h := NewRollupMerge1h(nil, 5*time.Minute, testLogger())
	m1h.pool = mock
	m1d := NewRollupMerge1d(nil, time.Hour, testLogger())
	m1d.pool = mock
	m1mo := NewRollupMerge1mo(nil, 24*time.Hour, testLogger())
	m1mo.pool = mock

	j := NewRollupCorrection(r5m, m1h, m1d, m1mo, 1, 24*time.Hour, testLogger())
	j.nowFn = func() time.Time { return time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC) }

	if err := j.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want 1mo sentinel", err)
	}
}

// A correction that crosses into a sealed month re-merges that
// month's 1mo bucket (from metric_rollup_1d), still without a watermark write.
func TestRollupCorrection_Run_CrossMonthRemergesSealedMonth(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mergeCols := []string{"id", "bucketStart", "metricName", "dimensionKey", "subDimension", "value", "metadata", "updatedAt"}
	// now = 2026-06-02, lookbackDays=3 → days 2026-05-30, 05-31 (sealed May) and
	// 06-01 (unsealed June). May is sealed → one 1mo re-merge of May.
	for range 3 {
		for range 288 {
			mock.ExpectBegin()
			mock.ExpectExec(`DELETE FROM "metric_rollup_5m"`).
				WithArgs(pgxmock.AnyArg()).
				WillReturnResult(pgxmock.NewResult("DELETE", 0))
			mock.ExpectQuery(`FROM traffic_event`).
				WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
				WillReturnRows(pgxmock.NewRows(trafficEventCols))
			mock.ExpectCommit()
		}
		for range 24 {
			mock.ExpectQuery(`FROM "metric_rollup_5m"`).
				WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
				WillReturnRows(pgxmock.NewRows(mergeCols))
		}
		mock.ExpectQuery(`FROM "metric_rollup_1h"`).
			WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
			WillReturnRows(pgxmock.NewRows(mergeCols))
	}
	// Exactly one 1mo re-merge for sealed May.
	mock.ExpectQuery(`FROM "metric_rollup_1d"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(mergeCols))

	r5m := NewRollup5m(nil, time.Minute, testLogger(), false)
	r5m.pool = mock
	m1h := NewRollupMerge1h(nil, 5*time.Minute, testLogger())
	m1h.pool = mock
	m1d := NewRollupMerge1d(nil, time.Hour, testLogger())
	m1d.pool = mock
	m1mo := NewRollupMerge1mo(nil, 24*time.Hour, testLogger())
	m1mo.pool = mock

	j := NewRollupCorrection(r5m, m1h, m1d, m1mo, 3, 24*time.Hour, testLogger())
	j.nowFn = func() time.Time { return time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC) }

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met (cross-month re-merge): %v", err)
	}
}

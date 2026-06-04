package metricsstore

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

var tNow = time.Date(2026, 5, 27, 0, 0, 0, 0, time.UTC)

func anyArgs(n int) []any {
	a := make([]any, n)
	for i := range a {
		a[i] = pgxmock.AnyArg()
	}
	return a
}

func newMock(t *testing.T) (*Store, pgxmock.PgxPoolIface) {
	t.Helper()
	m, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	t.Cleanup(m.Close)
	return New(m), m
}

var rollupCols = []string{"id", "bucketStart", "metricName", "dimensionKey", "subDimension", "value", "metadata", "updatedAt"}

func rollupRow(id string, meta any) []any {
	return []any{id, tNow, "req.count", "provider=openai", "model=gpt-4o", 1.5, meta, tNow}
}

// ---- tx-based package functions ----

func TestInsertRollupRows(t *testing.T) {
	// Empty rows → no-op, no DB calls.
	if err := InsertRollupRows(context.Background(), nil, "metric_rollup_5m", nil); err != nil {
		t.Fatalf("empty rows should be nil: %v", err)
	}

	_, m := newMock(t)
	m.ExpectBegin()
	tx, _ := m.Begin(context.Background())
	// Two rows: one with metadata, one without (meta="null").
	m.ExpectExec(`INSERT INTO "metric_rollup_5m"`).WithArgs(anyArgs(6)...).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	m.ExpectExec(`INSERT INTO "metric_rollup_5m"`).WithArgs(anyArgs(6)...).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	rows := []metrics.RollupRow{
		{BucketStart: tNow, MetricName: "m", Value: 1, Metadata: json.RawMessage(`{"k":1}`)},
		{BucketStart: tNow, MetricName: "m2", Value: 2},
	}
	if err := InsertRollupRows(context.Background(), tx, "metric_rollup_5m", rows); err != nil {
		t.Fatalf("InsertRollupRows: %v", err)
	}

	// Exec error.
	_, m2 := newMock(t)
	m2.ExpectBegin()
	tx2, _ := m2.Begin(context.Background())
	m2.ExpectExec(`INSERT INTO`).WithArgs(anyArgs(6)...).WillReturnError(errors.New("boom"))
	if err := InsertRollupRows(context.Background(), tx2, "metric_rollup_5m", rows); err == nil {
		t.Fatal("exec error must surface")
	}
}

func TestDeleteRollupBucket(t *testing.T) {
	_, m := newMock(t)
	m.ExpectBegin()
	tx, _ := m.Begin(context.Background())
	m.ExpectExec(`DELETE FROM "metric_rollup_1h"`).WithArgs(tNow).WillReturnResult(pgxmock.NewResult("DELETE", 1))
	if err := DeleteRollupBucket(context.Background(), tx, "metric_rollup_1h", tNow); err != nil {
		t.Fatalf("DeleteRollupBucket: %v", err)
	}
	m.ExpectExec(`DELETE FROM`).WithArgs(tNow).WillReturnError(errors.New("boom"))
	if err := DeleteRollupBucket(context.Background(), tx, "metric_rollup_1h", tNow); err == nil {
		t.Fatal("exec error must surface")
	}
}

func TestSetWatermark(t *testing.T) {
	_, m := newMock(t)
	m.ExpectBegin()
	tx, _ := m.Begin(context.Background())
	m.ExpectExec(`INSERT INTO "rollup_watermark"`).WithArgs("merge-1h", tNow).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	if err := SetWatermark(context.Background(), tx, "merge-1h", tNow); err != nil {
		t.Fatalf("SetWatermark: %v", err)
	}
	m.ExpectExec(`INSERT INTO "rollup_watermark"`).WithArgs("merge-1h", tNow).WillReturnError(errors.New("boom"))
	if err := SetWatermark(context.Background(), tx, "merge-1h", tNow); err == nil {
		t.Fatal("exec error must surface")
	}
}

func TestGetWatermark(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`SELECT "watermark" FROM "rollup_watermark"`).WithArgs("merge-1h").
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(tNow))
	wm, err := s.GetWatermark(context.Background(), "merge-1h")
	if err != nil || !wm.Equal(tNow) {
		t.Fatalf("GetWatermark: %v %v", wm, err)
	}
	m.ExpectQuery(`rollup_watermark`).WithArgs("missing").WillReturnError(errors.New("no rows"))
	if _, err := s.GetWatermark(context.Background(), "missing"); err == nil {
		t.Fatal("error must surface")
	}
}

// ---- QueryRollup / queryRollupOnTable ----

func TestQueryRollup(t *testing.T) {
	s, m := newMock(t)
	// Span 1h → 5m granularity. Global query (no metrics, empty dimension).
	start := tNow
	end := tNow.Add(time.Hour)
	m.ExpectQuery(`FROM "metric_rollup_5m"`).WithArgs(start, end).
		WillReturnRows(pgxmock.NewRows(rollupCols).
			AddRow(rollupRow("r1", []byte(`{"x":1}`))...).
			AddRow(rollupRow("r2", nil)...))
	got, err := s.QueryRollup(context.Background(), metrics.MetricsQuery{StartTime: start, EndTime: end})
	if err != nil || len(got) != 2 || got[0].Metadata == nil || got[1].Metadata != nil {
		t.Fatalf("QueryRollup: %+v %v", got, err)
	}

	// Empty window (start == end) → nil, no query.
	if got, err := s.QueryRollup(context.Background(), metrics.MetricsQuery{StartTime: start, EndTime: start}); err != nil || got != nil {
		t.Fatalf("empty window should be nil: %+v %v", got, err)
	}

	// Filters: metrics + dimension prefix + sub-dimension → 5 args.
	s2, m2 := newMock(t)
	m2.ExpectQuery(`FROM "metric_rollup_5m".*metricName" IN.*dimensionKey" LIKE.*subDimension"`).
		WithArgs(start, end, "req.count", "provider=%", "model=gpt-4o").
		WillReturnRows(pgxmock.NewRows(rollupCols).AddRow(rollupRow("r1", nil)...))
	q := metrics.MetricsQuery{StartTime: start, EndTime: end, Metrics: []string{"req.count"}, DimensionKey: "provider", SubDimension: "model=gpt-4o"}
	if got, err := s2.QueryRollup(context.Background(), q); err != nil || len(got) != 1 {
		t.Fatalf("filtered QueryRollup: %+v %v", got, err)
	}

	// Query error.
	s3, m3 := newMock(t)
	m3.ExpectQuery(`FROM "metric_rollup_5m"`).WithArgs(anyArgs(2)...).WillReturnError(errors.New("boom"))
	if _, err := s3.QueryRollup(context.Background(), metrics.MetricsQuery{StartTime: start, EndTime: end}); err == nil {
		t.Fatal("query error must surface")
	}

	// Scan error (bad bucketStart).
	s4, m4 := newMock(t)
	bad := []any{"r1", "not-a-time", "m", "", "", 1.0, nil, tNow}
	m4.ExpectQuery(`FROM "metric_rollup_5m"`).WithArgs(anyArgs(2)...).WillReturnRows(pgxmock.NewRows(rollupCols).AddRow(bad...))
	if _, err := s4.QueryRollup(context.Background(), metrics.MetricsQuery{StartTime: start, EndTime: end}); err == nil {
		t.Fatal("scan error must surface")
	}

	// Mid-stream iteration error.
	s5, m5 := newMock(t)
	m5.ExpectQuery(`FROM "metric_rollup_5m"`).
		WillReturnRows(pgxmock.NewRows(rollupCols).AddRow(rollupRow("r1", nil)...).CloseError(errors.New("conn reset")))
	if _, err := s5.QueryRollup(context.Background(), metrics.MetricsQuery{StartTime: start, EndTime: end}); err == nil {
		t.Fatal("iterate error must surface")
	}
}

// ---- QueryRollupAware ----

func aware1h() (start, end time.Time) { return tNow, tNow.Add(7 * 24 * time.Hour) } // span 7d → 1h gran

func TestQueryRollupAware(t *testing.T) {
	// 5m granularity (span 1h) → no tail table → single coarse query.
	s, m := newMock(t)
	st, en := tNow, tNow.Add(time.Hour)
	m.ExpectQuery(`FROM "metric_rollup_5m"`).WithArgs(st, en).WillReturnRows(pgxmock.NewRows(rollupCols).AddRow(rollupRow("r1", nil)...))
	if got, err := s.QueryRollupAware(context.Background(), metrics.MetricsQuery{StartTime: st, EndTime: en}); err != nil || len(got) != 1 {
		t.Fatalf("aware 5m: %+v %v", got, err)
	}

	start, end := aware1h()

	// Watermark missing → coarse-only (single 1h query).
	s2, m2 := newMock(t)
	m2.ExpectQuery(`rollup_watermark`).WithArgs("merge-1h").WillReturnError(errors.New("nope"))
	m2.ExpectQuery(`FROM "metric_rollup_1h"`).WithArgs(start, end).WillReturnRows(pgxmock.NewRows(rollupCols))
	if _, err := s2.QueryRollupAware(context.Background(), metrics.MetricsQuery{StartTime: start, EndTime: end}); err != nil {
		t.Fatalf("aware watermark-missing: %v", err)
	}

	// Watermark zero (nil err, zero time) → coarse-only.
	s3, m3 := newMock(t)
	m3.ExpectQuery(`rollup_watermark`).WithArgs("merge-1h").WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(time.Time{}))
	m3.ExpectQuery(`FROM "metric_rollup_1h"`).WithArgs(start, end).WillReturnRows(pgxmock.NewRows(rollupCols))
	if _, err := s3.QueryRollupAware(context.Background(), metrics.MetricsQuery{StartTime: start, EndTime: end}); err != nil {
		t.Fatalf("aware watermark-zero: %v", err)
	}

	// Valid in-range watermark → sealed (1h) + tail (5m), both non-empty → append.
	s4, m4 := newMock(t)
	wm := tNow.Add(3 * 24 * time.Hour)
	m4.ExpectQuery(`rollup_watermark`).WithArgs("merge-1h").WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(wm))
	m4.ExpectQuery(`FROM "metric_rollup_1h"`).WithArgs(start, wm).WillReturnRows(pgxmock.NewRows(rollupCols).AddRow(rollupRow("sealed", nil)...))
	m4.ExpectQuery(`FROM "metric_rollup_5m"`).WithArgs(wm, end).WillReturnRows(pgxmock.NewRows(rollupCols).AddRow(rollupRow("tail", nil)...))
	if got, err := s4.QueryRollupAware(context.Background(), metrics.MetricsQuery{StartTime: start, EndTime: end}); err != nil || len(got) != 2 {
		t.Fatalf("aware split append: %+v %v", got, err)
	}

	// Tail empty → return sealed.
	s5, m5 := newMock(t)
	m5.ExpectQuery(`rollup_watermark`).WithArgs("merge-1h").WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(wm))
	m5.ExpectQuery(`FROM "metric_rollup_1h"`).WithArgs(anyArgs(2)...).WillReturnRows(pgxmock.NewRows(rollupCols).AddRow(rollupRow("sealed", nil)...))
	m5.ExpectQuery(`FROM "metric_rollup_5m"`).WithArgs(anyArgs(2)...).WillReturnRows(pgxmock.NewRows(rollupCols))
	if got, err := s5.QueryRollupAware(context.Background(), metrics.MetricsQuery{StartTime: start, EndTime: end}); err != nil || len(got) != 1 || got[0].ID != "sealed" {
		t.Fatalf("aware tail-empty: %+v %v", got, err)
	}

	// Sealed empty → return tail.
	s6, m6 := newMock(t)
	m6.ExpectQuery(`rollup_watermark`).WithArgs("merge-1h").WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(wm))
	m6.ExpectQuery(`FROM "metric_rollup_1h"`).WithArgs(anyArgs(2)...).WillReturnRows(pgxmock.NewRows(rollupCols))
	m6.ExpectQuery(`FROM "metric_rollup_5m"`).WithArgs(anyArgs(2)...).WillReturnRows(pgxmock.NewRows(rollupCols).AddRow(rollupRow("tail", nil)...))
	if got, err := s6.QueryRollupAware(context.Background(), metrics.MetricsQuery{StartTime: start, EndTime: end}); err != nil || len(got) != 1 || got[0].ID != "tail" {
		t.Fatalf("aware sealed-empty: %+v %v", got, err)
	}

	// Watermark before start → boundary=start → sealed empty-window (no query), tail only.
	s7, m7 := newMock(t)
	m7.ExpectQuery(`rollup_watermark`).WithArgs("merge-1h").WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(start.Add(-time.Hour)))
	m7.ExpectQuery(`FROM "metric_rollup_5m"`).WithArgs(start, end).WillReturnRows(pgxmock.NewRows(rollupCols).AddRow(rollupRow("tail", nil)...))
	if got, err := s7.QueryRollupAware(context.Background(), metrics.MetricsQuery{StartTime: start, EndTime: end}); err != nil || len(got) != 1 {
		t.Fatalf("aware boundary<start: %+v %v", got, err)
	}

	// Watermark after end → boundary=end → sealed only, tail empty-window (no query).
	s8, m8 := newMock(t)
	m8.ExpectQuery(`rollup_watermark`).WithArgs("merge-1h").WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(end.Add(time.Hour)))
	m8.ExpectQuery(`FROM "metric_rollup_1h"`).WithArgs(start, end).WillReturnRows(pgxmock.NewRows(rollupCols).AddRow(rollupRow("sealed", nil)...))
	if got, err := s8.QueryRollupAware(context.Background(), metrics.MetricsQuery{StartTime: start, EndTime: end}); err != nil || len(got) != 1 {
		t.Fatalf("aware boundary>end: %+v %v", got, err)
	}

	// Sealed query error.
	s9, m9 := newMock(t)
	m9.ExpectQuery(`rollup_watermark`).WithArgs("merge-1h").WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(wm))
	m9.ExpectQuery(`FROM "metric_rollup_1h"`).WithArgs(anyArgs(2)...).WillReturnError(errors.New("boom"))
	if _, err := s9.QueryRollupAware(context.Background(), metrics.MetricsQuery{StartTime: start, EndTime: end}); err == nil {
		t.Fatal("sealed query error must surface")
	}

	// Tail query error.
	s10, m10 := newMock(t)
	m10.ExpectQuery(`rollup_watermark`).WithArgs("merge-1h").WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(wm))
	m10.ExpectQuery(`FROM "metric_rollup_1h"`).WithArgs(anyArgs(2)...).WillReturnRows(pgxmock.NewRows(rollupCols).AddRow(rollupRow("sealed", nil)...))
	m10.ExpectQuery(`FROM "metric_rollup_5m"`).WithArgs(anyArgs(2)...).WillReturnError(errors.New("boom"))
	if _, err := s10.QueryRollupAware(context.Background(), metrics.MetricsQuery{StartTime: start, EndTime: end}); err == nil {
		t.Fatal("tail query error must surface")
	}

	// 1d granularity (span ~180d) → rollupTailFor returns the 1h tail / merge-1d
	// job; watermark missing → coarse-only on metric_rollup_1d.
	s11, m11 := newMock(t)
	s1d, e1d := tNow, tNow.Add(180*24*time.Hour)
	m11.ExpectQuery(`rollup_watermark`).WithArgs("merge-1d").WillReturnError(errors.New("nope"))
	m11.ExpectQuery(`FROM "metric_rollup_1d"`).WithArgs(s1d, e1d).WillReturnRows(pgxmock.NewRows(rollupCols))
	if _, err := s11.QueryRollupAware(context.Background(), metrics.MetricsQuery{StartTime: s1d, EndTime: e1d}); err != nil {
		t.Fatalf("aware 1d: %v", err)
	}

	// 1mo granularity (span >365d) → rollupTailFor returns the 1d tail / merge-1mo job.
	s12, m12 := newMock(t)
	s1mo, e1mo := tNow, tNow.Add(400*24*time.Hour)
	m12.ExpectQuery(`rollup_watermark`).WithArgs("merge-1mo").WillReturnError(errors.New("nope"))
	m12.ExpectQuery(`FROM "metric_rollup_1mo"`).WithArgs(s1mo, e1mo).WillReturnRows(pgxmock.NewRows(rollupCols))
	if _, err := s12.QueryRollupAware(context.Background(), metrics.MetricsQuery{StartTime: s1mo, EndTime: e1mo}); err != nil {
		t.Fatalf("aware 1mo: %v", err)
	}
}

// ---- QueryRollupCascade ----

func TestQueryRollupCascade(t *testing.T) {
	start := tNow
	end := tNow.Add(400 * 24 * time.Hour) // 1mo gran for SelectGranularity, but cascade ignores that

	// All watermarks missing → boundaries collapse to start; only the 5m
	// trailing segment [start,end) issues a query.
	s, m := newMock(t)
	m.ExpectQuery(`rollup_watermark`).WithArgs("merge-1mo").WillReturnError(errors.New("nope"))
	m.ExpectQuery(`rollup_watermark`).WithArgs("merge-1d").WillReturnError(errors.New("nope"))
	m.ExpectQuery(`rollup_watermark`).WithArgs("merge-1h").WillReturnError(errors.New("nope"))
	m.ExpectQuery(`FROM "metric_rollup_5m"`).WithArgs(start, end).WillReturnRows(pgxmock.NewRows(rollupCols).AddRow(rollupRow("r1", nil)...))
	if got, err := s.QueryRollupCascade(context.Background(), metrics.MetricsQuery{StartTime: start, EndTime: end}); err != nil || len(got) != 1 {
		t.Fatalf("cascade missing watermarks: %+v %v", got, err)
	}

	// Watermarks set (monotonic): 1mo<1d<1h within range → four non-empty segments.
	s2, m2 := newMock(t)
	b1mo := start.Add(100 * 24 * time.Hour)
	b1d := start.Add(200 * 24 * time.Hour)
	b1h := start.Add(300 * 24 * time.Hour)
	m2.ExpectQuery(`rollup_watermark`).WithArgs("merge-1mo").WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(b1mo))
	m2.ExpectQuery(`rollup_watermark`).WithArgs("merge-1d").WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(b1d))
	m2.ExpectQuery(`rollup_watermark`).WithArgs("merge-1h").WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(b1h))
	m2.ExpectQuery(`FROM "metric_rollup_1mo"`).WithArgs(start, b1mo).WillReturnRows(pgxmock.NewRows(rollupCols).AddRow(rollupRow("mo", nil)...))
	m2.ExpectQuery(`FROM "metric_rollup_1d"`).WithArgs(b1mo, b1d).WillReturnRows(pgxmock.NewRows(rollupCols).AddRow(rollupRow("d", nil)...))
	m2.ExpectQuery(`FROM "metric_rollup_1h"`).WithArgs(b1d, b1h).WillReturnRows(pgxmock.NewRows(rollupCols).AddRow(rollupRow("h", nil)...))
	m2.ExpectQuery(`FROM "metric_rollup_5m"`).WithArgs(b1h, end).WillReturnRows(pgxmock.NewRows(rollupCols).AddRow(rollupRow("m5", nil)...))
	if got, err := s2.QueryRollupCascade(context.Background(), metrics.MetricsQuery{StartTime: start, EndTime: end}); err != nil || len(got) != 4 {
		t.Fatalf("cascade four segments: %+v %v", got, err)
	}

	// Non-monotonic watermarks (1d behind 1mo, 1h behind 1d) → clamped up so
	// the 1d and 1h segments collapse to empty; query error in the 5m segment.
	s3, m3 := newMock(t)
	m3.ExpectQuery(`rollup_watermark`).WithArgs("merge-1mo").WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(b1h))
	m3.ExpectQuery(`rollup_watermark`).WithArgs("merge-1d").WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(b1mo)) // behind 1mo
	m3.ExpectQuery(`rollup_watermark`).WithArgs("merge-1h").WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(b1d))  // behind clamped 1d
	m3.ExpectQuery(`FROM "metric_rollup_1mo"`).WithArgs(start, b1h).WillReturnRows(pgxmock.NewRows(rollupCols))
	m3.ExpectQuery(`FROM "metric_rollup_5m"`).WithArgs(b1h, end).WillReturnError(errors.New("boom"))
	if _, err := s3.QueryRollupCascade(context.Background(), metrics.MetricsQuery{StartTime: start, EndTime: end}); err == nil {
		t.Fatal("cascade segment query error must surface")
	}

	// Out-of-range watermarks exercise both clamp branches: merge-1mo before
	// start (clamp→start) and merge-1d/1h after end (clamp→end). Result:
	// only the 1d segment [start,end) is non-empty.
	s4, m4 := newMock(t)
	m4.ExpectQuery(`rollup_watermark`).WithArgs("merge-1mo").WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(start.Add(-100 * 24 * time.Hour)))
	m4.ExpectQuery(`rollup_watermark`).WithArgs("merge-1d").WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(end.Add(100 * 24 * time.Hour)))
	m4.ExpectQuery(`rollup_watermark`).WithArgs("merge-1h").WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(end.Add(200 * 24 * time.Hour)))
	m4.ExpectQuery(`FROM "metric_rollup_1d"`).WithArgs(start, end).WillReturnRows(pgxmock.NewRows(rollupCols).AddRow(rollupRow("d", nil)...))
	if got, err := s4.QueryRollupCascade(context.Background(), metrics.MetricsQuery{StartTime: start, EndTime: end}); err != nil || len(got) != 1 {
		t.Fatalf("cascade clamp: %+v %v", got, err)
	}
}

// ---- QueryRollupMergeSource / Purge / HasData / fleet ----

func TestQueryRollupMergeSource(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM "metric_rollup_5m"`).WithArgs(tNow, tNow.Add(time.Hour)).
		WillReturnRows(pgxmock.NewRows(rollupCols).AddRow(rollupRow("r1", nil)...))
	if got, err := s.QueryRollupMergeSource(context.Background(), "metric_rollup_5m", tNow, tNow.Add(time.Hour)); err != nil || len(got) != 1 {
		t.Fatalf("QueryRollupMergeSource: %+v %v", got, err)
	}
	m.ExpectQuery(`FROM "metric_rollup_5m"`).WithArgs(anyArgs(2)...).WillReturnError(errors.New("boom"))
	if _, err := s.QueryRollupMergeSource(context.Background(), "metric_rollup_5m", tNow, tNow.Add(time.Hour)); err == nil {
		t.Fatal("query error must surface")
	}
}

func TestPurgeRollupBefore(t *testing.T) {
	s, m := newMock(t)
	m.ExpectExec(`DELETE FROM "metric_rollup_5m"`).WithArgs(tNow).WillReturnResult(pgxmock.NewResult("DELETE", 7))
	if n, err := s.PurgeRollupBefore(context.Background(), "metric_rollup_5m", tNow); err != nil || n != 7 {
		t.Fatalf("PurgeRollupBefore: %d %v", n, err)
	}
	m.ExpectExec(`DELETE FROM`).WithArgs(tNow).WillReturnError(errors.New("boom"))
	if _, err := s.PurgeRollupBefore(context.Background(), "metric_rollup_5m", tNow); err == nil {
		t.Fatal("exec error must surface")
	}
}

func TestRollupHasData(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`SELECT EXISTS`).WithArgs(tNow, tNow.Add(time.Hour)).WillReturnRows(pgxmock.NewRows([]string{"e"}).AddRow(true))
	if ok, err := s.RollupHasData(context.Background(), "metric_rollup_5m", tNow, tNow.Add(time.Hour)); err != nil || !ok {
		t.Fatalf("RollupHasData true: %v %v", ok, err)
	}
	m.ExpectQuery(`SELECT EXISTS`).WillReturnRows(pgxmock.NewRows([]string{"e"}).AddRow(false))
	if ok, _ := s.RollupHasData(context.Background(), "metric_rollup_5m", tNow, tNow.Add(time.Hour)); ok {
		t.Fatal("RollupHasData should be false")
	}
	m.ExpectQuery(`SELECT EXISTS`).WillReturnError(errors.New("boom"))
	if _, err := s.RollupHasData(context.Background(), "metric_rollup_5m", tNow, tNow.Add(time.Hour)); err == nil {
		t.Fatal("error must surface")
	}
}

func TestListMetricRollupBuckets(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM metric_rollup_1h`).WithArgs("req.count", 10).
		WillReturnRows(pgxmock.NewRows([]string{"bucketStart", "dimensionKey", "value"}).
			AddRow(tNow, "provider=openai", 3.0).AddRow(tNow, "provider=anthropic", 2.0))
	got, err := s.ListMetricRollupBuckets(context.Background(), "req.count", 10)
	if err != nil || len(got) != 2 || got[0].Value != 3.0 {
		t.Fatalf("ListMetricRollupBuckets: %+v %v", got, err)
	}
	m.ExpectQuery(`FROM metric_rollup_1h`).WithArgs("req.count", 10).WillReturnError(errors.New("boom"))
	if _, err := s.ListMetricRollupBuckets(context.Background(), "req.count", 10); err == nil {
		t.Fatal("query error must surface")
	}
	s2, m2 := newMock(t)
	// 2-col row vs 3 Scan destinations → deterministic scan error.
	m2.ExpectQuery(`FROM metric_rollup_1h`).WithArgs("req.count", 10).
		WillReturnRows(pgxmock.NewRows([]string{"bucketStart", "dimensionKey"}).AddRow(tNow, "d"))
	if _, err := s2.ListMetricRollupBuckets(context.Background(), "req.count", 10); err == nil {
		t.Fatal("scan error must surface")
	}
	s3, m3 := newMock(t)
	m3.ExpectQuery(`FROM metric_rollup_1h`).WithArgs("req.count", 10).
		WillReturnRows(pgxmock.NewRows([]string{"bucketStart", "dimensionKey", "value"}).AddRow(tNow, "d", 1.0).CloseError(errors.New("conn reset")))
	if _, err := s3.ListMetricRollupBuckets(context.Background(), "req.count", 10); err == nil {
		t.Fatal("iterate error must surface")
	}
}

func TestScanRollupRowsIterateError(t *testing.T) {
	// MergeSource → scanRollupRows with a yielded row then a connection drop
	// covers the rows.Err() iterate-error arm.
	s, m := newMock(t)
	m.ExpectQuery(`FROM "metric_rollup_5m"`).WithArgs(tNow, tNow.Add(time.Hour)).
		WillReturnRows(pgxmock.NewRows(rollupCols).AddRow(rollupRow("r1", nil)...).CloseError(errors.New("conn reset")))
	if _, err := s.QueryRollupMergeSource(context.Background(), "metric_rollup_5m", tNow, tNow.Add(time.Hour)); err == nil {
		t.Fatal("scanRollupRows iterate error must surface")
	}
}

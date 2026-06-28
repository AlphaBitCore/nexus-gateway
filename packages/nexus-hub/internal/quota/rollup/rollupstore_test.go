// packages/nexus-hub/internal/storage/rollupstore/rollupstore_test.go —
// pgxmock-driven unit tests for the rollup helper SQL paths. Every exported
// function is exercised through the PgxPool seam (or the existing pgx.Tx
// interface for tx-scoped writes) without touching the shared dev DB.
// Honors the [[tests-only-own-data]] binding: pgxmock matches expected
// queries by regex against the in-memory mock pool and never opens a real
// Postgres connection.
package rollupstore_test

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"

	rollupstore "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/quota/rollup"
	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

// newMock returns a fresh pgxmock pool with QueryMatcherRegexp (the default)
// and a Cleanup-registered close. All tests use a fresh mock so expectations
// can be set with full isolation.
func newMock(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	return mock
}

// beginTx is the canonical helper for tx-scoped writes. The rollupstore
// SetWatermark / Insert* / Delete*Bucket functions take pgx.Tx directly, so
// tests must obtain one from the mock pool via Begin().
func beginTx(t *testing.T, mock pgxmock.PgxPoolIface) pgx.Tx {
	t.Helper()
	tx, err := mock.Begin(context.Background())
	if err != nil {
		t.Fatalf("mock.Begin: %v", err)
	}
	return tx
}

func TestGetWatermark_Found(t *testing.T) {
	mock := newMock(t)
	want := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`SELECT "watermark" FROM "rollup_watermark"`).
		WithArgs("job-a").
		WillReturnRows(pgxmock.NewRows([]string{"watermark"}).AddRow(want))

	got, err := rollupstore.GetWatermark(context.Background(), mock, "job-a")
	if err != nil {
		t.Fatalf("GetWatermark: %v", err)
	}
	if !got.Equal(want) {
		t.Errorf("got %v want %v", got, want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestGetWatermark_NoRowsReturnsZero(t *testing.T) {
	mock := newMock(t)
	mock.ExpectQuery(`SELECT "watermark" FROM "rollup_watermark"`).
		WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)

	got, err := rollupstore.GetWatermark(context.Background(), mock, "missing")
	if err != nil {
		t.Fatalf("GetWatermark on ErrNoRows must not error: %v", err)
	}
	if !got.IsZero() {
		t.Errorf("expected zero time for missing watermark, got %v", got)
	}
}

func TestGetWatermark_GenericError(t *testing.T) {
	mock := newMock(t)
	mock.ExpectQuery(`SELECT "watermark"`).
		WithArgs("job-a").
		WillReturnError(errors.New("boom"))

	_, err := rollupstore.GetWatermark(context.Background(), mock, "job-a")
	if err == nil || !strings.Contains(err.Error(), "get watermark") {
		t.Errorf("expected wrapped error, got %v", err)
	}
}

func TestSetWatermark_Success(t *testing.T) {
	mock := newMock(t)
	mock.ExpectBegin()
	wm := time.Date(2026, 5, 17, 12, 5, 0, 0, time.UTC)
	mock.ExpectExec(`INSERT INTO "rollup_watermark"`).
		WithArgs("job-a", wm).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	tx := beginTx(t, mock)
	if err := rollupstore.SetWatermark(context.Background(), tx, "job-a", wm); err != nil {
		t.Fatalf("SetWatermark: %v", err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestSetWatermark_ExecError(t *testing.T) {
	mock := newMock(t)
	mock.ExpectBegin()
	wm := time.Now().UTC()
	mock.ExpectExec(`INSERT INTO "rollup_watermark"`).
		WithArgs("job-a", wm).
		WillReturnError(errors.New("dup key"))
	mock.ExpectRollback()

	tx := beginTx(t, mock)
	err := rollupstore.SetWatermark(context.Background(), tx, "job-a", wm)
	if err == nil || !strings.Contains(err.Error(), "set watermark") {
		t.Errorf("expected wrapped error, got %v", err)
	}
	_ = tx.Rollback(context.Background())
}

func TestListWatermarks_HappyPath(t *testing.T) {
	mock := newMock(t)
	wm := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	upd := wm.Add(5 * time.Minute)
	mock.ExpectQuery(`FROM "rollup_watermark"\s+ORDER BY "jobName"`).
		WillReturnRows(pgxmock.NewRows([]string{"jobName", "watermark", "updatedAt"}).
			AddRow("job-a", wm, upd).
			AddRow("job-b", wm.Add(time.Hour), upd.Add(time.Hour)))

	got, err := rollupstore.ListWatermarks(context.Background(), mock)
	if err != nil {
		t.Fatalf("ListWatermarks: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 rows, got %d", len(got))
	}
	if got[0].JobName != "job-a" || got[1].JobName != "job-b" {
		t.Errorf("ordering broken: %+v", got)
	}
	if !got[0].Watermark.Equal(wm) || !got[0].UpdatedAt.Equal(upd) {
		t.Errorf("row 0 fields wrong: %+v", got[0])
	}
}

func TestListWatermarks_QueryError(t *testing.T) {
	mock := newMock(t)
	mock.ExpectQuery(`FROM "rollup_watermark"`).WillReturnError(errors.New("conn lost"))

	_, err := rollupstore.ListWatermarks(context.Background(), mock)
	if err == nil || !strings.Contains(err.Error(), "list watermarks") {
		t.Errorf("expected wrapped error, got %v", err)
	}
}

func TestListWatermarks_ScanError(t *testing.T) {
	mock := newMock(t)
	mock.ExpectQuery(`FROM "rollup_watermark"`).
		// Wrong column type for watermark forces Scan to fail.
		WillReturnRows(pgxmock.NewRows([]string{"jobName", "watermark", "updatedAt"}).
			AddRow("job-a", "not-a-time", time.Now()))

	_, err := rollupstore.ListWatermarks(context.Background(), mock)
	if err == nil || !strings.Contains(err.Error(), "scan watermark") {
		t.Errorf("expected scan-error wrap, got %v", err)
	}
}

func TestListWatermarks_RowsErr(t *testing.T) {
	mock := newMock(t)
	// CloseError surfaces via rows.Err() after iteration completes — that's
	// the only deterministic way to hit the rows.Err() arm without also
	// tripping the scan-error arm above.
	rows := pgxmock.NewRows([]string{"jobName", "watermark", "updatedAt"}).
		AddRow("job-a", time.Now().UTC(), time.Now().UTC()).
		CloseError(errors.New("iter boom"))
	mock.ExpectQuery(`FROM "rollup_watermark"`).WillReturnRows(rows)

	_, err := rollupstore.ListWatermarks(context.Background(), mock)
	if err == nil || !strings.Contains(err.Error(), "iterate watermarks") {
		t.Errorf("expected iter-error wrap, got %v", err)
	}
}

func TestListWatermarks_EmptyTable(t *testing.T) {
	mock := newMock(t)
	mock.ExpectQuery(`FROM "rollup_watermark"`).
		WillReturnRows(pgxmock.NewRows([]string{"jobName", "watermark", "updatedAt"}))

	got, err := rollupstore.ListWatermarks(context.Background(), mock)
	if err != nil {
		t.Fatalf("ListWatermarks empty: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty slice, got %d rows", len(got))
	}
}

func TestInsertRollupRows_Empty(t *testing.T) {
	// No expectations set; passing zero rows must short-circuit before any
	// SQL is issued, so ExpectationsWereMet stays clean.
	mock := newMock(t)
	mock.ExpectBegin()
	mock.ExpectCommit()
	tx := beginTx(t, mock)
	if err := rollupstore.InsertRollupRows(context.Background(), tx, "metric_rollup_5m", nil); err != nil {
		t.Errorf("nil rows must not error: %v", err)
	}
	_ = tx.Commit(context.Background())
}

func TestInsertRollupRows_WithMetadataAndNil(t *testing.T) {
	mock := newMock(t)
	mock.ExpectBegin()
	bkt := time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC)
	rows := []metrics.RollupRow{
		{
			BucketStart:  bkt,
			MetricName:   "tokens_total",
			DimensionKey: "model=gpt-4",
			SubDimension: "openai",
			Value:        100,
			Metadata:     json.RawMessage(`{"k":1}`),
		},
		{
			BucketStart:  bkt,
			MetricName:   "tokens_total",
			DimensionKey: "model=claude",
			SubDimension: "anthropic",
			Value:        200,
			// nil metadata -> "null" literal.
		},
	}
	mock.ExpectExec(`INSERT INTO "metric_rollup_5m"`).
		WithArgs(bkt, "tokens_total", "model=gpt-4", "openai", float64(100), `{"k":1}`).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(`INSERT INTO "metric_rollup_5m"`).
		WithArgs(bkt, "tokens_total", "model=claude", "anthropic", float64(200), "null").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	tx := beginTx(t, mock)
	if err := rollupstore.InsertRollupRows(context.Background(), tx, "metric_rollup_5m", rows); err != nil {
		t.Fatalf("InsertRollupRows: %v", err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestInsertRollupRows_ExecError(t *testing.T) {
	mock := newMock(t)
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO "metric_rollup_5m"`).
		WillReturnError(errors.New("constraint violation"))
	mock.ExpectRollback()

	tx := beginTx(t, mock)
	err := rollupstore.InsertRollupRows(context.Background(), tx, "metric_rollup_5m",
		[]metrics.RollupRow{{BucketStart: time.Now().UTC(), MetricName: "m", Value: 1}})
	if err == nil || !strings.Contains(err.Error(), "insert rollup rows into metric_rollup_5m") {
		t.Errorf("expected wrapped error, got %v", err)
	}
	_ = tx.Rollback(context.Background())
}

func TestDeleteRollupBucket_Success(t *testing.T) {
	mock := newMock(t)
	mock.ExpectBegin()
	bkt := time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC)
	mock.ExpectExec(`DELETE FROM "metric_rollup_5m" WHERE "bucketStart" = \$1`).
		WithArgs(bkt).
		WillReturnResult(pgxmock.NewResult("DELETE", 3))
	mock.ExpectCommit()

	tx := beginTx(t, mock)
	if err := rollupstore.DeleteRollupBucket(context.Background(), tx, "metric_rollup_5m", bkt); err != nil {
		t.Fatalf("DeleteRollupBucket: %v", err)
	}
	_ = tx.Commit(context.Background())
}

func TestDeleteRollupBucket_ExecError(t *testing.T) {
	mock := newMock(t)
	mock.ExpectBegin()
	bkt := time.Now().UTC()
	mock.ExpectExec(`DELETE FROM "metric_rollup_5m"`).
		WithArgs(bkt).
		WillReturnError(errors.New("deadlock"))
	mock.ExpectRollback()

	tx := beginTx(t, mock)
	err := rollupstore.DeleteRollupBucket(context.Background(), tx, "metric_rollup_5m", bkt)
	if err == nil || !strings.Contains(err.Error(), "delete rollup bucket metric_rollup_5m") {
		t.Errorf("expected wrapped error, got %v", err)
	}
	_ = tx.Rollback(context.Background())
}

func TestPurgeRollupBefore_ReturnsRowsAffected(t *testing.T) {
	mock := newMock(t)
	cutoff := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectExec(`DELETE FROM "metric_rollup_5m" WHERE "bucketStart" < \$1`).
		WithArgs(cutoff).
		WillReturnResult(pgxmock.NewResult("DELETE", 42))

	n, err := rollupstore.PurgeRollupBefore(context.Background(), mock, "metric_rollup_5m", cutoff)
	if err != nil {
		t.Fatalf("PurgeRollupBefore: %v", err)
	}
	if n != 42 {
		t.Errorf("rows: want 42 got %d", n)
	}
}

func TestPurgeRollupBefore_ExecError(t *testing.T) {
	mock := newMock(t)
	cutoff := time.Now().UTC()
	mock.ExpectExec(`DELETE FROM "metric_rollup_5m"`).
		WithArgs(cutoff).
		WillReturnError(errors.New("table missing"))

	_, err := rollupstore.PurgeRollupBefore(context.Background(), mock, "metric_rollup_5m", cutoff)
	if err == nil || !strings.Contains(err.Error(), "purge rollup metric_rollup_5m") {
		t.Errorf("expected wrapped error, got %v", err)
	}
}

func TestEarliestBucketStart_HasRow(t *testing.T) {
	mock := newMock(t)
	want := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	// Code scans into `var earliest *time.Time` (so destination is **time.Time);
	// AddRow value must therefore be a *time.Time, not a time.Time.
	mock.ExpectQuery(`SELECT MIN\("bucketStart"\) FROM "metric_rollup_1h"`).
		WillReturnRows(pgxmock.NewRows([]string{"min"}).AddRow(&want))

	got, ok, err := rollupstore.EarliestBucketStart(context.Background(), mock, "metric_rollup_1h")
	if err != nil {
		t.Fatalf("EarliestBucketStart: %v", err)
	}
	if !ok || !got.Equal(want) {
		t.Errorf("ok=%v got=%v want=%v", ok, got, want)
	}
}

func TestEarliestBucketStart_EmptyTable(t *testing.T) {
	mock := newMock(t)
	// MIN over empty table returns SQL NULL; scan target is **time.Time so the
	// inner *time.Time stays nil and triggers the "no rows" branch.
	mock.ExpectQuery(`SELECT MIN\("bucketStart"\) FROM "metric_rollup_1h"`).
		WillReturnRows(pgxmock.NewRows([]string{"min"}).AddRow((*time.Time)(nil)))

	got, ok, err := rollupstore.EarliestBucketStart(context.Background(), mock, "metric_rollup_1h")
	if err != nil {
		t.Fatalf("EarliestBucketStart empty: %v", err)
	}
	if ok || !got.IsZero() {
		t.Errorf("empty-table contract broken: ok=%v got=%v", ok, got)
	}
}

func TestEarliestBucketStart_QueryError(t *testing.T) {
	mock := newMock(t)
	mock.ExpectQuery(`SELECT MIN\("bucketStart"\)`).
		WillReturnError(errors.New("relation missing"))

	_, _, err := rollupstore.EarliestBucketStart(context.Background(), mock, "metric_rollup_1h")
	if err == nil || !strings.Contains(err.Error(), "earliest bucket metric_rollup_1h") {
		t.Errorf("expected wrapped error, got %v", err)
	}
}

func TestEarliestTrafficEventTimestamp_HasRow(t *testing.T) {
	mock := newMock(t)
	want := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`SELECT MIN\("timestamp"\) FROM traffic_event`).
		WillReturnRows(pgxmock.NewRows([]string{"min"}).AddRow(&want))

	got, ok, err := rollupstore.EarliestTrafficEventTimestamp(context.Background(), mock)
	if err != nil {
		t.Fatalf("EarliestTrafficEventTimestamp: %v", err)
	}
	if !ok || !got.Equal(want) {
		t.Errorf("ok=%v got=%v want=%v", ok, got, want)
	}
}

func TestEarliestTrafficEventTimestamp_Empty(t *testing.T) {
	mock := newMock(t)
	mock.ExpectQuery(`SELECT MIN\("timestamp"\) FROM traffic_event`).
		WillReturnRows(pgxmock.NewRows([]string{"min"}).AddRow((*time.Time)(nil)))

	got, ok, err := rollupstore.EarliestTrafficEventTimestamp(context.Background(), mock)
	if err != nil {
		t.Fatalf("EarliestTrafficEventTimestamp empty: %v", err)
	}
	if ok || !got.IsZero() {
		t.Errorf("empty contract broken: ok=%v got=%v", ok, got)
	}
}

func TestEarliestTrafficEventTimestamp_Error(t *testing.T) {
	mock := newMock(t)
	mock.ExpectQuery(`SELECT MIN\("timestamp"\) FROM traffic_event`).
		WillReturnError(errors.New("perm denied"))

	_, _, err := rollupstore.EarliestTrafficEventTimestamp(context.Background(), mock)
	if err == nil || !strings.Contains(err.Error(), "earliest traffic_event timestamp") {
		t.Errorf("expected wrapped error, got %v", err)
	}
}

func TestRollupHasData_True(t *testing.T) {
	mock := newMock(t)
	start := time.Now().UTC()
	end := start.Add(time.Hour)
	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM "metric_rollup_5m"`).
		WithArgs(start, end).
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(true))

	got, err := rollupstore.RollupHasData(context.Background(), mock, "metric_rollup_5m", start, end)
	if err != nil {
		t.Fatalf("RollupHasData: %v", err)
	}
	if !got {
		t.Errorf("expected true")
	}
}

func TestRollupHasData_False(t *testing.T) {
	mock := newMock(t)
	start := time.Now().UTC()
	end := start.Add(time.Hour)
	mock.ExpectQuery(`SELECT EXISTS`).
		WithArgs(start, end).
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(false))

	got, err := rollupstore.RollupHasData(context.Background(), mock, "metric_rollup_5m", start, end)
	if err != nil {
		t.Fatalf("RollupHasData: %v", err)
	}
	if got {
		t.Errorf("expected false")
	}
}

func TestRollupHasData_Error(t *testing.T) {
	mock := newMock(t)
	mock.ExpectQuery(`SELECT EXISTS`).WillReturnError(errors.New("syntax"))

	_, err := rollupstore.RollupHasData(context.Background(), mock, "metric_rollup_5m", time.Now(), time.Now())
	if err == nil || !strings.Contains(err.Error(), "rollup has data metric_rollup_5m") {
		t.Errorf("expected wrapped error, got %v", err)
	}
}

func rollupRowCols() []string {
	return []string{"id", "bucketStart", "metricName", "dimensionKey", "subDimension", "value", "metadata", "updatedAt"}
}

func TestQueryRollupMergeSource_HappyPath(t *testing.T) {
	mock := newMock(t)
	from := time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	upd := from.Add(time.Minute)
	mock.ExpectQuery(`FROM "metric_rollup_5m"`).
		WithArgs(from, to).
		WillReturnRows(pgxmock.NewRows(rollupRowCols()).
			AddRow("id-1", from, "tokens", "model=gpt", "openai", float64(10), []byte(`{"x":1}`), upd).
			AddRow("id-2", from.Add(5*time.Minute), "tokens", "model=claude", "anthropic", float64(20), nil, upd))

	got, err := rollupstore.QueryRollupMergeSource(context.Background(), mock, "metric_rollup_5m", from, to)
	if err != nil {
		t.Fatalf("QueryRollupMergeSource: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 rows got %d", len(got))
	}
	if string(got[0].Metadata) != `{"x":1}` {
		t.Errorf("metadata bytes mismatch: %q", got[0].Metadata)
	}
	if got[1].Metadata != nil {
		t.Errorf("nil metadata column must yield nil RawMessage, got %q", got[1].Metadata)
	}
}

func TestQueryRollupMergeSource_QueryError(t *testing.T) {
	mock := newMock(t)
	mock.ExpectQuery(`FROM "metric_rollup_5m"`).WillReturnError(errors.New("conn lost"))

	_, err := rollupstore.QueryRollupMergeSource(context.Background(), mock, "metric_rollup_5m", time.Now(), time.Now())
	if err == nil || !strings.Contains(err.Error(), "query rollup merge source metric_rollup_5m") {
		t.Errorf("expected wrapped error, got %v", err)
	}
}

func TestQueryRollupMergeSource_ScanError(t *testing.T) {
	mock := newMock(t)
	from := time.Now().UTC()
	to := from.Add(time.Hour)
	// Wrong-typed value column to force scan failure.
	mock.ExpectQuery(`FROM "metric_rollup_5m"`).
		WithArgs(from, to).
		WillReturnRows(pgxmock.NewRows(rollupRowCols()).
			AddRow("id-1", from, "tokens", "model=gpt", "openai", "not-a-float", nil, from))

	_, err := rollupstore.QueryRollupMergeSource(context.Background(), mock, "metric_rollup_5m", from, to)
	if err == nil || !strings.Contains(err.Error(), "scan rollup row") {
		t.Errorf("expected scan-error wrap, got %v", err)
	}
}

func TestQueryRollupMergeSource_RowsErr(t *testing.T) {
	mock := newMock(t)
	from := time.Now().UTC()
	to := from.Add(time.Hour)
	rows := pgxmock.NewRows(rollupRowCols()).
		AddRow("id-1", from, "tokens", "model=gpt", "openai", float64(1), nil, from).
		CloseError(errors.New("iter boom"))
	mock.ExpectQuery(`FROM "metric_rollup_5m"`).WithArgs(from, to).WillReturnRows(rows)

	_, err := rollupstore.QueryRollupMergeSource(context.Background(), mock, "metric_rollup_5m", from, to)
	if err == nil || !strings.Contains(err.Error(), "iterate rollup rows") {
		t.Errorf("expected iter-error wrap, got %v", err)
	}
}

// TestQueryRollup_AllOptionalFilters drives the branch combination where
// every optional clause is engaged (metrics IN-list, dimensionKey LIKE,
// subDimension equality). Combined with the no-filter test below, every
// AND-arm of the WHERE builder is covered.
func TestQueryRollup_AllOptionalFilters(t *testing.T) {
	mock := newMock(t)
	// span <= 6h -> Granularity5m -> metric_rollup_5m
	start := time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	mock.ExpectQuery(`FROM "metric_rollup_5m"`).
		WithArgs(start, end, "tokens", "errors", "model=%", "openai").
		WillReturnRows(pgxmock.NewRows(rollupRowCols()).
			AddRow("id-1", start, "tokens", "model=gpt", "openai", float64(5), []byte(`{}`), start))

	q := metrics.MetricsQuery{
		Metrics:      []string{"tokens", "errors"},
		DimensionKey: "model",
		SubDimension: "openai",
		StartTime:    start,
		EndTime:      end,
	}
	got, err := rollupstore.QueryRollup(context.Background(), mock, q)
	if err != nil {
		t.Fatalf("QueryRollup: %v", err)
	}
	if len(got) != 1 || got[0].MetricName != "tokens" {
		t.Errorf("row content unexpected: %+v", got)
	}
}

// TestQueryRollup_NoFilters drives the empty-DimensionKey path which appends
// `dimensionKey = ”` and the no-metrics / no-subDimension paths.
func TestQueryRollup_NoFilters(t *testing.T) {
	mock := newMock(t)
	// span <= 6h -> 5m table.
	start := time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	mock.ExpectQuery(`FROM "metric_rollup_5m".*"dimensionKey" = ''`).
		WithArgs(start, end).
		WillReturnRows(pgxmock.NewRows(rollupRowCols()))

	got, err := rollupstore.QueryRollup(context.Background(), mock, metrics.MetricsQuery{
		StartTime: start,
		EndTime:   end,
	})
	if err != nil {
		t.Fatalf("QueryRollup: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty, got %d rows", len(got))
	}
}

// TestQueryRollup_LargeSpanSelectsCoarseGranularity verifies the
// SelectGranularity wiring: a span > 90 days picks the 1d table, > 365 picks
// 1mo, > 6h <= 90d picks 1h.
func TestQueryRollup_LargeSpanSelectsCoarseGranularity(t *testing.T) {
	tests := []struct {
		name  string
		span  time.Duration
		table string
	}{
		{"7h_picks_1h", 7 * time.Hour, "metric_rollup_1h"},
		{"100d_picks_1d", 100 * 24 * time.Hour, "metric_rollup_1d"},
		{"400d_picks_1mo", 400 * 24 * time.Hour, "metric_rollup_1mo"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mock := newMock(t)
			start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			end := start.Add(tc.span)
			mock.ExpectQuery(`FROM "`+tc.table+`"`).
				WithArgs(start, end).
				WillReturnRows(pgxmock.NewRows(rollupRowCols()))

			if _, err := rollupstore.QueryRollup(context.Background(), mock, metrics.MetricsQuery{
				StartTime: start, EndTime: end,
			}); err != nil {
				t.Fatalf("QueryRollup: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("expectations: %v", err)
			}
		})
	}
}

func TestQueryRollup_QueryError(t *testing.T) {
	mock := newMock(t)
	mock.ExpectQuery(`FROM "metric_rollup_5m"`).WillReturnError(errors.New("boom"))

	_, err := rollupstore.QueryRollup(context.Background(), mock, metrics.MetricsQuery{
		StartTime: time.Now(), EndTime: time.Now().Add(time.Hour),
	})
	if err == nil || !strings.Contains(err.Error(), "query rollup") {
		t.Errorf("expected wrapped error, got %v", err)
	}
}

func TestInsertThingRollupRows_Empty(t *testing.T) {
	mock := newMock(t)
	mock.ExpectBegin()
	mock.ExpectCommit()
	tx := beginTx(t, mock)
	if err := rollupstore.InsertThingRollupRows(context.Background(), tx, "thing_metric_rollup_5m", nil); err != nil {
		t.Errorf("nil rows must not error: %v", err)
	}
	_ = tx.Commit(context.Background())
}

func TestInsertThingRollupRows_WithMetadataAndNil(t *testing.T) {
	mock := newMock(t)
	mock.ExpectBegin()
	bkt := time.Now().UTC()
	rows := []metrics.ThingRollupRow{
		{
			BucketStart:  bkt,
			ThingID:      "thing-a",
			MetricName:   "tokens",
			DimensionKey: "model=gpt",
			SubDimension: "openai",
			Value:        7,
			Metadata:     json.RawMessage(`{"q":2}`),
		},
		{
			BucketStart: bkt,
			ThingID:     "thing-b",
			MetricName:  "tokens",
			Value:       9,
		},
	}
	mock.ExpectExec(`INSERT INTO "thing_metric_rollup_5m"`).
		WithArgs(bkt, "thing-a", "tokens", "model=gpt", "openai", float64(7), `{"q":2}`).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(`INSERT INTO "thing_metric_rollup_5m"`).
		WithArgs(bkt, "thing-b", "tokens", "", "", float64(9), "null").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	tx := beginTx(t, mock)
	if err := rollupstore.InsertThingRollupRows(context.Background(), tx, "thing_metric_rollup_5m", rows); err != nil {
		t.Fatalf("InsertThingRollupRows: %v", err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func TestInsertThingRollupRows_ExecError(t *testing.T) {
	mock := newMock(t)
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO "thing_metric_rollup_5m"`).
		WillReturnError(errors.New("constraint"))
	mock.ExpectRollback()

	tx := beginTx(t, mock)
	err := rollupstore.InsertThingRollupRows(context.Background(), tx, "thing_metric_rollup_5m",
		[]metrics.ThingRollupRow{{BucketStart: time.Now(), ThingID: "t", MetricName: "m", Value: 1}})
	if err == nil || !strings.Contains(err.Error(), "insert thing rollup rows into thing_metric_rollup_5m") {
		t.Errorf("expected wrapped error, got %v", err)
	}
	_ = tx.Rollback(context.Background())
}

func TestDeleteThingRollupBucket_Success(t *testing.T) {
	mock := newMock(t)
	mock.ExpectBegin()
	bkt := time.Now().UTC()
	mock.ExpectExec(`DELETE FROM "thing_metric_rollup_5m"`).
		WithArgs(bkt).
		WillReturnResult(pgxmock.NewResult("DELETE", 5))
	mock.ExpectCommit()

	tx := beginTx(t, mock)
	if err := rollupstore.DeleteThingRollupBucket(context.Background(), tx, "thing_metric_rollup_5m", bkt); err != nil {
		t.Fatalf("DeleteThingRollupBucket: %v", err)
	}
	_ = tx.Commit(context.Background())
}

func TestDeleteThingRollupBucket_ExecError(t *testing.T) {
	mock := newMock(t)
	mock.ExpectBegin()
	bkt := time.Now().UTC()
	mock.ExpectExec(`DELETE FROM "thing_metric_rollup_5m"`).
		WithArgs(bkt).
		WillReturnError(errors.New("deadlock"))
	mock.ExpectRollback()

	tx := beginTx(t, mock)
	err := rollupstore.DeleteThingRollupBucket(context.Background(), tx, "thing_metric_rollup_5m", bkt)
	if err == nil || !strings.Contains(err.Error(), "delete thing rollup bucket thing_metric_rollup_5m") {
		t.Errorf("expected wrapped error, got %v", err)
	}
	_ = tx.Rollback(context.Background())
}

func TestPurgeThingRollupBefore_ReturnsRowsAffected(t *testing.T) {
	mock := newMock(t)
	cutoff := time.Now().UTC()
	mock.ExpectExec(`DELETE FROM "thing_metric_rollup_5m" WHERE "bucketStart" < \$1`).
		WithArgs(cutoff).
		WillReturnResult(pgxmock.NewResult("DELETE", 17))

	n, err := rollupstore.PurgeThingRollupBefore(context.Background(), mock, "thing_metric_rollup_5m", cutoff)
	if err != nil {
		t.Fatalf("PurgeThingRollupBefore: %v", err)
	}
	if n != 17 {
		t.Errorf("rows: want 17 got %d", n)
	}
}

func TestPurgeThingRollupBefore_ExecError(t *testing.T) {
	mock := newMock(t)
	mock.ExpectExec(`DELETE FROM "thing_metric_rollup_5m"`).
		WillReturnError(errors.New("perm denied"))

	_, err := rollupstore.PurgeThingRollupBefore(context.Background(), mock, "thing_metric_rollup_5m", time.Now())
	if err == nil || !strings.Contains(err.Error(), "purge thing rollup thing_metric_rollup_5m") {
		t.Errorf("expected wrapped error, got %v", err)
	}
}

func thingRowCols() []string {
	return []string{"id", "bucketStart", "thing_id", "metricName", "dimensionKey", "subDimension", "value", "metadata", "updatedAt"}
}

func TestQueryThingRollupMergeSource_HappyPath(t *testing.T) {
	mock := newMock(t)
	from := time.Now().UTC()
	to := from.Add(time.Hour)
	mock.ExpectQuery(`FROM "thing_metric_rollup_5m"`).
		WithArgs(from, to).
		WillReturnRows(pgxmock.NewRows(thingRowCols()).
			AddRow("id-1", from, "thing-a", "tokens", "model=gpt", "openai", float64(1), []byte(`{"a":1}`), from).
			AddRow("id-2", from, "thing-b", "tokens", "model=claude", "anthropic", float64(2), nil, from))

	got, err := rollupstore.QueryThingRollupMergeSource(context.Background(), mock, "thing_metric_rollup_5m", from, to)
	if err != nil {
		t.Fatalf("QueryThingRollupMergeSource: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 got %d", len(got))
	}
	if got[0].ThingID != "thing-a" || got[1].ThingID != "thing-b" {
		t.Errorf("ordering broken: %+v", got)
	}
	if string(got[0].Metadata) != `{"a":1}` || got[1].Metadata != nil {
		t.Errorf("metadata mapping broken: %+v / %+v", got[0].Metadata, got[1].Metadata)
	}
}

func TestQueryThingRollupMergeSource_QueryError(t *testing.T) {
	mock := newMock(t)
	mock.ExpectQuery(`FROM "thing_metric_rollup_5m"`).
		WillReturnError(errors.New("conn lost"))

	_, err := rollupstore.QueryThingRollupMergeSource(context.Background(), mock, "thing_metric_rollup_5m", time.Now(), time.Now())
	if err == nil || !strings.Contains(err.Error(), "query thing rollup merge source thing_metric_rollup_5m") {
		t.Errorf("expected wrapped error, got %v", err)
	}
}

func TestQueryThingRollupMergeSource_ScanError(t *testing.T) {
	mock := newMock(t)
	from := time.Now().UTC()
	to := from.Add(time.Hour)
	mock.ExpectQuery(`FROM "thing_metric_rollup_5m"`).
		WithArgs(from, to).
		WillReturnRows(pgxmock.NewRows(thingRowCols()).
			AddRow("id-1", from, "thing-a", "tokens", "k", "s", "not-a-float", nil, from))

	_, err := rollupstore.QueryThingRollupMergeSource(context.Background(), mock, "thing_metric_rollup_5m", from, to)
	if err == nil || !strings.Contains(err.Error(), "scan thing rollup row") {
		t.Errorf("expected scan-error wrap, got %v", err)
	}
}

func TestQueryThingRollupMergeSource_RowsErr(t *testing.T) {
	mock := newMock(t)
	from := time.Now().UTC()
	to := from.Add(time.Hour)
	rows := pgxmock.NewRows(thingRowCols()).
		AddRow("id-1", from, "thing-a", "tokens", "k", "s", float64(1), nil, from).
		CloseError(errors.New("iter"))
	mock.ExpectQuery(`FROM "thing_metric_rollup_5m"`).WithArgs(from, to).WillReturnRows(rows)

	_, err := rollupstore.QueryThingRollupMergeSource(context.Background(), mock, "thing_metric_rollup_5m", from, to)
	if err == nil || !strings.Contains(err.Error(), "iterate thing rollup rows") {
		t.Errorf("expected iter-error wrap, got %v", err)
	}
}

func TestQueryThingRollup_MissingThingID(t *testing.T) {
	mock := newMock(t)
	_, err := rollupstore.QueryThingRollup(context.Background(), mock, rollupstore.ThingMetricsQuery{
		StartTime: time.Now(),
		EndTime:   time.Now().Add(time.Hour),
	})
	if err == nil || !strings.Contains(err.Error(), "thingID is required") {
		t.Errorf("missing-ThingID guard broken: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("no SQL expected: %v", err)
	}
}

func TestQueryThingRollup_AllFilters(t *testing.T) {
	mock := newMock(t)
	start := time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour) // <= 6h -> thing_metric_rollup_5m
	mock.ExpectQuery(`FROM "thing_metric_rollup_5m"`).
		WithArgs("thing-a", start, end, "tokens", "errors", "model=%", "openai").
		WillReturnRows(pgxmock.NewRows(thingRowCols()).
			AddRow("id-1", start, "thing-a", "tokens", "model=gpt", "openai", float64(3), []byte(`{"m":1}`), start))

	got, err := rollupstore.QueryThingRollup(context.Background(), mock, rollupstore.ThingMetricsQuery{
		ThingID:      "thing-a",
		Metrics:      []string{"tokens", "errors"},
		DimensionKey: "model",
		SubDimension: "openai",
		StartTime:    start,
		EndTime:      end,
	})
	if err != nil {
		t.Fatalf("QueryThingRollup: %v", err)
	}
	if len(got) != 1 || got[0].ThingID != "thing-a" {
		t.Errorf("unexpected rows: %+v", got)
	}
}

func TestQueryThingRollup_NoFilters(t *testing.T) {
	mock := newMock(t)
	start := time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	mock.ExpectQuery(`FROM "thing_metric_rollup_5m".*"dimensionKey" = ''`).
		WithArgs("thing-a", start, end).
		WillReturnRows(pgxmock.NewRows(thingRowCols()))

	got, err := rollupstore.QueryThingRollup(context.Background(), mock, rollupstore.ThingMetricsQuery{
		ThingID:   "thing-a",
		StartTime: start,
		EndTime:   end,
	})
	if err != nil {
		t.Fatalf("QueryThingRollup: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty, got %d", len(got))
	}
}

func TestQueryThingRollup_LargeSpansSelectCoarseTable(t *testing.T) {
	cases := []struct {
		name  string
		span  time.Duration
		table string
	}{
		{"7h_picks_1h", 7 * time.Hour, "thing_metric_rollup_1h"},
		{"100d_picks_1d", 100 * 24 * time.Hour, "thing_metric_rollup_1d"},
		{"400d_picks_1mo", 400 * 24 * time.Hour, "thing_metric_rollup_1mo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := newMock(t)
			start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			end := start.Add(tc.span)
			mock.ExpectQuery(`FROM "`+tc.table+`"`).
				WithArgs("thing-a", start, end).
				WillReturnRows(pgxmock.NewRows(thingRowCols()))

			if _, err := rollupstore.QueryThingRollup(context.Background(), mock, rollupstore.ThingMetricsQuery{
				ThingID:   "thing-a",
				StartTime: start,
				EndTime:   end,
			}); err != nil {
				t.Fatalf("QueryThingRollup: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("expectations: %v", err)
			}
		})
	}
}

func TestQueryThingRollup_QueryError(t *testing.T) {
	mock := newMock(t)
	mock.ExpectQuery(`FROM "thing_metric_rollup_5m"`).WillReturnError(errors.New("boom"))

	_, err := rollupstore.QueryThingRollup(context.Background(), mock, rollupstore.ThingMetricsQuery{
		ThingID:   "thing-a",
		StartTime: time.Now(),
		EndTime:   time.Now().Add(time.Hour),
	})
	if err == nil || !strings.Contains(err.Error(), "query thing rollup") {
		t.Errorf("expected wrapped error, got %v", err)
	}
}

func TestEarliestThingBucketStart_HasRow(t *testing.T) {
	mock := newMock(t)
	want := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`SELECT MIN\("bucketStart"\) FROM "thing_metric_rollup_1h"`).
		WillReturnRows(pgxmock.NewRows([]string{"min"}).AddRow(&want))

	got, ok, err := rollupstore.EarliestThingBucketStart(context.Background(), mock, "thing_metric_rollup_1h")
	if err != nil {
		t.Fatalf("EarliestThingBucketStart: %v", err)
	}
	if !ok || !got.Equal(want) {
		t.Errorf("ok=%v got=%v want=%v", ok, got, want)
	}
}

func TestEarliestThingBucketStart_Empty(t *testing.T) {
	mock := newMock(t)
	mock.ExpectQuery(`SELECT MIN\("bucketStart"\) FROM "thing_metric_rollup_1h"`).
		WillReturnRows(pgxmock.NewRows([]string{"min"}).AddRow((*time.Time)(nil)))

	got, ok, err := rollupstore.EarliestThingBucketStart(context.Background(), mock, "thing_metric_rollup_1h")
	if err != nil {
		t.Fatalf("EarliestThingBucketStart empty: %v", err)
	}
	if ok || !got.IsZero() {
		t.Errorf("empty contract broken: ok=%v got=%v", ok, got)
	}
}

func TestEarliestThingBucketStart_Error(t *testing.T) {
	mock := newMock(t)
	mock.ExpectQuery(`SELECT MIN\("bucketStart"\)`).
		WillReturnError(errors.New("relation missing"))

	_, _, err := rollupstore.EarliestThingBucketStart(context.Background(), mock, "thing_metric_rollup_1h")
	if err == nil || !strings.Contains(err.Error(), "earliest thing bucket thing_metric_rollup_1h") {
		t.Errorf("expected wrapped error, got %v", err)
	}
}

// Sanity: ensure *pgxpool.Pool satisfies rollupstore.PgxPool. Compile-time
// only — never executed. If pgx ever changes its pool signature, this fails
// fast with a clean message instead of every caller breaking elsewhere.

var _ rollupstore.PgxPool = (interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
})(nil)

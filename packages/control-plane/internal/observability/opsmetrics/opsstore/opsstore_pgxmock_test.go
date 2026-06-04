package opsstore

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
)

// EncodeRaw base64-encodes an arbitrary cursor payload so DecodeDiagCursor's
// post-base64 validation arms (missing separator, bad time) are reachable.
func EncodeRaw(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

var (
	tNow  = time.Date(2026, 5, 27, 0, 0, 0, 0, time.UTC)
	tFrom = tNow.Add(-time.Hour)
	tTo   = tNow
)

var dimD = "d"

func fp(f float64) *float64 { return &f }
func sp(s string) *string   { return &s }

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

// ---- SelectGranularity (pure) ----

func TestSelectGranularity(t *testing.T) {
	cases := []struct {
		span time.Duration
		want string
	}{
		{time.Hour, "raw"},
		{3 * 24 * time.Hour, "1h"},
		{30 * 24 * time.Hour, "1d"},
		{200 * 24 * time.Hour, "1mo"},
	}
	for _, c := range cases {
		if got := SelectGranularity(tNow, tNow.Add(c.span)); got != c.want {
			t.Fatalf("SelectGranularity(%v) = %q, want %q", c.span, got, c.want)
		}
	}
}

// ---- GetOpsMetricsCurrent ----

var sampleCols = []string{"sampled_at", "thing_id", "thing_type", "metric_name", "metric_kind", "dimension_key", "value", "metadata"}

func TestGetOpsMetricsCurrent(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM ranked`).WithArgs(anyArgs(2)...).
		WillReturnRows(pgxmock.NewRows(sampleCols).AddRow(tNow, "t1", "agent", "cpu", "gauge", "", fp(0.5), []byte(`{"k":1}`)))
	out, err := s.GetOpsMetricsCurrent(context.Background(), OpsCurrentParams{ThingType: "agent", ThingID: "t1"})
	if err != nil || len(out) != 1 || out[0].ThingID != "t1" || out[0].Metadata["k"] == nil {
		t.Fatalf("GetOpsMetricsCurrent: %+v %v", out, err)
	}
	// No filters (nil thingType/thingID) + nil metadata row.
	s2, m2 := newMock(t)
	m2.ExpectQuery(`FROM ranked`).WithArgs(anyArgs(2)...).WillReturnRows(pgxmock.NewRows(sampleCols).AddRow(tNow, "t1", "agent", "cpu", "gauge", "", nil, nil))
	if _, err := s2.GetOpsMetricsCurrent(context.Background(), OpsCurrentParams{}); err != nil {
		t.Fatalf("unfiltered: %v", err)
	}
	// Query error.
	s3, m3 := newMock(t)
	m3.ExpectQuery(`FROM ranked`).WithArgs(anyArgs(2)...).WillReturnError(errors.New("boom"))
	if _, err := s3.GetOpsMetricsCurrent(context.Background(), OpsCurrentParams{}); err == nil {
		t.Fatal("query error must surface")
	}
	// Scan error.
	s4, m4 := newMock(t)
	m4.ExpectQuery(`FROM ranked`).WithArgs(anyArgs(2)...).WillReturnRows(pgxmock.NewRows([]string{"sampled_at"}).AddRow(tNow))
	if _, err := s4.GetOpsMetricsCurrent(context.Background(), OpsCurrentParams{}); err == nil {
		t.Fatal("scan error must surface")
	}
	// Iterate error.
	s5, m5 := newMock(t)
	m5.ExpectQuery(`FROM ranked`).WithArgs(anyArgs(2)...).WillReturnRows(pgxmock.NewRows(sampleCols).AddRow(tNow, "t1", "agent", "cpu", "gauge", "", nil, nil).CloseError(errors.New("conn reset")))
	if _, err := s5.GetOpsMetricsCurrent(context.Background(), OpsCurrentParams{}); err == nil {
		t.Fatal("iterate error must surface")
	}
}

// ---- GetOpsMetricsTimeseries + queryOpsRaw/Rollup ----

var bucketCols = []string{"bucket_start", "thing_id", "thing_type", "metric_name", "metric_kind", "dimension_key", "value_avg", "value_sum", "value_min", "value_max", "sample_count", "metadata"}

func TestGetOpsMetricsTimeseries_Validation(t *testing.T) {
	s, _ := newMock(t)
	base := OpsTimeseriesParams{ThingID: "t1", MetricName: "cpu", From: tFrom, To: tTo, Granularity: "raw"}
	checks := []OpsTimeseriesParams{
		func() OpsTimeseriesParams { p := base; p.ThingID = ""; return p }(),
		func() OpsTimeseriesParams { p := base; p.MetricName = ""; return p }(),
		func() OpsTimeseriesParams { p := base; p.From = time.Time{}; return p }(),
		func() OpsTimeseriesParams { p := base; p.To = p.From; return p }(),
		func() OpsTimeseriesParams { p := base; p.Granularity = "weird"; return p }(),
	}
	for i, p := range checks {
		if _, err := s.GetOpsMetricsTimeseries(context.Background(), p); err == nil {
			t.Fatalf("validation case %d must error", i)
		}
	}
}

func TestGetOpsMetricsTimeseries_Raw(t *testing.T) {
	s, m := newMock(t)
	dim := "d"
	m.ExpectQuery(`FROM metric_ops_raw`).WithArgs("t1", "cpu", pgxmock.AnyArg(), tFrom, tTo).
		WillReturnRows(pgxmock.NewRows(sampleCols).AddRow(tNow, "t1", "agent", "cpu", "gauge", "d", fp(0.5), []byte(`{"k":1}`)))
	out, err := s.GetOpsMetricsTimeseries(context.Background(), OpsTimeseriesParams{ThingID: "t1", MetricName: "cpu", DimensionKey: &dim, From: tFrom, To: tTo, Granularity: "raw"})
	if err != nil || len(out) != 1 || out[0].SampleCount != 1 || out[0].ValueAvg == nil || out[0].Metadata["k"] == nil {
		t.Fatalf("raw timeseries: %+v %v", out, err)
	}
	// Query error.
	s2, m2 := newMock(t)
	m2.ExpectQuery(`FROM metric_ops_raw`).WithArgs(anyArgs(5)...).WillReturnError(errors.New("boom"))
	if _, err := s2.GetOpsMetricsTimeseries(context.Background(), OpsTimeseriesParams{ThingID: "t1", MetricName: "cpu", From: tFrom, To: tTo, Granularity: "raw"}); err == nil {
		t.Fatal("raw query error must surface")
	}
	// Scan error.
	s3, m3 := newMock(t)
	m3.ExpectQuery(`FROM metric_ops_raw`).WithArgs(anyArgs(5)...).WillReturnRows(pgxmock.NewRows([]string{"sampled_at"}).AddRow(tNow))
	if _, err := s3.GetOpsMetricsTimeseries(context.Background(), OpsTimeseriesParams{ThingID: "t1", MetricName: "cpu", From: tFrom, To: tTo, Granularity: "raw"}); err == nil {
		t.Fatal("raw scan error must surface")
	}
	// Iterate error.
	s4, m4 := newMock(t)
	m4.ExpectQuery(`FROM metric_ops_raw`).WithArgs(anyArgs(5)...).WillReturnRows(pgxmock.NewRows(sampleCols).AddRow(tNow, "t1", "agent", "cpu", "gauge", "", nil, nil).CloseError(errors.New("conn reset")))
	if _, err := s4.GetOpsMetricsTimeseries(context.Background(), OpsTimeseriesParams{ThingID: "t1", MetricName: "cpu", From: tFrom, To: tTo, Granularity: "raw"}); err == nil {
		t.Fatal("raw iterate error must surface")
	}
}

func TestGetOpsMetricsTimeseries_Rollup(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM metric_ops_rollup_1h`).WithArgs("t1", "cpu", pgxmock.AnyArg(), tFrom, tTo).
		WillReturnRows(pgxmock.NewRows(bucketCols).AddRow(tNow, sp("t1"), "agent", "cpu", "gauge", "", fp(1), fp(2), fp(0), fp(3), int32(5), []byte(`{}`)))
	out, err := s.GetOpsMetricsTimeseries(context.Background(), OpsTimeseriesParams{ThingID: "t1", MetricName: "cpu", DimensionKey: &dimD, From: tFrom, To: tTo, Granularity: "1h"})
	if err != nil || len(out) != 1 || out[0].SampleCount != 5 {
		t.Fatalf("rollup timeseries: %+v %v", out, err)
	}
	// scanOpsBuckets scan error.
	s2, m2 := newMock(t)
	m2.ExpectQuery(`FROM metric_ops_rollup_1d`).WithArgs(anyArgs(5)...).WillReturnRows(pgxmock.NewRows([]string{"bucket_start"}).AddRow(tNow))
	if _, err := s2.GetOpsMetricsTimeseries(context.Background(), OpsTimeseriesParams{ThingID: "t1", MetricName: "cpu", From: tFrom, To: tTo, Granularity: "1d"}); err == nil {
		t.Fatal("rollup scan error must surface")
	}
	// Query error.
	s3, m3 := newMock(t)
	m3.ExpectQuery(`FROM metric_ops_rollup_1mo`).WithArgs(anyArgs(5)...).WillReturnError(errors.New("boom"))
	if _, err := s3.GetOpsMetricsTimeseries(context.Background(), OpsTimeseriesParams{ThingID: "t1", MetricName: "cpu", From: tFrom, To: tTo, Granularity: "1mo"}); err == nil {
		t.Fatal("rollup query error must surface")
	}
	// scanOpsBuckets iterate error.
	s4, m4 := newMock(t)
	m4.ExpectQuery(`FROM metric_ops_rollup_1h`).WithArgs(anyArgs(5)...).WillReturnRows(pgxmock.NewRows(bucketCols).AddRow(tNow, nil, "agent", "cpu", "gauge", "", nil, nil, nil, nil, int32(1), nil).CloseError(errors.New("conn reset")))
	if _, err := s4.GetOpsMetricsTimeseries(context.Background(), OpsTimeseriesParams{ThingID: "t1", MetricName: "cpu", From: tFrom, To: tTo, Granularity: "1h"}); err == nil {
		t.Fatal("rollup iterate error must surface")
	}
}

// ---- GetOpsMetricsFleet ----

func TestGetOpsMetricsFleet(t *testing.T) {
	s, m := newMock(t)
	// tFrom/tTo span exactly 1h → SelectGranularity returns "raw"; fleet has no
	// raw slice so the handler bumps to the smallest fleet tier, "5m".
	m.ExpectQuery(`FROM metric_ops_rollup_5m`).WithArgs("agent", "cpu", pgxmock.AnyArg(), tFrom, tTo).
		WillReturnRows(pgxmock.NewRows(bucketCols).AddRow(tNow, nil, "agent", "cpu", "gauge", "", fp(1), fp(2), fp(0), fp(3), int32(7), []byte(`{}`)))
	out, err := s.GetOpsMetricsFleet(context.Background(), OpsFleetParams{ThingType: "agent", MetricName: "cpu", DimensionKey: &dimD, From: tFrom, To: tTo})
	if err != nil || len(out) != 1 || out[0].ThingID != nil {
		t.Fatalf("fleet (auto-gran): %+v %v", out, err)
	}
	// Validation.
	for i, p := range []OpsFleetParams{
		{MetricName: "cpu", From: tFrom, To: tTo},
		{ThingType: "agent", From: tFrom, To: tTo},
		{ThingType: "agent", MetricName: "cpu"},
		{ThingType: "agent", MetricName: "cpu", From: tFrom, To: tTo, Granularity: "raw"},
	} {
		if _, err := s.GetOpsMetricsFleet(context.Background(), p); err == nil {
			t.Fatalf("fleet validation %d must error", i)
		}
	}
	// Auto-gran with a wide span (>90d → 1mo).
	s2, m2 := newMock(t)
	m2.ExpectQuery(`FROM metric_ops_rollup_1mo`).WithArgs(anyArgs(5)...).WillReturnRows(pgxmock.NewRows(bucketCols))
	if _, err := s2.GetOpsMetricsFleet(context.Background(), OpsFleetParams{ThingType: "agent", MetricName: "cpu", From: tNow.Add(-200 * 24 * time.Hour), To: tNow}); err != nil {
		t.Fatalf("fleet wide span: %v", err)
	}
	// Query error.
	s3, m3 := newMock(t)
	m3.ExpectQuery(`FROM metric_ops_rollup_1h`).WithArgs(anyArgs(5)...).WillReturnError(errors.New("boom"))
	if _, err := s3.GetOpsMetricsFleet(context.Background(), OpsFleetParams{ThingType: "agent", MetricName: "cpu", From: tFrom, To: tTo, Granularity: "1h"}); err == nil {
		t.Fatal("fleet query error must surface")
	}
}

// ---- Cursor codec ----

func TestDiagCursorRoundTrip(t *testing.T) {
	c := EncodeDiagCursor(tNow, "id1")
	gotT, gotID, err := DecodeDiagCursor(c)
	if err != nil || gotID != "id1" || !gotT.Equal(tNow) {
		t.Fatalf("cursor round-trip: %v %q %v", gotT, gotID, err)
	}
	if _, _, err := DecodeDiagCursor("!!!not-base64!!!"); err == nil {
		t.Fatal("bad base64 must error")
	}
	if _, _, err := DecodeDiagCursor(EncodeRaw("no-separator")); err == nil {
		t.Fatal("missing separator must error")
	}
	if _, _, err := DecodeDiagCursor(EncodeRaw("not-a-time|id")); err == nil {
		t.Fatal("bad time must error")
	}
}

// ---- ListDiagEvents ----

var diagCols = []string{"id", "thing_id", "thing_type", "occurred_at", "received_at", "level", "event_type", "source", "message", "message_hash", "attrs", "stack_trace", "repeat_count", "agent_version", "os_info"}

func diagRow(id string) []any {
	return []any{id, "t1", "agent", tNow, tNow, "ERROR", "error", "agent", "boom", "h1", []byte(`{"a":1}`), sp("stack"), 2, sp("1.0"), []byte(`{"os":"darwin"}`)}
}

func TestListDiagEvents(t *testing.T) {
	// All filters + cursor; limit defaulting.
	s, m := newMock(t)
	from, to := tFrom, tTo
	cursor := EncodeDiagCursor(tNow, "prev")
	p := DiagEventListParams{ThingID: "t1", Level: "ERROR", EventType: "error", Source: "agent",
		From: &from, To: &to, Search: "boom", Cursor: cursor}
	// 7 filter args + 2 cursor args + limit = 10.
	m.ExpectQuery(`FROM thing_diag_event`).WithArgs(anyArgs(10)...).WillReturnRows(pgxmock.NewRows(diagCols).AddRow(diagRow("e1")...))
	res, err := s.ListDiagEvents(context.Background(), p)
	if err != nil || len(res.Items) != 1 || res.Items[0].StackTrace != "stack" || res.Items[0].Attrs["a"] == nil {
		t.Fatalf("ListDiagEvents: %+v %v", res, err)
	}
	// Bad cursor → error before query.
	if _, err := s.ListDiagEvents(context.Background(), DiagEventListParams{Cursor: "!!!"}); err == nil {
		t.Fatal("bad cursor must error")
	}
	// Pagination: limit=1, 2 rows returned → NextCursor set, trimmed to 1.
	s2, m2 := newMock(t)
	m2.ExpectQuery(`FROM thing_diag_event`).WithArgs(2).WillReturnRows(pgxmock.NewRows(diagCols).AddRow(diagRow("e1")...).AddRow(diagRow("e2")...))
	res2, err := s2.ListDiagEvents(context.Background(), DiagEventListParams{Limit: 1})
	if err != nil || len(res2.Items) != 1 || res2.NextCursor == "" {
		t.Fatalf("pagination: %+v %v", res2, err)
	}
	// limit clamp >500.
	s3, m3 := newMock(t)
	m3.ExpectQuery(`FROM thing_diag_event`).WithArgs(501).WillReturnRows(pgxmock.NewRows(diagCols))
	if _, err := s3.ListDiagEvents(context.Background(), DiagEventListParams{Limit: 9999}); err != nil {
		t.Fatalf("limit clamp: %v", err)
	}
	// Query error.
	s4, m4 := newMock(t)
	m4.ExpectQuery(`FROM thing_diag_event`).WithArgs(anyArgs(1)...).WillReturnError(errors.New("boom"))
	if _, err := s4.ListDiagEvents(context.Background(), DiagEventListParams{}); err == nil {
		t.Fatal("query error must surface")
	}
	// Scan error.
	s5, m5 := newMock(t)
	m5.ExpectQuery(`FROM thing_diag_event`).WithArgs(anyArgs(1)...).WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("e1"))
	if _, err := s5.ListDiagEvents(context.Background(), DiagEventListParams{}); err == nil {
		t.Fatal("scan error must surface")
	}
	// Iterate error.
	s6, m6 := newMock(t)
	m6.ExpectQuery(`FROM thing_diag_event`).WithArgs(anyArgs(1)...).WillReturnRows(pgxmock.NewRows(diagCols).AddRow(diagRow("e1")...).CloseError(errors.New("conn reset")))
	if _, err := s6.ListDiagEvents(context.Background(), DiagEventListParams{}); err == nil {
		t.Fatal("iterate error must surface")
	}
}

// ---- ListDiagGroups ----

var groupCols = []string{"message_hash", "sample_message", "source", "affected_things", "total_occurrences", "first_seen", "last_seen", "max_level", "silenced"}

func TestListDiagGroups(t *testing.T) {
	s, m := newMock(t)
	// Groups query + buckets query.
	m.ExpectQuery(`FROM thing_diag_event e`).WithArgs(tFrom, tTo, pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(groupCols).AddRow("h1", "msg", "agent", 3, 10, tNow, tNow, "ERROR", false))
	m.ExpectQuery(`date_bin`).WithArgs(tFrom, tTo, pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"message_hash", "bucket_ts", "cnt"}).AddRow("h1", tNow, 5))
	out, err := s.ListDiagGroups(context.Background(), DiagGroupsParams{From: tFrom, To: tTo, ThingType: "agent", EventType: "error"})
	if err != nil || len(out) != 1 || len(out[0].Buckets) != 1 {
		t.Fatalf("ListDiagGroups: %+v %v", out, err)
	}
	// Validation.
	if _, err := s.ListDiagGroups(context.Background(), DiagGroupsParams{}); err == nil {
		t.Fatal("validation must error")
	}
	// No groups → early return (no bucket query).
	s2, m2 := newMock(t)
	m2.ExpectQuery(`FROM thing_diag_event e`).WithArgs(anyArgs(4)...).WillReturnRows(pgxmock.NewRows(groupCols))
	if out, err := s2.ListDiagGroups(context.Background(), DiagGroupsParams{From: tFrom, To: tTo}); err != nil || len(out) != 0 {
		t.Fatalf("empty groups: %+v %v", out, err)
	}
	// Groups query error.
	s3, m3 := newMock(t)
	m3.ExpectQuery(`FROM thing_diag_event e`).WithArgs(anyArgs(4)...).WillReturnError(errors.New("boom"))
	if _, err := s3.ListDiagGroups(context.Background(), DiagGroupsParams{From: tFrom, To: tTo}); err == nil {
		t.Fatal("groups query error must surface")
	}
	// Groups scan error.
	s4, m4 := newMock(t)
	m4.ExpectQuery(`FROM thing_diag_event e`).WithArgs(anyArgs(4)...).WillReturnRows(pgxmock.NewRows([]string{"message_hash"}).AddRow("h1"))
	if _, err := s4.ListDiagGroups(context.Background(), DiagGroupsParams{From: tFrom, To: tTo}); err == nil {
		t.Fatal("groups scan error must surface")
	}
	// Bucket query error.
	s5, m5 := newMock(t)
	m5.ExpectQuery(`FROM thing_diag_event e`).WithArgs(anyArgs(4)...).WillReturnRows(pgxmock.NewRows(groupCols).AddRow("h1", "m", "agent", 1, 1, tNow, tNow, "ERROR", false))
	m5.ExpectQuery(`date_bin`).WithArgs(anyArgs(4)...).WillReturnError(errors.New("boom"))
	if _, err := s5.ListDiagGroups(context.Background(), DiagGroupsParams{From: tFrom, To: tTo}); err == nil {
		t.Fatal("bucket query error must surface")
	}
	// Bucket scan error.
	s6, m6 := newMock(t)
	m6.ExpectQuery(`FROM thing_diag_event e`).WithArgs(anyArgs(4)...).WillReturnRows(pgxmock.NewRows(groupCols).AddRow("h1", "m", "agent", 1, 1, tNow, tNow, "ERROR", false))
	m6.ExpectQuery(`date_bin`).WithArgs(anyArgs(4)...).WillReturnRows(pgxmock.NewRows([]string{"message_hash"}).AddRow("h1"))
	if _, err := s6.ListDiagGroups(context.Background(), DiagGroupsParams{From: tFrom, To: tTo}); err == nil {
		t.Fatal("bucket scan error must surface")
	}
	// Groups mid-stream iterate error (rows.Err after a row yielded).
	s7, m7 := newMock(t)
	m7.ExpectQuery(`FROM thing_diag_event e`).WithArgs(anyArgs(4)...).
		WillReturnRows(pgxmock.NewRows(groupCols).AddRow("h1", "m", "agent", 1, 1, tNow, tNow, "ERROR", false).CloseError(errors.New("conn reset")))
	if _, err := s7.ListDiagGroups(context.Background(), DiagGroupsParams{From: tFrom, To: tTo}); err == nil {
		t.Fatal("groups iterate error must surface")
	}
	// Bucket mid-stream iterate error.
	s8, m8 := newMock(t)
	m8.ExpectQuery(`FROM thing_diag_event e`).WithArgs(anyArgs(4)...).WillReturnRows(pgxmock.NewRows(groupCols).AddRow("h1", "m", "agent", 1, 1, tNow, tNow, "ERROR", false))
	m8.ExpectQuery(`date_bin`).WithArgs(anyArgs(4)...).
		WillReturnRows(pgxmock.NewRows([]string{"message_hash", "bucket_ts", "cnt"}).AddRow("h1", tNow, 1).CloseError(errors.New("conn reset")))
	if _, err := s8.ListDiagGroups(context.Background(), DiagGroupsParams{From: tFrom, To: tTo}); err == nil {
		t.Fatal("bucket iterate error must surface")
	}
}

// ---- ListCrashCohorts ----

func TestListCrashCohorts(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`event_type = 'crash'`).WithArgs(tFrom, tTo).
		WillReturnRows(pgxmock.NewRows([]string{"av", "os", "osv", "cc", "at", "ls"}).AddRow("1.0", "darwin", "14", 3, 2, tNow))
	out, err := s.ListCrashCohorts(context.Background(), tFrom, tTo)
	if err != nil || len(out) != 1 || out[0].CrashCount != 3 {
		t.Fatalf("ListCrashCohorts: %+v %v", out, err)
	}
	if _, err := s.ListCrashCohorts(context.Background(), tTo, tFrom); err == nil {
		t.Fatal("validation must error")
	}
	s2, m2 := newMock(t)
	m2.ExpectQuery(`event_type = 'crash'`).WithArgs(anyArgs(2)...).WillReturnError(errors.New("boom"))
	if _, err := s2.ListCrashCohorts(context.Background(), tFrom, tTo); err == nil {
		t.Fatal("query error must surface")
	}
	s3, m3 := newMock(t)
	m3.ExpectQuery(`event_type = 'crash'`).WithArgs(anyArgs(2)...).WillReturnRows(pgxmock.NewRows([]string{"av"}).AddRow("1.0"))
	if _, err := s3.ListCrashCohorts(context.Background(), tFrom, tTo); err == nil {
		t.Fatal("scan error must surface")
	}
	s4, m4 := newMock(t)
	m4.ExpectQuery(`event_type = 'crash'`).WithArgs(anyArgs(2)...).WillReturnRows(pgxmock.NewRows([]string{"av", "os", "osv", "cc", "at", "ls"}).AddRow("1.0", "d", "14", 1, 1, tNow).CloseError(errors.New("conn reset")))
	if _, err := s4.ListCrashCohorts(context.Background(), tFrom, tTo); err == nil {
		t.Fatal("iterate error must surface")
	}
}

// ---- EnableDiagMode (tx) ----

var windowCols = []string{"id", "thing_id", "started_at", "ended_at", "set_by", "reason", "created_at"}

func TestEnableDiagMode(t *testing.T) {
	s, m := newMock(t)
	m.ExpectBegin()
	m.ExpectQuery(`SELECT id FROM thing WHERE id`).WithArgs("t1").WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("t1"))
	m.ExpectExec(`UPDATE thing_diag_mode_window`).WithArgs("t1").WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	m.ExpectQuery(`INSERT INTO thing_diag_mode_window`).WithArgs("t1", tTo, pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(windowCols).AddRow("w1", "t1", tNow, tTo, sp("admin"), sp("why"), tNow))
	m.ExpectCommit()
	w, err := s.EnableDiagMode(context.Background(), EnableDiagModeParams{ThingID: "t1", Until: tTo, SetBy: "admin", Reason: "why"})
	if err != nil || w.ID != "w1" {
		t.Fatalf("EnableDiagMode: %+v %v", w, err)
	}
	// Begin error.
	s2, m2 := newMock(t)
	m2.ExpectBegin().WillReturnError(errors.New("boom"))
	if _, err := s2.EnableDiagMode(context.Background(), EnableDiagModeParams{ThingID: "t1", Until: tTo}); err == nil {
		t.Fatal("begin error must surface")
	}
	// Thing not found.
	s3, m3 := newMock(t)
	m3.ExpectBegin()
	m3.ExpectQuery(`SELECT id FROM thing`).WithArgs("gone").WillReturnError(pgx.ErrNoRows)
	m3.ExpectRollback()
	if _, err := s3.EnableDiagMode(context.Background(), EnableDiagModeParams{ThingID: "gone", Until: tTo}); !errors.Is(err, ErrThingNotFound) {
		t.Fatalf("thing-not-found should be ErrThingNotFound: %v", err)
	}
	// Lookup other error.
	s4, m4 := newMock(t)
	m4.ExpectBegin()
	m4.ExpectQuery(`SELECT id FROM thing`).WithArgs(anyArgs(1)...).WillReturnError(errors.New("boom"))
	m4.ExpectRollback()
	if _, err := s4.EnableDiagMode(context.Background(), EnableDiagModeParams{ThingID: "t1", Until: tTo}); err == nil || errors.Is(err, ErrThingNotFound) {
		t.Fatalf("lookup error must surface: %v", err)
	}
	// Close-prior error.
	s5, m5 := newMock(t)
	m5.ExpectBegin()
	m5.ExpectQuery(`SELECT id FROM thing`).WithArgs(anyArgs(1)...).WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("t1"))
	m5.ExpectExec(`UPDATE thing_diag_mode_window`).WithArgs(anyArgs(1)...).WillReturnError(errors.New("boom"))
	m5.ExpectRollback()
	if _, err := s5.EnableDiagMode(context.Background(), EnableDiagModeParams{ThingID: "t1", Until: tTo}); err == nil {
		t.Fatal("close-prior error must surface")
	}
	// FK violation on set_by (23503).
	s6, m6 := newMock(t)
	m6.ExpectBegin()
	m6.ExpectQuery(`SELECT id FROM thing`).WithArgs(anyArgs(1)...).WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("t1"))
	m6.ExpectExec(`UPDATE thing_diag_mode_window`).WithArgs(anyArgs(1)...).WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	m6.ExpectQuery(`INSERT INTO thing_diag_mode_window`).WithArgs(anyArgs(4)...).WillReturnError(&pgconn.PgError{Code: "23503"})
	m6.ExpectRollback()
	if _, err := s6.EnableDiagMode(context.Background(), EnableDiagModeParams{ThingID: "t1", Until: tTo, SetBy: "ghost"}); err == nil {
		t.Fatal("FK violation must surface")
	}
	// Insert error that is NOT an FK violation → generic insert-error arm.
	s6b, m6b := newMock(t)
	m6b.ExpectBegin()
	m6b.ExpectQuery(`SELECT id FROM thing`).WithArgs(anyArgs(1)...).WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("t1"))
	m6b.ExpectExec(`UPDATE thing_diag_mode_window`).WithArgs(anyArgs(1)...).WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	m6b.ExpectQuery(`INSERT INTO thing_diag_mode_window`).WithArgs(anyArgs(4)...).WillReturnError(errors.New("boom"))
	m6b.ExpectRollback()
	if _, err := s6b.EnableDiagMode(context.Background(), EnableDiagModeParams{ThingID: "t1", Until: tTo}); err == nil {
		t.Fatal("insert error must surface")
	}
	// Commit error.
	s7, m7 := newMock(t)
	m7.ExpectBegin()
	m7.ExpectQuery(`SELECT id FROM thing`).WithArgs(anyArgs(1)...).WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("t1"))
	m7.ExpectExec(`UPDATE thing_diag_mode_window`).WithArgs(anyArgs(1)...).WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	m7.ExpectQuery(`INSERT INTO thing_diag_mode_window`).WithArgs(anyArgs(4)...).WillReturnRows(pgxmock.NewRows(windowCols).AddRow("w1", "t1", tNow, tTo, nil, nil, tNow))
	m7.ExpectCommit().WillReturnError(errors.New("boom"))
	if _, err := s7.EnableDiagMode(context.Background(), EnableDiagModeParams{ThingID: "t1", Until: tTo}); err == nil {
		t.Fatal("commit error must surface")
	}
}

// ---- DisableDiagMode (tx) ----

func TestDisableDiagMode(t *testing.T) {
	s, m := newMock(t)
	m.ExpectBegin()
	m.ExpectExec(`UPDATE thing_diag_mode_window`).WithArgs("t1").WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	m.ExpectCommit()
	if err := s.DisableDiagMode(context.Background(), "t1"); err != nil {
		t.Fatalf("DisableDiagMode: %v", err)
	}
	// Begin error.
	s2, m2 := newMock(t)
	m2.ExpectBegin().WillReturnError(errors.New("boom"))
	if err := s2.DisableDiagMode(context.Background(), "t1"); err == nil {
		t.Fatal("begin error must surface")
	}
	// Exec error.
	s3, m3 := newMock(t)
	m3.ExpectBegin()
	m3.ExpectExec(`UPDATE thing_diag_mode_window`).WithArgs(anyArgs(1)...).WillReturnError(errors.New("boom"))
	m3.ExpectRollback()
	if err := s3.DisableDiagMode(context.Background(), "t1"); err == nil {
		t.Fatal("exec error must surface")
	}
	// No active window → ErrNoActiveDiagMode.
	s4, m4 := newMock(t)
	m4.ExpectBegin()
	m4.ExpectExec(`UPDATE thing_diag_mode_window`).WithArgs("t1").WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	m4.ExpectRollback()
	if err := s4.DisableDiagMode(context.Background(), "t1"); !errors.Is(err, ErrNoActiveDiagMode) {
		t.Fatalf("0 rows should be ErrNoActiveDiagMode: %v", err)
	}
	// Commit error.
	s5, m5 := newMock(t)
	m5.ExpectBegin()
	m5.ExpectExec(`UPDATE thing_diag_mode_window`).WithArgs("t1").WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	m5.ExpectCommit().WillReturnError(errors.New("boom"))
	if err := s5.DisableDiagMode(context.Background(), "t1"); err == nil {
		t.Fatal("commit error must surface")
	}
}

// ---- ListActiveDiagModeWindows ----

func TestListActiveDiagModeWindows(t *testing.T) {
	s, m := newMock(t)
	cols := []string{"id", "thing_id", "type", "started_at", "ended_at", "set_by", "reason", "created_at"}
	m.ExpectQuery(`FROM thing_diag_mode_window w\s+JOIN thing`).
		WillReturnRows(pgxmock.NewRows(cols).AddRow("w1", "t1", "agent", tNow, tTo, sp("admin"), sp("why"), tNow))
	out, err := s.ListActiveDiagModeWindows(context.Background())
	if err != nil || len(out) != 1 || out[0].ThingType != "agent" {
		t.Fatalf("ListActiveDiagModeWindows: %+v %v", out, err)
	}
	m.ExpectQuery(`JOIN thing`).WillReturnError(errors.New("boom"))
	if _, err := s.ListActiveDiagModeWindows(context.Background()); err == nil {
		t.Fatal("query error must surface")
	}
	s2, m2 := newMock(t)
	m2.ExpectQuery(`JOIN thing`).WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("w1"))
	if _, err := s2.ListActiveDiagModeWindows(context.Background()); err == nil {
		t.Fatal("scan error must surface")
	}
	s3, m3 := newMock(t)
	m3.ExpectQuery(`JOIN thing`).WillReturnRows(pgxmock.NewRows(cols).AddRow("w1", "t1", "agent", tNow, tTo, nil, nil, tNow).CloseError(errors.New("conn reset")))
	if _, err := s3.ListActiveDiagModeWindows(context.Background()); err == nil {
		t.Fatal("iterate error must surface")
	}
}

// ---- ResolveBulkAgents ----

func TestResolveBulkAgents(t *testing.T) {
	// By explicit IDs.
	s, m := newMock(t)
	m.ExpectQuery(`WHERE id = ANY\(\$1\)\s+AND type = 'agent'`).WithArgs([]string{"t1", "t2"}, 501).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("t1"))
	out, err := s.ResolveBulkAgents(context.Background(), BulkAgentFilter{ThingIDs: []string{"t1", "t2"}}, 0)
	if err != nil || len(out) != 1 || out[0] != "t1" {
		t.Fatalf("ResolveBulkAgents by id: %+v %v", out, err)
	}
	// By-id query error.
	s2, m2 := newMock(t)
	m2.ExpectQuery(`WHERE id = ANY`).WithArgs(anyArgs(2)...).WillReturnError(errors.New("boom"))
	if _, err := s2.ResolveBulkAgents(context.Background(), BulkAgentFilter{ThingIDs: []string{"t1"}}, 10); err == nil {
		t.Fatal("by-id query error must surface")
	}
	// By-id scan error.
	s3, m3 := newMock(t)
	m3.ExpectQuery(`WHERE id = ANY`).WithArgs(anyArgs(2)...).WillReturnRows(pgxmock.NewRows([]string{"id", "extra"}).AddRow("t1", "x"))
	if _, err := s3.ResolveBulkAgents(context.Background(), BulkAgentFilter{ThingIDs: []string{"t1"}}, 10); err == nil {
		t.Fatal("by-id scan error must surface")
	}
	// Attribute filter (both set).
	s4, m4 := newMock(t)
	m4.ExpectQuery(`serviceVersion`).WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), 501).WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("t1").AddRow("t2"))
	out4, err := s4.ResolveBulkAgents(context.Background(), BulkAgentFilter{AgentVersion: "1.0", OS: "darwin"}, 0)
	if err != nil || len(out4) != 2 {
		t.Fatalf("ResolveBulkAgents by attr: %+v %v", out4, err)
	}
	// Attribute query error.
	s5, m5 := newMock(t)
	m5.ExpectQuery(`serviceVersion`).WithArgs(anyArgs(3)...).WillReturnError(errors.New("boom"))
	if _, err := s5.ResolveBulkAgents(context.Background(), BulkAgentFilter{}, 10); err == nil {
		t.Fatal("attr query error must surface")
	}
	// Attribute scan error.
	s6, m6 := newMock(t)
	m6.ExpectQuery(`serviceVersion`).WithArgs(anyArgs(3)...).WillReturnRows(pgxmock.NewRows([]string{"id", "extra"}).AddRow("t1", "x"))
	if _, err := s6.ResolveBulkAgents(context.Background(), BulkAgentFilter{}, 10); err == nil {
		t.Fatal("attr scan error must surface")
	}
}

// ---- Retention ----

func TestListRetentionConfig(t *testing.T) {
	s, m := newMock(t)
	m.ExpectQuery(`FROM metric_ops_retention_config`).
		WillReturnRows(pgxmock.NewRows([]string{"layer", "retention_days", "updated_at"}).AddRow("raw", 7, tNow))
	out, err := s.ListRetentionConfig(context.Background())
	if err != nil || len(out) != 1 || out[0].RetentionDays != 7 {
		t.Fatalf("ListRetentionConfig: %+v %v", out, err)
	}
	m.ExpectQuery(`FROM metric_ops_retention_config`).WillReturnError(errors.New("boom"))
	if _, err := s.ListRetentionConfig(context.Background()); err == nil {
		t.Fatal("query error must surface")
	}
	s2, m2 := newMock(t)
	m2.ExpectQuery(`FROM metric_ops_retention_config`).WillReturnRows(pgxmock.NewRows([]string{"layer"}).AddRow("raw"))
	if _, err := s2.ListRetentionConfig(context.Background()); err == nil {
		t.Fatal("scan error must surface")
	}
}

func TestUpdateRetentionConfig(t *testing.T) {
	// Empty updates → no-op.
	s, _ := newMock(t)
	if err := s.UpdateRetentionConfig(context.Background(), nil, "admin"); err != nil {
		t.Fatalf("empty updates: %v", err)
	}
	// Happy single-layer update.
	s1, m1 := newMock(t)
	m1.ExpectBegin()
	m1.ExpectExec(`UPDATE metric_ops_retention_config`).WithArgs(7, pgxmock.AnyArg(), "raw").WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	m1.ExpectCommit()
	if err := s1.UpdateRetentionConfig(context.Background(), map[string]int{"raw": 7}, "admin"); err != nil {
		t.Fatalf("UpdateRetentionConfig: %v", err)
	}
	// Begin error.
	s2, m2 := newMock(t)
	m2.ExpectBegin().WillReturnError(errors.New("boom"))
	if err := s2.UpdateRetentionConfig(context.Background(), map[string]int{"raw": 7}, ""); err == nil {
		t.Fatal("begin error must surface")
	}
	// Exec error.
	s3, m3 := newMock(t)
	m3.ExpectBegin()
	m3.ExpectExec(`UPDATE metric_ops_retention_config`).WithArgs(anyArgs(3)...).WillReturnError(errors.New("boom"))
	m3.ExpectRollback()
	if err := s3.UpdateRetentionConfig(context.Background(), map[string]int{"raw": 7}, ""); err == nil {
		t.Fatal("exec error must surface")
	}
	// Unknown layer (0 rows).
	s4, m4 := newMock(t)
	m4.ExpectBegin()
	m4.ExpectExec(`UPDATE metric_ops_retention_config`).WithArgs(anyArgs(3)...).WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	m4.ExpectRollback()
	if err := s4.UpdateRetentionConfig(context.Background(), map[string]int{"bogus": 7}, ""); err == nil {
		t.Fatal("unknown layer must error")
	}
	// Commit error.
	s5, m5 := newMock(t)
	m5.ExpectBegin()
	m5.ExpectExec(`UPDATE metric_ops_retention_config`).WithArgs(anyArgs(3)...).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	m5.ExpectCommit().WillReturnError(errors.New("boom"))
	if err := s5.UpdateRetentionConfig(context.Background(), map[string]int{"raw": 7}, "admin"); err == nil {
		t.Fatal("commit error must surface")
	}
}

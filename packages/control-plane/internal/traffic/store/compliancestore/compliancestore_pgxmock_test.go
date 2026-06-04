package compliancestore

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/settings/store/metricsstore"
)

var (
	tNow   = time.Date(2026, 5, 27, 0, 0, 0, 0, time.UTC)
	tStart = tNow.Add(-time.Hour)
	tEnd   = tNow
)

func sp(s string) *string   { return &s }
func ip(i int) *int         { return &i }
func fp(f float64) *float64 { return &f }

func anyArgs(n int) []any {
	a := make([]any, n)
	for i := range a {
		a[i] = pgxmock.AnyArg()
	}
	return a
}

func newMock(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	m, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	t.Cleanup(m.Close)
	return m
}

// metricsBackedStore returns a Store whose metrics field is a real metricsstore
// over a second mock pool, so the rollup-cascade glue paths are exercised.
func metricsBackedStore(t *testing.T) (*Store, pgxmock.PgxPoolIface, pgxmock.PgxPoolIface) {
	t.Helper()
	pool := newMock(t)
	mpool := newMock(t)
	return New(pool, metricsstore.New(mpool)), pool, mpool
}

// cascadeRows returns a superset of rollup rows covering every compliance/hook
// metric (Value 10 each) plus latency-histogram metadata, so metrics.BuildResult
// populates Summary + Metadata and the store's summary-math / percentile branches
// execute. buildSummary filters to each query's q.Metrics, so the superset is safe.
func cascadeRows() *pgxmock.Rows {
	hist := []byte(`{"buckets":[1,2,3,4,5,6]}`)
	r := pgxmock.NewRows([]string{"id", "bucketStart", "metricName", "dimensionKey", "subDimension", "value", "metadata", "updatedAt"})
	for i, mn := range []string{
		"request_count", "bump_success_count", "bump_failed_count", "bump_exempt_count", "bump_disabled_count",
		"hook_allow_count", "hook_deny_count", "hook_error_count", "hook_unknown_count",
	} {
		r.AddRow("r"+string(rune('a'+i)), tNow, mn, "", "", 10.0, nil, tNow)
	}
	r.AddRow("hl", tNow, "latency_histogram", "", "", 0.0, hist, tNow)
	r.AddRow("hh", tNow, "hook_latency_histogram", "", "", 0.0, hist, tNow)
	return r
}

// primeCascade makes one metricsstore.QueryRollupCascade call (3 missing
// watermarks → only the 5m segment over [tStart,tEnd] queries) return the
// full metric set.
func primeCascade(mp pgxmock.PgxPoolIface, segArgN int) {
	mp.ExpectQuery(`rollup_watermark`).WithArgs("merge-1mo").WillReturnError(errors.New("none"))
	mp.ExpectQuery(`rollup_watermark`).WithArgs("merge-1d").WillReturnError(errors.New("none"))
	mp.ExpectQuery(`rollup_watermark`).WithArgs("merge-1h").WillReturnError(errors.New("none"))
	mp.ExpectQuery(`FROM "metric_rollup_5m"`).WithArgs(anyArgs(segArgN)...).WillReturnRows(cascadeRows())
}

// primeCascadeEmpty makes a QueryRollupCascade call return zero rows (all
// watermarks missing AND the 5m segment query returns no rows), driving the
// store's "rollup empty → continue / fall back" branches.
func primeCascadeEmpty(mp pgxmock.PgxPoolIface, segArgN int) {
	mp.ExpectQuery(`rollup_watermark`).WithArgs("merge-1mo").WillReturnError(errors.New("none"))
	mp.ExpectQuery(`rollup_watermark`).WithArgs("merge-1d").WillReturnError(errors.New("none"))
	mp.ExpectQuery(`rollup_watermark`).WithArgs("merge-1h").WillReturnError(errors.New("none"))
	mp.ExpectQuery(`FROM "metric_rollup_5m"`).WithArgs(anyArgs(segArgN)...).
		WillReturnRows(pgxmock.NewRows([]string{"id", "bucketStart", "metricName", "dimensionKey", "subDimension", "value", "metadata", "updatedAt"}))
}

// ---- ListMatrixAuditEvents ----

var matrixCols = []string{"id", "tx", "sip", "host", "method", "path", "sc", "hd", "hrc", "lat", "ts", "tags"}

func matrixRow() []any {
	return []any{"e1", "tx1", "1.2.3.4", "api.x", sp("POST"), sp("/v1"), ip(200), sp("APPROVE"), sp("ok"), ip(50), tNow, []string{"pii"}}
}

func TestListMatrixAuditEvents(t *testing.T) {
	m := newMock(t)
	s := New(m, nil)
	m.ExpectQuery(`FROM traffic_event`).WithArgs(tStart, tEnd, 10, 0).WillReturnRows(pgxmock.NewRows(matrixCols).AddRow(matrixRow()...))
	m.ExpectQuery(`SELECT COUNT\(\*\) FROM traffic_event`).WithArgs(tStart, tEnd).WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	res, total, err := s.ListMatrixAuditEvents(context.Background(), &tStart, &tEnd, 10, 0)
	if err != nil || total != 1 || len(res) != 1 || res[0].ID != "e1" {
		t.Fatalf("ListMatrixAuditEvents: %+v total=%d err=%v", res, total, err)
	}
	m2 := newMock(t)
	m2.ExpectQuery(`FROM traffic_event`).WillReturnError(errors.New("boom"))
	if _, _, err := New(m2, nil).ListMatrixAuditEvents(context.Background(), nil, nil, 10, 0); err == nil {
		t.Fatal("data query error must surface")
	}
	m3 := newMock(t)
	m3.ExpectQuery(`FROM traffic_event`).WithArgs(10, 0).WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("e1"))
	if _, _, err := New(m3, nil).ListMatrixAuditEvents(context.Background(), nil, nil, 10, 0); err == nil {
		t.Fatal("scan error must surface")
	}
	m4 := newMock(t)
	m4.ExpectQuery(`FROM traffic_event`).WithArgs(10, 0).WillReturnRows(pgxmock.NewRows(matrixCols).AddRow(matrixRow()...))
	m4.ExpectQuery(`SELECT COUNT`).WillReturnError(errors.New("boom"))
	if _, _, err := New(m4, nil).ListMatrixAuditEvents(context.Background(), nil, nil, 10, 0); err == nil {
		t.Fatal("count error must surface")
	}
}

// ---- GetMatrixAuditEvent ----

func TestGetMatrixAuditEvent(t *testing.T) {
	m := newMock(t)
	s := New(m, nil)
	cols := make([]string, 22)
	rmReq := json.RawMessage(`{"req":1}`)
	rmResp := json.RawMessage(`{"resp":1}`)
	mk := func(body bool) []any {
		v := make([]any, 22)
		for i := range v {
			v[i] = nil
		}
		v[0] = "e1" // eid (string); complianceTags (idx 16) left nil
		if body {
			// request/response body are *json.RawMessage scanned via & → feed *json.RawMessage.
			v[20] = &rmReq
			v[21] = &rmResp
		}
		return v
	}
	for i := range cols {
		cols[i] = "c"
	}
	m.ExpectQuery(`LEFT JOIN traffic_event_payload`).WithArgs("e1").WillReturnRows(pgxmock.NewRows(cols).AddRow(mk(true)...))
	res, err := s.GetMatrixAuditEvent(context.Background(), "e1")
	if err != nil || res["id"] != "e1" || res["requestBody"] == nil || res["responseBody"] == nil {
		t.Fatalf("GetMatrixAuditEvent: %+v %v", res, err)
	}
	m2 := newMock(t)
	m2.ExpectQuery(`LEFT JOIN traffic_event_payload`).WithArgs("e2").WillReturnRows(pgxmock.NewRows(cols).AddRow(mk(false)...))
	res2, err := New(m2, nil).GetMatrixAuditEvent(context.Background(), "e2")
	if err != nil {
		t.Fatalf("GetMatrixAuditEvent nil bodies: %v", err)
	}
	if _, ok := res2["requestBody"]; ok {
		t.Fatal("nil requestBody must be absent")
	}
	m3 := newMock(t)
	m3.ExpectQuery(`LEFT JOIN traffic_event_payload`).WillReturnError(errors.New("boom"))
	if _, err := New(m3, nil).GetMatrixAuditEvent(context.Background(), "x"); err == nil {
		t.Fatal("error must surface")
	}
}

// ---- ListComplianceAuditEvents ----

var compCols = []string{"id", "source", "tx", "sip", "host", "method", "path", "sc", "hd", "hrc", "bump", "lat", "ts", "tags"}

func compRow() []any {
	return []any{"e1", "agent", "tx1", "1.2.3.4", "api.x", sp("POST"), sp("/v1"), ip(200), sp("APPROVE"), sp("ok"), sp("BUMP_SUCCESS"), ip(50), tNow, []string{"pii"}}
}

func TestListComplianceAuditEvents(t *testing.T) {
	m := newMock(t)
	s := New(m, nil)
	p := ComplianceAuditParams{Source: "agent", HookDecision: "APPROVE", ComplianceTags: []string{"pii"},
		SourceIP: "1.2", TargetHost: "api", Start: &tStart, End: &tEnd, Limit: 10, Offset: 0}
	m.ExpectQuery(`FROM traffic_event`).WithArgs(anyArgs(9)...).WillReturnRows(pgxmock.NewRows(compCols).AddRow(compRow()...))
	m.ExpectQuery(`SELECT COUNT`).WithArgs(anyArgs(7)...).WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	res, total, err := s.ListComplianceAuditEvents(context.Background(), p)
	if err != nil || total != 1 || len(res) != 1 || res[0].Source != "agent" {
		t.Fatalf("ListComplianceAuditEvents: %+v total=%d err=%v", res, total, err)
	}
	m2 := newMock(t)
	m2.ExpectQuery(`FROM traffic_event`).WillReturnError(errors.New("boom"))
	if _, _, err := New(m2, nil).ListComplianceAuditEvents(context.Background(), ComplianceAuditParams{}); err == nil {
		t.Fatal("data query error must surface")
	}
	m3 := newMock(t)
	m3.ExpectQuery(`FROM traffic_event`).WithArgs(0, 0).WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("e1"))
	if _, _, err := New(m3, nil).ListComplianceAuditEvents(context.Background(), ComplianceAuditParams{}); err == nil {
		t.Fatal("scan error must surface")
	}
	m4 := newMock(t)
	m4.ExpectQuery(`FROM traffic_event`).WithArgs(0, 0).WillReturnRows(pgxmock.NewRows(compCols).AddRow(compRow()...))
	m4.ExpectQuery(`SELECT COUNT`).WillReturnError(errors.New("boom"))
	if _, _, err := New(m4, nil).ListComplianceAuditEvents(context.Background(), ComplianceAuditParams{}); err == nil {
		t.Fatal("count error must surface")
	}
}

// ---- GetTrinityStats ----

func trinityRow(src string) []any {
	return []any{src, 100, 70, 5, 10, 5, 10, 60, 20, 15, 5}
}

var trinityCols = []string{"source", "total", "approve", "modify", "rsoft", "rhard", "abstain", "bsucc", "bfail", "bexempt", "bdisabled"}

func TestGetTrinityStats(t *testing.T) {
	m := newMock(t)
	s := New(m, nil)
	m.ExpectQuery(`GROUP BY source`).WithArgs(tStart, tEnd).WillReturnRows(
		pgxmock.NewRows(trinityCols).AddRow(trinityRow("ai-gateway")...).AddRow(trinityRow("compliance-proxy")...).AddRow(trinityRow("agent")...))
	st, err := s.GetTrinityStats(context.Background(), tStart, tEnd)
	if err != nil || st.AIGateway.TotalEvents != 100 || st.ComplianceProxy.BumpBreakdown == nil || st.Agent.CoveragePct == nil {
		t.Fatalf("GetTrinityStats: %+v %v", st, err)
	}
	m2 := newMock(t)
	m2.ExpectQuery(`GROUP BY source`).WillReturnError(errors.New("boom"))
	if _, err := New(m2, nil).GetTrinityStats(context.Background(), tStart, tEnd); err == nil {
		t.Fatal("query error must surface")
	}
	m3 := newMock(t)
	// "BAD" string into the int `total` destination → scan error.
	m3.ExpectQuery(`GROUP BY source`).WillReturnRows(pgxmock.NewRows(trinityCols).AddRow("ai-gateway", "BAD", 0, 0, 0, 0, 0, 0, 0, 0, 0))
	if _, err := New(m3, nil).GetTrinityStats(context.Background(), tStart, tEnd); err == nil {
		t.Fatal("scan error must surface")
	}
	m4 := newMock(t)
	m4.ExpectQuery(`GROUP BY source`).WillReturnRows(pgxmock.NewRows(trinityCols).AddRow(trinityRow("ai-gateway")...).CloseError(errors.New("conn reset")))
	if _, err := New(m4, nil).GetTrinityStats(context.Background(), tStart, tEnd); err == nil {
		t.Fatal("iterate error must surface")
	}
}

// ---- GetComplianceCoverage ----

func TestGetComplianceCoverage_Fallback(t *testing.T) {
	m := newMock(t)
	s := New(m, nil)
	m.ExpectQuery(`GROUP BY bump_status`).WithArgs(tStart, tEnd).
		WillReturnRows(pgxmock.NewRows([]string{"status", "count"}).AddRow("BUMP_SUCCESS", 80).AddRow("BUMP_FAILED_PASSTHROUGH", 20))
	cov, err := s.GetComplianceCoverage(context.Background(), tStart, tEnd)
	if err != nil || cov.CoveragePct == 0 || cov.Breakdown["BUMP_SUCCESS"] != 80 {
		t.Fatalf("coverage fallback: %+v %v", cov, err)
	}
	m2 := newMock(t)
	m2.ExpectQuery(`GROUP BY bump_status`).WillReturnError(errors.New("boom"))
	if _, err := New(m2, nil).GetComplianceCoverage(context.Background(), tStart, tEnd); err == nil {
		t.Fatal("query error must surface")
	}
	m3 := newMock(t)
	// "BAD" string into the int `count` destination → scan error.
	m3.ExpectQuery(`GROUP BY bump_status`).WillReturnRows(pgxmock.NewRows([]string{"status", "count"}).AddRow("BUMP_SUCCESS", "BAD"))
	if _, err := New(m3, nil).GetComplianceCoverage(context.Background(), tStart, tEnd); err == nil {
		t.Fatal("scan error must surface")
	}
	m4 := newMock(t)
	m4.ExpectQuery(`GROUP BY bump_status`).WillReturnRows(pgxmock.NewRows([]string{"status", "count"}).AddRow("BUMP_SUCCESS", 1).CloseError(errors.New("conn reset")))
	if _, err := New(m4, nil).GetComplianceCoverage(context.Background(), tStart, tEnd); err == nil {
		t.Fatal("iterate error must surface")
	}
}

func TestGetComplianceCoverage_Rollup(t *testing.T) {
	s, _, mp := metricsBackedStore(t)
	primeCascade(mp, 8) // proxy: 5 metrics + subdim
	primeCascade(mp, 8) // agent
	cov, err := s.GetComplianceCoverage(context.Background(), tStart, tEnd)
	if err != nil || cov == nil {
		t.Fatalf("coverage rollup: %+v %v", cov, err)
	}
}

func TestGetComplianceCoverage_RollupEmptyFallsBack(t *testing.T) {
	// metrics present but both cascades return zero rows → continue → rollupOK
	// stays false → direct traffic_event fallback on the main pool.
	s, pool, mp := metricsBackedStore(t)
	primeCascadeEmpty(mp, 8)
	primeCascadeEmpty(mp, 8)
	pool.ExpectQuery(`GROUP BY bump_status`).WithArgs(tStart, tEnd).
		WillReturnRows(pgxmock.NewRows([]string{"status", "count"}).AddRow("BUMP_SUCCESS", 5))
	cov, err := s.GetComplianceCoverage(context.Background(), tStart, tEnd)
	if err != nil || cov.Breakdown["BUMP_SUCCESS"] != 5 {
		t.Fatalf("coverage rollup-empty fallback: %+v %v", cov, err)
	}
}

// ---- GetHookHealth ----

func TestGetHookHealth_Fallback(t *testing.T) {
	decCols := []string{"t", "a", "d", "e", "u"}
	latCols := []string{"p50", "p95", "p99"}
	m := newMock(t)
	s := New(m, nil)
	m.ExpectQuery(`COUNT\(\*\) FILTER`).WithArgs(tStart, tEnd).WillReturnRows(pgxmock.NewRows(decCols).AddRow(100, 70, 20, 5, 5))
	m.ExpectQuery(`percentile_cont`).WithArgs(tStart, tEnd).WillReturnRows(pgxmock.NewRows(latCols).AddRow(fp(10), fp(20), fp(30)))
	m.ExpectQuery(`request_hook_reason_code, COUNT`).WithArgs(tStart, tEnd).WillReturnRows(pgxmock.NewRows([]string{"rc", "cnt"}).AddRow("pii", 5))
	hh, err := s.GetHookHealth(context.Background(), tStart, tEnd)
	if err != nil || hh.Total != 100 || len(hh.TopReasonCodes) != 1 || hh.LatencyP50 == nil {
		t.Fatalf("hook health fallback: %+v %v", hh, err)
	}
	m2 := newMock(t)
	m2.ExpectQuery(`COUNT\(\*\) FILTER`).WillReturnError(errors.New("boom"))
	if _, err := New(m2, nil).GetHookHealth(context.Background(), tStart, tEnd); err == nil {
		t.Fatal("decisions error must surface")
	}
	m3 := newMock(t)
	m3.ExpectQuery(`COUNT\(\*\) FILTER`).WithArgs(tStart, tEnd).WillReturnRows(pgxmock.NewRows(decCols).AddRow(1, 1, 0, 0, 0))
	m3.ExpectQuery(`percentile_cont`).WillReturnError(errors.New("boom"))
	if _, err := New(m3, nil).GetHookHealth(context.Background(), tStart, tEnd); err == nil {
		t.Fatal("latency error must surface")
	}
	m4 := newMock(t)
	m4.ExpectQuery(`COUNT\(\*\) FILTER`).WithArgs(tStart, tEnd).WillReturnRows(pgxmock.NewRows(decCols).AddRow(1, 1, 0, 0, 0))
	m4.ExpectQuery(`percentile_cont`).WithArgs(tStart, tEnd).WillReturnRows(pgxmock.NewRows(latCols).AddRow(nil, nil, nil))
	m4.ExpectQuery(`request_hook_reason_code, COUNT`).WillReturnError(errors.New("boom"))
	if _, err := New(m4, nil).GetHookHealth(context.Background(), tStart, tEnd); err == nil {
		t.Fatal("reason codes error must surface")
	}
	m5 := newMock(t)
	m5.ExpectQuery(`COUNT\(\*\) FILTER`).WithArgs(tStart, tEnd).WillReturnRows(pgxmock.NewRows(decCols).AddRow(1, 1, 0, 0, 0))
	m5.ExpectQuery(`percentile_cont`).WithArgs(tStart, tEnd).WillReturnRows(pgxmock.NewRows(latCols).AddRow(nil, nil, nil))
	m5.ExpectQuery(`request_hook_reason_code, COUNT`).WithArgs(tStart, tEnd).WillReturnRows(pgxmock.NewRows([]string{"rc"}).AddRow("pii"))
	if _, err := New(m5, nil).GetHookHealth(context.Background(), tStart, tEnd); err == nil {
		t.Fatal("reason codes scan error must surface")
	}
	m6 := newMock(t)
	m6.ExpectQuery(`COUNT\(\*\) FILTER`).WithArgs(tStart, tEnd).WillReturnRows(pgxmock.NewRows(decCols).AddRow(1, 1, 0, 0, 0))
	m6.ExpectQuery(`percentile_cont`).WithArgs(tStart, tEnd).WillReturnRows(pgxmock.NewRows(latCols).AddRow(nil, nil, nil))
	m6.ExpectQuery(`request_hook_reason_code, COUNT`).WithArgs(tStart, tEnd).WillReturnRows(pgxmock.NewRows([]string{"rc", "cnt"}).AddRow("pii", 1).CloseError(errors.New("conn reset")))
	if _, err := New(m6, nil).GetHookHealth(context.Background(), tStart, tEnd); err == nil {
		t.Fatal("reason codes iterate error must surface")
	}
}

func TestGetHookHealth_Rollup(t *testing.T) {
	s, pool, mp := metricsBackedStore(t)
	primeCascade(mp, 7) // 5 metrics, no subdim
	pool.ExpectQuery(`request_hook_reason_code, COUNT`).WithArgs(tStart, tEnd).WillReturnRows(pgxmock.NewRows([]string{"rc", "cnt"}).AddRow("pii", 5))
	hh, err := s.GetHookHealth(context.Background(), tStart, tEnd)
	if err != nil || hh == nil {
		t.Fatalf("hook health rollup: %+v %v", hh, err)
	}
}

// ---- GetComplianceDashboard ----

func dashTrinity(m pgxmock.PgxPoolIface) {
	m.ExpectQuery(`GROUP BY source`).WithArgs(tStart, tEnd).WillReturnRows(pgxmock.NewRows(trinityCols).AddRow(trinityRow("ai-gateway")...))
}

func dashTail(m pgxmock.PgxPoolIface) {
	// Dashboard reason codes use COALESCE(...); then 3× queryTopBlocked (GROUP BY 1).
	m.ExpectQuery(`COALESCE\(request_hook_reason_code`).WithArgs(tStart, tEnd).WillReturnRows(pgxmock.NewRows([]string{"rc", "cnt"}).AddRow("pii", 5))
	for range 3 {
		m.ExpectQuery(`GROUP BY 1`).WithArgs(tStart, tEnd).WillReturnRows(pgxmock.NewRows([]string{"label", "cnt"}).AddRow("api.x", 3))
	}
}

func TestGetComplianceDashboard_Fallback(t *testing.T) {
	m := newMock(t)
	s := New(m, nil)
	dashTrinity(m)
	// fallbackHookHealth QueryRow (9 cols).
	m.ExpectQuery(`percentile_cont\(0.50\)`).WithArgs(tStart, tEnd).WillReturnRows(
		pgxmock.NewRows([]string{"t", "a", "d", "mo", "ab", "u", "p50", "p95", "p99"}).AddRow(100, 70, 20, 5, 3, 2, fp(10), fp(20), fp(30)))
	dashTail(m)
	dash, err := s.GetComplianceDashboard(context.Background(), tStart, tEnd)
	if err != nil || dash.KPIs.TotalRequests != 100 || len(dash.TopBlocked.ByTarget) != 1 {
		t.Fatalf("dashboard fallback: %+v %v", dash, err)
	}
	m2 := newMock(t)
	m2.ExpectQuery(`GROUP BY source`).WillReturnError(errors.New("boom"))
	if _, err := New(m2, nil).GetComplianceDashboard(context.Background(), tStart, tEnd); err == nil {
		t.Fatal("trinity error must surface")
	}
}

func TestGetComplianceDashboard_Rollup(t *testing.T) {
	s, pool, mp := metricsBackedStore(t)
	dashTrinity(pool)
	primeCascade(mp, 7) // TLS proxy (4 metrics + subdim)
	primeCascade(mp, 7) // TLS agent
	primeCascade(mp, 7) // hook health (5 metrics)
	dashTail(pool)
	dash, err := s.GetComplianceDashboard(context.Background(), tStart, tEnd)
	if err != nil || dash == nil {
		t.Fatalf("dashboard rollup: %+v %v", dash, err)
	}
}

func TestGetComplianceDashboard_RollupEmpty(t *testing.T) {
	// metrics present but every cascade empty: TLS proxy+agent → continue;
	// hook-health → else → fallbackHookHealth on the main pool. Also drives a
	// failing top-blocked query (queryTopBlocked Query-error arm).
	s, pool, mp := metricsBackedStore(t)
	dashTrinity(pool)
	primeCascadeEmpty(mp, 7) // TLS proxy → continue
	primeCascadeEmpty(mp, 7) // TLS agent → continue
	primeCascadeEmpty(mp, 7) // hook health → else (fallback)
	pool.ExpectQuery(`percentile_cont\(0.50\)`).WithArgs(tStart, tEnd).WillReturnRows(
		pgxmock.NewRows([]string{"t", "a", "d", "mo", "ab", "u", "p50", "p95", "p99"}).AddRow(10, 7, 2, 1, 0, 0, fp(1), fp(2), fp(3)))
	pool.ExpectQuery(`COALESCE\(request_hook_reason_code`).WithArgs(tStart, tEnd).WillReturnRows(pgxmock.NewRows([]string{"rc", "cnt"}).AddRow("pii", 1))
	// First top-blocked query errors (Query-error arm); remaining two succeed.
	pool.ExpectQuery(`GROUP BY 1`).WithArgs(tStart, tEnd).WillReturnError(errors.New("boom"))
	pool.ExpectQuery(`GROUP BY 1`).WithArgs(tStart, tEnd).WillReturnRows(pgxmock.NewRows([]string{"label", "cnt"}).AddRow("rc", 1))
	pool.ExpectQuery(`GROUP BY 1`).WithArgs(tStart, tEnd).WillReturnRows(pgxmock.NewRows([]string{"label", "cnt"}).AddRow("1.2.3.4", 1))
	dash, err := s.GetComplianceDashboard(context.Background(), tStart, tEnd)
	if err != nil || dash == nil || len(dash.TopBlocked.ByTarget) != 0 {
		t.Fatalf("dashboard rollup-empty: %+v %v", dash, err)
	}
}

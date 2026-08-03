package trafficstore

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/pashagolub/pgxmock/v4"

	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

var tailCols = []string{"error_code", "status_range", "provider", "model", "bin_ts", "cnt", "first_seen", "last_seen"}

func classDimKey(code, rng, provider, model string) string {
	return metrics.DimensionErrorClass + "=" + metrics.BuildErrorClassValue(
		metrics.ErrorClass{ErrorCode: code, StatusRange: rng, Provider: provider, Model: model})
}

func seenMeta(t *testing.T, first, last time.Time) []byte {
	t.Helper()
	b, err := json.Marshal(metrics.TimestampMeta{
		FirstSeen: first.UTC().Format(time.RFC3339),
		LastSeen:  last.UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("marshal seen meta: %v", err)
	}
	return b
}

func expectWatermark(m pgxmock.PgxPoolIface, job string, wm time.Time) {
	rows := pgxmock.NewRows([]string{"watermark"})
	if !wm.IsZero() {
		rows.AddRow(wm)
	}
	m.ExpectQuery(`SELECT "watermark" FROM "rollup_watermark"`).
		WithArgs(job).
		WillReturnRows(rows)
}

// expectTierFloor arms BOTH coverage probes for the 5m tier (retention via
// request_count, series birth via the error-class count) with a floor early
// enough to cover any test window.
func expectTierFloor(m pgxmock.PgxPoolIface, floor time.Time) {
	m.ExpectQuery(`SELECT MIN\("bucketStart"\) FROM metric_rollup_5m`).
		WithArgs(metrics.MetricRequestCount).
		WillReturnRows(pgxmock.NewRows([]string{"min"}).AddRow(&floor))
	m.ExpectQuery(`SELECT MIN\("bucketStart"\) FROM metric_rollup_5m`).
		WithArgs(metrics.MetricTrafficErrorClassCount).
		WillReturnRows(pgxmock.NewRows([]string{"min"}).AddRow(&floor))
}

// segCountCols/segSeenCols mirror the split segment queries: counts collapse
// per (class, bin) in SQL; seen rows come back raw per source bucket.
var segCountCols = []string{"dimensionKey", "bin_ts", "total"}

var segSeenCols = []string{"dimensionKey", "metadata"}

func ptrTime(t time.Time) *time.Time { return &t }

// TestPlanErrorClassSegments pins the tier plan: each bin width reads its
// matching tier up to that tier's merge watermark, finer tiers fill the gap,
// and everything clamps into [from, to) without overlap.
func TestPlanErrorClassSegments(t *testing.T) {
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	wm1d := time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)
	wm1h := time.Date(2026, 7, 16, 23, 0, 0, 0, time.UTC)
	sealed := time.Date(2026, 7, 16, 23, 55, 0, 0, time.UTC)

	segs := planErrorClassSegments(from, to, sealed, wm1h, wm1d, 24*time.Hour)
	want := []errorClassSegment{
		{"metric_rollup_1d", from, wm1d},
		{"metric_rollup_1h", wm1d, wm1h},
		{"metric_rollup_5m", wm1h, sealed},
	}
	for i, w := range want {
		if segs[i] != w {
			t.Errorf("1d plan seg %d = %+v, want %+v", i, segs[i], w)
		}
	}

	// 1h bins: no 1d tier; 5m fills behind merge-1h.
	segs = planErrorClassSegments(from, to, sealed, wm1h, wm1d, time.Hour)
	if segs[0] != (errorClassSegment{"metric_rollup_1h", from, wm1h}) ||
		segs[1] != (errorClassSegment{"metric_rollup_5m", wm1h, sealed}) {
		t.Errorf("1h plan = %+v", segs)
	}

	// 5m bins: single 5m segment to the sealed boundary.
	segs = planErrorClassSegments(from, to, sealed, wm1h, wm1d, 5*time.Minute)
	if len(segs) != 1 || segs[0] != (errorClassSegment{"metric_rollup_5m", from, sealed}) {
		t.Errorf("5m plan = %+v", segs)
	}

	// A merge watermark AHEAD of the sealed boundary or the window end must
	// clamp; a watermark behind `from` must not create a negative window.
	segs = planErrorClassSegments(from, to, to.Add(time.Hour), to.Add(2*time.Hour), from.Add(-time.Hour), time.Hour)
	if segs[0] != (errorClassSegment{"metric_rollup_1h", from, to}) ||
		!segs[1].from.Equal(to) || !segs[1].to.Equal(to) {
		t.Errorf("clamped plan = %+v", segs)
	}
}

func TestDomainSubDims(t *testing.T) {
	got := domainSubDims([]string{"ai-gateway", "compliance-proxy", "agent", "not-a-source"})
	want := map[string]struct{}{"source=vk": {}, "source=proxy": {}, "source=agent": {}}
	if len(got) != len(want) {
		t.Fatalf("subDims = %v", got)
	}
	for _, sd := range got {
		if _, ok := want[sd]; !ok {
			t.Errorf("unexpected subDim %q", sd)
		}
	}
}

// TestListTrafficErrorGroupsScaled_FallsBackWithoutWatermark: a deployment
// whose rollup jobs never ran must transparently serve the full direct scan
// and label the response "direct".
func TestListTrafficErrorGroupsScaled_FallsBackWithoutWatermark(t *testing.T) {
	s, m := newMock(t)
	expectWatermark(m, "rollup-5m", time.Time{})

	first := tNow.Add(-2 * time.Hour)
	m.ExpectQuery(`GROUP BY 1, 2, 3, 4\s+ORDER BY cnt DESC`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(errGroupCols).
			AddRow("invalid_request", "4xx", "openai", "gpt-4o-mini", "bad temp", 3, 1, first, tNow))
	m.ExpectQuery(`date_bin\(\$4, sub\.ts`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(errBucketCols).
			AddRow("invalid_request", "4xx", "openai", "gpt-4o-mini", first, 3))

	got, src, err := s.ListTrafficErrorGroupsScaled(context.Background(), errGroupsParams())
	if err != nil {
		t.Fatalf("Scaled: %v", err)
	}
	if src != ErrorGroupsSourceDirect {
		t.Errorf("dataSource = %q, want direct", src)
	}
	if len(got) != 1 || got[0].Count != 3 {
		t.Errorf("groups = %+v", got)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestListTrafficErrorGroupsScaled_RollupPlusTail is the load-bearing parity
// test: rollup segments supply sealed history, the direct tail supplies the
// unsealed window, and the merged class carries summed counts, min/max
// first/last seen, per-bin buckets from both legs, plus the point-query
// enrichments (sample reason, affected end-users).
func TestListTrafficErrorGroupsScaled_RollupPlusTail(t *testing.T) {
	s, m := newMock(t)
	p := errGroupsParams() // 24h window, 5m bins, sources ai-gateway → source=vk

	wm := p.To.Add(-10 * time.Minute).Truncate(5 * time.Minute)
	sealed := wm.Add(5 * time.Minute)
	expectWatermark(m, "rollup-5m", wm)
	expectWatermark(m, "merge-1h", time.Time{})
	expectWatermark(m, "merge-1d", time.Time{})

	expectTierFloor(m, p.From.Add(-time.Hour))

	dim := classDimKey("RATE_LIMITED", "4xx", "openai", "gpt-4o-mini")
	bin1 := p.From.Add(time.Hour).Truncate(5 * time.Minute)
	rollupFirst := bin1.Add(10 * time.Second)
	rollupLast := bin1.Add(4 * time.Minute)
	m.ExpectQuery(`FROM metric_rollup_5m`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			metrics.MetricTrafficErrorClassCount, pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(segCountCols).
			AddRow(dim, bin1, float64(5)))
	m.ExpectQuery(`FROM metric_rollup_5m`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(),
			metrics.MetricTrafficErrorClassSeen, pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(segSeenCols).
			AddRow(dim, seenMeta(t, rollupFirst, rollupLast)))

	// Direct tail: same class gets 2 more rows in the unsealed window.
	tailBin := sealed.Truncate(5 * time.Minute)
	tailFirst := sealed.Add(30 * time.Second)
	tailLast := sealed.Add(90 * time.Second)
	m.ExpectQuery(`ORDER BY cnt DESC\s+LIMIT 5000`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(tailCols).
			AddRow("RATE_LIMITED", "4xx", "openai", "gpt-4o-mini", tailBin, 2, tailFirst, tailLast))

	// Enrichments for the single displayed class.
	m.ExpectQuery(`SELECT COALESCE\(a\.error_reason, ''\)`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			"RATE_LIMITED", "4xx", "openai", "gpt-4o-mini").
		WillReturnRows(pgxmock.NewRows([]string{"reason"}).AddRow("rate limit exceeded"))
	m.ExpectQuery(`COUNT\(DISTINCT a\.end_user_id\)`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			"RATE_LIMITED", "4xx", "openai", "gpt-4o-mini").
		WillReturnRows(pgxmock.NewRows([]string{"rows", "n"}).AddRow(7, 3))

	got, src, err := s.ListTrafficErrorGroupsScaled(context.Background(), p)
	if err != nil {
		t.Fatalf("Scaled: %v", err)
	}
	if src != ErrorGroupsSourceRollup {
		t.Errorf("dataSource = %q, want rollup", src)
	}
	if len(got) != 1 {
		t.Fatalf("groups = %d, want 1", len(got))
	}
	g := got[0]
	if g.Count != 7 {
		t.Errorf("count = %d, want 5 rollup + 2 tail = 7", g.Count)
	}
	if !g.FirstSeen.Equal(rollupFirst) || !g.LastSeen.Equal(tailLast) {
		t.Errorf("seen = %v..%v, want %v..%v", g.FirstSeen, g.LastSeen, rollupFirst, tailLast)
	}
	if len(g.Buckets) != 2 || g.Buckets[0].Count != 5 || g.Buckets[1].Count != 2 {
		t.Errorf("buckets = %+v", g.Buckets)
	}
	if g.SampleReason != "rate limit exceeded" {
		t.Errorf("sampleReason = %q", g.SampleReason)
	}
	if g.AffectedEndUsers == nil || *g.AffectedEndUsers != 3 {
		t.Errorf("affectedEndUsers = %v, want 3", g.AffectedEndUsers)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestListTrafficErrorGroupsScaled_ThresholdAndOverflow: a class whose count
// exceeds the affordability threshold gets a NULL affectedEndUsers (no
// distinct scan is even issued), and the overflow sentinel class skips both
// enrichment queries entirely.
func TestListTrafficErrorGroupsScaled_ThresholdAndOverflow(t *testing.T) {
	s, m := newMock(t)
	p := errGroupsParams()

	wm := p.To.Truncate(5 * time.Minute) // sealed beyond To → no tail query
	expectWatermark(m, "rollup-5m", wm)
	expectWatermark(m, "merge-1h", time.Time{})
	expectWatermark(m, "merge-1d", time.Time{})

	expectTierFloor(m, p.From.Add(-time.Hour))

	bigDim := classDimKey("PROVIDER_ERROR", "5xx", "openai", "gpt-4o")
	ovfDim := classDimKey(metrics.ErrorClassOverflowCode, "4xx", "", "")
	bin := p.From.Truncate(5 * time.Minute)
	m.ExpectQuery(`FROM metric_rollup_5m`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			metrics.MetricTrafficErrorClassCount, pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(segCountCols).
			AddRow(bigDim, bin, float64(affectedEndUsersMaxRows+1)).
			AddRow(ovfDim, bin, float64(40)).
			// A foreign/corrupt dimension row must be skipped, not misfiled.
			AddRow("error_class=not-a-class", bin, float64(9)))
	m.ExpectQuery(`FROM metric_rollup_5m`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(),
			metrics.MetricTrafficErrorClassSeen, pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(segSeenCols))

	// Only the big class gets a sample-reason query (returns no rows →
	// degrades to empty); its distinct scan is skipped by the threshold.
	m.ExpectQuery(`SELECT COALESCE\(a\.error_reason, ''\)`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			"PROVIDER_ERROR", "5xx", "openai", "gpt-4o").
		WillReturnRows(pgxmock.NewRows([]string{"reason"}))

	got, src, err := s.ListTrafficErrorGroupsScaled(context.Background(), p)
	if err != nil {
		t.Fatalf("Scaled: %v", err)
	}
	if src != ErrorGroupsSourceRollup {
		t.Errorf("dataSource = %q", src)
	}
	if len(got) != 2 {
		t.Fatalf("groups = %d, want 2 (corrupt dim row skipped)", len(got))
	}
	if got[0].AffectedEndUsers != nil {
		t.Errorf("above-threshold affectedEndUsers = %v, want nil", got[0].AffectedEndUsers)
	}
	if got[0].SampleReason != "" {
		t.Errorf("sampleReason = %q, want empty degradation", got[0].SampleReason)
	}
	if got[1].ErrorCode != metrics.ErrorClassOverflowCode || got[1].AffectedEndUsers != nil || got[1].SampleReason != "" {
		t.Errorf("overflow group = %+v", got[1])
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestListTrafficErrorGroupsScaled_Validation pins the input guards.
func TestListTrafficErrorGroupsScaled_Validation(t *testing.T) {
	s, _ := newMock(t)
	if _, _, err := s.ListTrafficErrorGroupsScaled(context.Background(), TrafficErrorGroupsParams{
		From: tNow, To: tNow.Add(-time.Hour), BucketInterval: time.Minute,
	}); err == nil {
		t.Error("inverted window accepted")
	}
	if _, _, err := s.ListTrafficErrorGroupsScaled(context.Background(), TrafficErrorGroupsParams{
		From: tNow.Add(-time.Hour), To: tNow,
	}); err == nil {
		t.Error("zero bucket interval accepted")
	}
}

// TestListTrafficErrorGroupsScaled_ErrorPaths: a failing segment query, a
// failing tail query, a scan-shape mismatch, and a failing distinct count
// must each surface as errors (not silent partial data); corrupt seen
// metadata must be skipped without failing.
func TestListTrafficErrorGroupsScaled_ErrorPaths(t *testing.T) {
	p := errGroupsParams()
	wm := p.To.Truncate(5 * time.Minute)
	floor := p.From.Add(-time.Hour)

	// Segment count-query error.
	s, m := newMock(t)
	expectWatermark(m, "rollup-5m", wm)
	expectWatermark(m, "merge-1h", time.Time{})
	expectWatermark(m, "merge-1d", time.Time{})
	expectTierFloor(m, floor)
	m.ExpectQuery(`FROM metric_rollup_5m`).WithArgs(anyArgs(6)...).WillReturnError(errors.New("segment down"))
	if _, _, err := s.ListTrafficErrorGroupsScaled(context.Background(), p); err == nil {
		t.Error("segment query error swallowed")
	}

	// Segment scan-shape mismatch.
	s2, m2 := newMock(t)
	expectWatermark(m2, "rollup-5m", wm)
	expectWatermark(m2, "merge-1h", time.Time{})
	expectWatermark(m2, "merge-1d", time.Time{})
	expectTierFloor(m2, floor)
	m2.ExpectQuery(`FROM metric_rollup_5m`).WithArgs(anyArgs(6)...).WillReturnRows(
		pgxmock.NewRows(segCountCols).AddRow("d", "not-a-time", float64(1)))
	if _, _, err := s2.ListTrafficErrorGroupsScaled(context.Background(), p); err == nil {
		t.Error("segment scan error swallowed")
	}

	// Corrupt seen metadata → skipped; then the tail query errors.
	s3, m3 := newMock(t)
	wmMid := p.To.Add(-10 * time.Minute).Truncate(5 * time.Minute)
	expectWatermark(m3, "rollup-5m", wmMid)
	expectWatermark(m3, "merge-1h", time.Time{})
	expectWatermark(m3, "merge-1d", time.Time{})
	expectTierFloor(m3, floor)
	dim := classDimKey("AUTH_INVALID", "4xx", "", "")
	m3.ExpectQuery(`FROM metric_rollup_5m`).WithArgs(anyArgs(6)...).WillReturnRows(
		pgxmock.NewRows(segCountCols).AddRow(dim, tNow, float64(1)))
	m3.ExpectQuery(`FROM metric_rollup_5m`).WithArgs(anyArgs(5)...).WillReturnRows(
		pgxmock.NewRows(segSeenCols).AddRow(dim, []byte("{not json")))
	m3.ExpectQuery(`ORDER BY cnt DESC\s+LIMIT 5000`).WithArgs(anyArgs(4)...).WillReturnError(errors.New("tail down"))
	if _, _, err := s3.ListTrafficErrorGroupsScaled(context.Background(), p); err == nil {
		t.Error("tail query error swallowed")
	}

	// Tail scan-shape mismatch.
	s4, m4 := newMock(t)
	expectWatermark(m4, "rollup-5m", wmMid)
	expectWatermark(m4, "merge-1h", time.Time{})
	expectWatermark(m4, "merge-1d", time.Time{})
	expectTierFloor(m4, floor)
	m4.ExpectQuery(`FROM metric_rollup_5m`).WithArgs(anyArgs(6)...).WillReturnRows(pgxmock.NewRows(segCountCols))
	m4.ExpectQuery(`FROM metric_rollup_5m`).WithArgs(anyArgs(5)...).WillReturnRows(pgxmock.NewRows(segSeenCols))
	m4.ExpectQuery(`ORDER BY cnt DESC\s+LIMIT 5000`).WithArgs(anyArgs(4)...).WillReturnRows(
		pgxmock.NewRows(tailCols).AddRow("c", "4xx", "p", "m", "not-a-time", 1, tNow, tNow))
	if _, _, err := s4.ListTrafficErrorGroupsScaled(context.Background(), p); err == nil {
		t.Error("tail scan error swallowed")
	}

	// Distinct-count failure surfaces (unlike the sample reason, a wrong
	// affected number would be presented as exact — fail loud instead).
	s5, m5 := newMock(t)
	expectWatermark(m5, "rollup-5m", wm)
	expectWatermark(m5, "merge-1h", time.Time{})
	expectWatermark(m5, "merge-1d", time.Time{})
	expectTierFloor(m5, floor)
	m5.ExpectQuery(`FROM metric_rollup_5m`).WithArgs(anyArgs(6)...).WillReturnRows(
		pgxmock.NewRows(segCountCols).AddRow(dim, tNow, float64(2)))
	m5.ExpectQuery(`FROM metric_rollup_5m`).WithArgs(anyArgs(5)...).WillReturnRows(pgxmock.NewRows(segSeenCols))
	m5.ExpectQuery(`SELECT COALESCE\(a\.error_reason, ''\)`).WithArgs(anyArgs(7)...).WillReturnError(errors.New("reason down"))
	m5.ExpectQuery(`COUNT\(DISTINCT a\.end_user_id\)`).WithArgs(anyArgs(7)...).WillReturnError(errors.New("distinct down"))
	if _, _, err := s5.ListTrafficErrorGroupsScaled(context.Background(), p); err == nil {
		t.Error("distinct-count error swallowed")
	}
}

// TestListTrafficErrorGroupsScaled_RetentionFallback: a window whose start
// predates the finest tier's earliest surviving bucket must fall back to the
// full direct scan (the rollup cannot cover it) — the honest alternative to
// silently under-reporting a forensic window.
func TestListTrafficErrorGroupsScaled_RetentionFallback(t *testing.T) {
	s, m := newMock(t)
	p := errGroupsParams()
	wm := p.To.Truncate(5 * time.Minute)
	expectWatermark(m, "rollup-5m", wm)
	expectWatermark(m, "merge-1h", time.Time{})
	expectWatermark(m, "merge-1d", time.Time{})
	// Tier floor AFTER the window start → coverage gap (first probe already
	// falls back; the series-birth probe is never reached).
	m.ExpectQuery(`SELECT MIN\("bucketStart"\) FROM metric_rollup_5m`).
		WithArgs(metrics.MetricRequestCount).
		WillReturnRows(pgxmock.NewRows([]string{"min"}).AddRow(ptrTime(p.From.Add(time.Hour))))
	first := tNow.Add(-2 * time.Hour)
	m.ExpectQuery(`GROUP BY 1, 2, 3, 4\s+ORDER BY cnt DESC`).WithArgs(anyArgs(3)...).
		WillReturnRows(pgxmock.NewRows(errGroupCols).
			AddRow("invalid_request", "4xx", "openai", "gpt-4o-mini", "bad temp", 3, 1, first, tNow))
	m.ExpectQuery(`date_bin\(\$4, sub\.ts`).WithArgs(anyArgs(8)...).
		WillReturnRows(pgxmock.NewRows(errBucketCols))

	got, src, err := s.ListTrafficErrorGroupsScaled(context.Background(), p)
	if err != nil {
		t.Fatalf("Scaled: %v", err)
	}
	if src != ErrorGroupsSourceDirect {
		t.Errorf("dataSource = %q, want direct (retention fallback)", src)
	}
	if len(got) != 1 {
		t.Errorf("groups = %d, want 1 from direct", len(got))
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestListTrafficErrorGroupsScaled_StalledWatermarkFallback: a rollup-5m
// watermark hours behind the window end means the "unsealed tail" would be a
// wide raw scan wearing a rollup label — the request must serve the plain
// direct path and say so.
func TestListTrafficErrorGroupsScaled_StalledWatermarkFallback(t *testing.T) {
	s, m := newMock(t)
	p := errGroupsParams()
	expectWatermark(m, "rollup-5m", p.To.Add(-3*time.Hour).Truncate(5*time.Minute))
	first := tNow.Add(-2 * time.Hour)
	m.ExpectQuery(`GROUP BY 1, 2, 3, 4\s+ORDER BY cnt DESC`).WithArgs(anyArgs(3)...).
		WillReturnRows(pgxmock.NewRows(errGroupCols).
			AddRow("invalid_request", "4xx", "openai", "gpt-4o-mini", "bad temp", 3, 1, first, tNow))
	m.ExpectQuery(`date_bin\(\$4, sub\.ts`).WithArgs(anyArgs(8)...).
		WillReturnRows(pgxmock.NewRows(errBucketCols))

	_, src, err := s.ListTrafficErrorGroupsScaled(context.Background(), p)
	if err != nil {
		t.Fatalf("Scaled: %v", err)
	}
	if src != ErrorGroupsSourceDirect {
		t.Errorf("dataSource = %q, want direct (stalled-watermark fallback)", src)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestListTrafficErrorGroupsScaled_TailCapResidualFold: when the tail query
// returns exactly the row cap, the residual per-status-range counts fold
// into the overflow class so the total stays exact.
func TestListTrafficErrorGroupsScaled_TailCapResidualFold(t *testing.T) {
	s, m := newMock(t)
	p := errGroupsParams()
	wmMid := p.To.Add(-10 * time.Minute).Truncate(5 * time.Minute)
	expectWatermark(m, "rollup-5m", wmMid)
	expectWatermark(m, "merge-1h", time.Time{})
	expectWatermark(m, "merge-1d", time.Time{})
	expectTierFloor(m, p.From.Add(-time.Hour))
	m.ExpectQuery(`FROM metric_rollup_5m`).WithArgs(anyArgs(6)...).WillReturnRows(pgxmock.NewRows(segCountCols))
	m.ExpectQuery(`FROM metric_rollup_5m`).WithArgs(anyArgs(5)...).WillReturnRows(pgxmock.NewRows(segSeenCols))

	tailRows := pgxmock.NewRows(tailCols)
	for i := range errorTailRowCap {
		tailRows.AddRow(fmt.Sprintf("code-%04d", i), "4xx", "p", "m", tNow.Truncate(5*time.Minute), 1, tNow, tNow)
	}
	m.ExpectQuery(`ORDER BY cnt DESC\s+LIMIT 5000`).WithArgs(anyArgs(4)...).WillReturnRows(tailRows)
	// Cap hit → totals query; 4xx total 5200 → residual 200 into overflow.
	m.ExpectQuery(`GROUP BY 1`).WithArgs(anyArgs(3)...).
		WillReturnRows(pgxmock.NewRows([]string{"status_range", "count"}).AddRow("4xx", 5200))

	// Enrichment for the displayed classes: overflow ranks first (count 200)
	// and skips enrichment; the remaining 99 singles each get reason+distinct.
	for range 99 {
		m.ExpectQuery(`SELECT COALESCE\(a\.error_reason, ''\)`).WithArgs(anyArgs(7)...).
			WillReturnRows(pgxmock.NewRows([]string{"reason"}))
		m.ExpectQuery(`COUNT\(DISTINCT a\.end_user_id\)`).WithArgs(anyArgs(7)...).
			WillReturnRows(pgxmock.NewRows([]string{"rows", "n"}).AddRow(1, 1))
	}

	got, src, err := s.ListTrafficErrorGroupsScaled(context.Background(), p)
	if err != nil {
		t.Fatalf("Scaled: %v", err)
	}
	if src != ErrorGroupsSourceRollup {
		t.Errorf("dataSource = %q", src)
	}
	var overflow *TrafficErrorGroup
	for i := range got {
		if got[i].ErrorCode == metrics.ErrorClassOverflowCode {
			overflow = &got[i]
		}
	}
	if overflow == nil {
		t.Fatal("overflow residual row missing")
	}
	if overflow.Count != 200 {
		t.Errorf("overflow count = %d, want 200 residual", overflow.Count)
	}
	if overflow.AffectedEndUsers != nil || overflow.SampleReason != "" {
		t.Errorf("overflow enrichment leaked: %+v", overflow)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestListTrafficErrorGroupsScaled_Top100Cut: more than 100 classes collapse
// to the 100 largest, mirroring the direct path's LIMIT 100.
func TestListTrafficErrorGroupsScaled_Top100Cut(t *testing.T) {
	s, m := newMock(t)
	p := errGroupsParams()
	wm := p.To.Truncate(5 * time.Minute)
	expectWatermark(m, "rollup-5m", wm)
	expectWatermark(m, "merge-1h", time.Time{})
	expectWatermark(m, "merge-1d", time.Time{})

	expectTierFloor(m, p.From.Add(-time.Hour))

	rows := pgxmock.NewRows(segCountCols)
	for i := range 110 {
		dim := classDimKey("invalid_request", "4xx", "p", fmt.Sprintf("m-%03d", i))
		rows.AddRow(dim, tNow, float64(200-i))
	}
	m.ExpectQuery(`FROM metric_rollup_5m`).WithArgs(anyArgs(6)...).WillReturnRows(rows)
	m.ExpectQuery(`FROM metric_rollup_5m`).WithArgs(anyArgs(5)...).
		WillReturnRows(pgxmock.NewRows(segSeenCols))
	for range 100 {
		m.ExpectQuery(`SELECT COALESCE\(a\.error_reason, ''\)`).
			WithArgs(anyArgs(7)...).
			WillReturnRows(pgxmock.NewRows([]string{"reason"}))
		m.ExpectQuery(`COUNT\(DISTINCT a\.end_user_id\)`).
			WithArgs(anyArgs(7)...).
			WillReturnRows(pgxmock.NewRows([]string{"rows", "n"}).AddRow(3, 1))
	}

	got, _, err := s.ListTrafficErrorGroupsScaled(context.Background(), p)
	if err != nil {
		t.Fatalf("Scaled: %v", err)
	}
	if len(got) != 100 {
		t.Fatalf("groups = %d, want top-100 cut", len(got))
	}
	if got[0].Count != 200 || got[99].Count != 101 {
		t.Errorf("cut kept wrong classes: first=%d last=%d", got[0].Count, got[99].Count)
	}
}

// TestListTrafficErrorGroupsScaled_PurgedRawRowsNullAffected: a class the
// rollup still counts but whose raw traffic_event rows retention has purged
// must report affectedEndUsers = null, never a fabricated exact 0.
func TestListTrafficErrorGroupsScaled_PurgedRawRowsNullAffected(t *testing.T) {
	s, m := newMock(t)
	p := errGroupsParams()
	wm := p.To.Truncate(5 * time.Minute)
	expectWatermark(m, "rollup-5m", wm)
	expectWatermark(m, "merge-1h", time.Time{})
	expectWatermark(m, "merge-1d", time.Time{})
	expectTierFloor(m, p.From.Add(-time.Hour))

	dim := classDimKey("PROVIDER_ERROR", "5xx", "openai", "gpt-4o")
	m.ExpectQuery(`FROM metric_rollup_5m`).WithArgs(anyArgs(6)...).WillReturnRows(
		pgxmock.NewRows(segCountCols).AddRow(dim, p.From, float64(12)))
	m.ExpectQuery(`FROM metric_rollup_5m`).WithArgs(anyArgs(5)...).WillReturnRows(pgxmock.NewRows(segSeenCols))
	m.ExpectQuery(`SELECT COALESCE\(a\.error_reason, ''\)`).WithArgs(anyArgs(7)...).
		WillReturnRows(pgxmock.NewRows([]string{"reason"}))
	// COUNT(*) = 0 → raw rows purged → null, not &0.
	m.ExpectQuery(`COUNT\(DISTINCT a\.end_user_id\)`).WithArgs(anyArgs(7)...).
		WillReturnRows(pgxmock.NewRows([]string{"rows", "n"}).AddRow(0, 0))

	got, _, err := s.ListTrafficErrorGroupsScaled(context.Background(), p)
	if err != nil {
		t.Fatalf("Scaled: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("groups = %d, want 1", len(got))
	}
	if got[0].AffectedEndUsers != nil {
		t.Errorf("affectedEndUsers = %v, want nil for purged raw rows", *got[0].AffectedEndUsers)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestEnsureErrClassAgg_CapFoldsToOverflow: the 5001st distinct class folds
// into the overflow class for its status range instead of growing the map;
// overflow classes themselves always admit.
func TestEnsureErrClassAgg_CapFoldsToOverflow(t *testing.T) {
	agg := map[string]*errClassAgg{}
	for i := range errClassAggCap {
		ensureErrClassAgg(agg, metrics.ErrorClass{ErrorCode: "invalid_request", StatusRange: "4xx", Provider: "p", Model: fmt.Sprintf("m-%05d", i)})
	}
	if len(agg) != errClassAggCap {
		t.Fatalf("agg = %d, want %d", len(agg), errClassAggCap)
	}
	e := ensureErrClassAgg(agg, metrics.ErrorClass{ErrorCode: "PROVIDER_ERROR", StatusRange: "5xx", Provider: "p", Model: "one-too-many"})
	if e.class.ErrorCode != metrics.ErrorClassOverflowCode || e.class.StatusRange != "5xx" {
		t.Errorf("beyond-cap class = %+v, want 5xx overflow", e.class)
	}
	if len(agg) != errClassAggCap+1 {
		t.Errorf("agg = %d, want cap+1 (overflow admitted)", len(agg))
	}
	// The same overflow entry is reused, and an ALREADY-admitted class still
	// resolves to its own entry.
	if e2 := ensureErrClassAgg(agg, metrics.ErrorClass{ErrorCode: "other", StatusRange: "5xx", Provider: "x", Model: "y"}); e2 != e {
		t.Error("second beyond-cap 5xx class did not reuse the overflow entry")
	}
	known := ensureErrClassAgg(agg, metrics.ErrorClass{ErrorCode: "invalid_request", StatusRange: "4xx", Provider: "p", Model: "m-00000"})
	if known.class.Model != "m-00000" {
		t.Error("admitted class no longer resolves to its own entry")
	}
}

// TestListTrafficErrorGroupsScaled_SeriesBirthFallback: buckets aggregated
// BEFORE the error-class series existed carry request_count rows but no
// error-class rows — a window reaching into that era must fall back to the
// direct scan (deploy-transition honesty; self-heals as the window slides
// past the series' first bucket).
func TestListTrafficErrorGroupsScaled_SeriesBirthFallback(t *testing.T) {
	s, m := newMock(t)
	p := errGroupsParams()
	wm := p.To.Truncate(5 * time.Minute)
	expectWatermark(m, "rollup-5m", wm)
	expectWatermark(m, "merge-1h", time.Time{})
	expectWatermark(m, "merge-1d", time.Time{})
	// Retention floor covers the window, but the series' first bucket is
	// inside it → series-birth gap.
	m.ExpectQuery(`SELECT MIN\("bucketStart"\) FROM metric_rollup_5m`).
		WithArgs(metrics.MetricRequestCount).
		WillReturnRows(pgxmock.NewRows([]string{"min"}).AddRow(ptrTime(p.From.Add(-time.Hour))))
	m.ExpectQuery(`SELECT MIN\("bucketStart"\) FROM metric_rollup_5m`).
		WithArgs(metrics.MetricTrafficErrorClassCount).
		WillReturnRows(pgxmock.NewRows([]string{"min"}).AddRow(ptrTime(p.From.Add(2 * time.Hour))))
	m.ExpectQuery(`GROUP BY 1, 2, 3, 4\s+ORDER BY cnt DESC`).WithArgs(anyArgs(3)...).
		WillReturnRows(pgxmock.NewRows(errGroupCols))

	_, src, err := s.ListTrafficErrorGroupsScaled(context.Background(), p)
	if err != nil {
		t.Fatalf("Scaled: %v", err)
	}
	if src != ErrorGroupsSourceDirect {
		t.Errorf("dataSource = %q, want direct (series-birth fallback)", src)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

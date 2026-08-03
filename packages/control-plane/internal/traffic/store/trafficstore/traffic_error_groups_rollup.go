package trafficstore

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/goccy/go-json"

	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/domain"
)

// Data-source labels for the error-governance response, surfaced so the UI
// (and the prod parity check) can tell which read path served the numbers.
const (
	ErrorGroupsSourceRollup = "rollup"
	ErrorGroupsSourceDirect = "direct"
)

// affectedEndUsersMaxRows bounds the per-class COUNT(DISTINCT end_user_id)
// the rollup path is willing to run: a class whose rollup count exceeds this
// gets a null affectedEndUsers (UI shows "—") instead of an unbounded
// distinct scan — the whole point of the rollup path is that read cost stops
// scaling with error-row volume.
const affectedEndUsersMaxRows = 50000

// errorTailRowCap bounds the direct tail query (the unsealed trailing window
// past the rollup-5m watermark, normally < ~10 minutes). 5000 class×bin rows
// is far beyond any organic tail; the cap only bites a deliberate
// unique-class spray — and when it does, the residual counts are folded into
// the overflow class (count-preserving) rather than dropped.
const errorTailRowCap = 5000

// errorTailMaxWindow bounds how wide an unsealed tail the rollup path will
// direct-scan. A healthy rollup-5m job keeps the tail under ~10 minutes; a
// tail wider than this means the pipeline is stalled (wedged watermark), and
// pretending the response is rollup-backed while raw-scanning hours of data
// would be both dishonest and unprotected — the whole request falls back to
// the plain direct path (dataSource "direct") instead.
const errorTailMaxWindow = time.Hour

// errClassAggCap bounds the read-side aggregation map. The write side caps
// classes per BUCKET, so a long window with per-bucket class rotation can
// still deliver far more distinct classes than any display needs; beyond the
// cap, new classes fold into the overflow class for their status range
// (count-preserving), mirroring the producer's discipline.
const errClassAggCap = 5000

// errClassAgg accumulates one error class across rollup segments + the
// direct tail before the final top-100 cut.
type errClassAgg struct {
	class     metrics.ErrorClass
	count     int
	firstSeen time.Time
	lastSeen  time.Time
	buckets   map[time.Time]int
}

// rollupWatermark reads the committed watermark for a rollup job; zero time
// when the job has never run (row absent) or the read fails — callers treat
// zero as "tier unavailable".
func (store *Store) rollupWatermark(ctx context.Context, jobName string) time.Time {
	var wm time.Time
	err := store.pool.QueryRow(ctx,
		`SELECT "watermark" FROM "rollup_watermark" WHERE "jobName" = $1`, jobName,
	).Scan(&wm)
	if err != nil {
		return time.Time{}
	}
	return wm
}

// rollupTierFloor returns the earliest bucket a rollup tier still holds for
// one metric — the tier's coverage floor for that series as observed in the
// data. ok=false when the tier holds no such rows at all or the probe fails.
func (store *Store) rollupTierFloor(ctx context.Context, table, metricName string) (time.Time, bool) {
	q := fmt.Sprintf(`SELECT MIN("bucketStart") FROM %s WHERE "metricName" = $1`, table)
	var floor *time.Time
	if err := store.pool.QueryRow(ctx, q, metricName).Scan(&floor); err != nil || floor == nil {
		return time.Time{}, false
	}
	return *floor, true
}

// errorClassSegment is one (table, window) slice of the rollup read plan.
type errorClassSegment struct {
	table string
	from  time.Time
	to    time.Time
}

// planErrorClassSegments builds the tier plan for the requested bin width.
// Each tier is read up to its own merge watermark, the finer tier covers the
// gap behind it, and metric_rollup_5m covers up to the rollup-5m sealed
// boundary; the caller direct-scans traffic_event past that. Windows are
// clamped monotonic so segments never overlap.
//
// The 1mo tier is deliberately unused: the 1d tier's 365-day retention
// exceeds traffic_event's own retention horizon, so a window old enough to
// need 1mo rows has no direct rows to reconcile against either.
func planErrorClassSegments(from, to, sealed, wm1h, wm1d time.Time, bin time.Duration) []errorClassSegment {
	clamp := func(t time.Time) time.Time {
		if t.Before(from) {
			return from
		}
		if t.After(to) {
			return to
		}
		return t
	}
	end5m := clamp(sealed)
	switch {
	case bin >= 24*time.Hour:
		b1d := clamp(wm1d)
		b1h := clamp(wm1h)
		if b1h.Before(b1d) {
			b1h = b1d
		}
		if end5m.Before(b1h) {
			end5m = b1h
		}
		return []errorClassSegment{
			{"metric_rollup_1d", from, b1d},
			{"metric_rollup_1h", b1d, b1h},
			{"metric_rollup_5m", b1h, end5m},
		}
	case bin >= time.Hour:
		b1h := clamp(wm1h)
		if end5m.Before(b1h) {
			end5m = b1h
		}
		return []errorClassSegment{
			{"metric_rollup_1h", from, b1h},
			{"metric_rollup_5m", b1h, end5m},
		}
	default:
		return []errorClassSegment{{"metric_rollup_5m", from, end5m}}
	}
}

// domainSubDims maps the direct path's DB-source filter to the rollup rows'
// sub-dimension values ("source=<domain>"). The API only ever filters whole
// product domains (parseTrafficDomainParam), so the two filters select the
// same event population.
func domainSubDims(dbSources []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, 3)
	for _, s := range dbSources {
		d, ok := domain.DBSourceToDomain(s)
		if !ok {
			continue
		}
		sd := "source=" + string(d)
		if _, dup := seen[sd]; !dup {
			seen[sd] = struct{}{}
			out = append(out, sd)
		}
	}
	return out
}

// ListTrafficErrorGroupsScaled serves the error-governance aggregation
// rollup-first: sealed history comes from the metric_rollup_* tiers
// (traffic.error_class.* series) and only the unsealed trailing window is
// scanned from raw traffic_event, so read cost no longer scales with the
// number of error rows in the window. Falls back to the full direct scan
// when the rollup pipeline has never run (no rollup-5m watermark). The
// returned string is the data-source label for the response.
func (store *Store) ListTrafficErrorGroupsScaled(ctx context.Context, p TrafficErrorGroupsParams) ([]TrafficErrorGroup, string, error) {
	if p.From.IsZero() || p.To.IsZero() || !p.From.Before(p.To) {
		return nil, "", fmt.Errorf("list_traffic_error_groups_scaled: from < to is required")
	}
	if p.BucketInterval <= 0 {
		return nil, "", fmt.Errorf("list_traffic_error_groups_scaled: bucket interval must be positive")
	}
	sources := p.DBSources
	if len(sources) == 0 {
		sources = domain.AllDataPlaneDBSources()
	}

	// Job names are the Hub's rollup job IDs (rollup_5m.go / rollup_merge.go
	// constants); a rename over there degrades this path to full-direct.
	wm5m := store.rollupWatermark(ctx, "rollup-5m")
	if wm5m.IsZero() {
		groups, err := store.ListTrafficErrorGroups(ctx, p)
		return groups, ErrorGroupsSourceDirect, err
	}
	// The watermark is the last committed bucketStart; that bucket's rows
	// cover up to watermark+5m.
	sealed := wm5m.Add(5 * time.Minute)
	// Stalled pipeline: the unsealed tail would be a wide raw scan wearing a
	// "rollup" label — serve the plain direct path honestly instead.
	if p.To.Sub(sealed) > errorTailMaxWindow {
		groups, err := store.ListTrafficErrorGroups(ctx, p)
		return groups, ErrorGroupsSourceDirect, err
	}
	segments := planErrorClassSegments(p.From, p.To, sealed,
		store.rollupWatermark(ctx, "merge-1h"), store.rollupWatermark(ctx, "merge-1d"), p.BucketInterval)

	// Coverage guard, two probes per planned tier:
	//  - Retention: tier selection is by bin width, but each tier only
	//    retains a bounded history (5m ≈ 7d by default). A segment starting
	//    before the earliest request_count bucket the tier still holds
	//    cannot be served from rollup.
	//  - Series birth: buckets aggregated before the error-class series
	//    existed carry request_count rows but no error-class rows. A segment
	//    starting before the series' own earliest bucket in the tier may
	//    silently miss history (a legitimately error-free era before the
	//    first error also lands here — the direct scan then returns the same
	//    zero rows, so falling back costs only the scan).
	// Either gap → serve the full direct scan honestly instead of
	// under-reporting under a "rollup" label.
	for _, seg := range segments {
		if !seg.to.After(seg.from) {
			continue
		}
		floor, ok := store.rollupTierFloor(ctx, seg.table, metrics.MetricRequestCount)
		if !ok || seg.from.Before(floor) {
			groups, err := store.ListTrafficErrorGroups(ctx, p)
			return groups, ErrorGroupsSourceDirect, err
		}
		seriesFloor, ok := store.rollupTierFloor(ctx, seg.table, metrics.MetricTrafficErrorClassCount)
		if !ok || seg.from.Before(seriesFloor) {
			groups, err := store.ListTrafficErrorGroups(ctx, p)
			return groups, ErrorGroupsSourceDirect, err
		}
	}

	agg := map[string]*errClassAgg{}
	for _, seg := range segments {
		if err := store.foldErrorClassSegment(ctx, seg, p.BucketInterval, domainSubDims(sources), agg); err != nil {
			return nil, "", err
		}
	}

	tailFrom := sealed
	if tailFrom.Before(p.From) {
		tailFrom = p.From
	}
	if tailFrom.Before(p.To) {
		if err := store.foldErrorClassTail(ctx, tailFrom, p.To, p.BucketInterval, sources, agg); err != nil {
			return nil, "", err
		}
	}

	groups, err := store.assembleErrorClassGroups(ctx, p, sources, agg)
	if err != nil {
		return nil, "", err
	}
	return groups, ErrorGroupsSourceRollup, nil
}

// foldErrorClassSegment reads one rollup tier slice and folds count + seen
// rows into the class aggregation map. Counts and seen metadata are fetched
// by two queries: count rows collapse per (class, bin) in SQL, while seen
// rows carry per-bucket jsonb metadata that cannot collapse (grouping by a
// jsonb column would defeat the aggregation) and are min/max-merged in Go.
func (store *Store) foldErrorClassSegment(ctx context.Context, seg errorClassSegment, bin time.Duration, subDims []string, agg map[string]*errClassAgg) error {
	if !seg.to.After(seg.from) {
		return nil
	}
	cq := fmt.Sprintf(`
		SELECT "dimensionKey",
		       date_bin($1, "bucketStart", TIMESTAMPTZ '2000-01-01') AS bin_ts,
		       SUM("value")::FLOAT8 AS total
		  FROM %s
		 WHERE "bucketStart" >= $2 AND "bucketStart" < $3
		   AND "metricName" = $4
		   AND "dimensionKey" LIKE $5
		   AND "subDimension" = ANY($6)
		 GROUP BY 1, 2`, seg.table)
	rows, err := store.pool.Query(ctx, cq, bin, seg.from, seg.to,
		metrics.MetricTrafficErrorClassCount, metrics.DimensionErrorClass+"=%", subDims)
	if err != nil {
		return fmt.Errorf("error-class rollup %s: %w", seg.table, err)
	}
	for rows.Next() {
		var dimKey string
		var binTs time.Time
		var total float64
		if err := rows.Scan(&dimKey, &binTs, &total); err != nil {
			rows.Close()
			return fmt.Errorf("error-class rollup %s scan: %w", seg.table, err)
		}
		class, ok := metrics.ParseErrorClassValue(strings.TrimPrefix(dimKey, metrics.DimensionErrorClass+"="))
		if !ok {
			continue
		}
		e := ensureErrClassAgg(agg, class)
		e.count += int(total)
		e.buckets[binTs] += int(total)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("error-class rollup %s iterate: %w", seg.table, err)
	}

	sq := fmt.Sprintf(`
		SELECT "dimensionKey", "metadata"
		  FROM %s
		 WHERE "bucketStart" >= $1 AND "bucketStart" < $2
		   AND "metricName" = $3
		   AND "dimensionKey" LIKE $4
		   AND "subDimension" = ANY($5)`, seg.table)
	srows, err := store.pool.Query(ctx, sq, seg.from, seg.to,
		metrics.MetricTrafficErrorClassSeen, metrics.DimensionErrorClass+"=%", subDims)
	if err != nil {
		return fmt.Errorf("error-class rollup seen %s: %w", seg.table, err)
	}
	defer srows.Close()
	for srows.Next() {
		var dimKey string
		var meta []byte
		if err := srows.Scan(&dimKey, &meta); err != nil {
			return fmt.Errorf("error-class rollup seen %s scan: %w", seg.table, err)
		}
		class, ok := metrics.ParseErrorClassValue(strings.TrimPrefix(dimKey, metrics.DimensionErrorClass+"="))
		if !ok {
			continue
		}
		var ts metrics.TimestampMeta
		if err := json.Unmarshal(meta, &ts); err != nil {
			continue
		}
		e := ensureErrClassAgg(agg, class)
		if first, perr := time.Parse(time.RFC3339, ts.FirstSeen); perr == nil {
			if e.firstSeen.IsZero() || first.Before(e.firstSeen) {
				e.firstSeen = first
			}
		}
		if last, perr := time.Parse(time.RFC3339, ts.LastSeen); perr == nil {
			if last.After(e.lastSeen) {
				e.lastSeen = last
			}
		}
	}
	return srows.Err()
}

// foldErrorClassTail direct-scans the unsealed trailing window with the same
// class-key expressions as the pure-direct path and folds the result into the
// aggregation map. Capped at errorTailRowCap class×bin rows, largest classes
// first; when the cap trips, the residual counts are recovered by a cheap
// per-status-range total and folded into the overflow class so the total
// stays exact (same discipline as the producer's bucket cap).
func (store *Store) foldErrorClassTail(ctx context.Context, from, to time.Time, bin time.Duration, sources []string, agg map[string]*errClassAgg) error {
	q := fmt.Sprintf(`
		SELECT sub.error_code, sub.status_range, sub.provider, sub.model,
		       date_bin($4, sub.ts, TIMESTAMPTZ '2000-01-01') AS bin_ts,
		       COUNT(*)::INT AS cnt,
		       MIN(sub.ts)   AS first_seen,
		       MAX(sub.ts)   AS last_seen
		  FROM (SELECT %s, a.timestamp AS ts
		          FROM traffic_event a
		         WHERE %s) sub
		 GROUP BY 1, 2, 3, 4, 5
		 ORDER BY cnt DESC
		 LIMIT %d`, trafficErrorGroupKeyExprs, trafficErrorWhere, errorTailRowCap)
	rows, err := store.pool.Query(ctx, q, from, to, sources, bin)
	if err != nil {
		return fmt.Errorf("error-class tail: %w", err)
	}
	defer rows.Close()

	returned := 0
	counted := map[string]int{} // per status range, for the truncation residual
	for rows.Next() {
		var class metrics.ErrorClass
		var binTs, first, last time.Time
		var cnt int
		if err := rows.Scan(&class.ErrorCode, &class.StatusRange, &class.Provider, &class.Model,
			&binTs, &cnt, &first, &last); err != nil {
			return fmt.Errorf("error-class tail scan: %w", err)
		}
		returned++
		counted[class.StatusRange] += cnt
		e := ensureErrClassAgg(agg, class)
		e.count += cnt
		e.buckets[binTs] += cnt
		if e.firstSeen.IsZero() || first.Before(e.firstSeen) {
			e.firstSeen = first
		}
		if last.After(e.lastSeen) {
			e.lastSeen = last
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if returned < errorTailRowCap {
		return nil
	}
	// Cap hit — recover the dropped counts. The residual joins the overflow
	// class per status range without bucket/seen detail (unknowable for
	// dropped rows); the count total stays exact.
	tq := fmt.Sprintf(`
		SELECT CASE WHEN a.status_code >= 500 THEN '5xx' ELSE '4xx' END AS status_range,
		       COUNT(*)::INT
		  FROM traffic_event a
		 WHERE %s
		 GROUP BY 1`, trafficErrorWhere)
	trows, err := store.pool.Query(ctx, tq, from, to, sources)
	if err != nil {
		return fmt.Errorf("error-class tail totals: %w", err)
	}
	defer trows.Close()
	for trows.Next() {
		var rng string
		var total int
		if err := trows.Scan(&rng, &total); err != nil {
			return fmt.Errorf("error-class tail totals scan: %w", err)
		}
		if residual := total - counted[rng]; residual > 0 {
			ovf := ensureErrClassAgg(agg, metrics.ErrorClass{ErrorCode: metrics.ErrorClassOverflowCode, StatusRange: rng})
			ovf.count += residual
		}
	}
	return trows.Err()
}

// ensureErrClassAgg returns the accumulator for a class, creating it if
// needed. Beyond errClassAggCap distinct classes, NEW classes fold into the
// overflow class for their status range (count-preserving) instead of
// growing the map — the write side caps classes per bucket, so a long window
// with per-bucket class rotation could otherwise deliver an unbounded number
// of distinct keys. Overflow classes themselves always admit.
func ensureErrClassAgg(agg map[string]*errClassAgg, class metrics.ErrorClass) *errClassAgg {
	key := metrics.BuildErrorClassValue(class)
	if e, ok := agg[key]; ok {
		return e
	}
	if len(agg) >= errClassAggCap && class.ErrorCode != metrics.ErrorClassOverflowCode {
		return ensureErrClassAgg(agg, metrics.ErrorClass{ErrorCode: metrics.ErrorClassOverflowCode, StatusRange: class.StatusRange})
	}
	e := &errClassAgg{class: class, buckets: map[time.Time]int{}}
	agg[key] = e
	return e
}

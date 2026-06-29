// Package localrollup is the agent-side mirror of Hub's per-Thing rollup
// pipeline. It scans the agent's local audit_events SQLite table on a
// 1-minute ticker, aggregates into 5m buckets in thing_metric_rollup_local_5m,
// then cascades into 1h / 1d / 1mo bins. Designed to keep agent native UI's
// stats panel responsive offline and to make the 10K-agent fleet scenario
// (per Hub config enableAgentRollup=false) safe without sacrificing detail.
//
// Schema lives in packages/agent/internal/observability/audit/queue/queue.go alongside the audit
// queue tables (single SQLite file, SQLCipher-encrypted in production).
// Watermarks share the rollup_watermark_local table; idempotent
// DELETE+INSERT inside a transaction is the same recovery contract as Hub
// rollup. Retention defaults match the user-decided policy:
// 5m=24h, 1h=30d, 1d=365d, 1mo=5y.
package localrollup

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/goccy/go-json"
	"log/slog"
	"strings"
	"time"
)

// Metric names mirror the Hub-side constants where applicable. We avoid
// importing packages/shared/core/metrics here to keep the agent module's
// dependency surface stable; the strings are stable contract anyway.
const (
	MetricRequestCount     = "request_count"
	MetricStatus2xxCount   = "status_2xx_count"
	MetricStatus4xxCount   = "status_4xx_count"
	MetricStatus5xxCount   = "status_5xx_count"
	MetricLatencySum       = "latency_sum"
	MetricLatencyCount     = "latency_count"
	MetricLatencyHistogram = "latency_histogram"
	// Phase aggregates. Same sum/count layout so the UI computes
	// average phase latency per dimension via sum/count division.
	MetricLatencyUsSum             = "latency_us_sum"
	MetricLatencyUsCount           = "latency_us_count"
	MetricLatencyUpstreamTtfbSum   = "latency_upstream_ttfb_sum"
	MetricLatencyUpstreamTtfbCount = "latency_upstream_ttfb_count"
	MetricLatencyUpstreamSum       = "latency_upstream_total_sum"
	MetricLatencyUpstreamCount     = "latency_upstream_total_count"
	MetricLatencyHooksSum          = "latency_hooks_sum"
	MetricLatencyHooksCount        = "latency_hooks_count"
	MetricBytesInSum               = "bytes_in_sum"
	MetricBytesOutSum              = "bytes_out_sum"
	MetricPromptTokens             = "prompt_tokens"
	MetricCompletionTokens         = "completion_tokens"
	MetricTotalTokens              = "total_tokens"
	MetricHookAllowCount           = "hook_allow_count"
	MetricHookDenyCount            = "hook_deny_count"
	MetricHookErrorCount           = "hook_error_count"
	MetricBumpSuccessCount         = "bump_success_count"
	MetricBumpFailedCount          = "bump_failed_count"
	MetricBumpExemptCount          = "bump_exempt_count"
	// Action breakdown — agent-unique. passthrough is "let through unbumped",
	// inspect is "TLS-bumped to allow content inspection", deny is "blocked".
	MetricActionPassthrough = "action_passthrough_count"
	MetricActionInspect     = "action_inspect_count"
	MetricActionDeny        = "action_deny_count"
)

// Retention defaults — match the user-decided policy (5m=24h / 1h=30d /
// 1d=365d / 1mo=5y). Overridable per Aggregator instance for ops control.
type Retention struct {
	Keep5m  time.Duration
	Keep1h  time.Duration
	Keep1d  time.Duration
	Keep1mo time.Duration
}

// DefaultRetention returns the standard agent-local retention window.
func DefaultRetention() Retention {
	return Retention{
		Keep5m:  24 * time.Hour,
		Keep1h:  30 * 24 * time.Hour,
		Keep1d:  365 * 24 * time.Hour,
		Keep1mo: 5 * 365 * 24 * time.Hour,
	}
}

// Aggregator owns the local rollup lifecycle. Tick() runs one pass of
// 5m → 1h → 1d → 1mo + retention. The agent main loop calls Tick on a
// 1-minute timer; failure is logged and retried next tick.
type Aggregator struct {
	db        *sql.DB
	logger    *slog.Logger
	retention Retention
}

// New constructs an Aggregator using the agent's SQLite handle exposed by
// audit.Queue.DB(). The schema must already include the rollup tables (added
// in audit/queue.go init).
func New(db *sql.DB, logger *slog.Logger) *Aggregator {
	return &Aggregator{
		db:        db,
		logger:    logger.With("component", "localrollup"),
		retention: DefaultRetention(),
	}
}

// WithRetention overrides the default retention windows (test / ops hook).
func (a *Aggregator) WithRetention(r Retention) *Aggregator {
	a.retention = r
	return a
}

// Tick runs one full pass: 5m aggregate → 1h merge → 1d merge → 1mo merge →
// retention purge. Returns the first error encountered; later stages do NOT
// run on earlier failure (the merge layers depend on the prior layer being
// up to date).
func (a *Aggregator) Tick(ctx context.Context) error {
	if err := a.aggregate5m(ctx); err != nil {
		return fmt.Errorf("aggregate 5m: %w", err)
	}
	if err := a.merge(ctx, "merge-1h-local", "thing_metric_rollup_local_5m", "thing_metric_rollup_local_1h", time.Hour, 6*time.Hour); err != nil {
		return fmt.Errorf("merge 1h: %w", err)
	}
	if err := a.merge(ctx, "merge-1d-local", "thing_metric_rollup_local_1h", "thing_metric_rollup_local_1d", 24*time.Hour, 48*time.Hour); err != nil {
		return fmt.Errorf("merge 1d: %w", err)
	}
	if err := a.mergeCalendarMonth(ctx); err != nil {
		return fmt.Errorf("merge 1mo: %w", err)
	}
	if err := a.purge(ctx); err != nil {
		return fmt.Errorf("purge: %w", err)
	}
	return nil
}

// Watermark helpers

func (a *Aggregator) getWatermark(ctx context.Context, job string) (time.Time, error) {
	var s string
	err := a.db.QueryRowContext(ctx, `SELECT watermark FROM rollup_watermark_local WHERE job_name = ?`, job).Scan(&s)
	if err == sql.ErrNoRows {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339Nano, s)
}

func (a *Aggregator) setWatermark(ctx context.Context, tx *sql.Tx, job string, wm time.Time) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO rollup_watermark_local (job_name, watermark, updated_at)
		VALUES (?, ?, datetime('now'))
		ON CONFLICT(job_name) DO UPDATE SET watermark = excluded.watermark, updated_at = datetime('now')
	`, job, wm.UTC().Format(time.RFC3339Nano))
	return err
}

// 5m aggregator

const bucket5m = 5 * time.Minute

type rollupKey struct {
	bucketStart  time.Time
	metricName   string
	dimensionKey string
	subDimension string
}

type histogram [6]int64 // matches shared/metrics.HistogramBucketCount

// Boundaries match shared/metrics.HistogramBoundaries — must stay aligned
// so the agent + Hub rollups merge predictably should the toggle flip.
var histBoundaries = [6]float64{50, 100, 200, 500, 1000, 1e9}

func bucketForLatency(ms float64) int {
	for i, b := range histBoundaries {
		if ms < b {
			return i
		}
	}
	return len(histBoundaries) - 1
}

func (a *Aggregator) aggregate5m(ctx context.Context) error {
	const job = "rollup-5m-local"

	wm, err := a.getWatermark(ctx, job)
	if err != nil {
		return fmt.Errorf("get watermark: %w", err)
	}
	if wm.IsZero() {
		wm = time.Now().UTC().Add(-1 * time.Hour).Truncate(bucket5m)
	}
	latestSealed := time.Now().UTC().Add(-bucket5m).Truncate(bucket5m)
	if !wm.Before(latestSealed) {
		return nil
	}

	for bucket := wm.Add(bucket5m); !bucket.After(latestSealed); bucket = bucket.Add(bucket5m) {
		if err := a.processBucket5m(ctx, bucket); err != nil {
			return fmt.Errorf("bucket %s: %w", bucket.Format(time.RFC3339), err)
		}
	}
	return nil
}

func (a *Aggregator) processBucket5m(ctx context.Context, bucket time.Time) error {
	end := bucket.Add(bucket5m)
	bs := bucket.UTC().Format(time.RFC3339Nano)
	bsLow := bucket.UTC().Format(time.RFC3339)
	endLow := end.UTC().Format(time.RFC3339)

	// audit_events.timestamp is a TEXT column with RFC3339-ish values written
	// by the agent's audit recorder. We compare as strings — lexicographic
	// order matches chronological order for fixed-width RFC3339.
	rows, err := a.db.QueryContext(ctx, `
		SELECT timestamp, source_process, source_user, dest_host, action,
		       bump_status, bytes_in, bytes_out, duration_ms,
		       hook_decision, compliance_tags,
		       provider_name, model_name,
		       prompt_tokens, completion_tokens,
		       upstream_ttfb_ms, upstream_total_ms,
		       request_hooks_ms, response_hooks_ms
		FROM audit_events
		WHERE timestamp >= ? AND timestamp < ?
	`, bsLow, endLow)
	if err != nil {
		return fmt.Errorf("scan audit_events: %w", err)
	}

	values := map[rollupKey]float64{}
	histos := map[rollupKey]histogram{}
	distinctProc := map[rollupKey]map[string]struct{}{}
	distinctHost := map[rollupKey]map[string]struct{}{}

	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			ts, srcProc, srcUser, destHost, action   string
			bumpStatus, hookDecision, complianceTags sql.NullString
			providerName, modelName                  sql.NullString
			bytesIn, bytesOut, durationMs            sql.NullInt64
			promptTokens, completionTokens           sql.NullInt64
			// Latency phase fields.
			upstreamTtfb, upstreamTotal sql.NullInt64
			requestHooks, responseHooks sql.NullInt64
		)
		if err := rows.Scan(&ts, &srcProc, &srcUser, &destHost, &action,
			&bumpStatus, &bytesIn, &bytesOut, &durationMs,
			&hookDecision, &complianceTags,
			&providerName, &modelName,
			&promptTokens, &completionTokens,
			&upstreamTtfb, &upstreamTotal,
			&requestHooks, &responseHooks); err != nil {
			return fmt.Errorf("scan row: %w", err)
		}

		// Build dimension list. Every event contributes to the global ("")
		// bucket plus typed dimensions when the source field is present.
		dims := []struct{ name, value string }{{"", ""}}
		if srcProc != "" {
			dims = append(dims, struct{ name, value string }{"source_process", srcProc})
		}
		if destHost != "" {
			dims = append(dims, struct{ name, value string }{"target_host", destHost})
		}
		if providerName.Valid && providerName.String != "" {
			dims = append(dims, struct{ name, value string }{"provider", providerName.String})
		}
		if modelName.Valid && modelName.String != "" {
			dims = append(dims, struct{ name, value string }{"model", modelName.String})
		}
		if action != "" {
			dims = append(dims, struct{ name, value string }{"action", action})
		}

		for _, d := range dims {
			dk := ""
			if d.name != "" {
				dk = d.name + "=" + d.value
			}
			sub := "source=agent"

			add := func(metric string, v float64) {
				values[rollupKey{bucket, metric, dk, sub}] += v
			}

			add(MetricRequestCount, 1)
			if bytesIn.Valid && bytesIn.Int64 > 0 {
				add(MetricBytesInSum, float64(bytesIn.Int64))
			}
			if bytesOut.Valid && bytesOut.Int64 > 0 {
				add(MetricBytesOutSum, float64(bytesOut.Int64))
			}
			if durationMs.Valid && durationMs.Int64 > 0 {
				add(MetricLatencySum, float64(durationMs.Int64))
				add(MetricLatencyCount, 1)
				k := rollupKey{bucket, MetricLatencyHistogram, dk, sub}
				h := histos[k]
				h[bucketForLatency(float64(durationMs.Int64))]++
				histos[k] = h
			}
			// Phase aggregates. "Our overhead" = max(0, total - upstream_total).
			if upstreamTtfb.Valid && upstreamTtfb.Int64 > 0 {
				add(MetricLatencyUpstreamTtfbSum, float64(upstreamTtfb.Int64))
				add(MetricLatencyUpstreamTtfbCount, 1)
			}
			// Stamp Avg Upstream + Avg Us for ALL flows including
			// passthrough. When only inspect flows have upstream_total
			// populated (Swift NE only tags URLSession-tracked flows),
			// Stats renders n/a on macOS where 90%+ of traffic is
			// passthrough. Semantic: when upstream_total is missing but
			// duration is known, treat
			// the whole flow as upstream wall time (Avg Upstream =
			// durationMs) and our overhead as 0 (raw relay does
			// nothing — bytes pass through with negligible CPU).
			// Inspect flows continue to compute Avg Us = duration -
			// upstream as before.
			if upstreamTotal.Valid && upstreamTotal.Int64 > 0 {
				add(MetricLatencyUpstreamSum, float64(upstreamTotal.Int64))
				add(MetricLatencyUpstreamCount, 1)
				if durationMs.Valid {
					us := durationMs.Int64 - upstreamTotal.Int64
					if us < 0 {
						us = 0
					}
					add(MetricLatencyUsSum, float64(us))
					add(MetricLatencyUsCount, 1)
				}
			} else if durationMs.Valid && durationMs.Int64 > 0 {
				// Passthrough / non-instrumented flow path. The whole
				// flow was upstream from the user's perspective; we
				// added effectively nothing.
				add(MetricLatencyUpstreamSum, float64(durationMs.Int64))
				add(MetricLatencyUpstreamCount, 1)
				add(MetricLatencyUsSum, 0)
				add(MetricLatencyUsCount, 1)
			}
			// Derived Success rate counters. audit_events has no
			// status_code column (the agent never sees HTTP status for
			// passthrough flows and rarely for inspect on macOS), so we
			// synthesise:
			//   2xx (success) ← action != deny AND hook ≠ reject/block
			//   4xx (admin block) ← hook reject_hard / block_soft
			//   5xx (deny) ← action = deny
			// Result: Success rate = 2xx / (2xx+4xx+5xx) is meaningful
			// for both passthrough and inspect cohorts.
			isAdminBlock := hookDecision.Valid && (hookDecision.String == "reject_hard" || hookDecision.String == "block_soft")
			isDeny := action == "deny"
			switch {
			case isDeny:
				add(MetricStatus5xxCount, 1)
			case isAdminBlock:
				add(MetricStatus4xxCount, 1)
			default:
				add(MetricStatus2xxCount, 1)
			}
			hooksTotal := int64(0)
			if requestHooks.Valid {
				hooksTotal += requestHooks.Int64
			}
			if responseHooks.Valid {
				hooksTotal += responseHooks.Int64
			}
			if hooksTotal > 0 {
				add(MetricLatencyHooksSum, float64(hooksTotal))
				add(MetricLatencyHooksCount, 1)
			}
			if promptTokens.Valid && promptTokens.Int64 > 0 {
				add(MetricPromptTokens, float64(promptTokens.Int64))
				add(MetricTotalTokens, float64(promptTokens.Int64))
			}
			if completionTokens.Valid && completionTokens.Int64 > 0 {
				add(MetricCompletionTokens, float64(completionTokens.Int64))
				add(MetricTotalTokens, float64(completionTokens.Int64))
			}

			// Action breakdown (agent-unique).
			switch action {
			case "passthrough":
				add(MetricActionPassthrough, 1)
			case "inspect":
				add(MetricActionInspect, 1)
			case "deny":
				add(MetricActionDeny, 1)
			}

			// Bump status.
			if bumpStatus.Valid {
				switch bs := bumpStatus.String; {
				case bs == "BUMP_SUCCESS":
					add(MetricBumpSuccessCount, 1)
				case bs == "BUMP_FAILED_PASSTHROUGH":
					add(MetricBumpFailedCount, 1)
				case strings.HasPrefix(bs, "BUMP_EXEMPT"):
					add(MetricBumpExemptCount, 1)
				}
			}

			// Hook decision.
			if hookDecision.Valid {
				switch hookDecision.String {
				case "APPROVE", "allow":
					add(MetricHookAllowCount, 1)
				case "REJECT_HARD", "BLOCK_SOFT", "reject":
					add(MetricHookDenyCount, 1)
				case "ERROR", "error":
					add(MetricHookErrorCount, 1)
				}
			}

			// Distinct counters (per dim/sub).
			if d.name != "source_process" && srcProc != "" {
				dkProc := rollupKey{bucket, "distinct_source_processes", dk, sub}
				if distinctProc[dkProc] == nil {
					distinctProc[dkProc] = map[string]struct{}{}
				}
				distinctProc[dkProc][srcProc] = struct{}{}
			}
			if d.name != "target_host" && destHost != "" {
				dkHost := rollupKey{bucket, "distinct_target_hosts", dk, sub}
				if distinctHost[dkHost] == nil {
					distinctHost[dkHost] = map[string]struct{}{}
				}
				distinctHost[dkHost][destHost] = struct{}{}
			}

			// Known limitation: compliance-tag breakdown via JSON-array
			// unnest is out of scope for the agent-side rollup — the
			// field is read but discarded here. The Hub-side metrics
			// rollup carries the breakdown for fleet-wide views.
			// (srcUser and ts are scanned to match the SELECT column
			// count but are not aggregated into any local metric.)
			_ = complianceTags
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate rows: %w", err)
	}

	// Single transaction: clear bucket → insert → advance watermark.
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM thing_metric_rollup_local_5m WHERE bucket_start = ?`, bs); err != nil {
		return fmt.Errorf("delete bucket: %w", err)
	}

	insert := `INSERT INTO thing_metric_rollup_local_5m (bucket_start, metric_name, dimension_key, sub_dimension, value, metadata, updated_at)
	           VALUES (?, ?, ?, ?, ?, ?, datetime('now'))`
	for k, v := range values {
		if v == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, insert, bs, k.metricName, k.dimensionKey, k.subDimension, v, nil); err != nil {
			return fmt.Errorf("insert value row: %w", err)
		}
	}
	for k, h := range histos {
		data, err := json.Marshal(h)
		if err != nil {
			continue
		}
		if _, err := tx.ExecContext(ctx, insert, bs, k.metricName, k.dimensionKey, k.subDimension, 0.0, string(data)); err != nil {
			return fmt.Errorf("insert histogram row: %w", err)
		}
	}
	for k, set := range distinctProc {
		if len(set) == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, insert, bs, k.metricName, k.dimensionKey, k.subDimension, float64(len(set)), nil); err != nil {
			return fmt.Errorf("insert distinct proc row: %w", err)
		}
	}
	for k, set := range distinctHost {
		if len(set) == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, insert, bs, k.metricName, k.dimensionKey, k.subDimension, float64(len(set)), nil); err != nil {
			return fmt.Errorf("insert distinct host row: %w", err)
		}
	}

	if err := a.setWatermark(ctx, tx, "rollup-5m-local", bucket); err != nil {
		return fmt.Errorf("set watermark: %w", err)
	}
	return tx.Commit()
}

// Merge cascade (fixed-duration)

func (a *Aggregator) merge(ctx context.Context, job, sourceTable, targetTable string, bucketDuration, initLookback time.Duration) error {
	wm, err := a.getWatermark(ctx, job)
	if err != nil {
		return err
	}
	if wm.IsZero() {
		wm = time.Now().UTC().Add(-initLookback).Truncate(bucketDuration)
	}
	latestSealed := time.Now().UTC().Add(-bucketDuration).Truncate(bucketDuration)
	if !wm.Before(latestSealed) {
		return nil
	}
	for bucket := wm.Add(bucketDuration); !bucket.After(latestSealed); bucket = bucket.Add(bucketDuration) {
		if err := a.mergeOneBucket(ctx, job, sourceTable, targetTable, bucket, bucket.Add(bucketDuration)); err != nil {
			return fmt.Errorf("bucket %s: %w", bucket.Format(time.RFC3339), err)
		}
	}
	return nil
}

func (a *Aggregator) mergeCalendarMonth(ctx context.Context) error {
	const job = "merge-1mo-local"
	wm, err := a.getWatermark(ctx, job)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	curMonthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	if wm.IsZero() {
		wm = time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, time.UTC)
	}
	for ms := nextMonth(wm); ms.Before(curMonthStart); ms = nextMonth(ms) {
		if err := a.mergeOneBucket(ctx, job, "thing_metric_rollup_local_1d", "thing_metric_rollup_local_1mo", ms, nextMonth(ms)); err != nil {
			return fmt.Errorf("bucket %s: %w", ms.Format("2006-01"), err)
		}
	}
	return nil
}

func nextMonth(t time.Time) time.Time {
	y, m, _ := t.Date()
	return time.Date(y, m+1, 1, 0, 0, 0, 0, time.UTC)
}

func (a *Aggregator) mergeOneBucket(ctx context.Context, job, sourceTable, targetTable string, bucketStart, bucketEnd time.Time) error {
	bs := bucketStart.UTC().Format(time.RFC3339Nano)
	bsLow := bucketStart.UTC().Format(time.RFC3339)
	endLow := bucketEnd.UTC().Format(time.RFC3339)

	// Pull source rows + sum/histogram-merge in Go. SQLite SUM is fine for
	// value rows; histogram metadata needs element-wise JSON addition which
	// we do in code.
	rows, err := a.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT metric_name, dimension_key, sub_dimension, value, metadata
		FROM %s
		WHERE bucket_start >= ? AND bucket_start < ?
	`, sourceTable), bsLow, endLow)
	if err != nil {
		return fmt.Errorf("read source: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type key struct{ m, d, s string }
	values := map[key]float64{}
	histos := map[key]histogram{}

	for rows.Next() {
		var m, d, s string
		var v float64
		var meta sql.NullString
		if err := rows.Scan(&m, &d, &s, &v, &meta); err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		k := key{m, d, s}
		if m == MetricLatencyHistogram && meta.Valid {
			var h histogram
			if err := json.Unmarshal([]byte(meta.String), &h); err == nil {
				existing := histos[k]
				for i := range h {
					existing[i] += h[i]
				}
				histos[k] = existing
			}
			continue
		}
		values[k] += v
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate: %w", err)
	}

	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE bucket_start = ?`, targetTable), bs); err != nil {
		return fmt.Errorf("delete target: %w", err)
	}
	insert := fmt.Sprintf(`INSERT INTO %s (bucket_start, metric_name, dimension_key, sub_dimension, value, metadata, updated_at)
	                      VALUES (?, ?, ?, ?, ?, ?, datetime('now'))`, targetTable)
	for k, v := range values {
		if v == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, insert, bs, k.m, k.d, k.s, v, nil); err != nil {
			return fmt.Errorf("insert value: %w", err)
		}
	}
	for k, h := range histos {
		data, err := json.Marshal(h)
		if err != nil {
			continue
		}
		if _, err := tx.ExecContext(ctx, insert, bs, k.m, k.d, k.s, 0.0, string(data)); err != nil {
			return fmt.Errorf("insert hist: %w", err)
		}
	}
	if err := a.setWatermark(ctx, tx, job, bucketStart); err != nil {
		return err
	}
	return tx.Commit()
}

// Query (read-side for agent native UI via IPC)

// Query is the read-side request shape. Empty MetricNames returns all
// metrics; DimensionKey empty returns only global ("") rows; SubDimension
// empty returns any.
type Query struct {
	StartTime    time.Time
	EndTime      time.Time
	MetricNames  []string
	DimensionKey string
	SubDimension string
}

// Row is a single rollup row returned to callers. Metadata is the raw JSON
// payload (histogram / timestamp meta) — callers deserialize as needed.
type Row struct {
	BucketStart  time.Time
	MetricName   string
	DimensionKey string
	SubDimension string
	Value        float64
	Metadata     string
}

// Granule returns the human-readable granule label that QueryRollup would
// pick for the time window. Stable across agent + Hub side selection logic.
func Granule(start, end time.Time) string {
	d := end.Sub(start)
	switch {
	case d <= 1*time.Hour:
		return "5m"
	case d <= 7*24*time.Hour:
		return "1h"
	case d <= 90*24*time.Hour:
		return "1d"
	default:
		return "1mo"
	}
}

func tableForGranule(g string) string {
	switch g {
	case "5m":
		return "thing_metric_rollup_local_5m"
	case "1h":
		return "thing_metric_rollup_local_1h"
	case "1d":
		return "thing_metric_rollup_local_1d"
	case "1mo":
		return "thing_metric_rollup_local_1mo"
	default:
		return "thing_metric_rollup_local_5m"
	}
}

// QueryRollup reads rollup rows for the given query window from the local
// SQLite. Auto-selects granularity via Granule(). Used by the agent IPC
// handler (packages/agent/internal/sync/status or platform bridge).
func (a *Aggregator) QueryRollup(ctx context.Context, q Query) ([]Row, error) {
	if !q.EndTime.After(q.StartTime) {
		return nil, nil
	}
	table := tableForGranule(Granule(q.StartTime, q.EndTime))

	where := []string{"bucket_start >= ?", "bucket_start < ?"}
	args := []any{q.StartTime.UTC().Format(time.RFC3339), q.EndTime.UTC().Format(time.RFC3339)}

	if len(q.MetricNames) > 0 {
		placeholders := make([]string, len(q.MetricNames))
		for i, m := range q.MetricNames {
			placeholders[i] = "?"
			args = append(args, m)
		}
		where = append(where, "metric_name IN ("+strings.Join(placeholders, ", ")+")")
	}
	if q.DimensionKey == "" {
		where = append(where, "dimension_key = ''")
	} else {
		where = append(where, "dimension_key LIKE ?")
		args = append(args, q.DimensionKey+"=%")
	}
	if q.SubDimension != "" {
		where = append(where, "sub_dimension = ?")
		args = append(args, q.SubDimension)
	}

	stmt := fmt.Sprintf(`
		SELECT bucket_start, metric_name, dimension_key, sub_dimension, value, metadata
		FROM %s
		WHERE %s
		ORDER BY bucket_start ASC
	`, table, strings.Join(where, " AND "))

	rows, err := a.db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("query rollup: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Row
	for rows.Next() {
		var r Row
		var bs string
		var meta sql.NullString
		if err := rows.Scan(&bs, &r.MetricName, &r.DimensionKey, &r.SubDimension, &r.Value, &meta); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		t, err := time.Parse(time.RFC3339Nano, bs)
		if err != nil {
			t, _ = time.Parse(time.RFC3339, bs)
		}
		r.BucketStart = t
		if meta.Valid {
			r.Metadata = meta.String
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate: %w", err)
	}
	return out, nil
}

// Retention purge

func (a *Aggregator) purge(ctx context.Context) error {
	now := time.Now().UTC()
	plans := []struct {
		table  string
		cutoff time.Time
	}{
		{"thing_metric_rollup_local_5m", now.Add(-a.retention.Keep5m)},
		{"thing_metric_rollup_local_1h", now.Add(-a.retention.Keep1h)},
		{"thing_metric_rollup_local_1d", now.Add(-a.retention.Keep1d)},
		{"thing_metric_rollup_local_1mo", now.Add(-a.retention.Keep1mo)},
	}
	for _, p := range plans {
		cutoff := p.cutoff.UTC().Format(time.RFC3339Nano)
		if _, err := a.db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE bucket_start < ?`, p.table), cutoff); err != nil {
			return fmt.Errorf("purge %s: %w", p.table, err)
		}
	}
	return nil
}

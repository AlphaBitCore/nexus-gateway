package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

// openRollupTestDB returns a *DB against DATABASE_URL, skipping the test
// when it is unset. Same skip contract as model_test.go +
// interception_domain_test.go.
func openRollupTestDB(t *testing.T) *DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping rollup-aware integration test")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	cfg.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect to DB: %v", err)
	}
	t.Cleanup(pool.Close)
	return &DB{Pool: pool, pool: pool}
}

// seedRollupRow inserts one row into the named rollup table for the test.
// Uses a unique sub-dimension keyed by the test name so concurrent test
// runs (or leftover rows from prior runs) cannot cross-contaminate.
func seedRollupRow(t *testing.T, db *DB, table string, bucket time.Time, metricName, dim, sub string, value float64) {
	t.Helper()
	_, err := db.Pool.Exec(context.Background(), `
		INSERT INTO "`+table+`" ("id", "bucketStart", "metricName", "dimensionKey", "subDimension", "value", "metadata", "updatedAt")
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, '{}'::jsonb, NOW())
		ON CONFLICT ("bucketStart", "metricName", "dimensionKey", "subDimension") DO UPDATE
			SET value = EXCLUDED.value, "updatedAt" = NOW()
	`, bucket, metricName, dim, sub, value)
	if err != nil {
		t.Fatalf("seed %s: %v", table, err)
	}
}

func setTestWatermark(t *testing.T, db *DB, jobName string, wm time.Time) {
	t.Helper()
	_, err := db.Pool.Exec(context.Background(), `
		INSERT INTO "rollup_watermark" ("jobName", "watermark", "updatedAt")
		VALUES ($1, $2, NOW())
		ON CONFLICT ("jobName") DO UPDATE
			SET "watermark" = EXCLUDED."watermark", "updatedAt" = NOW()
	`, jobName, wm)
	if err != nil {
		t.Fatalf("set watermark: %v", err)
	}
}

// restoreWatermark snapshots the merge-1h watermark before a test mutates
// it and restores the original on cleanup. Without this, the test would
// leave the production merge-1h watermark stuck at our anchor time and
// the next live merge tick would replay every 5m row from that point.
func restoreWatermark(t *testing.T, db *DB, jobName string) {
	t.Helper()
	original, err := db.GetWatermark(context.Background(), jobName)
	if err != nil {
		// No existing watermark — make sure cleanup deletes ours so we
		// leave the table the way we found it.
		t.Cleanup(func() {
			_, _ = db.Pool.Exec(context.Background(),
				`DELETE FROM "rollup_watermark" WHERE "jobName" = $1`, jobName)
		})
		return
	}
	t.Cleanup(func() {
		setTestWatermark(t, db, jobName, original)
	})
}

// TestQueryRollupAware_SealedAndTail covers the watermark-aware split
// behaviour. The range is sized > 6h so SelectGranularity picks 1h, the
// granularity that exercises the sealed/tail join. Each subtest isolates
// by subDimension so concurrent runs and pre-existing rows do not
// cross-contaminate.
func TestQueryRollupAware_SealedAndTail(t *testing.T) {
	db := openRollupTestDB(t)
	ctx := context.Background()
	restoreWatermark(t, db, "merge-1h")

	// Anchor on a stable point well in the past so the window cannot
	// drift into the live merge job's working range.
	wm := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC) // sealed/tail boundary
	hourBefore := wm.Add(-1 * time.Hour)               // sealed-side seed
	hourAfter := wm.Add(1 * time.Hour)                 // tail-side seed
	queryStart := wm.Add(-12 * time.Hour)              // > 6h span ⇒ Granularity1h
	queryEnd := wm.Add(2 * time.Hour)

	t.Run("range crossing watermark unions both tables", func(t *testing.T) {
		sub := "source=test_aware_cross"
		// Sealed bucket: 1h table at hourBefore.
		seedRollupRow(t, db, "metric_rollup_1h", hourBefore, "request_count", "model=m1", sub, 7)
		// Tail bucket: 5m table at hourAfter.
		seedRollupRow(t, db, "metric_rollup_5m", hourAfter, "request_count", "model=m1", sub, 5)
		setTestWatermark(t, db, "merge-1h", wm)

		got, err := db.QueryRollupAware(ctx, metrics.MetricsQuery{
			Metrics:      []string{"request_count"},
			DimensionKey: "model",
			SubDimension: sub,
			StartTime:    queryStart,
			EndTime:      queryEnd,
		})
		if err != nil {
			t.Fatalf("QueryRollupAware: %v", err)
		}
		var sum float64
		for _, r := range got {
			sum += r.Value
		}
		if sum != 12 {
			t.Errorf("sum = %v, want 12 (sealed 7 + tail 5)", sum)
		}
		if len(got) != 2 {
			t.Errorf("rows = %d, want 2 (one per source table)", len(got))
		}
	})

	t.Run("range fully sealed does not leak tail rows", func(t *testing.T) {
		sub := "source=test_aware_sealed"
		seedRollupRow(t, db, "metric_rollup_1h", hourBefore, "request_count", "model=m2", sub, 3)
		// Tail row that should NOT be selected — beyond the requested
		// EndTime, and the helper is asked to query strictly up to the
		// watermark.
		seedRollupRow(t, db, "metric_rollup_5m", hourAfter, "request_count", "model=m2", sub, 99)
		setTestWatermark(t, db, "merge-1h", wm)

		got, err := db.QueryRollupAware(ctx, metrics.MetricsQuery{
			Metrics:      []string{"request_count"},
			DimensionKey: "model",
			SubDimension: sub,
			StartTime:    queryStart,
			EndTime:      wm, // ends exactly at watermark
		})
		if err != nil {
			t.Fatalf("QueryRollupAware: %v", err)
		}
		var sum float64
		for _, r := range got {
			sum += r.Value
		}
		if sum != 3 {
			t.Errorf("sum = %v, want 3 (the 99 row in 5m must NOT leak in)", sum)
		}
	})

	t.Run("range fully past watermark only hits fine table", func(t *testing.T) {
		sub := "source=test_aware_tail"
		// Sealed row out of range — must NOT be summed.
		seedRollupRow(t, db, "metric_rollup_1h", hourBefore, "request_count", "model=m3", sub, 11)
		seedRollupRow(t, db, "metric_rollup_5m", hourAfter, "request_count", "model=m3", sub, 4)
		setTestWatermark(t, db, "merge-1h", wm)

		// Range > 6h forces 1h granularity even though it sits entirely
		// past the watermark — exercises the "boundary clamps to start"
		// path in QueryRollupAware.
		got, err := db.QueryRollupAware(ctx, metrics.MetricsQuery{
			Metrics:      []string{"request_count"},
			DimensionKey: "model",
			SubDimension: sub,
			StartTime:    wm,
			EndTime:      wm.Add(7 * time.Hour),
		})
		if err != nil {
			t.Fatalf("QueryRollupAware: %v", err)
		}
		var sum float64
		for _, r := range got {
			sum += r.Value
		}
		if sum != 4 {
			t.Errorf("sum = %v, want 4 (sealed 11 must NOT be included)", sum)
		}
	})

	t.Run("watermark before requested start uses tail table only", func(t *testing.T) {
		sub := "source=test_aware_watermark_before_start"
		// Sealed row well before our watermark — out of range.
		seedRollupRow(t, db, "metric_rollup_1h", hourBefore, "request_count", "model=m4", sub, 50)
		// Tail row inside the requested range.
		seedRollupRow(t, db, "metric_rollup_5m", hourAfter, "request_count", "model=m4", sub, 8)
		// Watermark earlier than queryStart ⇒ entire requested range is
		// "tail" by the helper's split rule. Verifies the boundary clamp
		// at queryStart works.
		setTestWatermark(t, db, "merge-1h", wm.Add(-3*time.Hour))

		got, err := db.QueryRollupAware(ctx, metrics.MetricsQuery{
			Metrics:      []string{"request_count"},
			DimensionKey: "model",
			SubDimension: sub,
			StartTime:    wm,
			EndTime:      wm.Add(7 * time.Hour),
		})
		if err != nil {
			t.Fatalf("QueryRollupAware: %v", err)
		}
		var sum float64
		for _, r := range got {
			sum += r.Value
		}
		if sum != 8 {
			t.Errorf("sum = %v, want 8 (sealed 50 must NOT be included; only tail 8)", sum)
		}
	})

	t.Run("5m granularity has no tail and queries 5m only", func(t *testing.T) {
		sub := "source=test_aware_5m_only"
		seedRollupRow(t, db, "metric_rollup_5m", hourAfter, "request_count", "model=m5", sub, 2)
		// Range ≤ 6h forces 5m granularity per metrics.SelectGranularity.
		got, err := db.QueryRollupAware(ctx, metrics.MetricsQuery{
			Metrics:      []string{"request_count"},
			DimensionKey: "model",
			SubDimension: sub,
			StartTime:    hourBefore,
			EndTime:      hourAfter.Add(time.Minute),
		})
		if err != nil {
			t.Fatalf("QueryRollupAware: %v", err)
		}
		var sum float64
		for _, r := range got {
			sum += r.Value
		}
		if sum != 2 {
			t.Errorf("sum = %v, want 2", sum)
		}
	})
}

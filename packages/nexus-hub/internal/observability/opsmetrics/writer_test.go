package opsmetrics

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// opsTestPool mirrors the jobs package's jobsTestPool pattern. Skips the test
// when the local Postgres test instance (port 55532, set by docker-compose
// dev-start) is unavailable, so unit-test runs without docker still pass.
//
// Test-data isolation: tests seed thing rows with fixed
// `test-opsmetrics-writer-*` ids and only DELETE those exact ids in
// cleanupTestThing. The FK CASCADE on metric_ops_raw means only rows
// belonging to the seeded thing are removed.
func opsTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("skip: DB unavailable (%v)", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Skipf("skip: DB ping failed (%v)", err)
	}
	return pool
}

// ensureTestThing inserts (or no-ops) a thing row so the foreign-key
// constraint on metric_ops_raw / thing_diag_event is satisfied. Returns the
// thing id passed in.
func ensureTestThing(t *testing.T, pool *pgxpool.Pool, id, ttype string) string {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
		INSERT INTO thing (id, name, type, status, last_seen_at, updated_at)
		VALUES ($1, $1, $2, 'online', NOW(), NOW())
		ON CONFLICT (id) DO NOTHING
	`, id, ttype)
	if err != nil {
		t.Fatalf("ensureTestThing(%s,%s): %v", id, ttype, err)
	}
	return id
}

// cleanupTestThing tears down the rows seeded by a writer test (in
// metric_ops_raw / thing_diag_event by FK CASCADE, and the thing itself).
func cleanupTestThing(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	ctx := context.Background()
	_, _ = pool.Exec(ctx, `DELETE FROM thing WHERE id = $1`, id)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestWriterFlushesBatch enqueues a small batch, calls FlushNow, and asserts
// the rows hit metric_ops_raw with the right column values for each metric
// kind (gauge value, counter value, histogram metadata buckets).
func TestWriterFlushesBatch(t *testing.T) {
	pool := opsTestPool(t)
	defer pool.Close()

	thingID := "test-opsmetrics-writer-1"
	ensureTestThing(t, pool, thingID, "agent")
	defer cleanupTestThing(t, pool, thingID)

	w := NewWriter(pool, discardLogger(), 100, 50*time.Millisecond)
	t.Cleanup(func() { _ = w.Stop(context.Background()) })

	now := time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC)
	batch := opsmetrics.SampleBatch{
		ThingID:   thingID,
		SampledAt: now,
		Samples: []opsmetrics.Sample{
			{Name: "runtime.heap_alloc_bytes", Kind: opsmetrics.KindGauge, DimensionKey: "", Value: 12345.0},
			{Name: "relay.dial_total", Kind: opsmetrics.KindCounter, DimensionKey: "mode=new", Value: 42.0},
			{
				Name:         "hook.pipeline_ms",
				Kind:         opsmetrics.KindHistogram,
				DimensionKey: "stage=request",
				Metadata:     map[string]any{"buckets": []int{10, 5, 2, 1, 0, 0}},
			},
		},
	}
	if err := w.Enqueue(context.Background(), thingID, "agent", batch); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if err := w.FlushNow(context.Background()); err != nil {
		t.Fatalf("FlushNow: %v", err)
	}

	ctx := context.Background()
	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM metric_ops_raw WHERE thing_id = $1`, thingID).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 3 {
		t.Fatalf("metric_ops_raw rows = %d, want 3", count)
	}

	// Spot-check one gauge row.
	var (
		gotKind string
		gotDim  string
		gotVal  *float64
	)
	if err := pool.QueryRow(ctx, `
		SELECT metric_kind, dimension_key, value
		  FROM metric_ops_raw
		 WHERE thing_id = $1 AND metric_name = 'runtime.heap_alloc_bytes'
	`, thingID).Scan(&gotKind, &gotDim, &gotVal); err != nil {
		t.Fatalf("scan gauge row: %v", err)
	}
	if gotKind != "gauge" {
		t.Errorf("kind = %q, want gauge", gotKind)
	}
	if gotDim != "" {
		t.Errorf("dim = %q, want empty", gotDim)
	}
	if gotVal == nil || *gotVal != 12345.0 {
		t.Errorf("value = %v, want 12345", gotVal)
	}

	// Histogram row should carry buckets in metadata JSONB.
	var metaJSON []byte
	if err := pool.QueryRow(ctx, `
		SELECT metadata
		  FROM metric_ops_raw
		 WHERE thing_id = $1 AND metric_name = 'hook.pipeline_ms'
	`, thingID).Scan(&metaJSON); err != nil {
		t.Fatalf("scan hist row: %v", err)
	}
	if len(metaJSON) == 0 {
		t.Fatal("histogram metadata must not be empty")
	}
}

// TestWriterDropsOnOverflow exercises the non-blocking Enqueue contract: a
// capacity-1 channel filled past the goroutine's drain rate must drop newer
// payloads and increment the dropped counter without returning an error.
func TestWriterDropsOnOverflow(t *testing.T) {
	pool := opsTestPool(t)
	defer pool.Close()

	thingID := "test-opsmetrics-writer-overflow"
	ensureTestThing(t, pool, thingID, "agent")
	defer cleanupTestThing(t, pool, thingID)

	// Capacity 1 with a long flush latency keeps the background loop idle
	// long enough that subsequent Enqueues can't be drained.
	w := NewWriter(pool, discardLogger(), 1, 1*time.Hour)
	t.Cleanup(func() { _ = w.Stop(context.Background()) })

	now := time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC)
	mkBatch := func(i int) opsmetrics.SampleBatch {
		return opsmetrics.SampleBatch{
			ThingID:   thingID,
			SampledAt: now,
			Samples: []opsmetrics.Sample{
				{Name: "test.counter", Kind: opsmetrics.KindCounter, DimensionKey: "", Value: float64(i)},
			},
		}
	}
	for i := range 200 {
		if err := w.Enqueue(context.Background(), thingID, "agent", mkBatch(i)); err != nil {
			t.Fatalf("Enqueue must not error on overflow, got: %v", err)
		}
	}
	if dropped := w.Dropped(); dropped == 0 {
		t.Errorf("dropped counter = 0, expected > 0 under overflow")
	}
}

package consumer

import (
	"context"
	"database/sql"
	"errors"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestTrafficWriter builds a TrafficEventWriter with a nil pool. Safe for
// tests that exercise only the handler path (handleMessage) — never call
// flush() through this writer, as the nil pool will panic.
func newTestTrafficWriter(t *testing.T) *TrafficEventWriter {
	t.Helper()
	return NewTrafficEventWriter(
		nil, // pool unused — handler path never flushes here
		nil, // consumer unused — tests invoke handleMessage directly
		TrafficEventWriterConfig{BatchSize: 100, FlushInterval: 24 * time.Hour},
		discardLogger(),
		newTestRegistry(),
	)
}

// noopBatch returns a batch accumulator whose flushFn never fires in tests
// (BatchSize=100 plus no items drained). Caller should not call Stop().
func noopBatch() *BatchAccumulator[pendingTrafficMessage] {
	return NewBatchAccumulator[pendingTrafficMessage](100, 24*time.Hour, func([]pendingTrafficMessage) error {
		return nil
	})
}

// TestTrafficEventWriter_HandleMessage_ReturnsErrDeferAck verifies the C3 fix:
// after a successful batch.Add, the handler returns mq.ErrDeferAck so the MQ
// driver does NOT auto-ack. The message's Ack() must fire only later, when
// ackAll runs after a successful DB flush.
func TestTrafficEventWriter_HandleMessage_ReturnsErrDeferAck(t *testing.T) {
	w := newTestTrafficWriter(t)
	batch := noopBatch()

	var ackCount, nakCount int32
	msg := &mq.Message{
		Data: []byte(`{"id":"evt-1","source":"ai-gateway","timestamp":"2026-04-18T00:00:00Z"}`),
		Ack:  func() error { atomic.AddInt32(&ackCount, 1); return nil },
		Nak:  func() error { atomic.AddInt32(&nakCount, 1); return nil },
	}

	err := w.handleMessage("nexus.event.ai-traffic", batch, msg)
	if !errors.Is(err, mq.ErrDeferAck) {
		t.Errorf("handleMessage returned %v; want mq.ErrDeferAck", err)
	}
	if got := atomic.LoadInt32(&ackCount); got != 0 {
		t.Errorf("Ack called %d times; want 0 (ack must defer until flush)", got)
	}
	if got := atomic.LoadInt32(&nakCount); got != 0 {
		t.Errorf("Nak called %d times; want 0", got)
	}
}

// testPool returns a live *pgxpool.Pool against the dev database. Tests that
// exercise the insert path need real schema validation — a nil pool cannot
// catch column-count / placeholder mismatches. Skips cleanly when DB is
// unavailable so `go test ./...` still passes on unconfigured hosts.
//
// Tests using this pool MUST only INSERT/DELETE rows they themselves created
// (scoped by their own unique id, never by table-wide TRUNCATE or wildcard
// DELETE). The tests below comply by using fixed `te-p-*` event ids and
// cleaning up only those rows. No NEXUS_DESTRUCTIVE_TESTS gate needed.
func testPool(t *testing.T) *pgxpool.Pool {
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

// TestTrafficConsumer_WritesInternalPurpose pins the MQ→DB mapping for the
// internal_purpose column added in Task 14. Events published with
// InternalPurpose="ai-guard" must land on traffic_event.internal_purpose so
// admin analytics can exclude them from customer billing views (Task 15).
func TestTrafficConsumer_WritesInternalPurpose(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()
	ctx := context.Background()
	const eventID = "te-p-b-14-1"

	// Pre-clean any stale row so reruns are deterministic.
	_, _ = pool.Exec(ctx, `DELETE FROM traffic_event WHERE id = $1`, eventID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM traffic_event WHERE id = $1`, eventID)
	})

	purpose := "ai-guard"
	msg := TrafficEventMessage{
		ID:              eventID,
		Source:          "ai-gateway",
		Timestamp:       time.Now(),
		InternalPurpose: &purpose,
	}

	w := NewTrafficEventWriter(
		pool, nil,
		TrafficEventWriterConfig{BatchSize: 1, FlushInterval: 24 * time.Hour},
		discardLogger(),
		newTestRegistry(),
	)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := w.InsertTrafficEventsForTest(ctx, tx, []TrafficEventMessage{msg}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var got sql.NullString
	if err := pool.QueryRow(ctx,
		`SELECT internal_purpose FROM traffic_event WHERE id = $1`, eventID).Scan(&got); err != nil {
		t.Fatalf("select: %v", err)
	}
	if !got.Valid || got.String != "ai-guard" {
		t.Fatalf("internal_purpose: got %#v; want 'ai-guard'", got)
	}
}

// TestTrafficConsumer_WritesBlockingRule pins the MQ→DB mapping for the
// request_blocking_rule JSONB column. Events published with
// RequestBlockingRule={pack, pack_version, rule_id} must land on
// traffic_event.request_blocking_rule so the admin UI traffic-event
// detail view can surface which rule pack triggered the rejection.
// (Originally wrote to a single blocking_rule column; the pipeline was
// split into request_/response_ blocking_rule pair.)
func TestTrafficConsumer_WritesBlockingRule(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()
	ctx := context.Background()
	const eventID = "te-p-c-15-1"

	// Pre-clean any stale row so reruns are deterministic.
	_, _ = pool.Exec(ctx, `DELETE FROM traffic_event WHERE id = $1`, eventID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM traffic_event WHERE id = $1`, eventID)
	})

	br := json.RawMessage(`{"pack":"nexus/prompt-injection","pack_version":"v1.0.0","rule_id":"pi-io-001"}`)
	msg := TrafficEventMessage{
		ID:                  eventID,
		Source:              "ai-gateway",
		Timestamp:           time.Now(),
		RequestBlockingRule: br,
	}

	w := NewTrafficEventWriter(
		pool, nil,
		TrafficEventWriterConfig{BatchSize: 1, FlushInterval: 24 * time.Hour},
		discardLogger(),
		newTestRegistry(),
	)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := w.InsertTrafficEventsForTest(ctx, tx, []TrafficEventMessage{msg}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var got []byte
	if err := pool.QueryRow(ctx,
		`SELECT request_blocking_rule FROM traffic_event WHERE id = $1`, eventID).Scan(&got); err != nil {
		t.Fatalf("read request_blocking_rule: %v", err)
	}
	var decoded map[string]string
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["pack"] != "nexus/prompt-injection" || decoded["rule_id"] != "pi-io-001" || decoded["pack_version"] != "v1.0.0" {
		t.Fatalf("decoded: %+v", decoded)
	}
}

// TestTrafficEventWriter_HandleMessage_AcksPoisonPill verifies that undeserializable
// messages are acked inline (dropped as poison pills) rather than deferred.
// This preserves the existing poison-pill behavior after the C3 fix.
func TestTrafficEventWriter_HandleMessage_AcksPoisonPill(t *testing.T) {
	w := newTestTrafficWriter(t)
	batch := noopBatch()

	var ackCount int32
	msg := &mq.Message{
		Data: []byte(`not json{{{`),
		Ack:  func() error { atomic.AddInt32(&ackCount, 1); return nil },
		Nak:  func() error { return nil },
	}

	err := w.handleMessage("nexus.event.ai-traffic", batch, msg)
	if err != nil {
		t.Errorf("handleMessage returned %v; want nil (poison pill acked inline)", err)
	}
	if got := atomic.LoadInt32(&ackCount); got != 1 {
		t.Errorf("Ack called %d times; want 1", got)
	}
}

func TestDrainWorkersPerQueue_defaultBandAndEnv(t *testing.T) {
	t.Setenv("NEXUS_HUB_DRAIN_WORKERS", "")
	if got := drainWorkersPerQueue(); got < 2 || got > 6 {
		t.Fatalf("default must clamp to [2,6], got %d", got)
	}
	t.Setenv("NEXUS_HUB_DRAIN_WORKERS", "11")
	if got := drainWorkersPerQueue(); got != 11 {
		t.Fatalf("explicit env overrides the band: got %d want 11", got)
	}
	// Non-positive / unparseable values fall back to the CPU-derived default band.
	for _, bad := range []string{"0", "-3", "x"} {
		t.Setenv("NEXUS_HUB_DRAIN_WORKERS", bad)
		if got := drainWorkersPerQueue(); got < 2 || got > 6 {
			t.Fatalf("bad env %q must fall back to [2,6], got %d", bad, got)
		}
	}
}

package opsmetrics

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// TestDiagWriterInserts verifies a single DiagEvent flows through Enqueue +
// FlushNow and lands in thing_diag_event with the right column values.
func TestDiagWriterInserts(t *testing.T) {
	pool := opsTestPool(t)
	defer pool.Close()

	thingID := "test-opsmetrics-diag-1"
	ensureTestThing(t, pool, thingID, "agent")
	defer cleanupTestThing(t, pool, thingID)

	w := NewDiagWriter(pool, discardLogger(), 100, 50*time.Millisecond)
	t.Cleanup(func() { _ = w.Stop(context.Background()) })

	now := time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC)
	evt := opsmetrics.DiagEvent{
		ThingID:      thingID,
		OccurredAt:   now,
		Level:        "error",
		EventType:    "error",
		Source:       "relay",
		Message:      "dial to upstream failed",
		MessageHash:  "abc123",
		Attrs:        map[string]any{"upstream": "api.openai.com:443"},
		RepeatCount:  1,
		AgentVersion: "v1.4.2",
		OSInfo:       map[string]any{"os": "darwin"},
	}
	if err := w.Enqueue(context.Background(), thingID, "agent", evt); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := w.FlushNow(context.Background()); err != nil {
		t.Fatalf("FlushNow: %v", err)
	}

	ctx := context.Background()
	var (
		level, eventType, source, message, msgHash string
		repeatCount                                int32
		agentVersion                               *string
	)
	if err := pool.QueryRow(ctx, `
		SELECT level, event_type, source, message, message_hash, repeat_count, agent_version
		  FROM thing_diag_event
		 WHERE thing_id = $1
	`, thingID).Scan(&level, &eventType, &source, &message, &msgHash, &repeatCount, &agentVersion); err != nil {
		t.Fatalf("query thing_diag_event: %v", err)
	}
	if level != "error" || eventType != "error" || source != "relay" {
		t.Errorf("level/eventType/source = %q/%q/%q", level, eventType, source)
	}
	if message != "dial to upstream failed" {
		t.Errorf("message = %q", message)
	}
	if msgHash != "abc123" {
		t.Errorf("message_hash = %q, want abc123 (must not be backfilled when client provided it)", msgHash)
	}
	if repeatCount != 1 {
		t.Errorf("repeat_count = %d, want 1", repeatCount)
	}
	if agentVersion == nil || *agentVersion != "v1.4.2" {
		t.Errorf("agent_version = %v", agentVersion)
	}
}

// TestDiagWriterBackfillsZeroOccurredAt asserts the writer fills
// occurred_at with the current UTC time when the wire payload omits it.
// Without this guard a caller emitting a bare DiagEvent literal (Go
// zero time = 0001-01-01) lands a column value that breaks CP UI's
// time-DESC sort: the row sinks to the bottom of every query, and
// the admin sees the events but can't reason about WHEN they
// happened. Mirrors the same guard the HTTP-drain path applies in
// insertDiagDrainEvent — keep the two paths consistent. See
// [[server-lifecycle-zero-time-bug]].
func TestDiagWriterBackfillsZeroOccurredAt(t *testing.T) {
	pool := opsTestPool(t)
	defer pool.Close()

	thingID := "test-opsmetrics-diag-zerotime"
	ensureTestThing(t, pool, thingID, "agent")
	defer cleanupTestThing(t, pool, thingID)

	w := NewDiagWriter(pool, discardLogger(), 100, 50*time.Millisecond)
	t.Cleanup(func() { _ = w.Stop(context.Background()) })

	// Emit a DiagEvent with OccurredAt left zero — the production bug
	// shape that nexus-hub / control-plane / ai-gateway / compliance-
	// proxy emitted at start time before the matching service-side
	// fix.
	before := time.Now().UTC().Add(-1 * time.Second)
	evt := opsmetrics.DiagEvent{
		ThingID:   thingID,
		Level:     "info",
		EventType: "lifecycle",
		Source:    "test-service",
		Message:   "test-service started",
		// OccurredAt: deliberately not set.
	}
	if err := w.Enqueue(context.Background(), thingID, "agent", evt); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := w.FlushNow(context.Background()); err != nil {
		t.Fatalf("FlushNow: %v", err)
	}
	after := time.Now().UTC().Add(1 * time.Second)

	var occurredAt time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT occurred_at FROM thing_diag_event WHERE thing_id = $1`,
		thingID,
	).Scan(&occurredAt); err != nil {
		t.Fatalf("query occurred_at: %v", err)
	}
	if occurredAt.IsZero() {
		t.Fatal("occurred_at is Go zero time — fallback did not fire")
	}
	if occurredAt.Year() < 2000 {
		// Catches the older bug shape where the column took the
		// 0001-01-01 value literally (Postgres accepts it).
		t.Fatalf("occurred_at = %v looks like Go zero time leaked in", occurredAt)
	}
	if occurredAt.Before(before) || occurredAt.After(after) {
		t.Errorf("occurred_at = %v, want between %v and %v (ingest-time fallback)",
			occurredAt, before, after)
	}
}

// TestDiagWriterBackfillsMessageHash asserts the writer computes a stable
// md5(level|source|firstStackOrMessage) when the wire payload omits
// message_hash. Matches the agent's slog-sink algorithm so server-computed
// hashes collide with client-computed hashes for the same logical event.
func TestDiagWriterBackfillsMessageHash(t *testing.T) {
	pool := opsTestPool(t)
	defer pool.Close()

	thingID := "test-opsmetrics-diag-backfill"
	ensureTestThing(t, pool, thingID, "agent")
	defer cleanupTestThing(t, pool, thingID)

	w := NewDiagWriter(pool, discardLogger(), 100, 50*time.Millisecond)
	t.Cleanup(func() { _ = w.Stop(context.Background()) })

	now := time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC)

	// Event 1: no stack trace — hash = md5(level|source|message).
	evt1 := opsmetrics.DiagEvent{
		ThingID:     thingID,
		OccurredAt:  now,
		Level:       "error",
		EventType:   "error",
		Source:      "relay",
		Message:     "dial failed",
		MessageHash: "", // omitted on wire — backfill path
		RepeatCount: 1,
	}
	want1 := md5HashHex("error|relay|dial failed")

	// Event 2: stack trace present — first frame is the line after the
	// first '\n'. Use a different OccurredAt so the (thing_id, occurred_at)
	// pair is distinct.
	stack := "goroutine 1 [running]:\nmain.crash()\n\t/app/main.go:42"
	wantFirstFrame := "main.crash()"
	evt2 := opsmetrics.DiagEvent{
		ThingID:     thingID,
		OccurredAt:  now.Add(time.Second),
		Level:       "fatal",
		EventType:   "crash",
		Source:      "main",
		Message:     "runtime error: nil pointer",
		MessageHash: "",
		StackTrace:  stack,
		RepeatCount: 1,
	}
	want2 := md5HashHex("fatal|main|" + wantFirstFrame)

	for _, e := range []opsmetrics.DiagEvent{evt1, evt2} {
		if err := w.Enqueue(context.Background(), thingID, "agent", e); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}
	if err := w.FlushNow(context.Background()); err != nil {
		t.Fatalf("FlushNow: %v", err)
	}

	ctx := context.Background()
	var got1 string
	if err := pool.QueryRow(ctx, `
		SELECT message_hash FROM thing_diag_event
		 WHERE thing_id = $1 AND message = 'dial failed'
	`, thingID).Scan(&got1); err != nil {
		t.Fatalf("scan evt1: %v", err)
	}
	if got1 != want1 {
		t.Errorf("evt1 message_hash = %q, want %q (md5 of level|source|message)", got1, want1)
	}

	var got2 string
	if err := pool.QueryRow(ctx, `
		SELECT message_hash FROM thing_diag_event
		 WHERE thing_id = $1 AND event_type = 'crash'
	`, thingID).Scan(&got2); err != nil {
		t.Fatalf("scan evt2: %v", err)
	}
	if got2 != want2 {
		t.Errorf("evt2 message_hash = %q, want %q (md5 of level|source|firstStackFrame)", got2, want2)
	}
}

// TestDiagWriterDropsOnOverflow mirrors the SampleWriter overflow contract:
// capacity-1 channel with a long latency, prove non-blocking semantics +
// dropped counter increments.
func TestDiagWriterDropsOnOverflow(t *testing.T) {
	pool := opsTestPool(t)
	defer pool.Close()

	thingID := "test-opsmetrics-diag-overflow"
	ensureTestThing(t, pool, thingID, "agent")
	defer cleanupTestThing(t, pool, thingID)

	w := NewDiagWriter(pool, discardLogger(), 1, 1*time.Hour)
	t.Cleanup(func() { _ = w.Stop(context.Background()) })

	now := time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC)
	mkEvt := func(i int) opsmetrics.DiagEvent {
		return opsmetrics.DiagEvent{
			ThingID:     thingID,
			OccurredAt:  now,
			Level:       "warn",
			EventType:   "error",
			Source:      "test",
			Message:     "overflow",
			MessageHash: "x",
			RepeatCount: 1,
		}
	}
	for i := range 200 {
		if err := w.Enqueue(context.Background(), thingID, "agent", mkEvt(i)); err != nil {
			t.Fatalf("Enqueue must not error on overflow, got: %v", err)
		}
	}
	if dropped := w.Dropped(); dropped == 0 {
		t.Errorf("dropped counter = 0, expected > 0 under overflow")
	}
}

// md5HashHex is the in-test mirror of the writer's computeMessageHash. Kept
// independent so a refactor of the production helper that breaks the contract
// shows up as a test failure.
func md5HashHex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestDiagWriterColsMatchSchema reads information_schema.columns for
// thing_diag_event and asserts the production diagEventCols slice matches
// the live table 1:1 in both name set and order. This is the schema-drift
// guard that catches the column-drift incident class — a renamed or
// dropped column on the table without an update here would silently
// break every batch insert with `42703 column ... does not exist` and
// wedge the diag pipeline for hours.
//
// Order matters because pgx.CopyFrom binds positional values; if the
// schema reorders columns (rare but possible on a DROP+ADD cycle), the
// COPY would succeed but write wrong values into wrong columns.
func TestDiagWriterColsMatchSchema(t *testing.T) {
	pool := opsTestPool(t)
	defer pool.Close()

	rows, err := pool.Query(context.Background(), `
		SELECT column_name
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'thing_diag_event'
		ORDER BY ordinal_position`)
	if err != nil {
		t.Fatalf("query information_schema: %v", err)
	}
	defer rows.Close()
	var schemaCols []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		schemaCols = append(schemaCols, name)
	}
	if len(schemaCols) == 0 {
		t.Fatalf("thing_diag_event has 0 columns — schema missing?")
	}

	// Schema name-set MUST match the writer slice name-set exactly.
	// (Order is asserted below; this first pass surfaces the actual
	// missing/extra names so a failure message is actionable.)
	schemaSet := map[string]bool{}
	for _, c := range schemaCols {
		schemaSet[c] = true
	}
	writerSet := map[string]bool{}
	for _, c := range diagEventCols {
		writerSet[c] = true
	}
	for c := range schemaSet {
		if !writerSet[c] {
			t.Errorf("schema has column %q but diagEventCols does not — pgx.CopyFrom will leave it unset on every insert", c)
		}
	}
	for c := range writerSet {
		if !schemaSet[c] {
			t.Errorf("diagEventCols has %q but the table doesn't — every batch will fail with 42703", c)
		}
	}

	// Order check: pgx.CopyFrom binds positionally. If the writer's order
	// drifts from the table's ordinal_position, the COPY would still
	// succeed (column-named CopyFrom is positional on the columns slice,
	// not the table), so this catches the silent-write-to-wrong-column
	// regression rather than a hard failure.
	if len(schemaCols) == len(diagEventCols) {
		for i := range schemaCols {
			if schemaCols[i] != diagEventCols[i] {
				t.Errorf("col[%d]: schema=%q writer=%q — order drift between table ordinal_position and diagEventCols", i, schemaCols[i], diagEventCols[i])
			}
		}
	}
}

// TestDiagWriterPromDropCounterIncrements verifies that when SetDropCounter
// is wired, overflow drops bump the Prometheus instrument with
// reason="queue_overflow" — not just the in-memory atomic. This is the
// observability gap that hid a 16-hour audit-pipeline outage;
// the prod operator-facing /metrics surface needs to show drops so they can
// be alerted on.
func TestDiagWriterPromDropCounterIncrements(t *testing.T) {
	pool := opsTestPool(t)
	defer pool.Close()

	thingID := "test-opsmetrics-diag-prom-drop"
	ensureTestThing(t, pool, thingID, "agent")
	defer cleanupTestThing(t, pool, thingID)

	promReg := prometheus.NewRegistry()
	reg := opsmetrics.NewRegistry(promReg)
	dropCounter := reg.NewCounter("diag.dropped_total", []string{"reason"})

	// capacity=1 + 1-hour latency forces a guaranteed overflow on the 2nd+
	// Enqueue, because the writer goroutine can't drain fast enough.
	w := NewDiagWriter(pool, discardLogger(), 1, 1*time.Hour)
	w.SetDropCounter(dropCounter)
	t.Cleanup(func() { _ = w.Stop(context.Background()) })

	evt := opsmetrics.DiagEvent{
		ThingID:     thingID,
		OccurredAt:  time.Now().UTC(),
		Level:       "error",
		EventType:   "error",
		Source:      "test",
		Message:     "overflow",
		MessageHash: "x",
		RepeatCount: 1,
	}
	for range 50 {
		_ = w.Enqueue(context.Background(), thingID, "agent", evt)
	}

	families, err := promReg.Gather()
	if err != nil {
		t.Fatalf("promReg.Gather: %v", err)
	}
	var got float64
	for _, mf := range families {
		if mf.GetName() != "nexus_diag_dropped_total" {
			continue
		}
		for _, m := range mf.Metric {
			for _, lbl := range m.Label {
				if lbl.GetName() == "reason" && lbl.GetValue() == "queue_overflow" {
					got = m.Counter.GetValue()
				}
			}
		}
	}
	if got <= 0 {
		t.Errorf("diag_dropped_total{reason=\"queue_overflow\"} = %v, expected > 0 after overflow", got)
	}
}

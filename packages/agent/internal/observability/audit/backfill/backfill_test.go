// Coverage for backfill.go: E50BackfillLatencyPhases end-to-end +
// sumStageLatenciesFromBlob/computeResidualUpstream/nullableIntFromPtr
// helpers.
//
// The agent's encrypted SQLite store is queried directly — no test
// fakes — so the WHERE NULL filter, COALESCE update, batch loop, and
// non-stage-row noop all surface as observable row state changes.
package backfill

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/queue"
)

// newTestDB opens a temporary encrypted Queue and returns its underlying
// *sql.DB handle plus a cleanup function. Using queue.NewQueue ensures the
// full schema (including audit_events columns) is present.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "audit.db")
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	q, err := queue.NewQueue(dbPath, key)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q.DB()
}

// insertBackfillRow writes a row directly via the *sql.DB so the test
// can simulate an older row without latency phases (NULL latency phases + a populated
// hooks_pipeline / duration_ms). Mirrors what Record() would have
// written before request_hooks_ms / response_hooks_ms columns existed.
func insertBackfillRow(t *testing.T, db *sql.DB, id string, duration any, hooksPipeline any) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO audit_events
		  (id, timestamp, source_process, dest_host, dest_ip, dest_port,
		   action, duration_ms, hooks_pipeline)
		VALUES (?, datetime('now'), 'p', 'h', '1.2.3.4', 443, 'inspect', ?, ?)`,
		id, duration, hooksPipeline,
	)
	if err != nil {
		t.Fatalf("insert backfill row %s: %v", id, err)
	}
}

func TestE50Backfill_NilLoggerDefault(t *testing.T) {
	db := newTestDB(t)
	// No rows: should still succeed under a nil logger (default fill).
	if err := E50BackfillLatencyPhases(context.Background(), db, nil); err != nil {
		t.Fatalf("E50Backfill (nil logger): %v", err)
	}
}

func TestE50Backfill_PopulatesFromHooksPipeline(t *testing.T) {
	db := newTestDB(t)
	// Older row with: duration=500ms, 2 request-stage hooks (60+40),
	// 1 response-stage hook (30). Expected backfill: req=100, resp=30,
	// upstream_total = max(0, 500-100-30) = 370.
	hooks := `[
		{"hook":"pii","stage":"request","latencyMs":60},
		{"hook":"rate","stage":"connection","latencyMs":40},
		{"hook":"safety","stage":"response","latencyMs":30}
	]`
	insertBackfillRow(t, db, "ev-1", 500, hooks)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := E50BackfillLatencyPhases(context.Background(), db, logger); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	var req, resp, total sql.NullInt64
	if err := db.QueryRow(
		`SELECT request_hooks_ms, response_hooks_ms, upstream_total_ms FROM audit_events WHERE id = ?`,
		"ev-1",
	).Scan(&req, &resp, &total); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !req.Valid || req.Int64 != 100 {
		t.Errorf("request_hooks_ms: got %v, want 100 (60+40 across request/connection stages)", req)
	}
	if !resp.Valid || resp.Int64 != 30 {
		t.Errorf("response_hooks_ms: got %v, want 30", resp)
	}
	if !total.Valid || total.Int64 != 370 {
		t.Errorf("upstream_total_ms: got %v, want 370 (500-100-30)", total)
	}
}

func TestE50Backfill_IdempotentSkipsAlreadyFilled(t *testing.T) {
	db := newTestDB(t)
	// Insert a row that's ALREADY filled. Backfill scan WHERE clause
	// excludes rows with both request_hooks_ms and upstream_total_ms
	// non-NULL, so this row must remain untouched even with a
	// hooks_pipeline payload that would otherwise contribute.
	_, err := db.Exec(`
		INSERT INTO audit_events
		  (id, timestamp, source_process, dest_host, dest_ip, dest_port,
		   action, duration_ms, request_hooks_ms, response_hooks_ms,
		   upstream_total_ms, hooks_pipeline)
		VALUES (?, datetime('now'), 'p', 'h', '1.2.3.4', 443,
		        'inspect', 200, 50, 25, 125, ?)`,
		"already-filled", `[{"stage":"request","latencyMs":999}]`,
	)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	if err := E50BackfillLatencyPhases(context.Background(), db, nil); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	var req sql.NullInt64
	if err := db.QueryRow(`SELECT request_hooks_ms FROM audit_events WHERE id = ?`, "already-filled").Scan(&req); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !req.Valid || req.Int64 != 50 {
		t.Errorf("idempotency violated: request_hooks_ms changed from 50 to %v", req)
	}
}

func TestE50Backfill_NoHooksPipelineYieldsNilHookCols(t *testing.T) {
	db := newTestDB(t)
	// Row with duration but no hooks_pipeline — older agent. Expected: req=NULL, resp=NULL, upstream_total=duration.
	insertBackfillRow(t, db, "no-pipeline", 250, nil)

	if err := E50BackfillLatencyPhases(context.Background(), db, nil); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	var req, resp, total sql.NullInt64
	if err := db.QueryRow(
		`SELECT request_hooks_ms, response_hooks_ms, upstream_total_ms FROM audit_events WHERE id = ?`,
		"no-pipeline",
	).Scan(&req, &resp, &total); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if req.Valid {
		t.Errorf("request_hooks_ms: must remain NULL with no pipeline; got %v", req.Int64)
	}
	if resp.Valid {
		t.Errorf("response_hooks_ms: must remain NULL with no pipeline; got %v", resp.Int64)
	}
	if !total.Valid || total.Int64 != 250 {
		t.Errorf("upstream_total_ms: got %v, want 250 (duration passthrough)", total)
	}
}

func TestE50Backfill_NullDurationLeavesUpstreamNULL(t *testing.T) {
	db := newTestDB(t)
	// No duration AND no pipeline: every output column stays NULL.
	insertBackfillRow(t, db, "no-anything", nil, nil)
	if err := E50BackfillLatencyPhases(context.Background(), db, nil); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	var req, resp, total sql.NullInt64
	_ = db.QueryRow(
		`SELECT request_hooks_ms, response_hooks_ms, upstream_total_ms FROM audit_events WHERE id = ?`,
		"no-anything",
	).Scan(&req, &resp, &total)
	if req.Valid || resp.Valid || total.Valid {
		t.Errorf("expected all cols NULL with no duration + no pipeline; got req=%v resp=%v total=%v", req, resp, total)
	}
}

func TestE50Backfill_BadJSONFallsBackToNilHooks(t *testing.T) {
	db := newTestDB(t)
	// Malformed JSON → sumStageLatenciesFromBlob returns (nil,nil).
	// Then computeResidualUpstream sees duration=400 + nil hooks →
	// upstream_total = 400.
	insertBackfillRow(t, db, "bad-json", 400, "{this-is-not-json")
	if err := E50BackfillLatencyPhases(context.Background(), db, nil); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	var total sql.NullInt64
	_ = db.QueryRow(`SELECT upstream_total_ms FROM audit_events WHERE id = ?`, "bad-json").Scan(&total)
	if !total.Valid || total.Int64 != 400 {
		t.Errorf("upstream_total: got %v, want 400", total)
	}
}

func TestE50Backfill_NegativeResidualClampsToZero(t *testing.T) {
	db := newTestDB(t)
	// Hook latencies exceed duration → residual would be negative;
	// computeResidualUpstream clamps to 0.
	hooks := `[{"stage":"request","latencyMs":900},{"stage":"response","latencyMs":900}]`
	insertBackfillRow(t, db, "clamp", 100, hooks)
	if err := E50BackfillLatencyPhases(context.Background(), db, nil); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	var total sql.NullInt64
	_ = db.QueryRow(`SELECT upstream_total_ms FROM audit_events WHERE id = ?`, "clamp").Scan(&total)
	if !total.Valid || total.Int64 != 0 {
		t.Errorf("upstream_total: clamp to 0 expected; got %v", total)
	}
}

func TestE50Backfill_MultiBatch(t *testing.T) {
	db := newTestDB(t)
	// Insert >500 rows so the batchSize=500 loop iterates twice.
	for i := 0; i < 550; i++ {
		insertBackfillRow(t, db, "mb-"+itoa(i), 100, `[{"stage":"request","latencyMs":10}]`)
	}
	if err := E50BackfillLatencyPhases(context.Background(), db, nil); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	// Re-running should be a no-op (everything is now filled).
	if err := E50BackfillLatencyPhases(context.Background(), db, nil); err != nil {
		t.Fatalf("backfill rerun: %v", err)
	}
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE request_hooks_ms IS NULL OR upstream_total_ms IS NULL`).Scan(&n)
	if n != 0 {
		t.Errorf("unfilled rows remain after backfill: %d", n)
	}
}

func TestE50Backfill_ContextCancellationSurfaces(t *testing.T) {
	db := newTestDB(t)
	// Pre-seed a row so the query has something to scan.
	insertBackfillRow(t, db, "ctx", 100, `[{"stage":"request","latencyMs":10}]`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediate cancel
	err := E50BackfillLatencyPhases(ctx, db, nil)
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
}

// sumStageLatenciesFromBlob — every parse arm

func TestSumStageLatenciesFromBlob_NullAndEmpty(t *testing.T) {
	if req, resp := sumStageLatenciesFromBlob(sql.NullString{Valid: false}); req != nil || resp != nil {
		t.Errorf("invalid NullString must produce (nil,nil); got (%v,%v)", req, resp)
	}
	if req, resp := sumStageLatenciesFromBlob(sql.NullString{Valid: true, String: ""}); req != nil || resp != nil {
		t.Errorf("empty NullString must produce (nil,nil); got (%v,%v)", req, resp)
	}
	if req, resp := sumStageLatenciesFromBlob(sql.NullString{Valid: true, String: "garbage"}); req != nil || resp != nil {
		t.Errorf("invalid JSON must produce (nil,nil); got (%v,%v)", req, resp)
	}
	// Empty array: parses, but len(rows)==0 → (nil,nil).
	if req, resp := sumStageLatenciesFromBlob(sql.NullString{Valid: true, String: "[]"}); req != nil || resp != nil {
		t.Errorf("empty array must produce (nil,nil); got (%v,%v)", req, resp)
	}
}

func TestSumStageLatenciesFromBlob_StageBucketing(t *testing.T) {
	in := sql.NullString{Valid: true, String: `[
		{"stage":"request","latencyMs":10},
		{"stage":"connection","latencyMs":5},
		{"stage":"response","latencyMs":20},
		{"stage":"unknown","latencyMs":999},
		{"stage":"request","latencyMs":-1}
	]`}
	req, resp := sumStageLatenciesFromBlob(in)
	if req == nil || *req != 15 {
		t.Errorf("request sum (request+connection, ignore negatives): got %v, want 15", req)
	}
	if resp == nil || *resp != 20 {
		t.Errorf("response sum: got %v, want 20", resp)
	}
}

func TestSumStageLatenciesFromBlob_StageSeenButZeroLatencyStillReturnsPointer(t *testing.T) {
	// reqSeen=true even when only stage="request" without positive
	// latencyMs is present; output pointer must NOT be nil so the
	// column gets written to 0 instead of staying NULL.
	in := sql.NullString{Valid: true, String: `[{"stage":"request","latencyMs":0}]`}
	req, resp := sumStageLatenciesFromBlob(in)
	if req == nil {
		t.Errorf("reqSeen=true must produce non-nil pointer (even at 0)")
	} else if *req != 0 {
		t.Errorf("req: got %d, want 0", *req)
	}
	if resp != nil {
		t.Errorf("respSeen=false must keep pointer nil; got %v", resp)
	}
}

func TestComputeResidualUpstream_AllArms(t *testing.T) {
	pi := func(v int) *int { return &v }
	cases := []struct {
		name string
		dur  sql.NullInt64
		req  *int
		resp *int
		want *int
	}{
		{"NULL duration → nil", sql.NullInt64{Valid: false}, pi(50), pi(50), nil},
		{"both hook nil → full duration", sql.NullInt64{Valid: true, Int64: 200}, nil, nil, pi(200)},
		{"req only", sql.NullInt64{Valid: true, Int64: 100}, pi(20), nil, pi(80)},
		{"resp only", sql.NullInt64{Valid: true, Int64: 100}, nil, pi(40), pi(60)},
		{"both → subtract", sql.NullInt64{Valid: true, Int64: 1000}, pi(100), pi(200), pi(700)},
		{"clamp below zero", sql.NullInt64{Valid: true, Int64: 50}, pi(40), pi(40), pi(0)},
		{"zero duration → zero", sql.NullInt64{Valid: true, Int64: 0}, nil, nil, pi(0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeResidualUpstream(tc.dur, tc.req, tc.resp)
			switch {
			case got == nil && tc.want == nil:
			case got == nil || tc.want == nil:
				t.Errorf("nil mismatch: got %v, want %v", got, tc.want)
			case *got != *tc.want:
				t.Errorf("value: got %d, want %d", *got, *tc.want)
			}
		})
	}
}

func TestNullableIntFromPtr(t *testing.T) {
	if got := nullableIntFromPtr(nil); got != nil {
		t.Errorf("nil ptr → nil; got %v", got)
	}
	v := 42
	got := nullableIntFromPtr(&v)
	n, ok := got.(int)
	if !ok || n != 42 {
		t.Errorf("non-nil ptr should dereference to int(42); got %T %v", got, got)
	}
}

// itoa is a small, allocation-free int → string helper used by the
// multi-batch loop above. Avoids dragging in strconv just for that.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	const digits = "0123456789"
	buf := [20]byte{}
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	return string(buf[i:])
}

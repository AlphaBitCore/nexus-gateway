package consumer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

// TestTrafficCopyEnabledFromEnv pins the shipped default: COPY is ON unless an
// explicit falsey token disables it (the kill switch / A/B control arm).
func TestTrafficCopyEnabledFromEnv(t *testing.T) {
	cases := []struct {
		env  string
		want bool
	}{
		{"", true},     // unset → ON (shipped optimum)
		{"1", true},    // explicit on
		{"true", true}, // explicit on
		{" ON ", true}, // trimmed, case-insensitive
		{"0", false},   // kill switch
		{"false", false},
		{"off", false},
		{"no", false},
		{"garbage", true}, // unrecognized → default ON, never silently off
	}
	for _, c := range cases {
		if got := trafficCopyEnabledFromEnv(c.env); got != c.want {
			t.Errorf("trafficCopyEnabledFromEnv(%q) = %v, want %v", c.env, got, c.want)
		}
	}
}

// sqlInsertColumns extracts the column-list of an `INSERT INTO <table> ( … ) VALUES`
// statement, tolerating `--` line comments inside the list (insertTrafficEventSQL
// carries several). It is the positional source of truth the COPY column slices
// must match.
func sqlInsertColumns(t *testing.T, sql string) []string {
	t.Helper()
	open := strings.Index(sql, "(")
	closeVals := strings.Index(sql, ") VALUES")
	if closeVals < 0 {
		closeVals = strings.Index(sql, ")\nSELECT")
	}
	if open < 0 || closeVals < 0 || closeVals < open {
		t.Fatalf("could not locate column list in SQL")
	}
	body := sql[open+1 : closeVals]
	var cols []string
	for _, line := range strings.Split(body, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		for _, c := range strings.Split(line, ",") {
			if c = strings.TrimSpace(c); c != "" {
				cols = append(cols, c)
			}
		}
	}
	return cols
}

// TestTrafficEventColumnsParity guards the COPY column slice + row builder against
// drift from insertTrafficEventSQL: same columns, same order, same count, and the
// row builder emits exactly one value per column. A column added to the SQL but
// not here (or vice versa) fails the build's tests, not silently in production.
func TestTrafficEventColumnsParity(t *testing.T) {
	want := sqlInsertColumns(t, insertTrafficEventSQL)
	if len(want) != 91 {
		t.Fatalf("parsed %d columns from insertTrafficEventSQL, want 91", len(want))
	}
	if len(trafficEventColumns) != len(want) {
		t.Fatalf("trafficEventColumns has %d, SQL has %d", len(trafficEventColumns), len(want))
	}
	for i := range want {
		if trafficEventColumns[i] != want[i] {
			t.Errorf("column %d: slice=%q SQL=%q", i, trafficEventColumns[i], want[i])
		}
	}
	if got := len(trafficEventRowValues(TrafficEventMessage{})); got != len(trafficEventColumns) {
		t.Errorf("trafficEventRowValues emits %d values, want %d", got, len(trafficEventColumns))
	}
}

// TestPayloadColumnsParity is the same guard for traffic_event_payload.
func TestPayloadColumnsParity(t *testing.T) {
	want := sqlInsertColumns(t, insertPayloadSQL)
	if len(want) != 13 {
		t.Fatalf("parsed %d columns from insertPayloadSQL, want 13", len(want))
	}
	if len(payloadColumns) != len(want) {
		t.Fatalf("payloadColumns has %d, SQL has %d", len(payloadColumns), len(want))
	}
	for i := range want {
		if payloadColumns[i] != want[i] {
			t.Errorf("column %d: slice=%q SQL=%q", i, payloadColumns[i], want[i])
		}
	}
	e := TrafficEventMessage{
		ID:          "evt-1",
		RequestBody: sharedaudit.NewInlineBody([]byte(`{"a":1}`), 7, false, "application/json"),
	}
	vals, ok := payloadRowValues(e)
	if !ok {
		t.Fatal("payloadRowValues returned ok=false for an inline body")
	}
	if len(vals) != len(payloadColumns) {
		t.Errorf("payloadRowValues emits %d values, want %d", len(vals), len(payloadColumns))
	}
}

// TestPayloadRowValues_AbsentSkips confirms a both-absent event yields ok=false
// (no payload row), matching insertPayloads' skip.
func TestPayloadRowValues_AbsentSkips(t *testing.T) {
	e := TrafficEventMessage{ID: "x", RequestBody: sharedaudit.EmptyBody(), ResponseBody: sharedaudit.EmptyBody()}
	if _, ok := payloadRowValues(e); ok {
		t.Error("both-absent event should yield ok=false")
	}
}

// TestTrafficEventRowValues_RichBranches exercises the tag-strip, embedding-model,
// and L2-entry-key arms of the row builder (the empty-event parity test skips them)
// and asserts the business outcome: a NUL in a tag is stripped, and a present
// embedding model id / L2 key is emitted (not nil-coalesced).
func TestTrafficEventRowValues_RichBranches(t *testing.T) {
	e := TrafficEventMessage{
		ID: "evt-1", Source: "ai-gateway", Timestamp: time.Now().UTC(),
		ComplianceTags:         []string{"pii\x00leak", "ok"},
		EmbeddingModelID:       "text-embedding-3-small",
		GatewayCacheL2EntryKey: "l2:abc",
		Identity:               []byte(`{"sub":"u1"}`),
	}
	vals := trafficEventRowValues(e)
	if len(vals) != len(trafficEventColumns) {
		t.Fatalf("emitted %d values, want %d", len(vals), len(trafficEventColumns))
	}
	// compliance_tags is column 39 (index 38); the NUL must be stripped.
	tags, ok := vals[38].([]string)
	if !ok || len(tags) != 2 || strings.ContainsRune(tags[0], 0) {
		t.Errorf("tags not stripped/typed: %#v", vals[38])
	}
	// embedding_model_id (index 85) + gateway_cache_l2_entry_key (index 88) present.
	if vals[85] != "text-embedding-3-small" {
		t.Errorf("embedding_model_id = %#v, want the model id", vals[85])
	}
	if vals[88] != "l2:abc" {
		t.Errorf("l2_entry_key = %#v, want l2:abc", vals[88])
	}
}

// TestTrafficEventRowValues_EmptyStringsCoalesceToNil confirms the ""→NULL arms:
// an absent embedding model / L2 key bind SQL NULL, not an empty string (so
// `WHERE embedding_model_id IS NOT NULL` filters correctly).
func TestTrafficEventRowValues_EmptyStringsCoalesceToNil(t *testing.T) {
	vals := trafficEventRowValues(TrafficEventMessage{ID: "x"})
	if vals[85] != nil {
		t.Errorf("empty embedding_model_id should coalesce to nil, got %#v", vals[85])
	}
	if vals[88] != nil {
		t.Errorf("empty l2_entry_key should coalesce to nil, got %#v", vals[88])
	}
}

// TestPayloadRowValues_ResponseInlineAndRequestSpill covers the response-inline
// arm and the request-spill arm (the parity test only hit request-inline): the
// spill ref is JSON-encoded and the inline response carries the BYTEA column form.
func TestPayloadRowValues_ResponseInlineAndRequestSpill(t *testing.T) {
	ref := &sharedaudit.SpillRef{Backend: "s3", Key: "k/1", Size: 4096, ContentType: "application/json"}
	e := TrafficEventMessage{
		ID:           "evt-1",
		RequestBody:  sharedaudit.NewSpillBody(ref, 4096, false, "application/json"),
		ResponseBody: sharedaudit.NewInlineBody([]byte("data: hi\n\n"), 9, false, "text/event-stream"),
	}
	vals, ok := payloadRowValues(e)
	if !ok || len(vals) != len(payloadColumns) {
		t.Fatalf("ok=%v len=%d want %d", ok, len(vals), len(payloadColumns))
	}
	// request_spill_ref (index 3) is set; inline_request_body (index 1) is nil.
	if vals[1] != nil {
		t.Errorf("inline_request_body should be nil for a spill body, got %#v", vals[1])
	}
	if vals[3] == nil {
		t.Error("request_spill_ref should be set for a spill body")
	}
	// inline_response_body (index 2) carries the BYTEA []byte column form.
	if _, isBytes := vals[2].([]byte); !isBytes {
		t.Errorf("inline_response_body should be []byte (BYTEA), got %T", vals[2])
	}
}

// TestCopyUpsert_ErrorBranches asserts each COPY-path failure surfaces a wrapped
// error so flushBatch returns and flush falls back to the per-item path — the
// no-strand guarantee. Covers create-staging, copy, and insert-select failures.
func TestCopyUpsert_ErrorBranches(t *testing.T) {
	rows := [][]any{trafficEventRowValues(TrafficEventMessage{ID: "x", Timestamp: time.Now()})}
	cases := []struct {
		name   string
		expect func(m pgxmock.PgxPoolIface)
		want   string
	}{
		{
			name: "create staging fails",
			expect: func(m pgxmock.PgxPoolIface) {
				m.ExpectExec(`CREATE TEMP TABLE _copy_traffic_event`).WillReturnError(errors.New("disk full"))
			},
			want: "create staging",
		},
		{
			name: "copy fails",
			expect: func(m pgxmock.PgxPoolIface) {
				m.ExpectExec(`CREATE TEMP TABLE _copy_traffic_event`).WillReturnResult(pgxmock.NewResult("CREATE", 0))
				m.ExpectCopyFrom(pgx.Identifier{"_copy_traffic_event"}, trafficEventColumns).WillReturnError(errors.New("encode"))
			},
			want: "copy into",
		},
		{
			name: "insert-select fails",
			expect: func(m pgxmock.PgxPoolIface) {
				m.ExpectExec(`CREATE TEMP TABLE _copy_traffic_event`).WillReturnResult(pgxmock.NewResult("CREATE", 0))
				m.ExpectCopyFrom(pgx.Identifier{"_copy_traffic_event"}, trafficEventColumns).WillReturnResult(1)
				m.ExpectExec(`INSERT INTO traffic_event`).WillReturnError(errors.New("conflict"))
			},
			want: "insert-select",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock, err := pgxmock.NewPool()
			if err != nil {
				t.Fatalf("pgxmock.NewPool: %v", err)
			}
			defer mock.Close()
			ctx := context.Background()
			mock.ExpectBegin()
			tc.expect(mock)
			tx, _ := mock.Begin(ctx)
			defer tx.Rollback(ctx) //nolint:errcheck
			err = copyUpsert(ctx, tx, "traffic_event", trafficEventColumns, rows, "id")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got err=%v, want wrapped %q", err, tc.want)
			}
		})
	}
}

// TestInsertTrafficEventsCopy_ControlFlow asserts the COPY fast path issues the
// staging-table create, the COPY, and the idempotent INSERT…SELECT…ON CONFLICT —
// in that order, in the one transaction.
func TestInsertTrafficEventsCopy_ControlFlow(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()
	ctx := context.Background()

	mock.ExpectBegin()
	mock.ExpectExec(`CREATE TEMP TABLE _copy_traffic_event`).WillReturnResult(pgxmock.NewResult("CREATE", 0))
	mock.ExpectCopyFrom(pgx.Identifier{"_copy_traffic_event"}, trafficEventColumns).WillReturnResult(2)
	mock.ExpectExec(`INSERT INTO traffic_event .* ON CONFLICT \(id\) DO NOTHING`).WillReturnResult(pgxmock.NewResult("INSERT", 2))
	mock.ExpectCommit()

	tx, _ := mock.Begin(ctx)
	defer tx.Rollback(ctx) //nolint:errcheck
	w := NewTrafficEventWriter(nil, nil, TrafficEventWriterConfig{}, discardLogger(), nil)
	items := []pendingTrafficMessage{
		{event: TrafficEventMessage{ID: "evt-1", Source: "ai-gateway", Timestamp: time.Now().UTC()}},
		{event: TrafficEventMessage{ID: "evt-2", Source: "ai-gateway", Timestamp: time.Now().UTC()}},
	}
	if err := w.insertTrafficEventsCopy(ctx, tx, items); err != nil {
		t.Fatalf("insertTrafficEventsCopy: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestInsertPayloadsCopy_ControlFlow asserts the COPY fast path for the payload
// table, and that body-absent rows are excluded from the COPY.
func TestInsertPayloadsCopy_ControlFlow(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()
	ctx := context.Background()

	mock.ExpectBegin()
	mock.ExpectExec(`CREATE TEMP TABLE _copy_traffic_event_payload`).WillReturnResult(pgxmock.NewResult("CREATE", 0))
	mock.ExpectCopyFrom(pgx.Identifier{"_copy_traffic_event_payload"}, payloadColumns).WillReturnResult(1)
	mock.ExpectExec(`INSERT INTO traffic_event_payload .* ON CONFLICT \(traffic_event_id\) DO NOTHING`).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	tx, _ := mock.Begin(ctx)
	defer tx.Rollback(ctx) //nolint:errcheck
	w := NewTrafficEventWriter(nil, nil, TrafficEventWriterConfig{}, discardLogger(), nil)
	items := []pendingTrafficMessage{
		{event: TrafficEventMessage{ID: "evt-1", RequestBody: sharedaudit.NewInlineBody([]byte(`{"a":1}`), 7, false, "application/json"), ResponseBody: sharedaudit.EmptyBody()}},
		{event: TrafficEventMessage{ID: "evt-2", RequestBody: sharedaudit.EmptyBody(), ResponseBody: sharedaudit.EmptyBody()}}, // both absent → excluded from COPY
	}
	if err := w.insertPayloadsCopy(ctx, tx, items); err != nil {
		t.Fatalf("insertPayloadsCopy: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestInsertPayloadsCopy_AllAbsentNoOp confirms the COPY path issues no statements
// when every event is body-absent (nothing to stage).
func TestInsertPayloadsCopy_AllAbsentNoOp(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()
	ctx := context.Background()
	mock.ExpectBegin()
	mock.ExpectCommit()

	tx, _ := mock.Begin(ctx)
	defer tx.Rollback(ctx) //nolint:errcheck
	w := NewTrafficEventWriter(nil, nil, TrafficEventWriterConfig{}, discardLogger(), nil)
	items := []pendingTrafficMessage{{event: TrafficEventMessage{ID: "x", RequestBody: sharedaudit.EmptyBody(), ResponseBody: sharedaudit.EmptyBody()}}}
	if err := w.insertPayloadsCopy(ctx, tx, items); err != nil {
		t.Fatalf("insertPayloadsCopy: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

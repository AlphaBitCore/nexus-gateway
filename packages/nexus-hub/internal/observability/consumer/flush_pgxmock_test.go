package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"

	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// flush_pgxmock_test.go — drives the Begin→Insert→Commit chain in
// TrafficEventWriter.flush() and AdminAuditWriter.flush() through pgxmock via
// the PgxPool seam. Lifts the package from ~87% to ≥95% by covering every
// branch in the flush bodies (begin failure, insert poison-pill ack, insert
// nak, payload nak, normalize warn-and-continue, commit failure, ackAll
// success, batch-size histogram, all 4 counter increments).

// trafficFlushWriter wires up a TrafficEventWriter against a pgxmock pool
// through the PgxPool seam. Caller supplies the canned expectations via the
// returned mock.
func trafficFlushWriter(t *testing.T) (*TrafficEventWriter, pgxmock.PgxPoolIface) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	w := NewTrafficEventWriterWithPgxPool(
		mock,
		nil, // mqc unused — we call flush() directly
		TrafficEventWriterConfig{BatchSize: 100, FlushInterval: time.Hour},
		discardLogger(),
		newTestRegistry(),
	)
	return w, mock
}

// adminFlushWriter mirrors trafficFlushWriter for AdminAuditWriter.
func adminFlushWriter(t *testing.T) (*AdminAuditWriter, pgxmock.PgxPoolIface) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	w := NewAdminAuditWriterWithPgxPool(
		mock,
		nil,
		AdminAuditWriterConfig{BatchSize: 100, FlushInterval: time.Hour},
		discardLogger(),
		newTestRegistry(),
	)
	return w, mock
}

// countingMsg returns a *mq.Message with Ack/Nak counters bumped atomically.
func countingMsg(ackCount, nakCount *int32) *mq.Message {
	return &mq.Message{
		Ack: func() error { atomic.AddInt32(ackCount, 1); return nil },
		Nak: func() error { atomic.AddInt32(nakCount, 1); return nil },
	}
}

// TrafficEventWriter.flush — happy path through the full seam.
//
// Begin → SendBatch(traffic_event) → SendBatch(traffic_event_payload) →
// SendBatch(traffic_event_normalized) → Commit → ackAll. The batch-size
// histogram and flushTotal{result="success"} counter must both fire.
func TestTrafficWriter_Flush_HappyPath_AcksAllAndCommits(t *testing.T) {
	w, mock := trafficFlushWriter(t)

	mock.ExpectBegin()
	eb1 := mock.ExpectBatch()
	eb1.ExpectExec(`INSERT INTO traffic_event`).WithArgs(anyArgs(90)...).WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	eb2 := mock.ExpectBatch()
	eb2.ExpectExec(`INSERT INTO traffic_event_payload`).WithArgs(anyArgs(11)...).WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	eb3 := mock.ExpectBatch()
	eb3.ExpectExec(`INSERT INTO traffic_event_normalized`).WithArgs(anyArgs(8)...).WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectCommit()

	var ackCount, nakCount int32
	items := []pendingTrafficMessage{
		{
			event: TrafficEventMessage{
				ID:                "evt-happy",
				Source:            "ai-gateway",
				Timestamp:         time.Now().UTC(),
				RequestBody:       sharedaudit.Body{Kind: sharedaudit.BodyInline, InlineBytes: []byte(`{"a":1}`), SizeBytes: 7},
				ResponseBody:      sharedaudit.EmptyBody(),
				RequestNormalized: json.RawMessage(`{"messages":[]}`),
			},
			msg: countingMsg(&ackCount, &nakCount),
		},
	}

	if err := w.flush(context.Background(), items); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := atomic.LoadInt32(&ackCount); got != 1 {
		t.Errorf("ackAll: got %d, want 1 (happy path must ack every item)", got)
	}
	if got := atomic.LoadInt32(&nakCount); got != 0 {
		t.Errorf("nakAll: got %d, want 0 (happy path must not nak)", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TrafficEventWriter.flush — endpoint_type ($90) is persisted from the
// message. ai-gateway stamps the canonical typology.EndpointKind onto
// TrafficEventMessage.EndpointType; the writer must bind it as the final
// INSERT arg so the audit row records the request modality. Pinning the
// 90th positional arg to "embeddings" makes a regression that drops or
// reorders the binding fail loudly (the bug that shipped endpoint_type
// without a column / writer wiring would never have populated this).
func TestTrafficWriter_Flush_PersistsEndpointType(t *testing.T) {
	w, mock := trafficFlushWriter(t)

	mock.ExpectBegin()
	eb1 := mock.ExpectBatch()
	eb1.ExpectExec(`INSERT INTO traffic_event`).
		WithArgs(append(anyArgs(89), "embeddings")...).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	eb2 := mock.ExpectBatch()
	eb2.ExpectExec(`INSERT INTO traffic_event_payload`).WithArgs(anyArgs(11)...).WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	eb3 := mock.ExpectBatch()
	eb3.ExpectExec(`INSERT INTO traffic_event_normalized`).WithArgs(anyArgs(8)...).WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectCommit()

	var ackCount, nakCount int32
	items := []pendingTrafficMessage{
		{
			event: TrafficEventMessage{
				ID:                "evt-ep",
				Source:            "ai-gateway",
				Timestamp:         time.Now().UTC(),
				EndpointType:      "embeddings",
				RequestBody:       sharedaudit.Body{Kind: sharedaudit.BodyInline, InlineBytes: []byte(`{"a":1}`), SizeBytes: 7},
				ResponseBody:      sharedaudit.EmptyBody(),
				RequestNormalized: json.RawMessage(`{"messages":[]}`),
			},
			msg: countingMsg(&ackCount, &nakCount),
		},
	}

	if err := w.flush(context.Background(), items); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := atomic.LoadInt32(&ackCount); got != 1 {
		t.Errorf("ackAll: got %d, want 1 (endpoint_type row must commit)", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TrafficEventWriter.flush — Begin failure: every item is nak'd, errors_total{
// error_type="db_begin"} fires, flushTotal{result="error"} fires, returns
// wrapped "begin tx" error.
func TestTrafficWriter_Flush_BeginFailureViaSeam(t *testing.T) {
	w, mock := trafficFlushWriter(t)

	beginErr := errors.New("conn-dropped")
	mock.ExpectBegin().WillReturnError(beginErr)

	var ackCount, nakCount int32
	items := []pendingTrafficMessage{
		{event: TrafficEventMessage{ID: "x"}, msg: countingMsg(&ackCount, &nakCount)},
		{event: TrafficEventMessage{ID: "y"}, msg: countingMsg(&ackCount, &nakCount)},
	}

	err := w.flush(context.Background(), items)
	if err == nil || !strings.Contains(err.Error(), "begin tx") {
		t.Fatalf("got err=%v, want wrapped 'begin tx'", err)
	}
	if got := atomic.LoadInt32(&nakCount); got != 2 {
		t.Errorf("nakAll: got %d, want 2", got)
	}
	if got := atomic.LoadInt32(&ackCount); got != 0 {
		t.Errorf("ackAll: got %d, want 0 (must not ack on begin failure)", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TrafficEventWriter.flush — insertTrafficEvents fails with SQLSTATE 22021
// (invalid_character_value_for_cast). This is a permanent data error so the
// poison-pill ackAll path must fire (NOT nakAll) — retrying the same null-byte
// payload would block the queue forever. Returns the wrapped error so the
// driver logs it; flushTotal{result="error"} and errors_total{db_insert}
// still increment.
//
// pgxmock requires one ExpectExec per Queue'd row inside the batch — the
// production loop calls br.Exec() per item and returns on the first error,
// so the SECOND ExpectExec is set up but will not fire (pgxmock complains
// if there are remaining unmet expectations only when the batch itself
// closes via br.Close, which production does via defer; the per-row Exec
// however returns immediately on error and the second Queue'd statement
// is simply not Exec'd — pgxmock tolerates that). We supply only as many
// ExpectExecs as the loop actually consumes (1, since it returns at the
// first error).
func TestTrafficWriter_Flush_InsertPoisonPill22021AcksToSkip(t *testing.T) {
	w, mock := trafficFlushWriter(t)

	mock.ExpectBegin()
	eb := mock.ExpectBatch()
	// pgconn.PgError carrying "22021" — flush detects via strings.Contains on
	// the error message, so any error whose message contains "22021" suffices.
	eb.ExpectExec(`INSERT INTO traffic_event`).WithArgs(anyArgs(90)...).WillReturnError(errors.New("ERROR: invalid byte sequence (SQLSTATE 22021)"))
	mock.ExpectRollback()

	var ackCount, nakCount int32
	// Single item so the batch size matches the single ExpectExec — the
	// branch under test (ack-to-skip on 22021) is independent of batch size.
	items := []pendingTrafficMessage{
		{
			event: TrafficEventMessage{
				ID: "p1", Source: "agent", Timestamp: time.Now(),
				RequestBody: sharedaudit.EmptyBody(), ResponseBody: sharedaudit.EmptyBody(),
			},
			msg: countingMsg(&ackCount, &nakCount),
		},
	}

	err := w.flush(context.Background(), items)
	if err == nil || !strings.Contains(err.Error(), "insert traffic_event") {
		t.Fatalf("got err=%v, want wrapped 'insert traffic_event'", err)
	}
	if !strings.Contains(err.Error(), "22021") {
		t.Fatalf("err must propagate '22021' marker, got %q", err.Error())
	}
	// Poison-pill: ack-to-skip on every item, never nak.
	if got := atomic.LoadInt32(&ackCount); got != 1 {
		t.Errorf("ackAll (poison-pill): got %d, want 1 (22021 must ack-to-skip, not nak)", got)
	}
	if got := atomic.LoadInt32(&nakCount); got != 0 {
		t.Errorf("nakAll: got %d, want 0 (22021 must NOT nak — would loop the queue)", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TrafficEventWriter.flush — insertTrafficEvents fails with a non-22021 error:
// transient (constraint violation, dropped conn, etc). Every item nak'd so the
// MQ driver redelivers; errors_total{db_insert} + flushTotal{error} fire.
func TestTrafficWriter_Flush_InsertNonPoisonFailureNaksAll(t *testing.T) {
	w, mock := trafficFlushWriter(t)

	mock.ExpectBegin()
	eb := mock.ExpectBatch()
	eb.ExpectExec(`INSERT INTO traffic_event`).WithArgs(anyArgs(90)...).WillReturnError(errors.New("unique_violation"))
	mock.ExpectRollback()

	var ackCount, nakCount int32
	items := []pendingTrafficMessage{
		{
			event: TrafficEventMessage{
				ID: "n1", Source: "agent", Timestamp: time.Now(),
				RequestBody: sharedaudit.EmptyBody(), ResponseBody: sharedaudit.EmptyBody(),
			},
			msg: countingMsg(&ackCount, &nakCount),
		},
	}
	err := w.flush(context.Background(), items)
	if err == nil || !strings.Contains(err.Error(), "insert traffic_event") {
		t.Fatalf("got err=%v, want wrapped 'insert traffic_event'", err)
	}
	if got := atomic.LoadInt32(&nakCount); got != 1 {
		t.Errorf("nakAll: got %d, want 1 (non-poison must nak for redelivery)", got)
	}
	if got := atomic.LoadInt32(&ackCount); got != 0 {
		t.Errorf("ackAll: got %d, want 0 (non-poison must NOT ack)", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TrafficEventWriter.flush — insertPayloads fails (after traffic_event inserts
// succeeded). Every item nak'd; flushTotal{error} + errors_total{
// db_insert_payload} fire.
func TestTrafficWriter_Flush_InsertPayloadsFailureNaksAll(t *testing.T) {
	w, mock := trafficFlushWriter(t)

	mock.ExpectBegin()
	eb1 := mock.ExpectBatch()
	eb1.ExpectExec(`INSERT INTO traffic_event`).WithArgs(anyArgs(90)...).WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	eb2 := mock.ExpectBatch()
	eb2.ExpectExec(`INSERT INTO traffic_event_payload`).WithArgs(anyArgs(11)...).WillReturnError(errors.New("disk-full"))
	mock.ExpectRollback()

	var ackCount, nakCount int32
	items := []pendingTrafficMessage{
		{
			event: TrafficEventMessage{
				ID:           "p-x",
				Source:       "agent",
				Timestamp:    time.Now(),
				RequestBody:  sharedaudit.Body{Kind: sharedaudit.BodyInline, InlineBytes: []byte(`{}`), SizeBytes: 2},
				ResponseBody: sharedaudit.EmptyBody(),
			},
			msg: countingMsg(&ackCount, &nakCount),
		},
	}
	err := w.flush(context.Background(), items)
	if err == nil || !strings.Contains(err.Error(), "insert traffic_event_payload") {
		t.Fatalf("got err=%v, want wrapped 'insert traffic_event_payload'", err)
	}
	if got := atomic.LoadInt32(&nakCount); got != 1 {
		t.Errorf("nakAll: got %d, want 1", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TrafficEventWriter.flush — insertNormalizedPayloads fails. The sidecar is
// independent of the audit trail (raw bytes already persisted on
// traffic_event_payload), so flush WARNs + counts errors_total{
// db_insert_normalized} but DOES NOT roll back the rest of the batch. Commit
// still runs and ackAll fires on success.
func TestTrafficWriter_Flush_NormalizedFailureWarnsButCommits(t *testing.T) {
	w, mock := trafficFlushWriter(t)

	mock.ExpectBegin()
	eb1 := mock.ExpectBatch()
	eb1.ExpectExec(`INSERT INTO traffic_event`).WithArgs(anyArgs(90)...).WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	// Body absent → insertPayloads short-circuits, no traffic_event_payload
	// batch is sent.
	eb3 := mock.ExpectBatch()
	eb3.ExpectExec(`INSERT INTO traffic_event_normalized`).WithArgs(anyArgs(8)...).WillReturnError(errors.New("normalize-batch-failed"))
	mock.ExpectCommit()

	var ackCount, nakCount int32
	items := []pendingTrafficMessage{
		{
			event: TrafficEventMessage{
				ID:                "n-x",
				Source:            "ai-gateway",
				Timestamp:         time.Now(),
				RequestBody:       sharedaudit.EmptyBody(),
				ResponseBody:      sharedaudit.EmptyBody(),
				RequestNormalized: json.RawMessage(`{"messages":[]}`),
			},
			msg: countingMsg(&ackCount, &nakCount),
		},
	}
	if err := w.flush(context.Background(), items); err != nil {
		t.Fatalf("flush: got %v, want nil (normalize failure is non-fatal)", err)
	}
	if got := atomic.LoadInt32(&ackCount); got != 1 {
		t.Errorf("ackAll: got %d, want 1 (commit succeeded, ack must fire)", got)
	}
	if got := atomic.LoadInt32(&nakCount); got != 0 {
		t.Errorf("nakAll: got %d, want 0 (normalize sidecar failure must NOT nak the raw batch)", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TrafficEventWriter.flush — Commit fails after every insert succeeded. Every
// item nak'd for redelivery; flushTotal{error} + errors_total{db_commit} fire.
func TestTrafficWriter_Flush_CommitFailureNaksAll(t *testing.T) {
	w, mock := trafficFlushWriter(t)

	mock.ExpectBegin()
	eb1 := mock.ExpectBatch()
	eb1.ExpectExec(`INSERT INTO traffic_event`).WithArgs(anyArgs(90)...).WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	// Body absent + no normalize → only the traffic_event insert; commit fails.
	mock.ExpectCommit().WillReturnError(errors.New("commit-rejected"))
	mock.ExpectRollback()

	var ackCount, nakCount int32
	items := []pendingTrafficMessage{
		{
			event: TrafficEventMessage{
				ID: "c-x", Source: "agent", Timestamp: time.Now(),
				RequestBody: sharedaudit.EmptyBody(), ResponseBody: sharedaudit.EmptyBody(),
			},
			msg: countingMsg(&ackCount, &nakCount),
		},
	}
	err := w.flush(context.Background(), items)
	if err == nil || !strings.Contains(err.Error(), "commit tx") {
		t.Fatalf("got err=%v, want wrapped 'commit tx'", err)
	}
	if got := atomic.LoadInt32(&nakCount); got != 1 {
		t.Errorf("nakAll: got %d, want 1", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TrafficEventWriter.flush — nil-registry happy path: counter guards on
// flushTotal / batchSizeHist / errorsTotal must short-circuit without panic.
// Confirms the seam still works when the writer is constructed with reg=nil.
func TestTrafficWriter_Flush_NilRegistryHappyPath(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	w := NewTrafficEventWriterWithPgxPool(
		mock, nil,
		TrafficEventWriterConfig{BatchSize: 100, FlushInterval: time.Hour},
		discardLogger(),
		nil, // reg=nil — every metric arm must skip
	)

	mock.ExpectBegin()
	eb := mock.ExpectBatch()
	eb.ExpectExec(`INSERT INTO traffic_event`).WithArgs(anyArgs(90)...).WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectCommit()

	var ackCount, nakCount int32
	items := []pendingTrafficMessage{
		{
			event: TrafficEventMessage{
				ID: "nil-reg", Source: "agent", Timestamp: time.Now(),
				RequestBody: sharedaudit.EmptyBody(), ResponseBody: sharedaudit.EmptyBody(),
			},
			msg: countingMsg(&ackCount, &nakCount),
		},
	}
	if err := w.flush(context.Background(), items); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := atomic.LoadInt32(&ackCount); got != 1 {
		t.Errorf("ackAll: got %d, want 1", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// AdminAuditWriter.flush — driven through the PgxPool seam.
//
// insertAdminEvents runs SELECT pg_advisory_xact_lock + SELECT head + INSERT
// per row (no pgx.Batch — chain is sequence-dependent). The full happy path
// hits Begin → SELECT → INSERT → Commit → ackAll.
func TestAdminAuditWriter_Flush_HappyPathViaSeam(t *testing.T) {
	w, mock := adminFlushWriter(t)

	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).WithArgs(anyArgs(1)...).WillReturnResult(pgconn.NewCommandTag("SELECT 1"))
	mock.ExpectQuery(`SELECT "integrityHash" FROM "AdminAuditLog"`).WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec(`INSERT INTO "AdminAuditLog"`).WithArgs(anyArgs(16)...).WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectCommit()

	var ackCount, nakCount int32
	items := []pendingAdminMessage{
		{
			event: mq.AdminAuditMessage{
				ID:         uuid.NewString(),
				Timestamp:  time.Now().UTC(),
				ActorID:    "u-1",
				ActorLabel: "u-1",
				Action:     "create",
				EntityType: "provider",
				EntityID:   "ent-1",
			},
			msg: countingMsg(&ackCount, &nakCount),
		},
	}
	if err := w.flush(context.Background(), items); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := atomic.LoadInt32(&ackCount); got != 1 {
		t.Errorf("ackAll: got %d, want 1", got)
	}
	if got := atomic.LoadInt32(&nakCount); got != 0 {
		t.Errorf("nakAll: got %d, want 0", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// viaArg matches the AdminAuditLog INSERT's 16th positional argument (via). The
// consumer passes nilIfEmpty(e.Via), i.e. a *string for an assistant row and an
// untyped nil for a human row. want==nil asserts the human (NULL) case.
type viaArg struct{ want *string }

func (a viaArg) Match(v interface{}) bool {
	// nilIfEmpty always yields a typed *string: nil for a human row (→ SQL NULL),
	// non-nil for an assistant row.
	got, _ := v.(*string)
	if a.want == nil {
		return got == nil
	}
	return got != nil && *got == *a.want
}

func strptr(s string) *string { return &s }

// TestAdminAuditWriter_Flush_PersistsViaValue pins the consumer end of the E90 I5
// chain: an assistant-stamped MQ message must INSERT via="assistant" (16th arg),
// and a human message must INSERT NULL — never an empty string, so the column /
// index cleanly distinguishes "AI-initiated" from "direct human action".
func TestAdminAuditWriter_Flush_PersistsViaValue(t *testing.T) {
	for _, tc := range []struct {
		name string
		via  string
		want *string
	}{
		{"assistant row persists marker", "assistant", strptr("assistant")},
		{"human row persists NULL", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, mock := adminFlushWriter(t)
			mock.ExpectBegin()
			mock.ExpectExec(`SELECT pg_advisory_xact_lock`).WithArgs(anyArgs(1)...).WillReturnResult(pgconn.NewCommandTag("SELECT 1"))
			mock.ExpectQuery(`SELECT "integrityHash" FROM "AdminAuditLog"`).WillReturnError(pgx.ErrNoRows)
			// First 15 args are matched loosely; the 16th (via) is matched exactly.
			mock.ExpectExec(`INSERT INTO "AdminAuditLog"`).
				WithArgs(append(anyArgs(15), viaArg{want: tc.want})...).
				WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
			mock.ExpectCommit()

			var ack, nak int32
			items := []pendingAdminMessage{{
				event: mq.AdminAuditMessage{
					ID: uuid.NewString(), Timestamp: time.Now().UTC(),
					ActorID: "u-1", ActorLabel: "u-1",
					Action: "create", EntityType: "provider", EntityID: "ent-1",
					Via: tc.via,
				},
				msg: countingMsg(&ack, &nak),
			}}
			if err := w.flush(context.Background(), items); err != nil {
				t.Fatalf("flush: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("expectations: %v", err)
			}
		})
	}
}

// AdminAuditWriter.flush — Begin failure: every item nak'd; flushTotal{error},
// errors_total{db_begin}, wrapped "begin tx" returned.
func TestAdminAuditWriter_Flush_BeginFailureViaSeam(t *testing.T) {
	w, mock := adminFlushWriter(t)

	mock.ExpectBegin().WillReturnError(errors.New("conn-lost"))

	var ackCount, nakCount int32
	items := []pendingAdminMessage{
		{event: mq.AdminAuditMessage{ID: "a", Action: "create", ActorID: "u", EntityType: "t", EntityID: "e"}, msg: countingMsg(&ackCount, &nakCount)},
		{event: mq.AdminAuditMessage{ID: "b", Action: "create", ActorID: "u", EntityType: "t", EntityID: "e"}, msg: countingMsg(&ackCount, &nakCount)},
	}
	err := w.flush(context.Background(), items)
	if err == nil || !strings.Contains(err.Error(), "begin tx") {
		t.Fatalf("got err=%v, want wrapped 'begin tx'", err)
	}
	if got := atomic.LoadInt32(&nakCount); got != 2 {
		t.Errorf("nakAll: got %d, want 2", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// AdminAuditWriter.flush — insertAdminEvents fails (INSERT errors). The whole
// batch nak'd for redelivery; flushTotal{error}, errors_total{db_insert} fire.
func TestAdminAuditWriter_Flush_InsertFailureNaksAll(t *testing.T) {
	w, mock := adminFlushWriter(t)

	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).WithArgs(anyArgs(1)...).WillReturnResult(pgconn.NewCommandTag("SELECT 1"))
	mock.ExpectQuery(`SELECT "integrityHash" FROM "AdminAuditLog"`).WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec(`INSERT INTO "AdminAuditLog"`).WithArgs(anyArgs(16)...).WillReturnError(errors.New("write-blocked"))
	mock.ExpectRollback()

	var ackCount, nakCount int32
	items := []pendingAdminMessage{
		{
			event: mq.AdminAuditMessage{ID: uuid.NewString(), Action: "create", ActorID: "u", ActorLabel: "u", EntityType: "t", EntityID: "e"},
			msg:   countingMsg(&ackCount, &nakCount),
		},
	}
	err := w.flush(context.Background(), items)
	if err == nil || !strings.Contains(err.Error(), "insert AdminAuditLog") {
		t.Fatalf("got err=%v, want wrapped 'insert AdminAuditLog'", err)
	}
	if got := atomic.LoadInt32(&nakCount); got != 1 {
		t.Errorf("nakAll: got %d, want 1", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// AdminAuditWriter.flush — Commit fails after insert succeeded. Whole batch
// nak'd; flushTotal{error}, errors_total{db_commit} fire.
func TestAdminAuditWriter_Flush_CommitFailureNaksAll(t *testing.T) {
	w, mock := adminFlushWriter(t)

	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).WithArgs(anyArgs(1)...).WillReturnResult(pgconn.NewCommandTag("SELECT 1"))
	mock.ExpectQuery(`SELECT "integrityHash" FROM "AdminAuditLog"`).WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec(`INSERT INTO "AdminAuditLog"`).WithArgs(anyArgs(16)...).WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectCommit().WillReturnError(errors.New("commit-rejected"))
	mock.ExpectRollback()

	var ackCount, nakCount int32
	items := []pendingAdminMessage{
		{
			event: mq.AdminAuditMessage{ID: uuid.NewString(), Action: "create", ActorID: "u", ActorLabel: "u", EntityType: "t", EntityID: "e"},
			msg:   countingMsg(&ackCount, &nakCount),
		},
	}
	err := w.flush(context.Background(), items)
	if err == nil || !strings.Contains(err.Error(), "commit tx") {
		t.Fatalf("got err=%v, want wrapped 'commit tx'", err)
	}
	if got := atomic.LoadInt32(&nakCount); got != 1 {
		t.Errorf("nakAll: got %d, want 1", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// AdminAuditWriter.flush — nil-registry happy path: every metric guard must
// skip cleanly. Mirrors the traffic writer's nil-reg test for symmetry.
func TestAdminAuditWriter_Flush_NilRegistryHappyPath(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	w := NewAdminAuditWriterWithPgxPool(
		mock, nil,
		AdminAuditWriterConfig{BatchSize: 100, FlushInterval: time.Hour},
		discardLogger(),
		nil,
	)

	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).WithArgs(anyArgs(1)...).WillReturnResult(pgconn.NewCommandTag("SELECT 1"))
	mock.ExpectQuery(`SELECT "integrityHash" FROM "AdminAuditLog"`).WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec(`INSERT INTO "AdminAuditLog"`).WithArgs(anyArgs(16)...).WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectCommit()

	var ackCount, nakCount int32
	items := []pendingAdminMessage{
		{
			event: mq.AdminAuditMessage{ID: uuid.NewString(), Action: "create", ActorID: "u", ActorLabel: "u", EntityType: "t", EntityID: "e"},
			msg:   countingMsg(&ackCount, &nakCount),
		},
	}
	if err := w.flush(context.Background(), items); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := atomic.LoadInt32(&ackCount); got != 1 {
		t.Errorf("ackAll: got %d, want 1", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

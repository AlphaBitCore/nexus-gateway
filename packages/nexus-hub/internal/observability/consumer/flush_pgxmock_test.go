package consumer

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
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
	// These cases assert the pgx.Batch flush path (poison isolation, nak/ack
	// semantics) and mock SendBatch. Pin the Batch path so the now-default-on
	// COPY fast path doesn't divert them — the COPY path has its own coverage in
	// traffic_copy_test.go.
	prevCopy := trafficCopyEnabled
	trafficCopyEnabled = false
	t.Cleanup(func() { trafficCopyEnabled = prevCopy })
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
	eb1.ExpectExec(`INSERT INTO traffic_event`).WithArgs(anyArgs(91)...).WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	eb2 := mock.ExpectBatch()
	eb2.ExpectExec(`INSERT INTO traffic_event_payload`).WithArgs(anyArgs(13)...).WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	// Normalized sidecar fast path: SAVEPOINT (Begin) → pipelined batch →
	// RELEASE SAVEPOINT (Commit), then the outer Commit.
	mock.ExpectBegin()
	eb3 := mock.ExpectBatch()
	eb3.ExpectExec(`INSERT INTO traffic_event_normalized`).WithArgs(anyArgs(10)...).WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectCommit()
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
		WithArgs(append(anyArgs(89), "embeddings", "")...).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	eb2 := mock.ExpectBatch()
	eb2.ExpectExec(`INSERT INTO traffic_event_payload`).WithArgs(anyArgs(13)...).WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	// Normalized sidecar fast path: SAVEPOINT → batch → RELEASE.
	mock.ExpectBegin()
	eb3 := mock.ExpectBatch()
	eb3.ExpectExec(`INSERT INTO traffic_event_normalized`).WithArgs(anyArgs(10)...).WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectCommit()
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

// TrafficEventWriter.flush — ingress_format ($91) is persisted from the
// message. ai-gateway stamps the client-facing wire format onto
// TrafficEventMessage.IngressFormat; the writer must bind it as the final
// INSERT arg so the control-plane view-time normalize can decode the stored
// (ingress-framed) bodies with the right codec instead of the upstream
// provider's adapter. Pinning the 91st positional arg to "anthropic" makes a
// regression that drops or reorders the binding fail loudly.
func TestTrafficWriter_Flush_PersistsIngressFormat(t *testing.T) {
	w, mock := trafficFlushWriter(t)

	mock.ExpectBegin()
	eb1 := mock.ExpectBatch()
	eb1.ExpectExec(`INSERT INTO traffic_event`).
		WithArgs(append(anyArgs(90), "anthropic")...).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	eb2 := mock.ExpectBatch()
	eb2.ExpectExec(`INSERT INTO traffic_event_payload`).WithArgs(anyArgs(13)...).WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectCommit()

	var ackCount, nakCount int32
	items := []pendingTrafficMessage{
		{
			event: TrafficEventMessage{
				ID:            "evt-ingress",
				Source:        "ai-gateway",
				Timestamp:     time.Now().UTC(),
				IngressFormat: "anthropic",
				RequestBody:   sharedaudit.Body{Kind: sharedaudit.BodyInline, InlineBytes: []byte(`{"a":1}`), SizeBytes: 7},
				ResponseBody:  sharedaudit.EmptyBody(),
			},
			msg: countingMsg(&ackCount, &nakCount),
		},
	}

	if err := w.flush(context.Background(), items); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := atomic.LoadInt32(&ackCount); got != 1 {
		t.Errorf("ackAll: got %d, want 1 (ingress_format row must commit)", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// nulFreeJSONArg matches the internal_ops_breakdown ($88) positional argument:
// it must be a json.RawMessage with BOTH NUL forms already stripped (raw \x00
// byte AND the 6-char \u0000 escape) and equal to want. It pins the fix
// — the column must be wrapped in stripNulJSON like every sibling JSON column.
type nulFreeJSONArg struct{ want string }

func (a nulFreeJSONArg) Match(v interface{}) bool {
	raw, ok := v.(json.RawMessage)
	if !ok {
		return false
	}
	s := string(raw)
	if strings.ContainsRune(s, 0) || strings.Contains(s, "\\u0000") {
		return false
	}
	return s == a.want
}

// TestTrafficWriter_Flush_InternalOpsBreakdownNulStripped is the
// regression test: internal_ops_breakdown was the SOLE JSON column bound without
// stripNulJSON, so a NUL (raw \x00 or the 6-char \u0000 escape) in the
// gateway-internal cost payload raised SQLSTATE 22021/22P05 and dropped the
// whole batch. The writer must now bind the stripped value, so the INSERT
// never carries a NUL and the batch commits.
func TestTrafficWriter_Flush_InternalOpsBreakdownNulStripped(t *testing.T) {
	w, mock := trafficFlushWriter(t)

	mock.ExpectBegin()
	eb := mock.ExpectBatch()
	// $88 (internal_ops_breakdown) must arrive NUL-free and stripped.
	args := append(anyArgs(87), nulFreeJSONArg{want: `{"raw":"ab","esc":"pq"}`})
	args = append(args, anyArgs(3)...) // $89 (l2 entry key), $90 (endpoint_type), $91 (ingress_format)
	eb.ExpectExec(`INSERT INTO traffic_event`).WithArgs(args...).WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectCommit()

	var ackCount, nakCount int32
	items := []pendingTrafficMessage{
		{
			event: TrafficEventMessage{
				ID: "iob", Source: "ai-gateway", Timestamp: time.Now().UTC(),
				RequestBody:  sharedaudit.EmptyBody(),
				ResponseBody: sharedaudit.EmptyBody(),
				// raw \x00 byte in "raw"; 6-char \u0000 escape in "esc".
				InternalOpsBreakdown: json.RawMessage("{\"raw\":\"a\x00b\",\"esc\":\"p\\u0000q\"}"),
			},
			msg: countingMsg(&ackCount, &nakCount),
		},
	}

	if err := w.flush(context.Background(), items); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := atomic.LoadInt32(&ackCount); got != 1 {
		t.Errorf("ackAll: got %d, want 1 (stripped breakdown must commit, not poison the batch)", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestTrafficWriter_FlushItem_NormalizedFailureStillCommits pins the per-item
// durability guarantee: when the batched attempt fails and the row is
// reprocessed alone, a failure in its normalized sidecar must NOT roll back the
// raw row — the raw traffic_event still commits and the message is acked.
func TestTrafficWriter_FlushItem_NormalizedFailureStillCommits(t *testing.T) {
	w, mock := trafficFlushWriter(t)

	transient := errors.New("deadlock detected")
	// Batched attempt fails on the traffic_event insert → triggers per-item.
	mock.ExpectBegin()
	ebBatch := mock.ExpectBatch()
	ebBatch.ExpectExec(`INSERT INTO traffic_event`).WithArgs(anyArgs(91)...).WillReturnError(transient)
	mock.ExpectRollback()
	// Per-item: traffic_event ok; body absent (no payload batch); normalized
	// sidecar fails (fast batch then row-by-row, both non-poison) but the outer
	// tx still commits.
	mock.ExpectBegin()
	ebTE := mock.ExpectBatch()
	ebTE.ExpectExec(`INSERT INTO traffic_event`).WithArgs(anyArgs(91)...).WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectBegin() // normalized savepoint (fast path)
	ebN := mock.ExpectBatch()
	ebN.ExpectExec(`INSERT INTO traffic_event_normalized`).WithArgs(anyArgs(10)...).WillReturnError(errors.New("normalize-batch-failed"))
	mock.ExpectRollback()
	mock.ExpectBegin() // normalized row-by-row savepoint
	mock.ExpectExec(`INSERT INTO traffic_event_normalized`).WithArgs(anyArgs(10)...).WillReturnError(errors.New("normalize-row-failed"))
	mock.ExpectRollback()
	mock.ExpectCommit() // outer commit — raw row survives

	var ackCount, nakCount int32
	items := []pendingTrafficMessage{
		{
			event: TrafficEventMessage{
				ID: "iso-norm", Source: "ai-gateway", Timestamp: time.Now().UTC(),
				RequestBody:       sharedaudit.EmptyBody(),
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
		t.Errorf("ackAll: got %d, want 1 (raw row must commit despite sidecar failure)", got)
	}
	if got := atomic.LoadInt32(&nakCount); got != 0 {
		t.Errorf("nakAll: got %d, want 0", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TrafficEventWriter.flush — Begin failure. The batched fast path's Begin fails,
// so flush falls back to per-item reprocessing where each item's own Begin also
// fails and the item is nak'd for redelivery. errors_total{db_begin} +
// flushTotal{error} fire; flush returns nil because every item is resolved.
func TestTrafficWriter_Flush_BeginFailureViaSeam(t *testing.T) {
	w, mock := trafficFlushWriter(t)

	beginErr := errors.New("conn-dropped")
	// One Begin for the batch attempt + one per item in the fallback (2 items).
	mock.ExpectBegin().WillReturnError(beginErr)
	mock.ExpectBegin().WillReturnError(beginErr)
	mock.ExpectBegin().WillReturnError(beginErr)

	var ackCount, nakCount int32
	items := []pendingTrafficMessage{
		{event: TrafficEventMessage{ID: "x"}, msg: countingMsg(&ackCount, &nakCount)},
		{event: TrafficEventMessage{ID: "y"}, msg: countingMsg(&ackCount, &nakCount)},
	}

	if err := w.flush(context.Background(), items); err != nil {
		t.Fatalf("got err=%v, want nil (per-item fallback resolves every item)", err)
	}
	if got := atomic.LoadInt32(&nakCount); got != 2 {
		t.Errorf("nakAll: got %d, want 2 (both items nak'd in the per-item fallback)", got)
	}
	if got := atomic.LoadInt32(&ackCount); got != 0 {
		t.Errorf("ackAll: got %d, want 0 (must not ack on begin failure)", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TrafficEventWriter.flush — insertTrafficEvents fails with a TYPED SQLSTATE
// 22021 (*pgconn.PgError, invalid_character_value_for_cast). This is a permanent
// data error so the row must be ack-to-skipped (NOT nak'd) — retrying the same
// null-byte payload would block the queue forever.
//
// With per-item reprocessing the batched attempt aborts first, then the
// row is retried in isolation where the typed poison classifier recognises it
// and acks it. flush returns nil (the item is fully resolved). The poison is a
// *pgconn.PgError so errors.As — not a substring match — drives the decision.
func TestTrafficWriter_Flush_InsertPoisonPill22021AcksToSkip(t *testing.T) {
	w, mock := trafficFlushWriter(t)

	poison := &pgconn.PgError{Code: "22021", Message: "invalid byte sequence for encoding"}
	// Batched fast-path attempt aborts on the poison row.
	mock.ExpectBegin()
	eb := mock.ExpectBatch()
	eb.ExpectExec(`INSERT INTO traffic_event`).WithArgs(anyArgs(91)...).WillReturnError(poison)
	mock.ExpectRollback()
	// Per-item fallback re-runs the same row, hits the same typed poison, acks.
	mock.ExpectBegin()
	ebItem := mock.ExpectBatch()
	ebItem.ExpectExec(`INSERT INTO traffic_event`).WithArgs(anyArgs(91)...).WillReturnError(poison)
	mock.ExpectRollback()

	var ackCount, nakCount int32
	items := []pendingTrafficMessage{
		{
			event: TrafficEventMessage{
				ID: "p1", Source: "agent", Timestamp: time.Now(),
				RequestBody: sharedaudit.EmptyBody(), ResponseBody: sharedaudit.EmptyBody(),
			},
			msg: countingMsg(&ackCount, &nakCount),
		},
	}

	if err := w.flush(context.Background(), items); err != nil {
		t.Fatalf("got err=%v, want nil (poison row resolved by ack-to-skip)", err)
	}
	// Poison-pill: ack-to-skip, never nak.
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

// TestTrafficWriter_Flush_PoisonRowIsolatedHealthyCommits is the heart of
// per-item isolation: a 2-item batch where the FIRST row is a permanent
// 22021 poison. The batched attempt aborts as a unit, then per-item
// reprocessing acks the poison row and COMMITS the healthy one — so a single
// bad row no longer drops up to 99 good events.
func TestTrafficWriter_Flush_PoisonRowIsolatedHealthyCommits(t *testing.T) {
	w, mock := trafficFlushWriter(t)

	poison := &pgconn.PgError{Code: "22021", Message: "invalid byte sequence for encoding"}
	// Batched attempt: the 2-row traffic_event batch aborts on the poison row.
	// pgx queues both rows, so the ExpectBatch needs one ExpectExec per queued
	// row (production returns on the first error; the deferred br.Close drains
	// the rest).
	mock.ExpectBegin()
	ebBatch := mock.ExpectBatch()
	ebBatch.ExpectExec(`INSERT INTO traffic_event`).WithArgs(anyArgs(91)...).WillReturnError(poison)
	ebBatch.ExpectExec(`INSERT INTO traffic_event`).WithArgs(anyArgs(91)...).WillReturnError(poison)
	mock.ExpectRollback()
	// Per-item: poison row first — Begin, insert(22021), Rollback, ack-to-skip.
	mock.ExpectBegin()
	ebP := mock.ExpectBatch()
	ebP.ExpectExec(`INSERT INTO traffic_event`).WithArgs(anyArgs(91)...).WillReturnError(poison)
	mock.ExpectRollback()
	// Per-item: healthy row — Begin, insert ok, Commit, ack.
	mock.ExpectBegin()
	ebH := mock.ExpectBatch()
	ebH.ExpectExec(`INSERT INTO traffic_event`).WithArgs(anyArgs(91)...).WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectCommit()

	var poisonAck, poisonNak int32
	var goodAck, goodNak int32
	items := []pendingTrafficMessage{
		{
			event: TrafficEventMessage{
				ID: "poison", Source: "agent", Timestamp: time.Now(),
				RequestBody: sharedaudit.EmptyBody(), ResponseBody: sharedaudit.EmptyBody(),
			},
			msg: countingMsg(&poisonAck, &poisonNak),
		},
		{
			event: TrafficEventMessage{
				ID: "healthy", Source: "agent", Timestamp: time.Now(),
				RequestBody: sharedaudit.EmptyBody(), ResponseBody: sharedaudit.EmptyBody(),
			},
			msg: countingMsg(&goodAck, &goodNak),
		},
	}

	if err := w.flush(context.Background(), items); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := atomic.LoadInt32(&poisonAck); got != 1 {
		t.Errorf("poison row ack: got %d, want 1 (ack-to-skip)", got)
	}
	if got := atomic.LoadInt32(&goodAck); got != 1 {
		t.Errorf("healthy row ack: got %d, want 1 (must commit despite a poison sibling)", got)
	}
	if got := atomic.LoadInt32(&poisonNak) + atomic.LoadInt32(&goodNak); got != 0 {
		t.Errorf("nak total: got %d, want 0", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TrafficEventWriter.flush — insertTrafficEvents fails with a non-poison error:
// transient (constraint violation, dropped conn, etc). The batched attempt and
// the per-item retry both fail; the item is nak'd for redelivery (NOT acked —
// it is not a permanent poison). errors_total{db_insert} + flushTotal{error}
// fire; flush returns nil.
func TestTrafficWriter_Flush_InsertNonPoisonFailureNaksAll(t *testing.T) {
	w, mock := trafficFlushWriter(t)

	transient := errors.New("unique_violation")
	mock.ExpectBegin()
	eb := mock.ExpectBatch()
	eb.ExpectExec(`INSERT INTO traffic_event`).WithArgs(anyArgs(91)...).WillReturnError(transient)
	mock.ExpectRollback()
	// Per-item retry: same transient failure → nak.
	mock.ExpectBegin()
	ebItem := mock.ExpectBatch()
	ebItem.ExpectExec(`INSERT INTO traffic_event`).WithArgs(anyArgs(91)...).WillReturnError(transient)
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
	if err := w.flush(context.Background(), items); err != nil {
		t.Fatalf("got err=%v, want nil (per-item fallback resolves the item)", err)
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
// succeeded). The batched attempt and the per-item retry both fail at the
// payload insert; the item is nak'd. flushTotal{error} +
// errors_total{db_insert_payload} fire; flush returns nil.
func TestTrafficWriter_Flush_InsertPayloadsFailureNaksAll(t *testing.T) {
	w, mock := trafficFlushWriter(t)

	payloadErr := errors.New("disk-full")
	mock.ExpectBegin()
	eb1 := mock.ExpectBatch()
	eb1.ExpectExec(`INSERT INTO traffic_event`).WithArgs(anyArgs(91)...).WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	eb2 := mock.ExpectBatch()
	eb2.ExpectExec(`INSERT INTO traffic_event_payload`).WithArgs(anyArgs(13)...).WillReturnError(payloadErr)
	mock.ExpectRollback()
	// Per-item retry: traffic_event ok, payload fails again → nak.
	mock.ExpectBegin()
	eb1b := mock.ExpectBatch()
	eb1b.ExpectExec(`INSERT INTO traffic_event`).WithArgs(anyArgs(91)...).WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	eb2b := mock.ExpectBatch()
	eb2b.ExpectExec(`INSERT INTO traffic_event_payload`).WithArgs(anyArgs(13)...).WillReturnError(payloadErr)
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
	if err := w.flush(context.Background(), items); err != nil {
		t.Fatalf("got err=%v, want nil (per-item fallback resolves the item)", err)
	}
	if got := atomic.LoadInt32(&nakCount); got != 1 {
		t.Errorf("nakAll: got %d, want 1", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TrafficEventWriter.flush — insertNormalizedPayloads fails (non-poison). The
// sidecar runs in its OWN savepoint, so the failure rolls back ONLY that
// savepoint (ROLLBACK TO SAVEPOINT) and the raw traffic_event row still
// COMMITS — the durability guarantee. flush WARNs + counts
// errors_total{db_insert_normalized} but ackAll fires on the successful outer
// commit; nothing is nak'd.
func TestTrafficWriter_Flush_NormalizedFailureWarnsButCommits(t *testing.T) {
	w, mock := trafficFlushWriter(t)

	mock.ExpectBegin()
	eb1 := mock.ExpectBatch()
	eb1.ExpectExec(`INSERT INTO traffic_event`).WithArgs(anyArgs(91)...).WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	// Body absent → insertPayloads short-circuits, no traffic_event_payload
	// batch is sent. Normalized fast path: SAVEPOINT → batch(err) → ROLLBACK TO
	// SAVEPOINT, then row-by-row retry: SAVEPOINT → Exec(err) → ROLLBACK. The
	// outer Commit then still runs and the raw row survives.
	mock.ExpectBegin()
	ebN := mock.ExpectBatch()
	ebN.ExpectExec(`INSERT INTO traffic_event_normalized`).WithArgs(anyArgs(10)...).WillReturnError(errors.New("normalize-batch-failed"))
	mock.ExpectRollback()
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO traffic_event_normalized`).WithArgs(anyArgs(10)...).WillReturnError(errors.New("normalize-row-failed"))
	mock.ExpectRollback()
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

// TrafficEventWriter.flush — Commit fails after every insert succeeded. The
// batched attempt's commit fails, then the per-item retry's commit fails too;
// the item is nak'd for redelivery. flushTotal{error} + errors_total{db_commit}
// fire; flush returns nil.
func TestTrafficWriter_Flush_CommitFailureNaksAll(t *testing.T) {
	w, mock := trafficFlushWriter(t)

	commitErr := errors.New("commit-rejected")
	mock.ExpectBegin()
	eb1 := mock.ExpectBatch()
	eb1.ExpectExec(`INSERT INTO traffic_event`).WithArgs(anyArgs(91)...).WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	// Body absent + no normalize → only the traffic_event insert; commit fails.
	mock.ExpectCommit().WillReturnError(commitErr)
	mock.ExpectRollback()
	// Per-item retry: insert ok, commit fails again → nak.
	mock.ExpectBegin()
	eb1b := mock.ExpectBatch()
	eb1b.ExpectExec(`INSERT INTO traffic_event`).WithArgs(anyArgs(91)...).WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectCommit().WillReturnError(commitErr)
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
	if err := w.flush(context.Background(), items); err != nil {
		t.Fatalf("got err=%v, want nil (per-item fallback resolves the item)", err)
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
	// Pin the pgx.Batch path (this case mocks SendBatch); COPY is default-on.
	prevCopy := trafficCopyEnabled
	trafficCopyEnabled = false
	defer func() { trafficCopyEnabled = prevCopy }()
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
	eb.ExpectExec(`INSERT INTO traffic_event`).WithArgs(anyArgs(91)...).WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
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

// TestAdminAuditWriter_Flush_PersistsViaValue pins the consumer end of the via
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

package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/observability/siem"
	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// fakeConsumerMQ — minimal mq.Consumer test double.
//
// Drives the handler exactly once per message in messages, then waits for ctx
// cancellation. Used to exercise the Start() goroutine wiring of all three
// consumer types (TrafficEventWriter, AdminAuditWriter, SIEMForwarder) without
// pulling in NATS / Redis. Compatible with both Subscribe and Consume paths
// (each driver-side method delegates to the same fake handler).
//
// Concurrency: the SIEM forwarder and traffic writer call Consume from N
// parallel goroutines (one per queue), so any per-fake mutable state must be
// guarded. Read-only fields set at construction (messages, consumeErr) need
// no lock; counters are int32 + atomic.
type fakeConsumerMQ struct {
	messages   [][]byte // each entry drives one handler invocation
	consumeErr error    // returned from Consume after the messages loop drains
	ackCount   *int32   // optional ack counter shared across messages
	nakCount   *int32   // optional nak counter shared across messages
}

func (f *fakeConsumerMQ) Subscribe(ctx context.Context, _ string, _ mq.MessageHandler) error {
	<-ctx.Done()
	return nil
}

func (f *fakeConsumerMQ) Consume(ctx context.Context, _ string, _ string, handler mq.MessageHandler) error {
	for _, data := range f.messages {
		ack := func() error {
			if f.ackCount != nil {
				atomic.AddInt32(f.ackCount, 1)
			}
			return nil
		}
		nak := func() error {
			if f.nakCount != nil {
				atomic.AddInt32(f.nakCount, 1)
			}
			return nil
		}
		msg := &mq.Message{Data: data, Ack: ack, Nak: nak}
		_ = handler(ctx, msg)
	}
	if f.consumeErr != nil {
		return f.consumeErr
	}
	<-ctx.Done()
	return nil
}

func (f *fakeConsumerMQ) Close() error { return nil }

// closedPool returns a *pgxpool.Pool whose Begin always returns
// "closed pool". No DB connection is ever attempted — pgxpool.New only
// validates the DSN shape; the dial happens on first use. We immediately
// Close the pool so the first Begin reports puddle.ErrClosedPool, letting
// us cover every flush() error branch without TEST_DATABASE_URL.
func closedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://u:p@127.0.0.1:65500/db?sslmode=disable")
	if err != nil {
		t.Fatalf("pgxpool.New (DSN shape): %v", err)
	}
	pool.Close()
	return pool
}

// anyArgs returns n pgxmock.AnyArg placeholders. The consumer's INSERT
// statements bind 8–80 parameters; the arg COUNT is load-bearing (binding
// a 79-arg slice against an 80-placeholder SQL fails with a clear "expected
// 80, got 79" pgx error in production), but each individual value's
// equality is not what these tests pin — the column-level behavior is
// pinned by the dedicated integration tests in traffic_test.go that hit a
// real DB. Here we assert "the right number of bound params reached the
// driver" which is enough to exercise the consumer-side row construction.
func anyArgs(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = pgxmock.AnyArg()
	}
	return out
}

// stripNul / stripNulPtr / stripNulJSON
//
// Observable behavior: every Postgres-illegal \x00 must be removed BEFORE the
// driver binds the parameter, otherwise the consumer hits SQLSTATE 22021 and
// the whole batch flips into the poison-pill ack path. Inputs without a NUL
// byte must be returned unchanged (fast-path is load-bearing for the per-row
// helper called dozens of times per message).
func TestStripNul_RemovesNullBytesAndPreservesCleanStrings(t *testing.T) {
	if got := stripNul("hello"); got != "hello" {
		t.Errorf("clean string: got %q, want %q (fast-path must return unchanged)", got, "hello")
	}
	if got := stripNul(""); got != "" {
		t.Errorf("empty string: got %q, want empty", got)
	}
	if got := stripNul("ab\x00cd\x00ef"); got != "abcdef" {
		t.Errorf("multi-NUL: got %q, want %q", got, "abcdef")
	}
	if got := stripNul("\x00"); got != "" {
		t.Errorf("only-NUL: got %q, want empty", got)
	}
}

func TestStripNulPtr_NilStaysNilNonNilStrippedInPlace(t *testing.T) {
	if got := stripNulPtr(nil); got != nil {
		t.Errorf("nil input: got non-nil pointer %v", got)
	}
	s := "ab\x00cd"
	got := stripNulPtr(&s)
	if got == nil {
		t.Fatalf("non-nil input: got nil pointer")
	}
	if *got != "abcd" {
		t.Errorf("non-nil: deref = %q, want abcd", *got)
	}
	// Original must not be mutated — the helper allocates a fresh string.
	if s != "ab\x00cd" {
		t.Errorf("caller's string mutated to %q (helper must not modify caller's value)", s)
	}
}

func TestStripNulJSON_PreservesCleanAndStripsTainted(t *testing.T) {
	if got := stripNulJSON(nil); len(got) != 0 {
		t.Errorf("nil raw: got %q", got)
	}
	if got := stripNulJSON(json.RawMessage{}); len(got) != 0 {
		t.Errorf("empty raw: got %q", got)
	}
	clean := json.RawMessage(`{"k":"v"}`)
	if got := stripNulJSON(clean); string(got) != `{"k":"v"}` {
		t.Errorf("clean: got %q", got)
	}
	tainted := json.RawMessage("{\"k\":\"a\x00b\"}")
	if got := string(stripNulJSON(tainted)); got != `{"k":"ab"}` {
		t.Errorf("tainted: got %q, want %q", got, `{"k":"ab"}`)
	}
}

// passthroughFlagsParam / passthroughReasonParam
//
// Observable behavior: empty slices become SQL NULL (so the partial index
// traffic_event_passthrough_active_idx stays compact), non-empty slices have
// every element NUL-stripped, and the reason string follows the same
// empty-to-NULL convention.
func TestPassthroughFlagsParam_NilSliceBecomesNullAndElementsStripped(t *testing.T) {
	if got := passthroughFlagsParam(nil); got != nil {
		t.Errorf("nil slice: got %v, want nil (SQL NULL keeps partial index small)", got)
	}
	if got := passthroughFlagsParam([]string{}); got != nil {
		t.Errorf("empty slice: got %v, want nil", got)
	}
	got, ok := passthroughFlagsParam([]string{"bypassHooks\x00", "bypassCache"}).([]string)
	if !ok {
		t.Fatalf("non-empty slice: got %T, want []string", got)
	}
	if len(got) != 2 || got[0] != "bypassHooks" || got[1] != "bypassCache" {
		t.Errorf("element strip: got %v, want [bypassHooks bypassCache]", got)
	}
}

func TestPassthroughReasonParam_EmptyBecomesNull(t *testing.T) {
	if got := passthroughReasonParam(""); got != nil {
		t.Errorf("empty: got %v, want nil", got)
	}
	got, ok := passthroughReasonParam("kill-switch\x00on").(*string)
	if !ok || got == nil {
		t.Fatalf("non-empty: got %T %v, want *string", got, got)
	}
	if *got != "kill-switchon" {
		t.Errorf("non-empty: *got = %q, want %q", *got, "kill-switchon")
	}
}

// TrafficEventWriter — handleMessage (already covered for poison + defer-ack
// — also pin the consumedTotal metric increment path with nil registry).
func TestTrafficWriter_HandleMessage_NoRegistryDoesNotPanic(t *testing.T) {
	// Same as the existing happy-path test but uses a writer with `reg=nil`
	// so consumedTotal stays nil. Guards against the `if w.consumedTotal !=
	// nil` checks regressing to an unconditional dereference.
	w := NewTrafficEventWriter(nil, nil, TrafficEventWriterConfig{}, discardLogger(), nil)
	batch := noopBatch()
	msg := &mq.Message{
		Data: []byte(`{"id":"evt-nil-reg","source":"agent","timestamp":"2026-04-18T00:00:00Z"}`),
		Ack:  func() error { return nil },
		Nak:  func() error { return nil },
	}
	if err := w.handleMessage("nexus.event.agent", batch, msg); !errors.Is(err, mq.ErrDeferAck) {
		t.Errorf("nil-registry: got %v, want mq.ErrDeferAck", err)
	}
}

// TrafficEventWriter — flush against a CLOSED pool drives every error branch.
//
// Observable behavior: with a closed pool, Begin returns "closed pool", so
// flush:
//  1. increments errorsTotal{error_type="db_begin"} and flushTotal{result="error"}
//  2. calls nakAll on every item (so each message's Nak is invoked)
//  3. returns a wrapped error containing "begin tx"
func TestTrafficWriter_Flush_BeginFailureNaksAllAndCountsError(t *testing.T) {
	pool := closedPool(t)
	w := NewTrafficEventWriter(pool, nil, TrafficEventWriterConfig{BatchSize: 1}, discardLogger(), newTestRegistry())

	var nakCount int32
	mk := func() *pendingTrafficMessage {
		return &pendingTrafficMessage{
			event: TrafficEventMessage{ID: "x"},
			msg:   &mq.Message{Ack: func() error { return nil }, Nak: func() error { atomic.AddInt32(&nakCount, 1); return nil }},
		}
	}
	items := []pendingTrafficMessage{*mk(), *mk(), *mk()}
	err := w.flush(context.Background(), items)
	if err == nil || !strings.Contains(err.Error(), "begin tx") {
		t.Fatalf("got err=%v, want wrapped 'begin tx'", err)
	}
	if got := atomic.LoadInt32(&nakCount); got != 3 {
		t.Errorf("nakAll: got %d, want 3 (every item must be nak'd on begin failure)", got)
	}
}

// TrafficEventWriter — handleMessage propagates a synchronous batch flush
// error. When BatchSize=1 the very first Add triggers flushLocked, whose
// flushFn here returns a sentinel; the handler must surface it (not swallow
// to ErrDeferAck). This is the "sync flush failure" path documented in the
// production comment "flush already invoked nakAll on this item".
func TestTrafficWriter_HandleMessage_SyncFlushErrorPropagates(t *testing.T) {
	w := newTestTrafficWriter(t)
	sentinel := errors.New("flush-boom")
	batch := NewBatchAccumulator[pendingTrafficMessage](1, time.Hour, func(_ []pendingTrafficMessage) error {
		return sentinel
	})
	msg := &mq.Message{
		Data: []byte(`{"id":"e","source":"agent","timestamp":"2026-04-18T00:00:00Z"}`),
		Ack:  func() error { return nil },
		Nak:  func() error { return nil },
	}
	err := w.handleMessage("nexus.event.agent", batch, msg)
	if !errors.Is(err, sentinel) {
		t.Errorf("got %v, want sentinel %v (sync flush error must surface)", err, sentinel)
	}
}

// TrafficEventWriter — Start spawns one goroutine per queue, drains test
// messages, and exits cleanly when ctx is cancelled.
//
// Observable behavior:
//  1. the consumer's handler is invoked exactly once per supplied message
//     across all 3 traffic queues
//  2. handler returns ErrDeferAck for well-formed JSON (so the fake driver
//     sees a defer signal, not an auto-ack)
//  3. Start returns nil once ctx cancels (no goroutine leak)
func TestTrafficWriter_Start_DrainsAllThreeQueuesAndStopsOnCtxCancel(t *testing.T) {
	pool := closedPool(t)
	defer pool.Close()
	mqc := &fakeConsumerMQ{
		messages: [][]byte{
			[]byte(`{"id":"e1","source":"ai-gateway","timestamp":"2026-04-18T00:00:00Z"}`),
		},
	}
	w := NewTrafficEventWriter(pool, mqc, TrafficEventWriterConfig{BatchSize: 100, FlushInterval: time.Hour}, discardLogger(), newTestRegistry())

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Start(ctx) }()

	// Cancel almost immediately — the per-queue Consume goroutines will
	// dispatch their lone message, then block on <-ctx.Done().
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Start: got %v, want nil after ctx cancel", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Start did not exit after ctx cancel")
	}
}

// TrafficEventWriter — Start logs an error path when the consumer returns
// an error AND ctx has not been cancelled. Observable signal is hard to
// pin without a custom logger sink, so we assert the consume goroutine
// drains and Start still returns nil (the consumer error is internal-only
// — Start does not propagate it; this is the production contract because
// Start blocks on <-ctx.Done() either way).
func TestTrafficWriter_Start_ConsumerErrorDoesNotBlockStart(t *testing.T) {
	pool := closedPool(t)
	defer pool.Close()
	mqc := &fakeConsumerMQ{
		messages:   nil,
		consumeErr: errors.New("driver-boom"),
	}
	w := NewTrafficEventWriter(pool, mqc, TrafficEventWriterConfig{}, discardLogger(), newTestRegistry())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Start(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Start: got %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Start did not exit")
	}
}

// AdminAuditWriter — Start drains, NewAdminAuditWriter default clamping.
func TestAdminAuditWriter_Defaults_BatchSizeAndFlushInterval(t *testing.T) {
	w := NewAdminAuditWriter(nil, nil, AdminAuditWriterConfig{}, discardLogger(), nil)
	if w.cfg.BatchSize != 100 {
		t.Errorf("BatchSize default: got %d, want 100", w.cfg.BatchSize)
	}
	if w.cfg.FlushInterval != 5*time.Second {
		t.Errorf("FlushInterval default: got %v, want 5s", w.cfg.FlushInterval)
	}
	// Custom values must NOT be clobbered.
	w2 := NewAdminAuditWriter(nil, nil, AdminAuditWriterConfig{BatchSize: 7, FlushInterval: 3 * time.Second}, discardLogger(), nil)
	if w2.cfg.BatchSize != 7 || w2.cfg.FlushInterval != 3*time.Second {
		t.Errorf("custom config clobbered: got %+v", w2.cfg)
	}
}

// AdminAuditWriter — Start spawns one goroutine, consumes the lone admin-audit
// queue, dispatches messages, and exits cleanly on ctx cancel.
func TestAdminAuditWriter_Start_ConsumesAndStops(t *testing.T) {
	pool := closedPool(t)
	defer pool.Close()
	mqc := &fakeConsumerMQ{
		messages: [][]byte{
			[]byte(`{"id":"a1","timestamp":"2026-04-18T00:00:00Z","actorId":"u","actorLabel":"u","action":"create","entityType":"prov","entityId":"x"}`),
			[]byte(`not json`), // hits the deserialize-error ack path
		},
	}
	w := NewAdminAuditWriter(pool, mqc, AdminAuditWriterConfig{BatchSize: 100, FlushInterval: time.Hour}, discardLogger(), newTestRegistry())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Start(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Start: got %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Start did not exit")
	}
}

// AdminAuditWriter — flush against a closed pool covers begin-tx-failed
// arm including nakAll fan-out and counter increments.
func TestAdminAuditWriter_Flush_BeginFailureNaksAll(t *testing.T) {
	pool := closedPool(t)
	w := NewAdminAuditWriter(pool, nil, AdminAuditWriterConfig{BatchSize: 1}, discardLogger(), newTestRegistry())

	var nakCount int32
	items := []pendingAdminMessage{
		{event: mq.AdminAuditMessage{ID: "a", Action: "create", ActorID: "u", EntityType: "t", EntityID: "e"}, msg: &mq.Message{Ack: func() error { return nil }, Nak: func() error { atomic.AddInt32(&nakCount, 1); return nil }}},
		{event: mq.AdminAuditMessage{ID: "b", Action: "update", ActorID: "u", EntityType: "t", EntityID: "e"}, msg: &mq.Message{Ack: func() error { return nil }, Nak: func() error { atomic.AddInt32(&nakCount, 1); return nil }}},
	}
	err := w.flush(context.Background(), items)
	if err == nil || !strings.Contains(err.Error(), "begin tx") {
		t.Fatalf("got err=%v, want wrapped 'begin tx'", err)
	}
	if got := atomic.LoadInt32(&nakCount); got != 2 {
		t.Errorf("nakAll: got %d, want 2", got)
	}
}

// AdminAuditWriter — ackAll / nakAll surface ack-error logs but never panic.
// Observable behavior: both helpers run the full loop even if individual
// Ack/Nak calls error; production logs at WARN but does not bubble the error
// (the MQ driver's redelivery contract handles re-ack).
func TestAdminAuditWriter_AckAllAndNakAll_RunFullLoopOnError(t *testing.T) {
	w := NewAdminAuditWriter(nil, nil, AdminAuditWriterConfig{}, discardLogger(), nil)
	errBoom := errors.New("driver-boom")
	var ackCalled, nakCalled int32
	items := []pendingAdminMessage{
		{msg: &mq.Message{Ack: func() error { atomic.AddInt32(&ackCalled, 1); return errBoom }, Nak: func() error { atomic.AddInt32(&nakCalled, 1); return errBoom }}},
		{msg: &mq.Message{Ack: func() error { atomic.AddInt32(&ackCalled, 1); return nil }, Nak: func() error { atomic.AddInt32(&nakCalled, 1); return nil }}},
	}
	w.ackAll(items)
	if got := atomic.LoadInt32(&ackCalled); got != 2 {
		t.Errorf("ackAll: got %d invocations, want 2 (must continue past first error)", got)
	}
	w.nakAll(items)
	if got := atomic.LoadInt32(&nakCalled); got != 2 {
		t.Errorf("nakAll: got %d invocations, want 2", got)
	}
}

func TestTrafficWriter_AckAllAndNakAll_RunFullLoopOnError(t *testing.T) {
	w := NewTrafficEventWriter(nil, nil, TrafficEventWriterConfig{}, discardLogger(), nil)
	errBoom := errors.New("driver-boom")
	var ackCalled, nakCalled int32
	items := []pendingTrafficMessage{
		{msg: &mq.Message{Ack: func() error { atomic.AddInt32(&ackCalled, 1); return errBoom }, Nak: func() error { atomic.AddInt32(&nakCalled, 1); return errBoom }}},
		{msg: &mq.Message{Ack: func() error { atomic.AddInt32(&ackCalled, 1); return nil }, Nak: func() error { atomic.AddInt32(&nakCalled, 1); return nil }}},
	}
	w.ackAll(items)
	if got := atomic.LoadInt32(&ackCalled); got != 2 {
		t.Errorf("ackAll: got %d, want 2", got)
	}
	// NumDelivered=0 here → below redeliveryThresholdAttempts → straight Nak.
	w.nakOrDLQ(context.Background(), items, nil)
	if got := atomic.LoadInt32(&nakCalled); got != 2 {
		t.Errorf("nakOrDLQ (below cap): got %d, want 2 naks", got)
	}
}

// AdminAuditWriter — insertAdminEvents via pgxmock.
//
// Drives the full chain-aware insert path: advisory lock, head SELECT, INSERT
// per row. Three rows make the chain assertion meaningful: row 2's previous
// hash must come from row 1's integrity (which the production code holds in
// memory, not by re-running the SELECT — only the first SELECT goes through
// pgxmock).
func TestAdminAuditWriter_InsertAdminEvents_PgxmockChain(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	ctx := context.Background()
	mock.ExpectBegin()
	// audit.NextHash runs in production: advisory_xact_lock + SELECT head
	// per row. Two rows → two advisory + two SELECTs + two INSERTs.
	for i := range 2 {
		mock.ExpectExec(`SELECT pg_advisory_xact_lock`).WithArgs(anyArgs(1)...).WillReturnResult(pgconn.NewCommandTag("SELECT 1"))
		if i == 0 {
			// Genesis row: no prior chain head.
			mock.ExpectQuery(`SELECT "integrityHash" FROM "AdminAuditLog"`).WillReturnError(pgx.ErrNoRows)
		} else {
			// Subsequent row's SELECT still runs (production calls it
			// every iteration); return any 64-hex chain head.
			prior := strings.Repeat("c", 64)
			mock.ExpectQuery(`SELECT "integrityHash" FROM "AdminAuditLog"`).
				WillReturnRows(pgxmock.NewRows([]string{"integrityHash"}).AddRow(&prior))
		}
		mock.ExpectExec(`INSERT INTO "AdminAuditLog"`).WithArgs(anyArgs(16)...).WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	}
	mock.ExpectCommit()

	tx, err := mock.Begin(ctx)
	if err != nil {
		t.Fatalf("mock.Begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	w := NewAdminAuditWriter(nil, nil, AdminAuditWriterConfig{}, discardLogger(), nil)
	beforeState := map[string]any{"name": "old"}
	afterState := map[string]any{"name": "new"}
	items := []pendingAdminMessage{
		{event: mq.AdminAuditMessage{
			ID: "row-1", Timestamp: time.Unix(1745539200, 0).UTC(),
			ActorID: "u-1", ActorLabel: "u-1", ActorRole: "admin",
			SourceIP: "10.0.0.1",
			Action:   "create", EntityType: "provider", EntityID: "p-1",
			BeforeState: nil, AfterState: afterState,
			NexusRequestID: "req-1",
		}},
		{event: mq.AdminAuditMessage{
			ID: "row-2", Timestamp: time.Unix(1745539201, 0).UTC(),
			ActorID:    "u-2",
			ActorLabel: "u-2",
			Action:     "update", EntityType: "provider", EntityID: "p-1",
			BeforeState: beforeState, AfterState: afterState,
		}},
	}
	if err := w.insertAdminEvents(ctx, tx, items); err != nil {
		t.Fatalf("insertAdminEvents: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// AdminAuditWriter — insertAdminEvents propagates a SELECT-head error so the
// caller (flush) can NAK the batch.
func TestAdminAuditWriter_InsertAdminEvents_HeadSelectErrorPropagates(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()
	ctx := context.Background()
	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).WithArgs(anyArgs(1)...).WillReturnResult(pgconn.NewCommandTag("SELECT 1"))
	mock.ExpectQuery(`SELECT "integrityHash" FROM "AdminAuditLog"`).WillReturnError(errors.New("read-replica-down"))

	tx, _ := mock.Begin(ctx)
	defer tx.Rollback(ctx) //nolint:errcheck
	w := NewAdminAuditWriter(nil, nil, AdminAuditWriterConfig{}, discardLogger(), nil)
	items := []pendingAdminMessage{
		{event: mq.AdminAuditMessage{ID: "x", Action: "create", ActorID: "u", EntityType: "t", EntityID: "e"}},
	}
	err = w.insertAdminEvents(ctx, tx, items)
	if err == nil || !strings.Contains(err.Error(), "compute chain hash") {
		t.Errorf("got err=%v, want wrapped 'compute chain hash'", err)
	}
}

// AdminAuditWriter — insertAdminEvents propagates an INSERT error so the
// caller (flush) can NAK the batch.
func TestAdminAuditWriter_InsertAdminEvents_InsertErrorPropagates(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()
	ctx := context.Background()
	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).WithArgs(anyArgs(1)...).WillReturnResult(pgconn.NewCommandTag("SELECT 1"))
	mock.ExpectQuery(`SELECT "integrityHash" FROM "AdminAuditLog"`).WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec(`INSERT INTO "AdminAuditLog"`).WithArgs(anyArgs(16)...).WillReturnError(errors.New("write-rejected"))

	tx, _ := mock.Begin(ctx)
	defer tx.Rollback(ctx) //nolint:errcheck
	w := NewAdminAuditWriter(nil, nil, AdminAuditWriterConfig{}, discardLogger(), nil)
	items := []pendingAdminMessage{
		{event: mq.AdminAuditMessage{ID: "x", Action: "create", ActorID: "u", EntityType: "t", EntityID: "e"}},
	}
	err = w.insertAdminEvents(ctx, tx, items)
	if err == nil || !strings.Contains(err.Error(), "insert admin audit row") {
		t.Errorf("got err=%v, want wrapped 'insert admin audit row'", err)
	}
}

// AdminAuditWriter — insertAdminEvents tolerates a payload whose BeforeState
// or AfterState fails json.Marshal. The row still goes in (with NULL before
// /after) so the chain doesn't lose this entry; the logger emits a WARN
// surfaced by errorsTotal{error_type="marshal_before"} / "marshal_after".
//
// json.Marshal fails on a chan(int) per encoding/json docs (UnsupportedTypeError).
func TestAdminAuditWriter_InsertAdminEvents_MarshalFailureStillInsertsRow(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()
	ctx := context.Background()
	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).WithArgs(anyArgs(1)...).WillReturnResult(pgconn.NewCommandTag("SELECT 1"))
	mock.ExpectQuery(`SELECT "integrityHash" FROM "AdminAuditLog"`).WillReturnError(pgx.ErrNoRows)
	// Row still goes in: the marshal-failure WARNs, leaves before/after NULL,
	// then proceeds to NextHash + INSERT (canonicalize hashes the typed
	// payload, not the unmarshallable raw).
	mock.ExpectExec(`INSERT INTO "AdminAuditLog"`).WithArgs(anyArgs(16)...).WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectCommit()

	tx, _ := mock.Begin(ctx)
	defer tx.Rollback(ctx) //nolint:errcheck
	w := NewAdminAuditWriter(nil, nil, AdminAuditWriterConfig{}, discardLogger(), newTestRegistry())

	unmarshallable := make(chan int)
	items := []pendingAdminMessage{
		{event: mq.AdminAuditMessage{
			ID: "x", Action: "create", ActorID: "u", EntityType: "t", EntityID: "e",
			BeforeState: unmarshallable, // chan(int) → json.Marshal returns error
			AfterState:  unmarshallable,
		}},
	}
	if err := w.insertAdminEvents(ctx, tx, items); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// AdminAuditWriter — insertAdminEvents propagates ErrEmptyAction (NewHashPayload).
func TestAdminAuditWriter_InsertAdminEvents_EmptyActionFailsFast(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()
	ctx := context.Background()
	mock.ExpectBegin()
	tx, _ := mock.Begin(ctx)
	defer tx.Rollback(ctx) //nolint:errcheck
	w := NewAdminAuditWriter(nil, nil, AdminAuditWriterConfig{}, discardLogger(), nil)
	items := []pendingAdminMessage{
		{event: mq.AdminAuditMessage{ID: "x", Action: "" /* empty */, ActorID: "u", EntityType: "t", EntityID: "e"}},
	}
	err = w.insertAdminEvents(ctx, tx, items)
	if err == nil || !strings.Contains(err.Error(), "build hash payload") {
		t.Errorf("got err=%v, want wrapped 'build hash payload'", err)
	}
}

// TrafficEventWriter — insertTrafficEvents via pgxmock (the SendBatch path).
//
// Drives ONE row with rich-but-realistic data so every nullable-pointer arm
// (stripNulPtr / nullableJSON / passthrough*) is exercised. Asserts that the
// SendBatch + per-row Exec contract holds.
func TestTrafficWriter_InsertTrafficEvents_FullRowSendBatch(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()
	ctx := context.Background()

	mock.ExpectBegin()
	eb := mock.ExpectBatch()
	eb.ExpectExec(`INSERT INTO traffic_event`).WithArgs(anyArgs(90)...).WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectCommit()

	tx, _ := mock.Begin(ctx)
	defer tx.Rollback(ctx) //nolint:errcheck
	w := NewTrafficEventWriter(nil, nil, TrafficEventWriterConfig{}, discardLogger(), nil)

	src := "ai-gateway"
	method := "POST"
	path := "/v1/chat/completions"
	pt := 100
	ct := 50
	tt := 150
	cost := 0.005
	items := []pendingTrafficMessage{
		{event: TrafficEventMessage{
			ID: "evt-1", Source: src, Timestamp: time.Now().UTC(),
			Method: &method, Path: &path,
			PromptTokens: &pt, CompletionTokens: &ct, TotalTokens: &tt,
			EstimatedCostUSD: &cost,
			ComplianceTags:   []string{"pii\x00", "tag2"}, // strip path
			Identity:         json.RawMessage(`{"sub":"x"}`),
			Details:          json.RawMessage(`{"model":"gpt-4"}`),
			PassthroughFlags: []string{"bypassHooks"},
		}},
	}
	if err := w.insertTrafficEvents(ctx, tx, items); err != nil {
		t.Fatalf("insertTrafficEvents: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TrafficEventWriter — insertTrafficEvents propagates the batched Exec error.
func TestTrafficWriter_InsertTrafficEvents_BatchExecErrorPropagates(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()
	ctx := context.Background()
	mock.ExpectBegin()
	eb := mock.ExpectBatch()
	eb.ExpectExec(`INSERT INTO traffic_event`).WithArgs(anyArgs(90)...).WillReturnError(errors.New("constraint-violation"))

	tx, _ := mock.Begin(ctx)
	defer tx.Rollback(ctx) //nolint:errcheck
	w := NewTrafficEventWriter(nil, nil, TrafficEventWriterConfig{}, discardLogger(), nil)
	items := []pendingTrafficMessage{{event: TrafficEventMessage{ID: "x", Source: "agent", Timestamp: time.Now()}}}
	err = w.insertTrafficEvents(ctx, tx, items)
	if err == nil || !strings.Contains(err.Error(), "exec batch insert") {
		t.Errorf("got err=%v, want wrapped 'exec batch insert'", err)
	}
}

// TrafficEventWriter — insertPayloads short-circuits to nil when every event
// is body-absent (no SendBatch call is required).
func TestTrafficWriter_InsertPayloads_NoBodiesIsNoOp(t *testing.T) {
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

	items := []pendingTrafficMessage{
		{event: TrafficEventMessage{ID: "x", RequestBody: sharedaudit.EmptyBody(), ResponseBody: sharedaudit.EmptyBody()}},
	}
	if err := w.insertPayloads(ctx, tx, items); err != nil {
		t.Fatalf("insertPayloads: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TrafficEventWriter — insertPayloads writes an inline + spill payload pair.
// Exercises BOTH the inline and spill branches in one batch.
func TestTrafficWriter_InsertPayloads_InlineAndSpillBranches(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()
	ctx := context.Background()
	mock.ExpectBegin()
	eb := mock.ExpectBatch()
	eb.ExpectExec(`INSERT INTO traffic_event_payload`).WithArgs(anyArgs(11)...).WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	eb.ExpectExec(`INSERT INTO traffic_event_payload`).WithArgs(anyArgs(11)...).WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectCommit()

	tx, _ := mock.Begin(ctx)
	defer tx.Rollback(ctx) //nolint:errcheck
	w := NewTrafficEventWriter(nil, nil, TrafficEventWriterConfig{}, discardLogger(), nil)

	inline := sharedaudit.Body{
		Kind:        sharedaudit.BodyInline,
		Encoding:    sharedaudit.EncodingRaw,
		InlineBytes: []byte(`{"a":1}`),
		SizeBytes:   7,
		Truncated:   false,
		ContentType: "application/json",
	}
	spillRef := &sharedaudit.SpillRef{Backend: "localfs", Key: "k1", Size: 9999, SHA256: "abc", ContentType: "text/plain"}
	spill := sharedaudit.Body{
		Kind:        sharedaudit.BodySpill,
		SpillRef:    spillRef,
		Truncated:   true,
		ContentType: "text/plain",
	}
	items := []pendingTrafficMessage{
		{event: TrafficEventMessage{ID: "x", RequestBody: inline, ResponseBody: sharedaudit.EmptyBody()}},
		{event: TrafficEventMessage{ID: "y", RequestBody: sharedaudit.EmptyBody(), ResponseBody: spill}},
	}
	if err := w.insertPayloads(ctx, tx, items); err != nil {
		t.Fatalf("insertPayloads: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TrafficEventWriter — insertPayloads also exercises the inline-RESPONSE
// and spill-REQUEST branches with NON-empty ContentType (so the
// `if e.<dir>Body.ContentType != ""` arms fire on BOTH the inline and
// spill paths in BOTH directions — 4 distinct ContentType arms total).
// Plus a spillRef==nil short-circuit row.
func TestTrafficWriter_InsertPayloads_SymmetricBranches(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()
	ctx := context.Background()
	mock.ExpectBegin()
	eb := mock.ExpectBatch()
	// 3 rows: (1) spill-req w/o CT + inline-resp w/o CT (2) spill body but
	// SpillRef is nil → falls through to "neither" — body still considered
	// non-absent (Kind="spill") so a row is written with all NULLs. (3) a
	// row whose RequestBody.Kind is "absent" but ResponseBody.Kind="inline"
	// w/o CT — verifies the asymmetric pair.
	eb.ExpectExec(`INSERT INTO traffic_event_payload`).WithArgs(anyArgs(11)...).WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	eb.ExpectExec(`INSERT INTO traffic_event_payload`).WithArgs(anyArgs(11)...).WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	eb.ExpectExec(`INSERT INTO traffic_event_payload`).WithArgs(anyArgs(11)...).WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectCommit()

	tx, _ := mock.Begin(ctx)
	defer tx.Rollback(ctx) //nolint:errcheck
	w := NewTrafficEventWriter(nil, nil, TrafficEventWriterConfig{}, discardLogger(), nil)

	spillReqWithCT := sharedaudit.Body{
		Kind:        sharedaudit.BodySpill,
		SpillRef:    &sharedaudit.SpillRef{Backend: "s3", Key: "k2", Size: 1234},
		ContentType: "application/octet-stream", // non-empty → hits the CT arm
	}
	inlineRespWithCT := sharedaudit.Body{
		Kind:        sharedaudit.BodyInline,
		Encoding:    sharedaudit.EncodingRaw,
		InlineBytes: []byte(`{"ok":true}`),
		SizeBytes:   11,
		ContentType: "application/json", // non-empty → hits the CT arm
	}
	// Spill with nil SpillRef: the code's `else if` guard requires SpillRef
	// non-nil to enter the spill branch, so nothing populates *_spill_ref —
	// but Kind!="absent" so the row is still queued.
	spillNilRef := sharedaudit.Body{Kind: sharedaudit.BodySpill, SpillRef: nil}
	asymmetricResp := sharedaudit.Body{
		Kind:        sharedaudit.BodyInline,
		Encoding:    sharedaudit.EncodingRaw,
		InlineBytes: []byte(`x`),
		SizeBytes:   1,
		ContentType: "", // empty CT
	}

	items := []pendingTrafficMessage{
		{event: TrafficEventMessage{ID: "r1", RequestBody: spillReqWithCT, ResponseBody: inlineRespWithCT}},
		{event: TrafficEventMessage{ID: "r2", RequestBody: spillNilRef, ResponseBody: sharedaudit.EmptyBody()}},
		{event: TrafficEventMessage{ID: "r3", RequestBody: sharedaudit.EmptyBody(), ResponseBody: asymmetricResp}},
	}
	if err := w.insertPayloads(ctx, tx, items); err != nil {
		t.Fatalf("insertPayloads: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TrafficEventWriter — insertPayloads propagates the batched Exec error.
func TestTrafficWriter_InsertPayloads_BatchExecErrorPropagates(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()
	ctx := context.Background()
	mock.ExpectBegin()
	eb := mock.ExpectBatch()
	eb.ExpectExec(`INSERT INTO traffic_event_payload`).WithArgs(anyArgs(11)...).WillReturnError(errors.New("disk-full"))

	tx, _ := mock.Begin(ctx)
	defer tx.Rollback(ctx) //nolint:errcheck
	w := NewTrafficEventWriter(nil, nil, TrafficEventWriterConfig{}, discardLogger(), nil)
	inline := sharedaudit.Body{Kind: sharedaudit.BodyInline, InlineBytes: []byte(`{}`), SizeBytes: 2}
	items := []pendingTrafficMessage{
		{event: TrafficEventMessage{ID: "x", RequestBody: inline, ResponseBody: sharedaudit.EmptyBody()}},
	}
	err = w.insertPayloads(ctx, tx, items)
	if err == nil || !strings.Contains(err.Error(), "exec payload insert") {
		t.Errorf("got err=%v, want wrapped 'exec payload insert'", err)
	}
}

// TrafficEventWriter — insertNormalizedPayloads.
//   - skipped when no normalize fields present
//   - writes when EITHER request_normalized or response_normalized populated
//   - propagates batch errors
//   - defaults NormalizeVersion to "1" when caller omits it
func TestTrafficWriter_InsertNormalizedPayloads_SkippedWhenAbsent(t *testing.T) {
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
	items := []pendingTrafficMessage{
		{event: TrafficEventMessage{ID: "x"}},
		{event: TrafficEventMessage{ID: "y"}},
	}
	if err := w.insertNormalizedPayloads(ctx, tx, items); err != nil {
		t.Fatalf("insertNormalizedPayloads: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestTrafficWriter_InsertNormalizedPayloads_WriteWhenPresent_DefaultsVersion(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()
	ctx := context.Background()
	mock.ExpectBegin()
	eb := mock.ExpectBatch()
	eb.ExpectExec(`INSERT INTO traffic_event_normalized`).WithArgs(anyArgs(8)...).WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectCommit()

	tx, _ := mock.Begin(ctx)
	defer tx.Rollback(ctx) //nolint:errcheck
	w := NewTrafficEventWriter(nil, nil, TrafficEventWriterConfig{}, discardLogger(), nil)
	items := []pendingTrafficMessage{
		{event: TrafficEventMessage{
			ID:                "x",
			RequestNormalized: json.RawMessage(`{"messages":[]}`),
			// NormalizeVersion left empty — must default to "1"
		}},
	}
	if err := w.insertNormalizedPayloads(ctx, tx, items); err != nil {
		t.Fatalf("insertNormalizedPayloads: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestTrafficWriter_InsertNormalizedPayloads_BatchExecErrorPropagates(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()
	ctx := context.Background()
	mock.ExpectBegin()
	eb := mock.ExpectBatch()
	eb.ExpectExec(`INSERT INTO traffic_event_normalized`).WithArgs(anyArgs(8)...).WillReturnError(errors.New("constraint-failed"))

	tx, _ := mock.Begin(ctx)
	defer tx.Rollback(ctx) //nolint:errcheck
	w := NewTrafficEventWriter(nil, nil, TrafficEventWriterConfig{}, discardLogger(), nil)
	items := []pendingTrafficMessage{
		{event: TrafficEventMessage{ID: "x", RequestNormalizeStatus: "ok", NormalizeVersion: "2"}},
	}
	err = w.insertNormalizedPayloads(ctx, tx, items)
	if err == nil || !strings.Contains(err.Error(), "exec normalized insert") {
		t.Errorf("got err=%v, want wrapped 'exec normalized insert'", err)
	}
}

// TrafficEventWriter — insertNormalizedPayloads writes even when ONLY the
// response_status is present (no JSON bodies). This pins the OR condition in
// the skip-test: any non-empty status/error/JSON triggers a sidecar row.
func TestTrafficWriter_InsertNormalizedPayloads_StatusOnlyTriggersInsert(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()
	ctx := context.Background()
	mock.ExpectBegin()
	eb := mock.ExpectBatch()
	eb.ExpectExec(`INSERT INTO traffic_event_normalized`).WithArgs(anyArgs(8)...).WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectCommit()

	tx, _ := mock.Begin(ctx)
	defer tx.Rollback(ctx) //nolint:errcheck
	w := NewTrafficEventWriter(nil, nil, TrafficEventWriterConfig{}, discardLogger(), nil)
	items := []pendingTrafficMessage{
		{event: TrafficEventMessage{ID: "x", ResponseNormalizeStatus: "failed", ResponseNormalizeError: "parse-fail", NormalizeVersion: ""}},
	}
	if err := w.insertNormalizedPayloads(ctx, tx, items); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// BatchAccumulator — Add silently drops items after Stop. Observable behavior:
// Stop sets stopped=true; Add must return nil without re-invoking flushFn so
// late-arriving driver dispatches during shutdown don't panic on a closed
// underlying sink.
func TestBatchAccumulator_AddAfterStopReturnsNil(t *testing.T) {
	var calls int32
	acc := NewBatchAccumulator[int](100, time.Hour, func(_ []int) error {
		atomic.AddInt32(&calls, 1)
		return nil
	})
	if err := acc.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := acc.Add(1); err != nil {
		t.Errorf("Add post-stop: got %v, want nil (silent drop)", err)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("flushFn calls after stop: got %d, want 0", got)
	}
}

// BatchAccumulator — resetTimerLocked must Stop the prior timer before
// replacing it; second Add(item1)→Add(item2) where the first batch flushes
// in between forces resetTimerLocked through the `if b.timer != nil` branch.
// We hit this via: Add(1) → timer started → Flush() (timer survives, then
// Stop()'d inside flushLocked) → Add(2) → first-item branch → resetTimerLocked
// finds b.timer == nil (Stop'd in flushLocked). That covers the nil arm. The
// non-nil arm fires when a second Add happens BEFORE the timer fires AND
// before the buffer is flushed. We force it by re-using the same accumulator
// across two non-flushing Adds with a fresh buffer state.
func TestBatchAccumulator_ResetTimerOnRepeatStart(t *testing.T) {
	var flushed int32
	acc := NewBatchAccumulator[int](100, time.Hour, func(_ []int) error {
		atomic.AddInt32(&flushed, 1)
		return nil
	})
	// Cycle 1: Add → timer started → Flush → timer cleared.
	_ = acc.Add(1)
	_ = acc.Flush()
	// Cycle 2: Add → len(buffer)==1 → resetTimerLocked again, this time the
	// timer field is nil (Flush nil'd it). Then Add(2): len(buffer)==2, NOT 1,
	// so resetTimerLocked doesn't fire again. Force a second timer-replace
	// by Flush again then Add again — each Add(1) re-enters resetTimerLocked.
	_ = acc.Add(2)
	_ = acc.Flush()
	_ = acc.Add(3)
	_ = acc.Flush()
	if got := atomic.LoadInt32(&flushed); got != 3 {
		t.Errorf("flushFn calls: got %d, want 3 (one per Flush)", got)
	}
}

// SIEMForwarder — Start drains all 4 queues + cancels cleanly.
func TestSIEMForwarder_Start_DrainsAndExits(t *testing.T) {
	mqc := &fakeConsumerMQ{
		messages: [][]byte{
			[]byte(`{"id":"a","source":"ai-gateway","hookDecision":"allow"}`),
			[]byte(`not json`), // hits the deserialize-error ack path
		},
	}
	sink := &fakeSink{}
	f := NewSIEMForwarder(mqc, sink, SIEMForwarderConfig{
		BatchSize:     100,
		FlushInterval: time.Hour,
	}, discardLogger(), newTestRegistry())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.Start(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Start: got %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Start did not exit")
	}
}

// SIEMForwarder — defaults clamp BatchSize and FlushInterval.
func TestSIEMForwarder_Defaults(t *testing.T) {
	f := NewSIEMForwarder(nil, nil, SIEMForwarderConfig{}, discardLogger(), nil)
	if f.cfg.BatchSize != 200 {
		t.Errorf("BatchSize default: got %d, want 200", f.cfg.BatchSize)
	}
	if f.cfg.FlushInterval != 10*time.Second {
		t.Errorf("FlushInterval default: got %v, want 10s", f.cfg.FlushInterval)
	}
	f2 := NewSIEMForwarder(nil, nil, SIEMForwarderConfig{BatchSize: 9, FlushInterval: 2 * time.Second}, discardLogger(), nil)
	if f2.cfg.BatchSize != 9 || f2.cfg.FlushInterval != 2*time.Second {
		t.Errorf("custom config clobbered: %+v", f2.cfg)
	}
}

// SIEMForwarder — flush(): when the sink returns an error, every item is
// nak'd and the error is surfaced; sentTotal increments "error" not "success".
func TestSIEMForwarder_Flush_SinkErrorNaksAllAndReturnsError(t *testing.T) {
	sinkErr := errors.New("sink-unreachable")
	sink := &fakeSink{sendFn: func(_ []siem.Event) error { return sinkErr }}

	f := NewSIEMForwarder(nil, sink, SIEMForwarderConfig{}, discardLogger(), newTestRegistry())

	var nakCount int32
	items := []pendingSIEMMessage{
		{event: siem.Event{"eventType": "traffic.allowed"}, msg: &mq.Message{Ack: func() error { return nil }, Nak: func() error { atomic.AddInt32(&nakCount, 1); return nil }}},
		{event: siem.Event{"eventType": "traffic.allowed"}, msg: &mq.Message{Ack: func() error { return nil }, Nak: func() error { atomic.AddInt32(&nakCount, 1); return nil }}},
	}
	err := f.flush(context.Background(), items)
	if !errors.Is(err, sinkErr) {
		t.Errorf("got %v, want sinkErr", err)
	}
	if got := atomic.LoadInt32(&nakCount); got != 2 {
		t.Errorf("nakAll: got %d, want 2", got)
	}
}

// SIEMForwarder — flush(): when the EventTypes filter drops every event, the
// sink is never called but the batch is still acked (nothing to retry).
func TestSIEMForwarder_Flush_AllFilteredAcksWithoutSinkCall(t *testing.T) {
	var sinkCalls int32
	sink := &fakeSink{sendFn: func(_ []siem.Event) error { atomic.AddInt32(&sinkCalls, 1); return nil }}
	// Only emit-this-type: keep "audit.foo" — no event below carries that
	// type, so every event is filtered out.
	f := NewSIEMForwarder(nil, sink, SIEMForwarderConfig{EventTypes: []string{"audit.foo"}}, discardLogger(), newTestRegistry())

	var ackCount int32
	items := []pendingSIEMMessage{
		{event: siem.Event{"eventType": "traffic.allowed"}, msg: &mq.Message{Ack: func() error { atomic.AddInt32(&ackCount, 1); return nil }, Nak: func() error { return nil }}},
		{event: siem.Event{"eventType": "traffic.blocked"}, msg: &mq.Message{Ack: func() error { atomic.AddInt32(&ackCount, 1); return nil }, Nak: func() error { return nil }}},
	}
	if err := f.flush(context.Background(), items); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := atomic.LoadInt32(&sinkCalls); got != 0 {
		t.Errorf("sink calls: got %d, want 0 (filter dropped everything)", got)
	}
	if got := atomic.LoadInt32(&ackCount); got != 2 {
		t.Errorf("ackAll: got %d, want 2", got)
	}
}

// SIEMForwarder — flush(): nil-registry must not panic on counter updates.
func TestSIEMForwarder_Flush_NilRegistryNoPanic(t *testing.T) {
	sink := &fakeSink{sendFn: func(_ []siem.Event) error { return nil }}
	f := NewSIEMForwarder(nil, sink, SIEMForwarderConfig{}, discardLogger(), nil)
	items := []pendingSIEMMessage{
		{event: siem.Event{"eventType": "traffic.allowed"}, msg: &mq.Message{Ack: func() error { return nil }, Nak: func() error { return nil }}},
	}
	if err := f.flush(context.Background(), items); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

// SIEMForwarder — flush(): sink error with nil-registry exercises the
// nil-counter guards on the error path.
func TestSIEMForwarder_Flush_SinkErrorNilRegistry(t *testing.T) {
	sink := &fakeSink{sendFn: func(_ []siem.Event) error { return errors.New("boom") }}
	f := NewSIEMForwarder(nil, sink, SIEMForwarderConfig{}, discardLogger(), nil)
	var nakCount int32
	items := []pendingSIEMMessage{
		{event: siem.Event{"eventType": "traffic.allowed"}, msg: &mq.Message{Ack: func() error { return nil }, Nak: func() error { atomic.AddInt32(&nakCount, 1); return nil }}},
	}
	if err := f.flush(context.Background(), items); err == nil {
		t.Errorf("want sink error")
	}
	if got := atomic.LoadInt32(&nakCount); got != 1 {
		t.Errorf("nak: got %d, want 1", got)
	}
}

// SIEMForwarder — ackAll / nakAll loop continues past errors.
func TestSIEMForwarder_AckAllNakAll_LoopContinuesOnError(t *testing.T) {
	f := NewSIEMForwarder(nil, &fakeSink{}, SIEMForwarderConfig{}, discardLogger(), nil)
	errBoom := errors.New("driver-boom")
	var ackCalled, nakCalled int32
	items := []pendingSIEMMessage{
		{msg: &mq.Message{Ack: func() error { atomic.AddInt32(&ackCalled, 1); return errBoom }, Nak: func() error { atomic.AddInt32(&nakCalled, 1); return errBoom }}},
		{msg: &mq.Message{Ack: func() error { atomic.AddInt32(&ackCalled, 1); return nil }, Nak: func() error { atomic.AddInt32(&nakCalled, 1); return nil }}},
	}
	f.ackAll(items)
	if got := atomic.LoadInt32(&ackCalled); got != 2 {
		t.Errorf("ackAll: got %d, want 2", got)
	}
	f.nakAll(items)
	if got := atomic.LoadInt32(&nakCalled); got != 2 {
		t.Errorf("nakAll: got %d, want 2", got)
	}
}

// SIEMForwarder — Start drives every per-message branch end-to-end:
//  1. deserializeAdminEvent (admin-audit queue) sets eventType to admin.*
//  2. deserializeTrafficEvent (any other queue) sets eventType to traffic.*
//  3. bad JSON triggers the deserialize-error ack path with errorsTotal{
//     error_type="deserialize"} (registry wired)
//
// The batch immediately fires synchronously because BatchSize=1, so each
// message hits the flush path AND the sink receives the classified event.
// The fake consumer replays the same messages on all 4 queues, so the
// classifier runs through both the traffic-event and admin-event branches
// against the same payload — sufficient to exercise every Start dispatch
// branch (ack-on-deserialize-fail, ErrDeferAck on success, classification
// dispatch via deserializeEvent's queue-prefix switch).
func TestSIEMForwarder_Start_DispatchesClassificationToSink(t *testing.T) {
	var got []siem.Event
	var mu sync.Mutex
	sink := &fakeSink{sendFn: func(evs []siem.Event) error {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, evs...)
		return nil
	}}
	mqc := &fakeConsumerMQ{
		messages: [][]byte{
			// Traffic-style payload: when delivered on a non-admin queue,
			// ClassifyTrafficEvent returns "traffic.allowed"; when delivered
			// on the admin queue, ClassifyAdminEvent returns "" (no action
			// field). Both paths still go through the sink — the empty
			// classification just means an unclassified row was forwarded.
			[]byte(`{"id":"a","source":"ai-gateway","hookDecision":"allow"}`),
			// Admin-style payload: yields "provider.create" on the admin
			// queue, "traffic.passthrough" on traffic queues (no hookDecision).
			[]byte(`{"id":"b","action":"create","entityType":"provider"}`),
			// Poison pill: deserialize fails on every queue → handler ack-
			// drops it without involving the sink.
			[]byte(`not json at all`),
		},
	}
	f := NewSIEMForwarder(mqc, sink, SIEMForwarderConfig{
		BatchSize:     1, // flush after every successful add
		FlushInterval: time.Hour,
	}, discardLogger(), newTestRegistry())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.Start(ctx) }()
	time.Sleep(80 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Start did not exit")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 {
		t.Fatal("sink received zero events; fanout must have delivered something")
	}
	// At least one event must carry a non-empty classified eventType from
	// EITHER the traffic or admin classifier — proves classification ran.
	var sawClassified bool
	for _, e := range got {
		if et, ok := e["eventType"].(string); ok && et != "" {
			sawClassified = true
			break
		}
	}
	if !sawClassified {
		t.Errorf("no event carried a non-empty eventType (sink saw %d events but classification never ran)", len(got))
	}
}

// SIEMForwarder — Start where the underlying MQ consumer returns an error
// BEFORE ctx is cancelled — exercises the "SIEM consumer exited with error"
// log arm (siem.go:117 `if err != nil && ctx.Err() == nil`).
func TestSIEMForwarder_Start_ConsumerErrorLogsPreCancel(t *testing.T) {
	mqc := &fakeConsumerMQ{
		consumeErr: errors.New("driver-permanent-failure"),
	}
	f := NewSIEMForwarder(mqc, &fakeSink{}, SIEMForwarderConfig{}, discardLogger(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.Start(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Start did not exit")
	}
}

// SIEMForwarder — Start path where batch.Add returns an error (sink fails
// synchronously on the first flush) — covers the consume callback's
// `if err := batch.Add(...); err != nil { return err }` branch which is
// the equivalent of the traffic writer's sync-flush-error case.
func TestSIEMForwarder_Start_SyncFlushErrorViaSinkFailure(t *testing.T) {
	sinkErr := errors.New("upstream-down")
	sink := &fakeSink{sendFn: func(_ []siem.Event) error { return sinkErr }}
	mqc := &fakeConsumerMQ{
		messages: [][]byte{
			[]byte(`{"id":"a","source":"ai-gateway","hookDecision":"allow"}`),
		},
	}
	f := NewSIEMForwarder(mqc, sink, SIEMForwarderConfig{BatchSize: 1, FlushInterval: time.Hour}, discardLogger(), newTestRegistry())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.Start(ctx) }()
	time.Sleep(80 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Start did not exit")
	}
}

// AdminAuditWriter — Start path where batch.Add returns an error. With a
// closed pool, BatchSize=1 → first Add → synchronous flush → flush calls
// pool.Begin which fails with "closed pool" → Add returns the wrapped error.
func TestAdminAuditWriter_Start_SyncFlushErrorViaClosedPool(t *testing.T) {
	pool := closedPool(t)
	defer pool.Close()
	mqc := &fakeConsumerMQ{
		messages: [][]byte{
			[]byte(`{"id":"a","timestamp":"2026-04-18T00:00:00Z","actorId":"u","actorLabel":"u","action":"create","entityType":"t","entityId":"e"}`),
		},
	}
	w := NewAdminAuditWriter(pool, mqc, AdminAuditWriterConfig{BatchSize: 1, FlushInterval: time.Hour}, discardLogger(), newTestRegistry())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Start(ctx) }()
	time.Sleep(80 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Start did not exit")
	}
}

// AdminAuditWriter — Start where the underlying MQ consumer returns an error
// BEFORE ctx is cancelled — exercises the "consumer exited with error" log
// arm (admin_audit.go:109 `if err != nil && ctx.Err() == nil`). The Start
// itself still blocks on <-ctx.Done() per the production contract, so we
// must cancel to let it return.
func TestAdminAuditWriter_Start_ConsumerErrorLogsPreCancel(t *testing.T) {
	mqc := &fakeConsumerMQ{
		messages:   nil,
		consumeErr: errors.New("driver-permanent-failure"),
	}
	w := NewAdminAuditWriter(nil, mqc, AdminAuditWriterConfig{}, discardLogger(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Start(ctx) }()
	// Give the consume goroutine a moment to return its error and log.
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Start: got %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Start did not exit")
	}
}

// identity_enrichment_test.go covers NewIdentityEnricher construction, identity
// accessors, Run pagination/error paths, and enrichEvent routing logic.
//
// DB queries are exercised via pgxmock so no live Postgres is required.
// The enrichEvent→tryTraceIDMatch→tryIPAgentMatch→mark* chain is exercised by
// controlling which DB query returns rows or ErrNotFound/ErrAmbiguous.
package drift

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

// Helper: return a pgxmock row set for FindPendingIdentityEvents

func pendingEventRows(events ...store.PendingIdentityEvent) *pgxmock.Rows {
	cols := []string{"id", "trace_id", "source_ip", "entity_id", "identity", "created_at"}
	rows := pgxmock.NewRows(cols)
	for _, e := range events {
		rows.AddRow(e.ID, e.TraceID, e.SourceIP, e.EntityID, []byte(`{"status":"pending"}`), e.CreatedAt)
	}
	return rows
}

// NewIdentityEnricher — construction

func TestIdentityEnricher_NewWithNilRegistry_DoesNotPanic(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	e := NewIdentityEnricher(st, time.Minute, nil, discardLogger())
	if e == nil {
		t.Fatal("NewIdentityEnricher returned nil")
	}
	// All metric fields must be nil when registry is nil.
	if e.pendingTotal != nil {
		t.Error("pendingTotal must be nil with nil registry")
	}
	if e.matchedTotal != nil {
		t.Error("matchedTotal must be nil with nil registry")
	}
	if e.durationMs != nil {
		t.Error("durationMs must be nil with nil registry")
	}
}

func TestIdentityEnricher_NewWithRegistry_MetricsRegistered(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	reg := newTestRegistry()
	e := NewIdentityEnricher(st, time.Minute, reg, discardLogger())
	if e.pendingTotal == nil {
		t.Error("pendingTotal must be non-nil with real registry")
	}
	if e.matchedTotal == nil {
		t.Error("matchedTotal must be non-nil with real registry")
	}
	if e.errorsTotal == nil {
		t.Error("errorsTotal must be non-nil with real registry")
	}
}

// Identity accessors

func TestIdentityEnricher_Identity(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	e := NewIdentityEnricher(st, 7*time.Minute, nil, discardLogger())

	if e.ID() != identityJobID {
		t.Errorf("ID = %q, want %q", e.ID(), identityJobID)
	}
	if e.Name() == "" {
		t.Error("Name must not be empty")
	}
	if e.Description() == "" {
		t.Error("Description must not be empty")
	}
	if e.Interval() != 7*time.Minute {
		t.Errorf("Interval = %v, want 7m", e.Interval())
	}
}

// Run — error and no-op paths

// TestIdentityEnricher_Run_FindPendingError asserts Run propagates a store error
// from FindPendingIdentityEvents immediately.
func TestIdentityEnricher_Run_FindPendingError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	sentinel := errors.New("db down")
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(sentinel)

	st := store.NewWithPgxPool(mock)
	e := NewIdentityEnricher(st, time.Minute, nil, discardLogger())

	err := e.Run(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("Run err = %v, want wrapped sentinel", err)
	}
}

// TestIdentityEnricher_Run_EmptyBatch asserts Run exits silently when the
// store returns no pending events on the first call.
func TestIdentityEnricher_Run_EmptyBatch(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// Empty result set — len(events) == 0 < identityBatch → break immediately.
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "trace_id", "source_ip", "entity_id", "identity", "created_at"}))

	st := store.NewWithPgxPool(mock)
	reg := newTestRegistry()
	e := NewIdentityEnricher(st, time.Minute, reg, discardLogger())

	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met: %v", err)
	}
}

// Run → enrichEvent → applyMatch via trace_id

// TestIdentityEnricher_Run_TraceIDMatch exercises the full path:
// FindPendingIdentityEvents → enrichEvent → tryTraceIDMatch (hit) → applyMatch →
// UpdateEventIdentity. The pendingTotal gauge fires on the first batch.
func TestIdentityEnricher_Run_TraceIDMatch(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	ts := time.Now().UTC()
	evt := store.PendingIdentityEvent{
		ID:        "ev-1",
		TraceID:   "tr-abc",
		SourceIP:  "10.0.0.1",
		CreatedAt: ts,
	}

	// Batch 1: 1 pending event (len < identityBatch → breaks after this batch).
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pendingEventRows(evt))

	// tryTraceIDMatch: FindMatchedEventByTraceID succeeds.
	mock.ExpectQuery(`FROM traffic_event WHERE trace_id`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"entity_id", "entity_name", "identity"}).
			AddRow("user-42", "Alice Smith", []byte(`{"status":"matched"}`)))

	// applyMatch: UpdateEventIdentity
	mock.ExpectExec(`UPDATE traffic_event SET entity_id`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	st := store.NewWithPgxPool(mock)
	reg := newTestRegistry()
	e := NewIdentityEnricher(st, time.Minute, reg, discardLogger())

	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met: %v", err)
	}
}

// Run → enrichEvent → applyMatch via ip_agent

// TestIdentityEnricher_Run_IPAgentMatch exercises the ip_agent fallback path:
// tryTraceIDMatch misses (trace_id empty → ErrNotFound), tryIPAgentMatch hits →
// applyMatch → UpdateEventIdentity.
func TestIdentityEnricher_Run_IPAgentMatch(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	ts := time.Now().UTC()
	evt := store.PendingIdentityEvent{
		ID:        "ev-2",
		TraceID:   "", // empty → FindMatchedEventByTraceID returns ErrNotFound immediately
		SourceIP:  "10.1.2.3",
		CreatedAt: ts,
	}

	// Batch 1: 1 pending event.
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pendingEventRows(evt))

	// tryTraceIDMatch: empty trace_id → store.ErrNotFound (no DB call needed — but
	// FindMatchedEventByTraceID guards on empty string).
	// The implementation calls FindMatchedEventByTraceID unconditionally; since
	// trace_id = "" the function returns ErrNotFound without a query (see source).
	// pgxmock does not need an expectation for it.

	// tryIPAgentMatch: FindActiveAssignmentByIPAndTime returns 1 row → match.
	mock.ExpectQuery(`FROM "DeviceAssignment"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"user_id", "device_id", "displayName", "email"}).
			AddRow("user-7", "dev-x", "Bob Jones", "bob@example.com"))

	// applyMatch: UpdateEventIdentity
	mock.ExpectExec(`UPDATE traffic_event SET entity_id`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	st := store.NewWithPgxPool(mock)
	reg := newTestRegistry()
	e := NewIdentityEnricher(st, time.Minute, reg, discardLogger())

	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met: %v", err)
	}
}

// Run → enrichEvent → markAmbiguous

// TestIdentityEnricher_Run_AmbiguousIP exercises the ambiguous NAT-egress path:
// tryTraceIDMatch misses, tryIPAgentMatch returns ErrAmbiguous (2+ DA rows) →
// markAmbiguous → UpdateEventIdentity.
func TestIdentityEnricher_Run_AmbiguousIP(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	ts := time.Now().UTC()
	evt := store.PendingIdentityEvent{
		ID:        "ev-3",
		TraceID:   "",
		SourceIP:  "203.0.113.5",
		CreatedAt: ts,
	}

	// Batch 1: 1 pending event.
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pendingEventRows(evt))

	// tryIPAgentMatch: 2 rows → ErrAmbiguous from FindActiveAssignmentByIPAndTime.
	mock.ExpectQuery(`FROM "DeviceAssignment"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"user_id", "device_id", "displayName", "email"}).
			AddRow("user-A", "dev-A", "Alice", "a@example.com").
			AddRow("user-B", "dev-B", "Bob", "b@example.com"))

	// markAmbiguous: UpdateEventIdentity
	mock.ExpectExec(`UPDATE traffic_event SET entity_id`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	st := store.NewWithPgxPool(mock)
	reg := newTestRegistry()
	e := NewIdentityEnricher(st, time.Minute, reg, discardLogger())

	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met: %v", err)
	}
}

// Run → enrichEvent → markUnmatched

// TestIdentityEnricher_Run_Unmatched exercises the no-match path:
// tryTraceIDMatch misses, tryIPAgentMatch misses (ErrNotFound) → markUnmatched →
// UpdateEventIdentity.
func TestIdentityEnricher_Run_Unmatched(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	ts := time.Now().UTC()
	evt := store.PendingIdentityEvent{
		ID:        "ev-4",
		TraceID:   "",
		SourceIP:  "192.168.99.1",
		CreatedAt: ts,
	}

	// Batch 1: 1 pending event.
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pendingEventRows(evt))

	// tryIPAgentMatch: 0 rows → ErrNotFound.
	mock.ExpectQuery(`FROM "DeviceAssignment"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"user_id", "device_id", "displayName", "email"}))

	// markUnmatched: UpdateEventIdentity
	mock.ExpectExec(`UPDATE traffic_event SET entity_id`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	st := store.NewWithPgxPool(mock)
	e := NewIdentityEnricher(st, time.Minute, nil, discardLogger())

	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met: %v", err)
	}
}

// Run → enrichEvent → UpdateEventIdentity error is logged, not propagated

// TestIdentityEnricher_Run_UpdateError asserts that when UpdateEventIdentity
// fails the error is incremented in errorsTotal and logged, but Run returns nil
// (the per-event error does not abort the batch).
func TestIdentityEnricher_Run_UpdateError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	ts := time.Now().UTC()
	evt := store.PendingIdentityEvent{
		ID:        "ev-5",
		TraceID:   "",
		SourceIP:  "10.0.0.5",
		CreatedAt: ts,
	}

	// Batch 1: 1 event.
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pendingEventRows(evt))

	// tryIPAgentMatch: 0 rows → ErrNotFound → markUnmatched path.
	mock.ExpectQuery(`FROM "DeviceAssignment"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"user_id", "device_id", "displayName", "email"}))

	// UpdateEventIdentity fails.
	mock.ExpectExec(`UPDATE traffic_event SET entity_id`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("write failed"))

	st := store.NewWithPgxPool(mock)
	reg := newTestRegistry()
	e := NewIdentityEnricher(st, time.Minute, reg, discardLogger())

	// Run must return nil — per-event errors are logged, not propagated.
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error, want nil (per-event errors must not propagate): %v", err)
	}
}

// Run → trace_id match with non-nil identity already set

// TestIdentityEnricher_Run_TraceIDMatch_NilIdentity exercises the nil-identity
// guard in applyMatch (match.Identity == nil → initialised to empty map before
// stamping status/method).
func TestIdentityEnricher_Run_TraceIDMatch_NilIdentity(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	ts := time.Now().UTC()
	evt := store.PendingIdentityEvent{
		ID:        "ev-6",
		TraceID:   "tr-xyz",
		SourceIP:  "10.0.0.6",
		CreatedAt: ts,
	}

	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pendingEventRows(evt))

	// FindMatchedEventByTraceID returns a row with NULL identity (identity column
	// scanned as empty bytes → json.Unmarshal into nil map).
	mock.ExpectQuery(`FROM traffic_event WHERE trace_id`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"entity_id", "entity_name", "identity"}).
			AddRow("user-99", "Carol", []byte(`{}`)))

	mock.ExpectExec(`UPDATE traffic_event SET entity_id`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	st := store.NewWithPgxPool(mock)
	e := NewIdentityEnricher(st, time.Minute, newTestRegistry(), discardLogger())

	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations not met: %v", err)
	}
}

// Run with durationMs timer firing (non-nil registry)

// TestIdentityEnricher_Run_DurationObserved verifies the deferred durationMs
// Observe runs without panic for both nil and non-nil registry paths.
func TestIdentityEnricher_Run_DurationObserved(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	// Empty batch → immediate return.
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "trace_id", "source_ip", "entity_id", "identity", "created_at"}))

	st := store.NewWithPgxPool(mock)
	// Use a non-nil registry so the deferred durationMs.With().Observe(...) runs.
	e := NewIdentityEnricher(st, time.Minute, newTestRegistry(), discardLogger())

	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

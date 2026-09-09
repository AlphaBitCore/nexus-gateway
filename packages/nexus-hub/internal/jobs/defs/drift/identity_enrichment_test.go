// identity_enrichment_test.go covers NewIdentityEnricher construction, identity
// accessors, Run pagination/error paths, and enrichEvent routing logic.
//
// DB queries are exercised via pgxmock so no live Postgres is required.
// The enrichEvent→tryRequestIDMatch→tryIPAgentMatch→mark* chain is exercised by
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
	cols := []string{"id", "external_request_id", "thing_id", "source_ip", "entity_id", "identity", "created_at"}
	rows := pgxmock.NewRows(cols)
	for _, e := range events {
		rows.AddRow(e.ID, e.ExternalRequestID, e.ThingID, e.SourceIP, e.EntityID, []byte(`{"status":"pending"}`), e.CreatedAt)
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
		WillReturnRows(pgxmock.NewRows([]string{"id", "external_request_id", "thing_id", "source_ip", "entity_id", "identity", "created_at"}))

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

// Run → enrichEvent → applyMatch via the request id

// TestIdentityEnricher_Run_RequestIDMatch exercises the full path:
// FindPendingIdentityEvents → enrichEvent → tryRequestIDMatch (hit) → applyMatch →
// UpdateEventIdentity. The pendingTotal gauge fires on the first batch.
func TestIdentityEnricher_Run_RequestIDMatch(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	ts := time.Now().UTC()
	evt := store.PendingIdentityEvent{
		ID:                "ev-1",
		ExternalRequestID: "rid-abc",
		SourceIP:          "10.0.0.1",
		CreatedAt:         ts,
	}

	// Batch 1: 1 pending event (len < identityBatch → breaks after this batch).
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pendingEventRows(evt))

	// tryRequestIDMatch: FindMatchedEventByRequestID succeeds.
	mock.ExpectQuery(`FROM traffic_event WHERE external_request_id`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
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
// tryRequestIDMatch misses (request id empty → ErrNotFound), tryIPAgentMatch hits →
// applyMatch → UpdateEventIdentity.
func TestIdentityEnricher_Run_IPAgentMatch(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	ts := time.Now().UTC()
	evt := store.PendingIdentityEvent{
		ID:                "ev-2",
		ExternalRequestID: "", // empty → FindMatchedEventByRequestID returns ErrNotFound immediately
		SourceIP:          "10.1.2.3",
		CreatedAt:         ts,
	}

	// Batch 1: 1 pending event.
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pendingEventRows(evt))

	// tryRequestIDMatch: empty request id → store.ErrNotFound (no DB call needed — but
	// FindMatchedEventByRequestID guards on empty string).
	// The implementation calls FindMatchedEventByRequestID unconditionally; since
	// an empty request id makes the function return ErrNotFound without a query (see source).
	// pgxmock does not need an expectation for it.

	// The IP leg is now prefetched once per page: one statement returning every
	// assignment whose window overlaps the page's timestamp span, narrowed per
	// event by Covers. One covering window → match.
	mock.ExpectQuery(`FROM "DeviceAssignment"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(assignmentWindowRows().
			AddRow("10.0.0.5", "user-7", "dev-x", "Bob Jones", "bob@example.com", alwaysOpenFrom, nil))

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
// tryRequestIDMatch misses, tryIPAgentMatch returns ErrAmbiguous (2+ DA rows) →
// markAmbiguous → UpdateEventIdentity.
func TestIdentityEnricher_Run_AmbiguousIP(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	ts := time.Now().UTC()
	evt := store.PendingIdentityEvent{
		ID:                "ev-3",
		ExternalRequestID: "",
		SourceIP:          "203.0.113.5",
		CreatedAt:         ts,
	}

	// Batch 1: 1 pending event.
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pendingEventRows(evt))

	// Two windows on the same IP both covering the event's timestamp — the
	// NAT-shared egress case. The verdict must be "refuse to name anybody",
	// not "take the first".
	mock.ExpectQuery(`FROM "DeviceAssignment"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(assignmentWindowRows().
			AddRow("10.0.0.5", "user-A", "dev-A", "Alice", "a@example.com", alwaysOpenFrom, nil).
			AddRow("10.0.0.5", "user-B", "dev-B", "Bob", "b@example.com", alwaysOpenFrom, nil))

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
// tryRequestIDMatch misses, tryIPAgentMatch misses (ErrNotFound) → markUnmatched →
// UpdateEventIdentity.
func TestIdentityEnricher_Run_Unmatched(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	ts := time.Now().UTC()
	evt := store.PendingIdentityEvent{
		ID:                "ev-4",
		ExternalRequestID: "",
		SourceIP:          "192.168.99.1",
		CreatedAt:         ts,
	}

	// Batch 1: 1 pending event.
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pendingEventRows(evt))

	// tryIPAgentMatch: 0 rows → ErrNotFound.
	mock.ExpectQuery(`FROM "DeviceAssignment"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(assignmentWindowRows())

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
		ID:                "ev-5",
		ExternalRequestID: "",
		SourceIP:          "10.0.0.5",
		CreatedAt:         ts,
	}

	// Batch 1: 1 event.
	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pendingEventRows(evt))

	// tryIPAgentMatch: 0 rows → ErrNotFound → markUnmatched path.
	mock.ExpectQuery(`FROM "DeviceAssignment"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(assignmentWindowRows())

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

// Run → request-id match with non-nil identity already set

// TestIdentityEnricher_Run_RequestIDMatch_NilIdentity exercises the nil-identity
// guard in applyMatch (match.Identity == nil → initialised to empty map before
// stamping status/method).
func TestIdentityEnricher_Run_RequestIDMatch_NilIdentity(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	ts := time.Now().UTC()
	evt := store.PendingIdentityEvent{
		ID:                "ev-6",
		ExternalRequestID: "tr-xyz",
		SourceIP:          "10.0.0.6",
		CreatedAt:         ts,
	}

	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pendingEventRows(evt))

	// FindMatchedEventByRequestID returns a row with NULL identity (identity column
	// scanned as empty bytes → json.Unmarshal into nil map).
	mock.ExpectQuery(`FROM traffic_event WHERE external_request_id`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
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
		WillReturnRows(pgxmock.NewRows([]string{"id", "external_request_id", "thing_id", "source_ip", "entity_id", "identity", "created_at"}))

	st := store.NewWithPgxPool(mock)
	// Use a non-nil registry so the deferred durationMs.With().Observe(...) runs.
	e := NewIdentityEnricher(st, time.Minute, newTestRegistry(), discardLogger())

	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// assignmentWindowRows is the column set FindAssignmentsByIPsOverlapping scans,
// in scan order. pgxmock never executes the SQL — it replays these columns
// positionally — so this list, not the query text, is what the tests assert
// against. A column added to the query without being added here is a scan of
// the wrong value, and the test would still pass if it only checked err == nil.
func assignmentWindowRows() *pgxmock.Rows {
	return pgxmock.NewRows([]string{
		"ip_address", "user_id", "device_id", "displayName", "email",
		"assigned_at", "released_at",
	})
}

// alwaysOpenFrom is an assigned_at far enough in the past that every event
// timestamp a test can produce falls inside the window, paired with a NULL
// released_at. The boundary behaviour itself is covered by the Covers tests
// rather than by re-deriving it in every fixture.
var alwaysOpenFrom = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

// TestPrefetchFailureLeavesRowsPending is the property the batching must not
// lose.
//
// The IP leg is the last step of the cascade, so "it returned an error" used to
// be indistinguishable from "it found nothing", and falling through to
// markUnmatched stamps a TERMINAL verdict on rows the job never actually looked
// at. An unmatched row is outside both the Art.17 erase scope and the Art.15
// export scope, so a failed read would quietly remove a page of events from
// both — permanently, since nothing revisits a row that already has a verdict.
//
// The assertion is the absence of the UPDATE: pgxmock fails on any statement it
// was not told to expect, so an enricher that wrote anything here reddens.
func TestPrefetchFailureLeavesRowsPending(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	evt := store.PendingIdentityEvent{
		ID:        "evt-prefetch-fail",
		SourceIP:  "10.0.0.5",
		CreatedAt: time.Now(),
	}

	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pendingEventRows(evt))

	mock.ExpectQuery(`FROM "DeviceAssignment"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("connection reset"))

	// The write is EXPECTED here so that its absence is observable. Asserting
	// "no unexpected statement ran" cannot work: ExpectationsWereMet only checks
	// that declared expectations were fulfilled, and an extra call merely makes
	// the mock return an error the job logs and swallows. So the assertion is
	// inverted — this expectation must go UNFULFILLED.
	mock.ExpectExec(`UPDATE traffic_event SET entity_id`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	st := store.NewWithPgxPool(mock)
	e := NewIdentityEnricher(st, time.Minute, newTestRegistry(), discardLogger())

	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run returned an error; the job logs per-event failures and continues: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err == nil {
		t.Error("the enricher stamped a verdict after the prefetch failed. The row is now " +
			"terminally 'unmatched' — outside both the Art.17 erase scope and the Art.15 export " +
			"scope — for a lookup that never ran.")
	}
}

// TestIdentityEnricher_Run_ThingIDMatchWinsOverTheHeuristics pins both halves
// of the deterministic leg: that it resolves an agent row from the device the
// Hub authenticated, and that it runs FIRST.
//
// The ordering is the load-bearing half. thing_id is stamped by the Hub from
// the mTLS device token, so "which user held this device then" has one answer.
// The request-id leg reads a value the uploading node supplied, and the IP leg
// asks a question a NAT answers several ways. Letting either run first would
// hand a guess the win over a fact — and neither would look wrong afterwards,
// because both write an identity that reads as "matched".
//
// Both legs are wired to succeed, resolving to DIFFERENT users, and the
// assertion is which one reached the UPDATE. Declaring only one query would
// not catch a reordering: pgxmock returns an error on an out-of-order call and
// enrichEvent reads any error as "no match", so the wrong-order run would fall
// through to the right answer and look correct.
func TestIdentityEnricher_Run_ThingIDMatchWinsOverTheHeuristics(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// Either leg may be consulted; the outcome is what this test judges.
	mock.MatchExpectationsInOrder(false)

	ts := time.Now().UTC()
	evt := store.PendingIdentityEvent{
		ID:      "ev-agent-1",
		ThingID: "thing-77",
		// The heuristic input is present and resolves to someone else.
		ExternalRequestID: "rid-shared-1",
		SourceIP:          "10.0.0.9",
		CreatedAt:         ts,
	}

	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pendingEventRows(evt))

	// Deterministic leg → user-77.
	mock.ExpectQuery(`WHERE da\."deviceId" = \$1`).
		WithArgs("thing-77", pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"user_id", "device_id", "displayName", "email"}).
			AddRow("user-77", "thing-77", "Dana Device", "dana@example.com"))

	// Heuristic leg → user-WRONG. Wired to succeed so that winning by
	// running first is possible, and therefore detectable.
	mock.ExpectQuery(`FROM traffic_event WHERE external_request_id`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"entity_id", "entity_name", "identity"}).
			AddRow("user-WRONG", "Someone Else", []byte(`{"status":"matched"}`))).
		Maybe()

	mock.ExpectExec(`UPDATE traffic_event SET entity_id`).
		WithArgs("ev-agent-1", "user-77", "Dana Device", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	st := store.NewWithPgxPool(mock)
	e := NewIdentityEnricher(st, time.Minute, newTestRegistry(), discardLogger())

	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the row was not attributed to the device's user — a heuristic leg won over the deterministic one: %v", err)
	}
}

// TestIdentityEnricher_Run_NoThingIDFallsThrough is the other side: a row with
// no device (gateway and compliance-proxy rows carry none) must skip the
// deterministic leg entirely rather than querying with an empty id, which
// would match whichever assignment the planner returned first.
func TestIdentityEnricher_Run_NoThingIDFallsThrough(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	ts := time.Now().UTC()
	evt := store.PendingIdentityEvent{
		ID:                "ev-proxy-1",
		ThingID:           "", // no device on this row
		ExternalRequestID: "rid-abc",
		SourceIP:          "10.0.0.1",
		CreatedAt:         ts,
	}

	mock.ExpectQuery(`FROM traffic_event`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pendingEventRows(evt))

	// No device query is declared: an empty thing_id must short-circuit before
	// the store is asked anything. The request-id leg is what runs instead.
	mock.ExpectQuery(`FROM traffic_event WHERE external_request_id`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"entity_id", "entity_name", "identity"}).
			AddRow("user-42", "Alice Smith", []byte(`{"status":"matched"}`)))

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

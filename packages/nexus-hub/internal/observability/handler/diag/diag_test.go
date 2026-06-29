package diag

import (
	"bytes"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// makeDiagCtx builds a POST context with a JSON body for the diag drain endpoint.
func makeDiagCtx(t *testing.T, body any) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/internal/things/diag-events:batch", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec), rec
}

// diagInsertArgCount is the count of bound parameters in the INSERT INTO
// thing_diag_event query in opsmetrics_diag.go. Tracks the column list
// 1:1 — bump in lockstep when a new column lands on the drain path. The
// trace_id column lifted the count from 14 to 15.
const diagInsertArgCount = 15

// expectInsert registers a pgxmock expectation for the INSERT INTO
// thing_diag_event query.
func expectInsert(mock pgxmock.PgxPoolIface) {
	args := make([]any, diagInsertArgCount)
	for i := range args {
		args[i] = pgxmock.AnyArg()
	}
	mock.ExpectExec(`INSERT INTO thing_diag_event`).
		WithArgs(args...).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
}

// expectInsertError registers an error expectation for the INSERT query.
func expectInsertError(mock pgxmock.PgxPoolIface, err error) {
	args := make([]any, diagInsertArgCount)
	for i := range args {
		args[i] = pgxmock.AnyArg()
	}
	mock.ExpectExec(`INSERT INTO thing_diag_event`).
		WithArgs(args...).
		WillReturnError(err)
}

// UploadDiagEvents: routing invariants

func TestDiagDrain_NilPool_503(t *testing.T) {
	h := &DiagDrainAPI{Pool: nil}
	c, rec := makeDiagCtx(t, DiagDrainRequest{Events: []DiagDrainEvent{{ID: "e1"}}})
	if err := h.UploadDiagEvents(c); err != nil {
		t.Fatalf("UploadDiagEvents: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d; want 503 when Pool is nil", rec.Code)
	}
}

func TestDiagDrain_EmptyBatch_400(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	h := &DiagDrainAPI{Pool: mock}
	c, rec := makeDiagCtx(t, DiagDrainRequest{Events: nil})
	if err := h.UploadDiagEvents(c); err != nil {
		t.Fatalf("UploadDiagEvents: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status %d; want 400 for empty event list", rec.Code)
	}
}

func TestDiagDrain_OversizeBatch_413(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	h := &DiagDrainAPI{Pool: mock}
	events := make([]DiagDrainEvent, maxDiagDrainBatchSize+1)
	for i := range events {
		events[i] = DiagDrainEvent{ID: "e", DiagEvent: opsmetrics.DiagEvent{Source: "s"}}
	}
	c, rec := makeDiagCtx(t, DiagDrainRequest{Events: events})
	if err := h.UploadDiagEvents(c); err != nil {
		t.Fatalf("UploadDiagEvents: %v", err)
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status %d; want 413 for oversized batch", rec.Code)
	}
}

func TestDiagDrain_InvalidBody_400(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	h := &DiagDrainAPI{Pool: mock}
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte("not json{")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.UploadDiagEvents(c); err != nil {
		t.Fatalf("UploadDiagEvents: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status %d; want 400 for invalid body", rec.Code)
	}
}

// TestDiagDrain_HappyPath verifies that a well-formed event batch is inserted
// and the acceptedIds list reflects the events that were stored.
func TestDiagDrain_HappyPath(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	expectInsert(mock)

	h := &DiagDrainAPI{Pool: mock}

	evt := DiagDrainEvent{
		ID: "event-abc",
		DiagEvent: opsmetrics.DiagEvent{
			Level:     "error",
			EventType: "crash",
			Source:    "nexus-agent",
			Message:   "null pointer",
		},
	}
	c, rec := makeDiagCtx(t, DiagDrainRequest{Events: []DiagDrainEvent{evt}})
	if err := h.UploadDiagEvents(c); err != nil {
		t.Fatalf("UploadDiagEvents: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status %d; want 200", rec.Code)
	}

	var resp DiagDrainResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(resp.AcceptedIds) != 1 || resp.AcceptedIds[0] != "event-abc" {
		t.Errorf("acceptedIds = %v; want [event-abc]", resp.AcceptedIds)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unfulfilled expectations: %v", err)
	}
}

// TestDiagDrain_SkipEmptyID verifies that events with empty id are skipped
// (partial-ack semantics) — only events with non-empty id are acked.
func TestDiagDrain_SkipEmptyID(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	// Only the second event (non-empty id) triggers an INSERT.
	expectInsert(mock)

	h := &DiagDrainAPI{Pool: mock}
	events := []DiagDrainEvent{
		{ID: "", DiagEvent: opsmetrics.DiagEvent{Source: "s", Message: "no-id"}},
		{ID: "valid-id", DiagEvent: opsmetrics.DiagEvent{Source: "s", Message: "has-id"}},
	}
	c, rec := makeDiagCtx(t, DiagDrainRequest{Events: events})
	if err := h.UploadDiagEvents(c); err != nil {
		t.Fatalf("UploadDiagEvents: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status %d; want 200", rec.Code)
	}

	var resp DiagDrainResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.AcceptedIds) != 1 || resp.AcceptedIds[0] != "valid-id" {
		t.Errorf("acceptedIds = %v; want [valid-id] (empty-id event skipped)", resp.AcceptedIds)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unfulfilled DB expectations: %v", err)
	}
}

// TestDiagDrain_DBError_SkipsEvent verifies that a DB insert error causes
// the event to be omitted from acceptedIds (retry-next-startup semantics).
func TestDiagDrain_DBError_SkipsEvent(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	expectInsertError(mock, errDiagDB)

	h := &DiagDrainAPI{Pool: mock}
	events := []DiagDrainEvent{
		{ID: "fail-id", DiagEvent: opsmetrics.DiagEvent{Source: "s", Message: "fail"}},
	}
	c, rec := makeDiagCtx(t, DiagDrainRequest{Events: events})
	if err := h.UploadDiagEvents(c); err != nil {
		t.Fatalf("UploadDiagEvents: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status %d; want 200 even on insert error (partial-ack)", rec.Code)
	}

	var resp DiagDrainResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.AcceptedIds) != 0 {
		t.Errorf("acceptedIds = %v; want [] on insert error", resp.AcceptedIds)
	}
}

// TestDiagDrain_ThingFromContext verifies that when a Thing is set in the
// context (e.g. via DeviceOrServiceAuth middleware), its ID and Type are used.
func TestDiagDrain_ThingFromContext(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	expectInsert(mock)

	h := &DiagDrainAPI{Pool: mock}

	b, _ := json.Marshal(DiagDrainRequest{Events: []DiagDrainEvent{
		{ID: "ctx-evt", DiagEvent: opsmetrics.DiagEvent{Source: "agent"}},
	}})
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(thingContextKey, &store.Thing{ID: "thing-xyz", Type: "agent"})

	if err := h.UploadDiagEvents(c); err != nil {
		t.Fatalf("UploadDiagEvents: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status %d; want 200", rec.Code)
	}
}

// TestDiagDrain_XThingIdFallback verifies that the X-Thing-Id header is used
// when no Thing is set in the context.
func TestDiagDrain_XThingIdFallback(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	expectInsert(mock)

	h := &DiagDrainAPI{Pool: mock}

	b, _ := json.Marshal(DiagDrainRequest{Events: []DiagDrainEvent{
		{ID: "hdr-evt", DiagEvent: opsmetrics.DiagEvent{Source: "agent"}},
	}})
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Thing-Id", "hdr-thing-id")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := h.UploadDiagEvents(c); err != nil {
		t.Fatalf("UploadDiagEvents: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status %d; want 200", rec.Code)
	}
}

// TestDiagDrain_MessageHashComputed verifies that insertDiagDrainEvent
// computes and stores a message hash when the event's MessageHash is empty.
func TestDiagDrain_MessageHashComputed(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	expectInsert(mock)

	// Call insertDiagDrainEvent directly with empty MessageHash.
	evt := DiagDrainEvent{
		ID: "hash-test",
		DiagEvent: opsmetrics.DiagEvent{
			Source:      "nexus-agent",
			Message:     "test message",
			MessageHash: "", // triggers ComputeMessageHash
		},
	}
	err = insertDiagDrainEvent(t.Context(), mock, "thing-1", "agent", evt)
	if err != nil {
		t.Fatalf("insertDiagDrainEvent: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB expectations not met: %v", err)
	}
}

// TestDiagDrain_WithAttrsAndOSInfo verifies that non-nil Attrs and OSInfo
// are serialised to JSON and passed to the INSERT query.
func TestDiagDrain_WithAttrsAndOSInfo(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	expectInsert(mock)

	evt := DiagDrainEvent{
		ID: "attrs-test",
		DiagEvent: opsmetrics.DiagEvent{
			Source:  "nexus-agent",
			Message: "crash",
			Attrs:   map[string]any{"key": "val"},
			OSInfo:  map[string]any{"os": "macOS"},
		},
	}
	if err := insertDiagDrainEvent(t.Context(), mock, "thing-1", "agent", evt); err != nil {
		t.Fatalf("insertDiagDrainEvent: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB expectations not met: %v", err)
	}
}

// TestDiagDrain_WithStackTraceAndAgentVersion verifies the optional pointer
// fields (StackTrace, AgentVersion) are correctly handled.
func TestDiagDrain_WithStackTraceAndAgentVersion(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	expectInsert(mock)

	evt := DiagDrainEvent{
		ID: "stack-test",
		DiagEvent: opsmetrics.DiagEvent{
			Source:       "nexus-agent",
			Message:      "crash",
			StackTrace:   "goroutine 1 [running]: ...",
			AgentVersion: "1.2.3",
		},
	}
	if err := insertDiagDrainEvent(t.Context(), mock, "thing-1", "agent", evt); err != nil {
		t.Fatalf("insertDiagDrainEvent: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB expectations not met: %v", err)
	}
}

// TestDiagDrain_WithTraceID verifies that a non-empty TraceID on the
// drain-event payload reaches the INSERT as a non-nil bound argument at
// position 10 (right after message_hash, before attrs). The Hub-side
// "" → NULL pointer indirection lives in insertDiagDrainEvent — this
// test pins that a populated value survives the indirection rather than
// being dropped to NULL.
func TestDiagDrain_WithTraceID(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	// Build the expectation by hand so we can pin the trace_id arg by
	// position. The other 14 args remain AnyArg.
	args := make([]any, diagInsertArgCount)
	for i := range args {
		args[i] = pgxmock.AnyArg()
	}
	// Positions (1-indexed in the SQL VALUES, 0-indexed in the args slice):
	//   1: id,  2: thing_id,  3: thing_type,  4: occurred_at,  5: level,
	//   6: event_type,  7: source,  8: message,  9: message_hash,
	//  10: trace_id,  11: attrs,  ...
	// Match a *string pointing at the expected value (NULL-when-empty
	// contract uses *string, never the raw string).
	expectedTrace := "trace-drain-abc"
	args[9] = &expectedTrace
	mock.ExpectExec(`INSERT INTO thing_diag_event`).
		WithArgs(args...).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	evt := DiagDrainEvent{
		ID: "trace-test",
		DiagEvent: opsmetrics.DiagEvent{
			Source:  "nexus-agent",
			Message: "with trace",
			TraceID: expectedTrace,
		},
	}
	if err := insertDiagDrainEvent(t.Context(), mock, "thing-1", "agent", evt); err != nil {
		t.Fatalf("insertDiagDrainEvent: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB expectations not met: %v", err)
	}
}

// TestDiagDrain_EmptyTraceIDIsNull asserts the inverse: an empty TraceID
// hits the INSERT as a NULL bound arg (Go nil *string) instead of an empty
// string. Without this guard, admin queries that filter `WHERE trace_id
// IS NULL` would silently miss legacy rows.
func TestDiagDrain_EmptyTraceIDIsNull(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	args := make([]any, diagInsertArgCount)
	for i := range args {
		args[i] = pgxmock.AnyArg()
	}
	// pgxmock's typed-nil match: declare the *string nil expectation by
	// supplying an explicit (*string)(nil). Using untyped nil would match
	// any nil-able type and weaken the assertion.
	args[9] = (*string)(nil)
	mock.ExpectExec(`INSERT INTO thing_diag_event`).
		WithArgs(args...).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	evt := DiagDrainEvent{
		ID: "no-trace",
		DiagEvent: opsmetrics.DiagEvent{
			Source:  "nexus-agent",
			Message: "boot fault",
			// TraceID left empty intentionally.
		},
	}
	if err := insertDiagDrainEvent(t.Context(), mock, "thing-1", "agent", evt); err != nil {
		t.Fatalf("insertDiagDrainEvent: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB expectations not met: %v", err)
	}
}

// TestDiagDrain_ZeroOccurredAt verifies that a zero OccurredAt is replaced
// by time.Now (the sentinel for "not provided by agent").
func TestDiagDrain_ZeroOccurredAt(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	expectInsert(mock)

	evt := DiagDrainEvent{
		ID: "zero-time",
		DiagEvent: opsmetrics.DiagEvent{
			Source:     "nexus-agent",
			Message:    "msg",
			OccurredAt: time.Time{}, // zero → should be set to Now
		},
	}
	if err := insertDiagDrainEvent(t.Context(), mock, "thing-1", "agent", evt); err != nil {
		t.Fatalf("insertDiagDrainEvent: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB expectations not met: %v", err)
	}
}

func TestDiagHelpers_BadRequest(t *testing.T) {
	e := echo.New()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), httptest.NewRecorder())
	_ = badRequest(c, "bad")
}

func TestDiagHelpers_NotFound(t *testing.T) {
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
	_ = notFound(c, "nf")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status %d; want 404", rec.Code)
	}
}

func TestDiagHelpers_InternalError(t *testing.T) {
	e := echo.New()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), httptest.NewRecorder())
	_ = internalError(c, "err")
}

func TestDiagHelpers_ServiceUnavailable(t *testing.T) {
	e := echo.New()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), httptest.NewRecorder())
	_ = serviceUnavailable(c, "svc")
}

func TestDiagHelpers_Unauthorized(t *testing.T) {
	e := echo.New()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), httptest.NewRecorder())
	_ = unauthorized(c, "unauth")
}

func TestDiagHelpers_Forbidden(t *testing.T) {
	e := echo.New()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), httptest.NewRecorder())
	_ = forbidden(c, "forbidden")
}

func TestDiagHelpers_HandleErr_ErrNotFound(t *testing.T) {
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
	_ = handleErr(c, store.ErrNotFound)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status %d; want 404", rec.Code)
	}
}

func TestDiagHelpers_HandleErr_OtherErr(t *testing.T) {
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
	_ = handleErr(c, errDiagDB)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status %d; want 500", rec.Code)
	}
}

func TestDiagHelpers_ParseIntDefault(t *testing.T) {
	if parseIntDefault("", 10) != 10 {
		t.Error("empty → default")
	}
	if parseIntDefault("5", 10) != 5 {
		t.Error("valid → 5")
	}
	if parseIntDefault("0", 10) != 10 {
		t.Error("zero → default")
	}
}

func TestDiagHelpers_Clamp(t *testing.T) {
	if clamp(5, 1, 10) != 5 {
		t.Error("in range")
	}
	if clamp(0, 1, 10) != 1 {
		t.Error("below min")
	}
	if clamp(11, 1, 10) != 10 {
		t.Error("above max")
	}
}

func TestDiagHelpers_ParseTimeOrNil(t *testing.T) {
	if parseTimeOrNil("") != nil {
		t.Error("empty → nil")
	}
	if parseTimeOrNil("bad") != nil {
		t.Error("bad → nil")
	}
	if parseTimeOrNil("2026-01-15T00:00:00Z") == nil {
		t.Error("valid → non-nil")
	}
}

func TestThingFromContext_DiagNil(t *testing.T) {
	e := echo.New()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), httptest.NewRecorder())
	if ThingFromContext(c) != nil {
		t.Error("must be nil for empty context")
	}
}

func TestThingFromContext_DiagSet(t *testing.T) {
	e := echo.New()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), httptest.NewRecorder())
	c.Set(thingContextKey, &store.Thing{ID: "t1", Type: "agent"})
	th := ThingFromContext(c)
	if th == nil || th.ID != "t1" {
		t.Errorf("unexpected thing: %v", th)
	}
}

func TestThingFromContext_DiagWrongType(t *testing.T) {
	e := echo.New()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), httptest.NewRecorder())
	c.Set(thingContextKey, "wrong")
	if ThingFromContext(c) != nil {
		t.Error("wrong type must return nil")
	}
}

// errDiagDB is a sentinel error used in DB-error tests.
var errDiagDB = errDiagType("db error")

type errDiagType string

func (e errDiagType) Error() string { return string(e) }

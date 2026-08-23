package queue

import (
	"context"
	"testing"
	"time"

	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

// external_request_id round-trip, asserted ALONGSIDE trace_id.
//
// The two are adjacent columns carrying similar-looking values, and every
// INSERT and SELECT in this file binds by POSITION. A column inserted at the
// wrong offset swaps them silently: both round-trip, both are non-empty, and
// the only symptom is that a caller's id shows up as the trace and vice versa
// — in the Hub, weeks later. So the values are deliberately distinguishable
// and each is asserted into its own field.
//
// The column exists because the agent shares tlsbump with the compliance
// proxy, which reads the caller's x-request-id off the intercepted request.
// Without somewhere to put it, the value was captured and then dropped between
// tlsbump and upload on the agent path alone.
func TestRecord_RoundtripExternalRequestID(t *testing.T) {
	q := newTestQueue(t)

	e := makeEvent("erid-1")
	e.TraceID = "trace-from-nexus"
	e.ExternalRequestID = "caller-own-id"
	if err := q.Record(e); err != nil {
		t.Fatalf("Record: %v", err)
	}

	got, err := q.DrainBatch(10)
	if err != nil {
		t.Fatalf("DrainBatch: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("DrainBatch len=%d, want 1", len(got))
	}
	if got[0].ExternalRequestID != "caller-own-id" {
		t.Errorf("DrainBatch ExternalRequestID = %q, want caller-own-id", got[0].ExternalRequestID)
	}
	if got[0].TraceID != "trace-from-nexus" {
		t.Errorf("DrainBatch TraceID = %q, want trace-from-nexus — the two columns are bound by position and a misplaced one swaps them", got[0].TraceID)
	}
}

// A caller that sent no id leaves the column empty rather than borrowing the
// trace, which would silently claim they had supplied one.
func TestRecord_NoExternalRequestIDStaysEmpty(t *testing.T) {
	q := newTestQueue(t)

	e := makeEvent("erid-2")
	e.TraceID = "trace-only"
	if err := q.Record(e); err != nil {
		t.Fatalf("Record: %v", err)
	}
	got, err := q.DrainBatch(10)
	if err != nil {
		t.Fatalf("DrainBatch: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("DrainBatch len=%d, want 1", len(got))
	}
	if got[0].ExternalRequestID != "" {
		t.Errorf("ExternalRequestID = %q, want empty", got[0].ExternalRequestID)
	}
	if got[0].TraceID != "trace-only" {
		t.Errorf("TraceID = %q, want trace-only", got[0].TraceID)
	}
}

// The query path reads a different column list from the drain path, so it can
// drift independently. Both are asserted.
func TestQueryEvents_CarriesExternalRequestID(t *testing.T) {
	q := newTestQueue(t)

	e := makeEvent("erid-3")
	e.TraceID = "trace-q"
	e.ExternalRequestID = "caller-q"
	if err := q.Record(e); err != nil {
		t.Fatalf("Record: %v", err)
	}

	rows, _, err := q.QueryEvents("", "", 0, 10)
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("QueryEvents len=%d, want 1", len(rows))
	}
	if rows[0].ExternalRequestID != "caller-q" {
		t.Errorf("QueryEvents ExternalRequestID = %q, want caller-q", rows[0].ExternalRequestID)
	}
	if rows[0].TraceID != "trace-q" {
		t.Errorf("QueryEvents TraceID = %q, want trace-q", rows[0].TraceID)
	}
}

// The PRODUCTION path, which the cases above do not touch.
//
// Captured traffic reaches SQLite through QueueWriter.Enqueue → buildRow →
// flush → RecordBatch. Queue.Record is only called for non-bumped flows, which
// have no HTTP layer and so never carry an x-request-id at all. So a test that
// exercises Record alone guards the one path that can never hold the value:
// dropping the field from buildRow, or from the RecordBatch binding, leaves
// this package green. Both mutants were confirmed to survive before this case
// existed.
func TestQueueWriter_CarriesExternalRequestIDThroughTheFlushPath(t *testing.T) {
	q := newTestQueue(t)
	w := NewQueueWriter(q)

	w.Enqueue(sharedaudit.AuditEvent{
		ID:                "wq-1",
		Timestamp:         time.Now().UTC(),
		TargetHost:        "api.openai.com",
		TraceID:           "trace-through-writer",
		ExternalRequestID: "caller-through-writer",
	})
	if err := w.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	got, err := q.DrainBatch(10)
	if err != nil {
		t.Fatalf("DrainBatch: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("DrainBatch len=%d, want 1", len(got))
	}
	if got[0].ExternalRequestID != "caller-through-writer" {
		t.Errorf("ExternalRequestID = %q, want caller-through-writer — the flush path is how real captured traffic gets here", got[0].ExternalRequestID)
	}
	if got[0].TraceID != "trace-through-writer" {
		t.Errorf("TraceID = %q, want trace-through-writer", got[0].TraceID)
	}
}

// EventByID reads its own column list, and a mis-ORDERED list there produces no
// error at all — database/sql only complains when the COUNT disagrees. The
// existing full-detail test asserts neither id, so a swap was invisible.
func TestEventByID_KeepsTheTwoIdsApart(t *testing.T) {
	q := newTestQueue(t)

	e := makeEvent("byid-1")
	e.TraceID = "trace-byid"
	e.ExternalRequestID = "caller-byid"
	if err := q.Record(e); err != nil {
		t.Fatalf("Record: %v", err)
	}

	got, err := q.EventByID("byid-1")
	if err != nil {
		t.Fatalf("EventByID: %v", err)
	}
	if got.ExternalRequestID != "caller-byid" {
		t.Errorf("ExternalRequestID = %q, want caller-byid", got.ExternalRequestID)
	}
	if got.TraceID != "trace-byid" {
		t.Errorf("TraceID = %q, want trace-byid — a swapped column list here raises no error, only wrong values", got.TraceID)
	}
}

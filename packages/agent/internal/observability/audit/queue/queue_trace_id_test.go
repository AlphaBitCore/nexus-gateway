package queue

import (
	"encoding/json"
	"testing"
	"time"
)

// #70 — trace_id column round-trip: Record → DrainBatch + QueryEvents
// both must read the value back exactly as written. Pre-#70 the column
// did not exist in the schema; Row.TraceID was scanned but the INSERT
// never carried it, so every audit row uploaded to Hub with traceId=""
// and cp-ui Detail showed an empty cross-service correlation id.

func TestRecord_RoundtripTraceID_NonEmpty(t *testing.T) {
	q := newTestQueue(t)
	want := "tx-1234-abcd"
	e := makeEvent("rt1")
	e.TraceID = want
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
	if got[0].TraceID != want {
		t.Errorf("DrainBatch traceId=%q, want %q", got[0].TraceID, want)
	}
	page, total, err := q.QueryEvents("", "", 0, 10)
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if total < 1 || len(page) < 1 {
		t.Fatalf("QueryEvents total=%d len=%d", total, len(page))
	}
	if page[0].TraceID != want {
		t.Errorf("QueryEvents traceId=%q, want %q", page[0].TraceID, want)
	}
}

func TestRecord_RoundtripAdditionalFields(t *testing.T) {
	// Covers the if-Valid populate branches in DrainBatch + QueryEvents
	// for NormalizedRequest / NormalizedResponse / StatusCode that ship
	// alongside trace_id in #69/#70. Pre-test these branches existed
	// but had no coverage hit; failing to read them back here would
	// surface as silent NULL on agent UI immediately.
	q := newTestQueue(t)
	e := makeEvent("rt3")
	e.TraceID = "tx-rt3"
	e.StatusCode = 200
	e.NormalizedRequest = json.RawMessage(`{"kind":"ai-chat","model":"gpt-4o"}`)
	e.NormalizedResponse = json.RawMessage(`{"kind":"ai-chat","stream":true}`)
	if err := q.Record(e); err != nil {
		t.Fatalf("Record: %v", err)
	}
	got, err := q.DrainBatch(10)
	if err != nil {
		t.Fatalf("DrainBatch: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("DrainBatch len=%d", len(got))
	}
	if got[0].StatusCode != 200 {
		t.Errorf("statusCode=%d, want 200", got[0].StatusCode)
	}
	if string(got[0].NormalizedRequest) != `{"kind":"ai-chat","model":"gpt-4o"}` {
		t.Errorf("normalizedRequest=%s", got[0].NormalizedRequest)
	}
	if string(got[0].NormalizedResponse) != `{"kind":"ai-chat","stream":true}` {
		t.Errorf("normalizedResponse=%s", got[0].NormalizedResponse)
	}
	// also exercises QueryEvents read path for these columns
	page, _, err := q.QueryEvents("", "", 0, 10)
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(page) < 1 || page[0].TraceID != "tx-rt3" || page[0].StatusCode != 200 {
		t.Errorf("QueryEvents row mismatch: %+v", page[0])
	}
}

func TestRecord_RoundtripTraceID_EmptyStaysEmpty(t *testing.T) {
	// Empty TraceID must store as SQL NULL → read back as "" so
	// downstream wire serialization can omit the field (json
	// "traceId,omitempty") instead of shipping an empty string.
	q := newTestQueue(t)
	e := makeEvent("rt2")
	e.TraceID = ""
	e.Timestamp = time.Now()
	if err := q.Record(e); err != nil {
		t.Fatalf("Record: %v", err)
	}
	got, err := q.DrainBatch(10)
	if err != nil {
		t.Fatalf("DrainBatch: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len=%d, want 1", len(got))
	}
	if got[0].TraceID != "" {
		t.Errorf("DrainBatch traceId=%q, want empty", got[0].TraceID)
	}
}

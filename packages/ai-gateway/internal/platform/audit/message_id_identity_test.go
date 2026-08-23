package audit

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/decision"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
)

// The traffic_event primary key is the NATS-redelivery idempotency key: the
// consumer inserts with ON CONFLICT (id) DO NOTHING so a redelivered message
// does not double-insert and double-bill. That contract only holds while the
// key identifies ONE event. A caller-supplied correlation value does not: an
// X-Nexus-Request-Id reused across two genuinely distinct calls — routine in
// client retry code — makes the second event look like a redelivery of the
// first, and it is dropped with no error to anyone.
//
// So: the id is generated per emitted record, and the caller's correlation
// value rides on TraceID, which is allowed to repeat by design.
func TestRecordToMessageIDIsPerRecordNotPerCorrelationValue(t *testing.T) {
	w := &Writer{}

	const shared = "caller-supplied-correlation-id"
	first := w.recordToMessage(&Record{RequestID: shared, TraceID: shared, Path: "/v1/chat/completions"})
	second := w.recordToMessage(&Record{RequestID: shared, TraceID: shared, Path: "/v1/chat/completions"})

	if first.ID == "" || second.ID == "" {
		t.Fatalf("both events need an id, got %q and %q", first.ID, second.ID)
	}
	if first.ID == second.ID {
		t.Errorf("two distinct events share id %q — the second is silently dropped by ON CONFLICT (id) DO NOTHING", first.ID)
	}
	if first.ID == shared || second.ID == shared {
		t.Errorf("id is the caller-supplied correlation value %q; the caller must not control the primary key", shared)
	}
	if first.TraceID != shared || second.TraceID != shared {
		t.Errorf("TraceID = %q / %q, want both %q — correlation must survive", first.TraceID, second.TraceID, shared)
	}
}

// marshalRecordPlain rebuilds the message from the same record when a marker
// collision knocks the splice path out, so recordToMessage runs twice for one
// event. Both encodings must carry the same id: a second id would sail past
// the consumer's ON CONFLICT (id) DO NOTHING and insert a duplicate row —
// double-counting one call's tokens and cost.
func TestRecordToMessageIDIsStableAcrossReconversion(t *testing.T) {
	w := &Writer{}
	rec := &Record{RequestID: "req-1", TraceID: "req-1", Path: "/v1/chat/completions"}

	first := w.recordToMessage(rec)
	second := w.recordToMessage(rec)

	if first.ID != second.ID {
		t.Errorf("re-converting one record produced ids %q then %q; the marker-collision fallback would insert a duplicate row", first.ID, second.ID)
	}
}

// The payload blob's storage key is "<date>/<eventID>-<direction>.bin", and
// PutOptions.EventID is documented as the traffic_event id. Keying it on the
// caller-supplied correlation value instead reproduces the collision this
// change removes, one layer down and worse: two calls sharing one
// X-Nexus-Request-Id on the same UTC day now correctly get two rows, but both
// bodies write to ONE key, so the second Put overwrites the first and the
// first row's drawer renders the SECOND call's prompt and response. Silent
// cross-request body disclosure, where the row-level bug was only a lost row.
func TestRecordToMessageSpillKeyIsTheEventIDNotTheCallerValue(t *testing.T) {
	store := &stubSpillStore{}
	w := NewWriter(nil, "q", nil, slog.Default()).
		WithSpillStore(store).
		WithPayloadCaptureStore(payloadcapture.NewStore(payloadcapture.Config{MaxInlineBodyBytes: 4}))

	const callerValue = "caller-supplied-correlation-id"
	big := []byte(`{"body":"long enough to exceed the inline threshold"}`)
	msg := w.recordToMessage(&Record{
		RequestID:     callerValue,
		TraceID:       callerValue,
		Timestamp:     time.Now(),
		RequestBody:   big,
		RequestAction: decision.ActionApprove,
	})

	if store.putKey == "" {
		t.Fatal("body did not take the spill path; the test cannot observe the key")
	}
	if strings.HasPrefix(store.putKey, callerValue+"/") {
		t.Errorf("spill key %q is the caller's correlation value; two calls sharing it would overwrite each other's bodies", store.putKey)
	}
	if !strings.HasPrefix(store.putKey, msg.ID+"/") {
		t.Errorf("spill key = %q, want it keyed on the event id %q", store.putKey, msg.ID)
	}
}

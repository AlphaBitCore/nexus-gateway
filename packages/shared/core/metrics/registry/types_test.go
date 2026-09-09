package registry

import (
	"github.com/goccy/go-json"
	"strings"
	"testing"
	"time"
)

func TestSampleMarshalIncludesAllFields(t *testing.T) {
	s := Sample{
		Name:         "runtime.heap_alloc_bytes",
		Kind:         KindGauge,
		DimensionKey: "",
		Value:        1234.5,
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"name":"runtime.heap_alloc_bytes","kind":"gauge","dim":"","value":1234.5}`
	if string(b) != want {
		t.Fatalf("got %s, want %s", b, want)
	}
}

func TestHistogramSampleSerializesBuckets(t *testing.T) {
	s := Sample{
		Name:         "hook.pipeline_ms",
		Kind:         KindHistogram,
		DimensionKey: "stage=request",
		Metadata:     map[string]any{"buckets": []int{10, 5, 2, 1, 0, 0}},
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"buckets":[10,5,2,1,0,0]`) {
		t.Fatalf("missing buckets field, got %s", b)
	}
}

// TestDiagEventExternalRequestIDRoundTripJSON asserts the on-wire envelope
// keeps a populated ExternalRequestID across a marshal → unmarshal cycle under
// the `externalRequestId` json tag. The thingclient WS path embeds DiagEvent inside
// diagEventEnvelope, so any tag drift here would silently move the value
// from the typed field into Attrs (or drop it on the floor) when the
// Hub-side receiver decodes.
func TestDiagEventExternalRequestIDRoundTripJSON(t *testing.T) {
	occurred := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	original := DiagEvent{
		ThingID:           "thing-abc",
		OccurredAt:        occurred,
		Level:             LevelError,
		EventType:         EventTypeError,
		Source:            "ai-gateway",
		Message:           "upstream timeout",
		MessageHash:       "deadbeef",
		ExternalRequestID: "req-2026-01-02-xyz",
		Attrs:             map[string]any{"upstream": "api.openai.com"},
		RepeatCount:       1,
	}

	b, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"externalRequestId":"req-2026-01-02-xyz"`) {
		t.Fatalf("missing externalRequestId field in wire payload, got %s", b)
	}

	var roundTrip DiagEvent
	if err := json.Unmarshal(b, &roundTrip); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if roundTrip.ExternalRequestID != original.ExternalRequestID {
		t.Errorf("ExternalRequestID round-trip = %q, want %q", roundTrip.ExternalRequestID, original.ExternalRequestID)
	}
}

// TestDiagEventExternalRequestIDOmitemptyWhenBlank asserts that a DiagEvent
// emitted off any request scope does NOT pollute the wire payload with an empty
// externalRequestId key — the omitempty tag keeps it out. This matters because
// thingclient serializes thousands of these per minute on a busy fleet
// and a stray empty key per row is just noise.
func TestDiagEventExternalRequestIDOmitemptyWhenBlank(t *testing.T) {
	evt := DiagEvent{
		ThingID:     "thing-1",
		Level:       LevelError,
		EventType:   EventTypeError,
		Source:      "boot",
		Message:     "init fault",
		MessageHash: "cafebabe",
		RepeatCount: 1,
	}
	b, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), `"externalRequestId"`) {
		t.Fatalf("empty externalRequestId leaked into wire payload, got %s", b)
	}
}

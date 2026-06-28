package audit

import (
	"bytes"
	"github.com/goccy/go-json"
	"log/slog"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/decision"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// TestMarshalRecord_RoundTrips pins the audit wire contract that actually
// matters: the envelope is NOT required to be byte-identical across encoders
// (writer_batch.go documents that the Hub Unmarshals it), but the marshaled
// bytes MUST Unmarshal back into a TrafficEventMessage that preserves the
// load-bearing data — especially the request/response body bytes (with their
// original interior whitespace, which is legal JSON and must survive) and the
// scalar identity fields. This is the safety net for swapping the marshal
// encoder (stdlib -> goccy): the swap may change envelope bytes, but must never
// change the decoded data.
func TestMarshalRecord_RoundTrips(t *testing.T) {
	w := NewWriter(nil, "q", nil, slog.Default())

	// Bodies carry interior whitespace, as captured from the wire. A faithful
	// round-trip must preserve the decoded structure (not necessarily the exact
	// inner bytes, since RawMessage handling compacts — but the data must match).
	reqBody := []byte(`{ "model" : "mock-gpt-4o-mini" , "messages" : [ { "role" : "user" , "content" : "hi there" } ] }`)
	respBody := []byte(`{ "id" : "chatcmpl-x" , "choices" : [ { "index" : 0 } ] }`)

	rec := &Record{
		RequestID:      "req-roundtrip-1",
		Timestamp:      time.Unix(1700000123, 0).UTC(),
		RequestBody:    reqBody,
		ResponseBody:   respBody,
		RequestAction:  decision.ActionApprove,
		ResponseAction: decision.ActionApprove,
		ModelName:      "mock-gpt-4o-mini",
		Path:           "/v1/chat/completions",
		StatusCode:     200,
	}

	data, _, ok := w.marshalRecord(rec)
	if !ok {
		t.Fatal("marshalRecord returned !ok")
	}

	var msg mq.TrafficEventMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("marshaled bytes do not Unmarshal into TrafficEventMessage: %v\nbytes=%s", err, data)
	}

	// Scalar identity fields survive.
	if msg.ID != "req-roundtrip-1" {
		t.Errorf("ID = %q, want req-roundtrip-1", msg.ID)
	}
	if msg.ModelName != "mock-gpt-4o-mini" {
		t.Errorf("ModelName = %q, want mock-gpt-4o-mini", msg.ModelName)
	}
	if msg.Path != "/v1/chat/completions" {
		t.Errorf("Path = %q, want /v1/chat/completions", msg.Path)
	}

	// Body bytes survive as semantically-equal JSON (compare canonicalized).
	assertJSONEqual(t, "requestBody.inlineBytes", reqBody, msg.RequestBody.InlineBytes)
	assertJSONEqual(t, "responseBody.inlineBytes", respBody, msg.ResponseBody.InlineBytes)
}

// assertJSONEqual compares two JSON byte slices for semantic equality
// (whitespace-insensitive) by compacting both through the standard library.
func assertJSONEqual(t *testing.T, label string, want, got []byte) {
	t.Helper()
	if len(got) == 0 {
		t.Errorf("%s: round-tripped body is empty (want %s)", label, want)
		return
	}
	var wb, gb bytes.Buffer
	if err := json.Compact(&wb, want); err != nil {
		t.Fatalf("%s: want not valid JSON: %v", label, err)
	}
	if err := json.Compact(&gb, got); err != nil {
		t.Fatalf("%s: got not valid JSON: %v", label, err)
	}
	if !bytes.Equal(wb.Bytes(), gb.Bytes()) {
		t.Errorf("%s: body changed across marshal round-trip\n want=%s\n got =%s", label, wb.Bytes(), gb.Bytes())
	}
}

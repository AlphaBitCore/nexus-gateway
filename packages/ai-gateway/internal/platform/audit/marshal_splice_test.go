package audit

import (
	"bytes"
	"github.com/goccy/go-json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/decision"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// TestMarshalRecord_SpliceLargeInlineRawRoundTrips exercises the body-splice path:
// large (>spliceMinBodyBytes) inline-raw request/response bodies are detached to
// markers, encoded, and spliced back as verbatim bytes. The marshaled record must
// decode to a TrafficEventMessage whose body bytes exactly match the originals.
func TestMarshalRecord_SpliceLargeInlineRawRoundTrips(t *testing.T) {
	w := NewWriter(nil, "q", nil, slog.Default())
	bigReq := []byte(`{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("a", 4096) + `"}]}`)
	bigResp := []byte(`{"id":"x","choices":[{"index":0,"message":{"content":"` + strings.Repeat("b", 4096) + `"}}]}`)
	rec := &Record{
		RequestID: "splice-1", Timestamp: time.Unix(1700000000, 0).UTC(),
		RequestBody: bigReq, ResponseBody: bigResp,
		RequestAction: decision.ActionApprove, ResponseAction: decision.ActionApprove,
		RequestContentType: "application/json", ResponseContentType: "application/json",
		ModelName: "m", StatusCode: 200, Path: "/v1/x",
	}
	data, _, ok := w.marshalRecord(rec)
	if !ok {
		t.Fatal("marshalRecord !ok")
	}
	if bytes.IndexByte(data, '\n') != -1 {
		t.Fatalf("spliced output has a raw newline (breaks NDJSON framing)")
	}
	var msg mq.TrafficEventMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("spliced output does not decode: %v", err)
	}
	if !bytes.Equal(msg.RequestBody.InlineBytes, bigReq) {
		t.Fatalf("request body corrupted by splice:\n got=%s", msg.RequestBody.InlineBytes)
	}
	if !bytes.Equal(msg.ResponseBody.InlineBytes, bigResp) {
		t.Fatalf("response body corrupted by splice")
	}
}

// TestMarshalRecord_SpliceLargeTextResponseRoundTrips exercises the splice on the
// dominant streaming case: a large text/event-stream (SSE) response body, which
// NewInlineBody classifies as encoding=text (valid UTF-8, not JSON). The body is
// detached to a marker, encoded, and spliced back as an escaped JSON string. It
// must decode to the exact original bytes — proving the text splice is lossless on
// the hot SSE audit path.
func TestMarshalRecord_SpliceLargeTextResponseRoundTrips(t *testing.T) {
	w := NewWriter(nil, "q", nil, slog.Default())
	bigReq := []byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":"` + strings.Repeat("a", 2048) + `"}]}`)
	// A realistic SSE stream: not valid JSON, contains HTML chars + control bytes
	// that exercise the escaper, well over the splice threshold.
	var sse strings.Builder
	for range 80 {
		sse.WriteString("data: {\"choices\":[{\"delta\":{\"content\":\"<chunk> & more\"}}]}\n\n")
	}
	sse.WriteString("data: [DONE]\n\n")
	bigResp := []byte(sse.String())
	rec := &Record{
		RequestID: "splice-text", Timestamp: time.Unix(1700000000, 0).UTC(),
		RequestBody: bigReq, ResponseBody: bigResp,
		RequestAction: decision.ActionApprove, ResponseAction: decision.ActionApprove,
		RequestContentType: "application/json", ResponseContentType: "text/event-stream",
		ModelName: "m", StatusCode: 200, Path: "/v1/chat/completions",
	}
	data, _, ok := w.marshalRecord(rec)
	if !ok {
		t.Fatal("marshalRecord !ok")
	}
	if bytes.IndexByte(data, '\n') != -1 {
		t.Fatalf("spliced output has a raw newline (breaks NDJSON framing)")
	}
	var msg mq.TrafficEventMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("spliced output does not decode: %v", err)
	}
	if msg.ResponseBody.Encoding != "text" {
		t.Fatalf("expected response encoding=text, got %q", msg.ResponseBody.Encoding)
	}
	if !bytes.Equal(msg.ResponseBody.InlineBytes, bigResp) {
		t.Fatalf("SSE text response corrupted by splice")
	}
	if !bytes.Equal(msg.RequestBody.InlineBytes, bigReq) {
		t.Fatalf("request body corrupted by splice")
	}
}

// TestMarshalRecord_SpliceLargeBase64BodyRoundTrips exercises the splice on a
// binary (NUL-bearing) body, which NewInlineBody classifies as encoding=base64.
// The detached body is spliced back as a quoted base64 string and must decode to
// the exact original bytes.
func TestMarshalRecord_SpliceLargeBase64BodyRoundTrips(t *testing.T) {
	w := NewWriter(nil, "q", nil, slog.Default())
	bin := make([]byte, 4096)
	for i := range bin {
		bin[i] = byte(i % 256) // includes NUL -> base64 classification
	}
	rec := &Record{
		RequestID: "splice-b64", Timestamp: time.Unix(1700000000, 0).UTC(),
		ResponseBody: bin, ResponseAction: decision.ActionApprove, ResponseContentType: "application/octet-stream",
		ModelName: "m", StatusCode: 200, Path: "/v1/x",
	}
	data, _, ok := w.marshalRecord(rec)
	if !ok {
		t.Fatal("marshalRecord !ok")
	}
	if bytes.IndexByte(data, '\n') != -1 {
		t.Fatalf("spliced output has a raw newline")
	}
	var msg mq.TrafficEventMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("spliced output does not decode: %v", err)
	}
	if msg.ResponseBody.Encoding != "base64" {
		t.Fatalf("expected response encoding=base64, got %q", msg.ResponseBody.Encoding)
	}
	if !bytes.Equal(msg.ResponseBody.InlineBytes, bin) {
		t.Fatalf("binary body corrupted by base64 splice")
	}
}

// TestMarshalRecord_SpliceTextMatchesPlain: a large text body must produce a wire
// record byte-identical to the un-spliced (plain) encode — splice is a pure
// allocation optimization, output is unchanged.
func TestMarshalRecord_SpliceTextMatchesPlain(t *testing.T) {
	w := NewWriter(nil, "q", nil, slog.Default())
	body := []byte("event: msg\ndata: " + strings.Repeat("héllo <&> ", 200) + "\n\n")
	rec := &Record{
		RequestID: "txt-cmp", Timestamp: time.Unix(1700000000, 0).UTC(),
		ResponseBody: body, ResponseAction: decision.ActionApprove, ResponseContentType: "text/event-stream",
		ModelName: "m", StatusCode: 200, Path: "/v1/x",
	}
	spliced, _, ok := w.marshalRecord(rec)
	if !ok {
		t.Fatal("splice !ok")
	}
	plain, _, ok := w.marshalRecordPlain(rec)
	if !ok {
		t.Fatal("plain !ok")
	}
	if !bytes.Equal(spliced, plain) {
		t.Fatalf("spliced text wire != plain wire\n spliced=%s\n   plain=%s", spliced, plain)
	}
}

// TestMarshalRecord_SpliceMatchesPlain: the spliced encode must decode IDENTICALLY
// to the un-spliced (plain) encode of the same record — splicing is a pure
// allocation optimization, never a data change.
func TestMarshalRecord_SpliceMatchesPlain(t *testing.T) {
	w := NewWriter(nil, "q", nil, slog.Default())
	big := []byte(`{"k":"` + strings.Repeat("z", 2048) + `","n":42}`)
	rec := &Record{
		RequestID: "cmp", Timestamp: time.Unix(1700000000, 0).UTC(),
		RequestBody: big, RequestAction: decision.ActionApprove, RequestContentType: "application/json",
		ModelName: "m", StatusCode: 200, Path: "/v1/x",
	}
	spliced, _, ok := w.marshalRecord(rec)
	if !ok {
		t.Fatal("splice !ok")
	}
	plain, _, ok := w.marshalRecordPlain(rec)
	if !ok {
		t.Fatal("plain !ok")
	}
	var a, b mq.TrafficEventMessage
	if err := json.Unmarshal(spliced, &a); err != nil {
		t.Fatalf("spliced decode: %v", err)
	}
	if err := json.Unmarshal(plain, &b); err != nil {
		t.Fatalf("plain decode: %v", err)
	}
	if !bytes.Equal(a.RequestBody.InlineBytes, b.RequestBody.InlineBytes) {
		t.Fatalf("spliced vs plain body differ")
	}
	if a.ID != b.ID || a.ModelName != b.ModelName {
		t.Fatalf("spliced vs plain scalar fields differ")
	}
}

// TestMarshalRecord_SpliceCollisionFallsBackToPlain: when the marker byte sequence
// appears in a NON-body field, the occurrence count != 1 and the record must fall
// back to a plain re-encode — still producing correct, decodable bytes.
func TestMarshalRecord_SpliceCollisionFallsBackToPlain(t *testing.T) {
	w := NewWriter(nil, "q", nil, slog.Default())
	big := []byte(`{"k":"` + strings.Repeat("c", 1024) + `"}`)
	// Force a collision: put the marker's inner token into a scalar field.
	collide := "__nexus_body_splice_req_7b3f9e2a__"
	rec := &Record{
		RequestID: "collide-" + collide, Timestamp: time.Unix(1700000000, 0).UTC(),
		RequestBody: big, RequestAction: decision.ActionApprove, RequestContentType: "application/json",
		ModelName: collide, StatusCode: 200, Path: "/v1/x",
	}
	data, _, ok := w.marshalRecord(rec)
	if !ok {
		t.Fatal("marshalRecord !ok on collision")
	}
	var msg mq.TrafficEventMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("collision-fallback output does not decode: %v", err)
	}
	if !bytes.Equal(msg.RequestBody.InlineBytes, big) {
		t.Fatalf("collision fallback corrupted the body")
	}
	if msg.ModelName != collide {
		t.Fatalf("collision fallback corrupted ModelName: %q", msg.ModelName)
	}
}

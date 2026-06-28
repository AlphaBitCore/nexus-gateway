package audit

import (
	"log/slog"
	"testing"
	"time"

	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/decision"
)

// TestRecordToMessage_ReuseFieldsAreInert locks that the write path no longer
// emits request_normalized from the carried Phase-E reuse bytes nor fires the
// reuse metric: normalized is recomputed at view time, so the reuse seam is
// inert even when the proxy populated RequestNormalizedReuse / Protocol / Kind.
func TestRecordToMessage_ReuseFieldsAreInert(t *testing.T) {
	var reuseMetricCalls int
	w := NewWriter(nil, "q", nil, slog.Default()).
		WithNormalizer(func(direction, _, _, _, _ string, _ bool, _ []byte) (json.RawMessage, string, string) {
			t.Fatalf("normalize bridge must never run at write time (direction=%s)", direction)
			return nil, "", ""
		}).
		WithReuseMetric(func(_, _, _ string, _ int) { reuseMetricCalls++ })

	rec := &Record{
		RequestID:                 "reuse",
		Timestamp:                 time.Now(),
		RequestBody:               []byte(`{"model":"gpt-4o"}`),
		RequestAction:             decision.ActionApprove,
		RequestNormalizedReuse:    json.RawMessage(`{"kind":"ai-chat","protocol":"openai"}`),
		RequestNormalizedProtocol: "openai",
		RequestNormalizedKind:     "ai-chat",
	}
	msg := w.recordToMessage(rec)

	if msg.RequestNormalized != nil {
		t.Fatalf("request_normalized must stay nil (view-time recompute), got %s", msg.RequestNormalized)
	}
	if msg.RequestNormalizeStatus != "" {
		t.Fatalf("request normalize status must stay empty, got %q", msg.RequestNormalizeStatus)
	}
	if reuseMetricCalls != 0 {
		t.Fatalf("reuse metric must not fire (reuse seam is inert), got %d", reuseMetricCalls)
	}
	// The raw body is still persisted under the approve action.
	if string(msg.RequestBody.InlineBytes) != `{"model":"gpt-4o"}` {
		t.Fatalf("approve must persist the captured request body, got %q", msg.RequestBody.InlineBytes)
	}
}

// TestRequestNormalizedReuse_ByteIdentity proves pointer-marshal and
// value-marshal of the same canonical payload are byte-identical — the
// invariant that let the request goroutine pre-marshal the canonical without
// changing the persisted bytes. Retained as a marshal-stability guard.
func TestRequestNormalizedReuse_ByteIdentity(t *testing.T) {
	payloadJSON := `{"kind":"ai-chat","normalizeVersion":"2","protocol":"openai","model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`
	var asValue map[string]any
	if err := json.Unmarshal([]byte(payloadJSON), &asValue); err != nil {
		t.Fatal(err)
	}
	stamp, err := json.Marshal(&asValue)
	if err != nil {
		t.Fatal(err)
	}
	bridge, err := json.Marshal(asValue)
	if err != nil {
		t.Fatal(err)
	}
	if string(stamp) != string(bridge) {
		t.Fatalf("pointer-marshal %s != value-marshal %s", stamp, bridge)
	}
}

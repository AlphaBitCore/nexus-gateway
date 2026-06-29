package audit

import (
	"log/slog"
	"testing"
	"time"

	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/decision"
)

// TestRecordToMessage_NeverPersistsNormalized locks the write-path invariant for
// full-chain view-time normalize: recordToMessage never emits a normalized
// projection or redaction spans and never invokes the normalize bridge — the
// control plane recomputes normalized at view time from the action-governed raw
// body. The raw body itself is still persisted under the approve action.
func TestRecordToMessage_NeverPersistsNormalized(t *testing.T) {
	w := NewWriter(nil, "q", nil, slog.Default()).
		WithNormalizer(func(direction, _, _, _, _ string, _ bool, _ []byte) (json.RawMessage, string, string) {
			t.Fatalf("normalize bridge must never run at write time (direction=%s)", direction)
			return nil, "", ""
		})
	rec := &Record{
		RequestID:      "gate",
		Timestamp:      time.Now(),
		RequestBody:    []byte(`{"model":"gpt-4o"}`),
		ResponseBody:   []byte(`{"choices":[]}`),
		RequestAction:  decision.ActionApprove,
		ResponseAction: decision.ActionApprove,
	}
	msg := w.recordToMessage(rec)

	if msg.RequestNormalized != nil {
		t.Errorf("RequestNormalized must stay nil, got %s", msg.RequestNormalized)
	}
	if msg.ResponseNormalized != nil {
		t.Errorf("ResponseNormalized must stay nil, got %s", msg.ResponseNormalized)
	}
	if msg.RequestRedactionSpans != nil {
		t.Errorf("RequestRedactionSpans must stay nil, got %s", msg.RequestRedactionSpans)
	}
	// The raw body is still persisted under the approve action.
	if string(msg.RequestBody.InlineBytes) != `{"model":"gpt-4o"}` {
		t.Errorf("approve must persist the captured request body, got %q", msg.RequestBody.InlineBytes)
	}
}

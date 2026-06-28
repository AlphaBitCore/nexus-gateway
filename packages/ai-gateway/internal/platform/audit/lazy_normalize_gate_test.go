package audit

import (
	"log/slog"
	"testing"
	"time"

	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/decision"
)

// TestRecordToMessage_NormalizeFieldsAreInert locks that the write path no
// longer persists a normalized projection nor invokes the normalize bridge,
// regardless of the (retained-but-inert) SkipRequestNormalize /
// SkipResponseNormalize / RequestNormalizedReuse fields. Under the single-action
// contract the control plane recomputes normalized at view time from the
// action-governed raw body, so these columns always stay nil and the wired
// normalizer is never called.
func TestRecordToMessage_NormalizeFieldsAreInert(t *testing.T) {
	cases := []struct {
		name              string
		skipReq, skipResp bool
		reuse             json.RawMessage
	}{
		{"no flags", false, false, nil},
		{"skip request", true, false, nil},
		{"skip response", false, true, nil},
		{"skip both", true, true, nil},
		{"reuse bytes present", false, false, json.RawMessage(`{"reused":true}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := NewWriter(nil, "q", nil, slog.Default()).
				WithNormalizer(func(direction, _, _, _, _ string, _ bool, _ []byte) (json.RawMessage, string, string) {
					t.Fatalf("normalize bridge must never run at write time (direction=%s)", direction)
					return nil, "", ""
				})
			rec := &Record{
				RequestID:              "gate",
				Timestamp:              time.Now(),
				RequestBody:            []byte(`{"model":"gpt-4o"}`),
				ResponseBody:           []byte(`{"choices":[]}`),
				RequestAction:          decision.ActionApprove,
				ResponseAction:         decision.ActionApprove,
				SkipRequestNormalize:   tc.skipReq,
				SkipResponseNormalize:  tc.skipResp,
				RequestNormalizedReuse: tc.reuse,
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
		})
	}
}

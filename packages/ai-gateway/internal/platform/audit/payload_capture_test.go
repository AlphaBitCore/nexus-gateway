package audit

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/decision"
)

// TestRecordToMessage_PopulatesBodiesWhenCaptured asserts that once the
// handler sets Record.RequestBody / ResponseBody (i.e. payload capture
// is enabled for the request), those bytes land verbatim on the MQ
// wire format for consumption by the Hub db-writer. Nil slices stay
// nil so the downstream `omitempty` tags do their job.
func TestRecordToMessage_PopulatesBodiesWhenCaptured(t *testing.T) {
	w := &Writer{producer: nil, logger: slog.Default()}

	reqBody := []byte(`{"model":"gpt-4","messages":[]}`)
	respBody := []byte(`{"id":"chatcmpl-123","choices":[]}`)

	t.Run("capture on", func(t *testing.T) {
		msg := w.recordToMessage(&Record{
			RequestID:      "r1",
			RequestBody:    reqBody,
			ResponseBody:   respBody,
			RequestAction:  decision.ActionApprove,
			ResponseAction: decision.ActionApprove,
		})
		if !bytes.Equal(msg.RequestBody.InlineBytes, reqBody) {
			t.Errorf("RequestBody: want %q, got %q", reqBody, msg.RequestBody.InlineBytes)
		}
		if !bytes.Equal(msg.ResponseBody.InlineBytes, respBody) {
			t.Errorf("ResponseBody: want %q, got %q", respBody, msg.ResponseBody.InlineBytes)
		}
	})

	t.Run("capture off leaves bodies absent", func(t *testing.T) {
		msg := w.recordToMessage(&Record{RequestID: "r2"})
		if msg.RequestBody.Kind != "absent" {
			t.Errorf("RequestBody: want absent, got %q", msg.RequestBody.Kind)
		}
		if msg.ResponseBody.Kind != "absent" {
			t.Errorf("ResponseBody: want absent, got %q", msg.ResponseBody.Kind)
		}
	})

	t.Run("empty body treated as absent on wire", func(t *testing.T) {
		msg := w.recordToMessage(&Record{
			RequestID:    "r3",
			RequestBody:  []byte{},
			ResponseBody: []byte{},
		})
		if msg.RequestBody.Kind != "absent" {
			t.Errorf("empty RequestBody should be absent, got %q", msg.RequestBody.Kind)
		}
		if msg.ResponseBody.Kind != "absent" {
			t.Errorf("empty ResponseBody should be absent, got %q", msg.ResponseBody.Kind)
		}
	})
}

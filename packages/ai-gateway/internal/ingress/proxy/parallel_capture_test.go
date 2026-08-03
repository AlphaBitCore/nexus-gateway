package proxy

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/redact"
)

func newCaptureHandler(store bool) *Handler {
	return &Handler{deps: &Deps{
		PayloadCapture: payloadcapture.NewStore(payloadcapture.Config{StoreResponseBody: store}),
	}}
}

func TestCaptureParallelResponse(t *testing.T) {
	body := []byte(`{"text":"hello world"}`)

	t.Run("capture on → body + content-type survive the shared redact gate", func(t *testing.T) {
		h := newCaptureHandler(true)
		rec := &audit.Record{}
		h.captureParallelResponse(rec, body, "application/json")
		if string(rec.ResponseBody) != string(body) {
			t.Errorf("ResponseBody=%q", rec.ResponseBody)
		}
		if rec.ResponseContentType != "application/json" {
			t.Errorf("ResponseContentType=%q", rec.ResponseContentType)
		}
		// This used to assert an explicit ResponseAction=approve stamp and call it
		// load-bearing, because the gate dropped a zero-value action. The rule now
		// lives in redact.StorageRawBodyChecked, the one gate all three services
		// persist through, so the stamp is gone and what gets asserted is the
		// outcome it existed to produce: an unset action means no redaction demand,
		// and the captured body reaches the store.
		if rec.ResponseAction != "" {
			t.Errorf("ResponseAction=%q — a parallel handler runs no response hook, so the action stays unset "+
				"and the gate decides what that means", rec.ResponseAction)
		}
		got, ok := redact.StorageRawBodyChecked(rec.ResponseBody, nil, rec.ResponseAction)
		if !ok {
			t.Error("the shared gate reported an unnameable action for an unset one")
		}
		if string(got) != string(body) {
			t.Errorf("the gate persisted %q; want the captured body %q — a hookless response is being dropped", got, body)
		}
	})

	t.Run("capture off → nothing stamped", func(t *testing.T) {
		h := newCaptureHandler(false)
		rec := &audit.Record{}
		h.captureParallelResponse(rec, body, "application/json")
		if rec.ResponseBody != nil || rec.ResponseAction != "" {
			t.Errorf("off leaked: body=%v action=%q", rec.ResponseBody, rec.ResponseAction)
		}
	})

	t.Run("empty body → nothing stamped even with capture on", func(t *testing.T) {
		h := newCaptureHandler(true)
		rec := &audit.Record{}
		h.captureParallelResponse(rec, nil, "application/json")
		if rec.ResponseBody != nil || rec.ResponseAction != "" {
			t.Errorf("empty leaked: body=%v action=%q", rec.ResponseBody, rec.ResponseAction)
		}
	})

	t.Run("nil rec → no panic", func(t *testing.T) {
		newCaptureHandler(true).captureParallelResponse(nil, body, "application/json")
	})

	t.Run("request body is never stamped (R-7 audio stays out)", func(t *testing.T) {
		h := newCaptureHandler(true)
		rec := &audit.Record{}
		h.captureParallelResponse(rec, body, "application/json")
		if rec.RequestBody != nil {
			t.Errorf("RequestBody must stay nil (R-7), got %q", rec.RequestBody)
		}
	})
}

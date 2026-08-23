package proxy

import (
	"testing"

	"github.com/goccy/go-json"

	gr "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/guardrail"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// guardrailAuditBodies drives one guardrail request through the handler with the
// given capture config and returns the request/response bodies as the audit
// writer actually emitted them — i.e. AFTER the storage gate, which is where a
// block or redact disposition drops raw bytes.
func guardrailAuditBodies(t *testing.T, cfg payloadcapture.Config, body string) (req, resp []byte) {
	t.Helper()
	deps, prod := sttDeps(t, "http://127.0.0.1:0")
	deps.HookConfigCache = newPiiRedactHookCache(t)
	deps.PayloadCapture = payloadcapture.NewStore(cfg)
	aw := deps.AuditWriter
	h := NewHandler(deps).ServeGuardrail()

	doGuardrail(h, body)
	aw.Close()

	prod.mu.Lock()
	msgs := append([][]byte(nil), prod.messages...)
	prod.mu.Unlock()
	if len(msgs) != 1 {
		t.Fatalf("captured %d audit messages, want 1", len(msgs))
	}
	var evt mq.TrafficEventMessage
	if err := json.Unmarshal(msgs[0], &evt); err != nil {
		t.Fatalf("decode audit message: %v", err)
	}
	return evt.RequestBody.InlineBytes, evt.ResponseBody.InlineBytes
}

func captureOn() payloadcapture.Config {
	c := payloadcapture.DefaultConfig()
	c.StoreRequestBody = true
	c.StoreResponseBody = true
	return c
}

// TestServeGuardrail_CapturesBodiesWhenEnabled is the regression for the defect
// the owner reported twice: /v1/guardrail stored neither body, so a verdict row
// could not be traced back to the text it judged. The endpoint now answers to the
// same operator switch as every other one.
func TestServeGuardrail_CapturesBodiesWhenEnabled(t *testing.T) {
	const body = `{"stage":"input","content":"hello world"}`
	req, resp := guardrailAuditBodies(t, captureOn(), body)

	if string(req) != body {
		t.Errorf("captured request body = %q, want the verbatim request %q", string(req), body)
	}
	// The verdict is our own output and carries no raw evaluated text, so it is
	// stored as sent. Assert on the decoded verdict rather than a byte string so
	// the test pins the CONTENT, not the field order.
	var v gr.Response
	if err := json.Unmarshal(resp, &v); err != nil {
		t.Fatalf("captured response body is not a verdict: %v; body=%q", err, string(resp))
	}
	if v.Action != "allow" || v.Coverage != gr.CoverageFull {
		t.Errorf("captured verdict = action=%q coverage=%q, want allow/full", v.Action, v.Coverage)
	}
}

// TestServeGuardrail_CaptureOffStoresNothing pins the other half: the switch is
// the operator's, and an endpoint that captured regardless of it would be just as
// wrong as one that never captured.
func TestServeGuardrail_CaptureOffStoresNothing(t *testing.T) {
	req, resp := guardrailAuditBodies(t, payloadcapture.DefaultConfig(), `{"content":"hello world"}`)
	if len(req) != 0 {
		t.Errorf("request body stored with capture off: %q", string(req))
	}
	if len(resp) != 0 {
		t.Errorf("response body stored with capture off: %q", string(resp))
	}
}

// TestServeGuardrail_RedactVerdictStoresMaskedRequest is the compliance half of
// the fix. Under a redact disposition the audit writer's storage gate persists
// RequestBodyRedacted and fail-safes the raw bytes to NULL, so the stored body
// must be the masked copy — the email must be unreachable from the row while the
// row still shows what was judged.
func TestServeGuardrail_RedactVerdictStoresMaskedRequest(t *testing.T) {
	req, _ := guardrailAuditBodies(t, captureOn(), `{"stage":"input","content":"ping alice@example.com"}`)
	if len(req) == 0 {
		t.Fatal("redact verdict stored no request body; the masked copy must survive")
	}
	var stored gr.Request
	if err := json.Unmarshal(req, &stored); err != nil {
		t.Fatalf("stored request body is not a guardrail request: %v; body=%q", err, string(req))
	}
	if stored.Content != "ping [REDACTED_EMAIL]" {
		t.Errorf("stored content = %q, want the masked copy %q", stored.Content, "ping [REDACTED_EMAIL]")
	}
}

// TestServeGuardrail_MalformedBodyStillCaptured pins the capture-before-parse
// ordering: a 400 tells the caller the JSON was bad, and the row has to show what
// the bad JSON actually was, or the report is unactionable.
func TestServeGuardrail_MalformedBodyStillCaptured(t *testing.T) {
	const bad = `{"content":`
	req, _ := guardrailAuditBodies(t, captureOn(), bad)
	if string(req) != bad {
		t.Errorf("captured request body = %q, want the malformed bytes %q", string(req), bad)
	}
}

// TestRedactedRequestBody_MessagesKeepPositions pins the placement rule that made
// the direct block read necessary: masking a whole segment to "" must not shift
// the later segments onto the wrong message. A wrongly-placed mask is worse than
// no stored body, so the projection is read block-by-block, not through
// TextProjection (which drops empty blocks).
func TestRedactedRequestBody_MessagesKeepPositions(t *testing.T) {
	req := &gr.Request{Messages: []gr.Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: ""},
		{Role: "user", Content: "second"},
	}}
	out := gr.RedactedRequestBody(req, nil)
	if out == nil {
		t.Fatal("RedactedRequestBody returned nil for a spanless request")
	}
	var got gr.Request
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := []string{"first", "", "second"}
	if len(got.Messages) != len(want) {
		t.Fatalf("got %d messages, want %d", len(got.Messages), len(want))
	}
	for i := range want {
		if got.Messages[i].Content != want[i] {
			t.Errorf("message[%d].content = %q, want %q — empty segments must not shift positions",
				i, got.Messages[i].Content, want[i])
		}
	}
}

// TestRedactedRequestBody_NilOnNothingToMask pins the fail-safe direction: when a
// masked copy cannot be produced, the answer is nil ("store no body"), never the
// raw request.
func TestRedactedRequestBody_NilOnNothingToMask(t *testing.T) {
	if out := gr.RedactedRequestBody(nil, nil); out != nil {
		t.Errorf("nil request produced %q, want nil", string(out))
	}
	if out := gr.RedactedRequestBody(&gr.Request{}, nil); out != nil {
		t.Errorf("empty request produced %q, want nil", string(out))
	}
}

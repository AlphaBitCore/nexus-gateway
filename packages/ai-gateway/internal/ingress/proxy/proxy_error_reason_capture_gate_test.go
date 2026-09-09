package proxy

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/executor"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// A WAF page is the shape that makes this matter: it is not JSON, so
// ProviderErrorMessage falls through both structured branches and copies raw
// upstream bytes verbatim. Those bytes are chosen by someone else and routinely
// quote the caller's own request back — here, its Authorization header.
const wafErrorPage = `<html><head><title>403 Forbidden</title></head><body>` +
	`Request blocked by policy. Offending header: Authorization: Bearer sk-live-CALLER-SECRET` +
	`</body></html>`

// TestServeProxy_ErrorReasonObeysThePayloadCaptureGate pins the operator's
// opt-out across BOTH audit columns.
//
// StoreResponseBody=false is a compliance control: an operator turns it off so
// upstream response bodies are not persisted. The gateway honoured it for
// traffic_event.payloads.response_body and bypassed it for the column beside —
// error_reason quoted 300 verbatim bytes of the same body regardless. An
// operator who excluded the body then read it in the Traffic drawer anyway.
//
// makeFakeDeps already builds the handler with StoreResponseBody=false, which
// is the configuration under test.
func TestServeProxy_ErrorReasonObeysThePayloadCaptureGate(t *testing.T) {
	fexec := &fakeExecutor{Result: &executor.ExecutionResult{
		StatusCode: http.StatusForbidden,
		Headers:    http.Header{"Content-Type": []string{"text/html"}},
		Body:       []byte(wafErrorPage),
		Target: routingcore.RoutingTarget{
			ProviderID: "p-openai", ProviderName: "openai",
			ModelID: "gpt-4o", ModelCode: "gpt-4o", AdapterType: "openai",
		},
		Attempts: []executor.Attempt{{StatusCode: http.StatusForbidden, Dispatched: true}},
	}}
	prod := &captureProducer{}
	deps := makeFakeDeps(t, fexec, &fakeBridge{})
	aw := audit.NewWriter(prod, "nexus.event.ai-traffic", nil, slog.Default())
	deps.AuditWriter = aw

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))

	aw.Close()
	prod.mu.Lock()
	msgs := append([][]byte(nil), prod.messages...)
	prod.mu.Unlock()
	if len(msgs) == 0 {
		t.Fatal("no audit event captured — the gate cannot see anything")
	}

	var env map[string]json.RawMessage
	if err := json.Unmarshal(msgs[len(msgs)-1], &env); err != nil {
		t.Fatalf("unmarshalling the audit envelope: %v", err)
	}
	raw, ok := env["errorReason"]
	if !ok {
		t.Fatal("the audit event carried no error_reason at all — this test can no longer see the column it guards")
	}
	var reason string
	if err := json.Unmarshal(raw, &reason); err != nil {
		t.Fatalf("error_reason was not a string: %s", raw)
	}

	if strings.Contains(reason, "CALLER-SECRET") || strings.Contains(reason, "<html>") {
		t.Fatalf("error_reason carried the response body the operator excluded from capture: %q", reason)
	}
	// With nothing it may quote, ProviderErrorMessage falls back to the status
	// line — which still tells an operator what happened.
	if !strings.Contains(reason, "403") {
		t.Fatalf("error_reason must still name the failure; got %q", reason)
	}
}

// TestServeProxy_ErrorReasonKeepsTheProvidersOwnDiagnostic is the half that
// keeps the gate from being satisfied by breaking the column.
//
// makeFakeDeps builds the handler with StoreResponseBody=false, which is not an
// exotic setting: it is payloadcapture.DefaultConfig(). The provider's own
// error.message is a bounded, provider-authored diagnostic — the thing an
// operator triages a 4xx with — and no payload policy is about it. Only a
// verbatim quote of the response body is.
func TestServeProxy_ErrorReasonKeepsTheProvidersOwnDiagnostic(t *testing.T) {
	const providerMsg = "You exceeded your current quota, please check your plan and billing details"
	fexec := &fakeExecutor{Result: &executor.ExecutionResult{
		StatusCode: http.StatusTooManyRequests,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       []byte(`{"error":{"message":"` + providerMsg + `","type":"insufficient_quota"}}`),
		Target: routingcore.RoutingTarget{
			ProviderID: "p-openai", ProviderName: "openai",
			ModelID: "gpt-4o", ModelCode: "gpt-4o", AdapterType: "openai",
		},
		Attempts: []executor.Attempt{{StatusCode: http.StatusTooManyRequests, Dispatched: true}},
	}}
	prod := &captureProducer{}
	deps := makeFakeDeps(t, fexec, &fakeBridge{})
	aw := audit.NewWriter(prod, "nexus.event.ai-traffic", nil, slog.Default())
	deps.AuditWriter = aw

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))

	aw.Close()
	prod.mu.Lock()
	msgs := append([][]byte(nil), prod.messages...)
	prod.mu.Unlock()
	if len(msgs) == 0 {
		t.Fatal("no audit event captured")
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(msgs[len(msgs)-1], &env); err != nil {
		t.Fatalf("unmarshalling the audit envelope: %v", err)
	}
	raw, ok := env["errorReason"]
	if !ok {
		t.Fatal("the audit event carried no error_reason at all")
	}
	var reason string
	if err := json.Unmarshal(raw, &reason); err != nil {
		t.Fatalf("error_reason was not a string: %s", raw)
	}

	if reason != providerMsg {
		t.Fatalf("the provider's own diagnostic was lost on the DEFAULT payload-capture config.\n"+
			" got: %q\nwant: %q\n"+
			"Collapsing every failure to a status line is a worse column than the leak this gate guards.",
			reason, providerMsg)
	}
}

package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/executor"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// Go sets the server write deadline when the request header is read, so a
// proxy response only written after a multi-minute upstream call is already
// past a flat server.writeTimeout by the time it is written: the write fails
// and the client sees a severed connection instead of its answer. Every proxy
// write path therefore has to lift the deadline to the upstream budget before
// writing. The Responses-API path (proxy_responses.go) and the streaming
// preamble (stream_preamble.go) already do; this pins the helper the general
// non-stream path uses so all of them agree.
//
// Asserts the invariant (deadline == now + the active upstream budget) rather
// than the literal 600s, so a deployment that raises upstream.timeoutSec stays
// covered.
func TestExtendWriteDeadlineToUpstreamBudget_LiftsDeadlineToUpstreamBudget(t *testing.T) {
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	budget := specutil.ActiveConfig().Timeout

	before := time.Now()
	extendWriteDeadlineToUpstreamBudget(rec)
	after := time.Now()

	if len(rec.deadlines) != 1 {
		t.Fatalf("expected exactly one write-deadline extension, got %d", len(rec.deadlines))
	}
	got := rec.deadlines[0]
	if got.Before(before.Add(budget)) || got.After(after.Add(budget)) {
		t.Fatalf("write deadline %v is not now+%v (call window %v..%v); a long non-stream inference would still be severed by the flat server.writeTimeout",
			got, budget, before, after)
	}
}

// A ResponseWriter with no SetWriteDeadline (the common case behind a
// middleware wrapper that does not unwrap) must not panic or block the write.
// The response still has to go out — losing a completed inference because the
// deadline could not be extended would be strictly worse than the truncation
// this helper exists to prevent.
func TestExtendWriteDeadlineToUpstreamBudget_UnsupportedWriterIsNotFatal(t *testing.T) {
	plain := httptest.NewRecorder()

	extendWriteDeadlineToUpstreamBudget(plain) // must not panic

	if _, err := plain.Write([]byte(`{"ok":true}`)); err != nil {
		t.Fatalf("the response must still be writable after a no-op deadline extension: %v", err)
	}
	if plain.Body.String() != `{"ok":true}` {
		t.Fatalf("body: got %q", plain.Body.String())
	}
}

// The two tests below drive the real ServeProxy path rather than the helper in
// isolation, because the defect was never the helper — it was that the general
// non-stream path never called one. A helper-only test would have passed
// against the broken code.

// Success arm: a completed inference must reach the client. This is the request
// the customer paid for and that we have already billed and audited, so losing
// it to a stale write deadline is the worst outcome on this path.
func TestServeProxy_NonStreamSuccess_ArmsWriteDeadlineToUpstreamBudget(t *testing.T) {
	successBody := []byte(`{"id":"x","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	fexec := &fakeExecutor{Result: &executor.ExecutionResult{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       successBody,
		Target:     routingcore.RoutingTarget{ProviderID: "p-openai", ProviderName: "openai", ModelID: "gpt-4o", ModelCode: "gpt-4o", AdapterType: "openai"},
		Usage:      provcore.Usage{PromptTokens: iPtr(1), CompletionTokens: iPtr(1), TotalTokens: iPtr(2)},
		Attempts:   []executor.Attempt{{StatusCode: http.StatusOK}},
	}}
	deps := makeFakeDeps(t, fexec, &fakeBridge{})
	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})

	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	budget := specutil.ActiveConfig().Timeout
	before := time.Now()
	h(rec, freshChatRequest(t, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	after := time.Now()

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(rec.deadlines) == 0 {
		t.Fatal("the non-stream success path never armed a write deadline; a response that took minutes upstream would be severed mid-write")
	}
	got := rec.deadlines[len(rec.deadlines)-1]
	if got.Before(before.Add(budget)) || got.After(after.Add(budget)) {
		t.Fatalf("write deadline %v is not now+%v (window %v..%v)", got, budget, before, after)
	}
}

// Error arm: the 502 an operator most needs is written only after the executor
// has spent the full budget retrying and failing over — the point at which the
// flat deadline is most certainly blown. A severed connection here destroys the
// one artefact that explains the failure.
func TestServeProxy_NonStreamError_ArmsWriteDeadlineToUpstreamBudget(t *testing.T) {
	fexec := &fakeExecutor{Result: &executor.ExecutionResult{
		Error:    executor.ErrAllTargetsExhausted,
		Attempts: []executor.Attempt{{StatusCode: http.StatusBadGateway, Error: "upstream 502"}},
	}}
	deps := makeFakeDeps(t, fexec, &fakeBridge{})
	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})

	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	budget := specutil.ActiveConfig().Timeout
	before := time.Now()
	h(rec, freshChatRequest(t, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	after := time.Now()

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d want 502; body=%s", rec.Code, rec.Body.String())
	}
	if len(rec.deadlines) == 0 {
		t.Fatal("the non-stream error path never armed a write deadline; a 502 after a long failover would be severed and the client would lose the explanation")
	}
	got := rec.deadlines[0]
	if got.Before(before.Add(budget)) || got.After(after.Add(budget)) {
		t.Fatalf("write deadline %v is not now+%v (window %v..%v)", got, budget, before, after)
	}
}

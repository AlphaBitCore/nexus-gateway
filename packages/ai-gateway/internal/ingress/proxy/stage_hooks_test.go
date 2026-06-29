// stage_hooks_test.go — characterization pins for the request-hooks
// stage of the proxy pipeline: the Modify rewrite reaching the upstream
// wire and the rewrite-failure arms (adapter-unsupported vs hard error).
package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	goHooks "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/openai"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// rewriteStubAdapter delegates everything to the embedded adapter but
// fails RewriteRequestBody with the configured error, driving the
// Modify-rewrite failure arms.
type rewriteStubAdapter struct {
	traffic.Adapter
	rewriteErr error
}

func (a *rewriteStubAdapter) RewriteRequestBody(_ context.Context, _ []byte, _ string, _ traffic.NormalizedContent) ([]byte, int, error) {
	return nil, 0, a.rewriteErr
}

// toolArgNonMaskerAdapter embeds the real OpenAI adapter for extraction but
// reports MasksToolCallArgs()==false and succeeds RewriteRequestBody without
// touching tool args — modelling a per-host (anthropic/gemini) adapter that
// masks text yet cannot reconstruct masked tool-call arguments onto its wire.
type toolArgNonMaskerAdapter struct{ *openai.Adapter }

func (a *toolArgNonMaskerAdapter) RewriteRequestBody(_ context.Context, body []byte, _ string, _ traffic.NormalizedContent) ([]byte, int, error) {
	return body, 1, nil // text masked; tool args silently dropped (non-masker)
}

func (a *toolArgNonMaskerAdapter) MasksToolCallArgs() bool { return false }

// TestRunRequestHooks_ToolArgMasking_FailsClosedOnNonMasker pins that the
// AI-gateway request path fails closed when a redact hook masked a tool-call
// argument but the resolved (non-OpenAI) adapter cannot reconstruct it onto its
// wire: a nil-error text rewrite that silently drops the masked tool args would
// forward the tool-arg PII upstream while the audit records it redacted.
// GuardToolArgMasking turns that divergence into a 403.
func TestRunRequestHooks_ToolArgMasking_FailsClosedOnNonMasker(t *testing.T) {
	h := &Handler{deps: &Deps{
		HookConfigCache: newPiiRedactHookCache(t),
		TrafficAdapter:  &toolArgNonMaskerAdapter{Adapter: &openai.Adapter{}},
		Logger:          slog.Default(),
	}}

	// Request history carries an assistant tool_call whose arguments hold PII.
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"send","arguments":"{\"to\":\"alice@example.com\"}"}}]}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	auditRec := &audit.Record{RequestID: "req-toolarg"}

	rewritten, _, rejected := h.runRequestHooks(req, rec, auditRec, "req-toolarg", body, routingcore.RoutingTarget{}, openAIIngress, nil, slog.Default())
	if !rejected {
		t.Fatalf("masked tool args + non-masker adapter must fail closed; response=%s", rec.Body.String())
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status=%d want 403", rec.Code)
	}
	if rewritten != nil {
		t.Errorf("rewritten=%q want nil (rejected, nothing forwarded)", string(rewritten))
	}
	if auditRec.RequestAction != goHooks.ActionBlock {
		t.Errorf("RequestAction=%q want block", auditRec.RequestAction)
	}
	if auditRec.HookReasonCode != goHooks.ReasonRedactInflightUnsupported {
		t.Errorf("HookReasonCode=%q want %q", auditRec.HookReasonCode, goHooks.ReasonRedactInflightUnsupported)
	}
}

// TestServeProxy_RequestHookModify_ForwardsRewrittenBodyUpstream pins the
// end-to-end Modify contract: a request-stage redact hook's rewritten
// body — not the caller's original bytes — is what reaches the upstream
// provider.
func TestServeProxy_RequestHookModify_ForwardsRewrittenBodyUpstream(t *testing.T) {
	var mu sync.Mutex
	var upstreamGot []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		upstreamGot = b
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"id":"x","object":"chat.completion","model":"gpt-4o",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
	}))
	defer upstream.Close()

	deps := makeOpenAIDeps(t, upstream.URL, newPiiRedactHookCache(t))
	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, `{"model":"gpt-4o","messages":[{"role":"user","content":"ping alice@example.com"}]}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	mu.Lock()
	got := string(upstreamGot)
	mu.Unlock()
	if !strings.Contains(got, "[REDACTED_EMAIL]") {
		t.Errorf("upstream body=%s want redacted placeholder forwarded", got)
	}
	if strings.Contains(got, "alice@example.com") {
		t.Errorf("upstream body=%s must NOT carry the original email", got)
	}
}

// TestRunRequestHooks_RewriteUnsupported_FailsClosed pins the degraded
// Modify path: when the traffic adapter cannot reverse-encode
// (ErrRewriteUnsupported) the request is rejected (fail CLOSED) rather than
// forwarding the unredacted original upstream. The audit row records the
// block action and the inflight-unsupported reason code.
func TestRunRequestHooks_RewriteUnsupported_FailsClosed(t *testing.T) {
	h := &Handler{deps: &Deps{
		HookConfigCache: newPiiRedactHookCache(t),
		TrafficAdapter:  &rewriteStubAdapter{Adapter: &openai.Adapter{}, rewriteErr: traffic.ErrRewriteUnsupported},
		Logger:          slog.Default(),
	}}

	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"ping alice@example.com"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	auditRec := &audit.Record{RequestID: "req-test"}

	rewritten, _, rejected := h.runRequestHooks(req, rec, auditRec, "req-test", body, routingcore.RoutingTarget{}, openAIIngress, nil, slog.Default())
	if !rejected {
		t.Fatalf("unsupported rewrite must fail closed (reject); response=%s", rec.Body.String())
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status=%d want 403", rec.Code)
	}
	if rewritten != nil {
		t.Errorf("rewritten=%q want nil (rejected, nothing forwarded)", string(rewritten))
	}
	if auditRec.RequestAction != goHooks.ActionBlock {
		t.Errorf("RequestAction=%q want %q (block)", auditRec.RequestAction, goHooks.ActionBlock)
	}
	if auditRec.HookReasonCode != goHooks.ReasonRedactInflightUnsupported {
		t.Errorf("HookReasonCode=%q want %q", auditRec.HookReasonCode, goHooks.ReasonRedactInflightUnsupported)
	}
}

// TestRunRequestHooks_RewriteFailure_Returns500 pins the hard-failure
// Modify arm: a rewrite error that is not ErrRewriteUnsupported indicates
// internal inconsistency and surfaces as a 500 with the request rejected.
func TestRunRequestHooks_RewriteFailure_Returns500(t *testing.T) {
	h := &Handler{deps: &Deps{
		HookConfigCache: newPiiRedactHookCache(t),
		TrafficAdapter:  &rewriteStubAdapter{Adapter: &openai.Adapter{}, rewriteErr: io.ErrUnexpectedEOF},
		Logger:          slog.Default(),
	}}

	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"ping alice@example.com"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	auditRec := &audit.Record{RequestID: "req-test"}

	_, _, rejected := h.runRequestHooks(req, rec, auditRec, "req-test", body, routingcore.RoutingTarget{}, openAIIngress, nil, slog.Default())
	if !rejected {
		t.Fatal("hard rewrite failure must reject the request")
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "request rewrite failed") {
		t.Errorf("body=%s want rewrite-failure message", rec.Body.String())
	}
}

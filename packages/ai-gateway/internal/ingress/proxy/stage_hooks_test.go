// stage_hooks_test.go — characterization pins for the request-hooks
// stage of the proxy pipeline: the Modify rewrite reaching the upstream
// wire, the storage-policy reason-code stamps, and the rewrite-failure
// arms (adapter-unsupported vs hard error).
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
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/builtins"
	goHooks "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/openai"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// storagePolicyHook approves the request inflight while declaring a
// storage policy (and optionally storage-side transform spans) on its
// result — the "audit-only" hook shape that diverges the persisted copy
// from the wire copy.
type storagePolicyHook struct {
	goHooks.AnyEndpointAnyModality
	storage goHooks.StorageAction
	spans   []normcore.TransformSpan
}

func (h storagePolicyHook) Execute(_ context.Context, _ *goHooks.HookInput) (*goHooks.HookResult, error) {
	return &goHooks.HookResult{
		Decision:       goHooks.Approve,
		StorageAction:  h.storage,
		TransformSpans: h.spans,
	}, nil
}

// newRequestStageHookCache builds a HookConfigCache serving exactly one
// request-stage hook backed by the supplied implementation.
func newRequestStageHookCache(t *testing.T, impl goHooks.Hook) *compliance.HookConfigCache {
	t.Helper()
	reg := builtins.Registry.Clone()
	reg.Register("req-storage-hook", func(_ *goHooks.HookConfig) (goHooks.Hook, error) {
		return impl, nil
	})
	reg.Freeze()
	loader := func(_ context.Context) ([]goHooks.HookConfig, error) {
		return []goHooks.HookConfig{{
			ID:                "req-storage-1",
			ImplementationID:  "req-storage-hook",
			Name:              "req-storage",
			Priority:          1,
			Enabled:           true,
			Stage:             "request",
			FailBehavior:      "fail-closed",
			TimeoutMs:         1000,
			ApplicableIngress: []string{"ALL"},
			Config:            map[string]any{},
		}}, nil
	}
	cache := compliance.NewHookConfigCache(loader, reg, 0, slog.Default())
	if err := cache.Start(context.Background()); err != nil {
		t.Fatalf("cache.Start: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	return cache
}

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

// TestRunRequestHooks_StorageDropContent_StampsReasonCode pins the
// storage-policy stamp for drop-content: an approve-inflight hook whose
// storage policy is drop-content lets the request proceed unmodified but
// stamps the audit row with the dropped-by-policy reason code so audit
// consumers can tell why the persisted content is absent.
func TestRunRequestHooks_StorageDropContent_StampsReasonCode(t *testing.T) {
	h := &Handler{deps: &Deps{
		HookConfigCache: newRequestStageHookCache(t, storagePolicyHook{storage: goHooks.StorageDropContent}),
		TrafficAdapter:  &openai.Adapter{},
		Logger:          slog.Default(),
	}}

	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	auditRec := &audit.Record{RequestID: "req-test"}

	rewritten, _, rejected := h.runRequestHooks(req, rec, auditRec, "req-test", body, routingcore.RoutingTarget{}, openAIIngress, slog.Default())
	if rejected {
		t.Fatalf("unexpected rejection; response=%s", rec.Body.String())
	}
	if rewritten != nil {
		t.Errorf("inflight approve must not rewrite the wire body; got %q", string(rewritten))
	}
	if auditRec.HookReasonCode != goHooks.ReasonStorageDroppedByPolicy {
		t.Errorf("HookReasonCode=%q want %q", auditRec.HookReasonCode, goHooks.ReasonStorageDroppedByPolicy)
	}
	if auditRec.RequestStorageAction != string(goHooks.StorageDropContent) {
		t.Errorf("RequestStorageAction=%q want %q", auditRec.RequestStorageAction, goHooks.StorageDropContent)
	}
}

// TestRunRequestHooks_StorageRedactOnly_StampsReasonCode pins the
// audit-only redact stamp: an approve decision carrying storage-side
// transform spans plus a redact storage policy means the persisted copy
// diverges from the wire copy — the audit row records the
// redact-storage-only reason code and the spans.
func TestRunRequestHooks_StorageRedactOnly_StampsReasonCode(t *testing.T) {
	span := normcore.TransformSpan{
		Source:         normcore.SourceHook,
		SourceID:       "email",
		Action:         normcore.ActionRedact,
		ContentAddress: "messages[0].content",
		Start:          5,
		End:            22,
		Replacement:    "[REDACTED_EMAIL]",
	}
	h := &Handler{deps: &Deps{
		HookConfigCache: newRequestStageHookCache(t, storagePolicyHook{
			storage: goHooks.StorageRedact,
			spans:   []normcore.TransformSpan{span},
		}),
		TrafficAdapter: &openai.Adapter{},
		Logger:         slog.Default(),
	}}

	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"ping alice@example.com"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	auditRec := &audit.Record{RequestID: "req-test"}

	rewritten, _, rejected := h.runRequestHooks(req, rec, auditRec, "req-test", body, routingcore.RoutingTarget{}, openAIIngress, slog.Default())
	if rejected {
		t.Fatalf("unexpected rejection; response=%s", rec.Body.String())
	}
	if rewritten != nil {
		t.Errorf("storage-only redact must not rewrite the wire body; got %q", string(rewritten))
	}
	if auditRec.HookReasonCode != goHooks.ReasonRedactStorageOnlyByPolicy {
		t.Errorf("HookReasonCode=%q want %q", auditRec.HookReasonCode, goHooks.ReasonRedactStorageOnlyByPolicy)
	}
	if len(auditRec.RequestTransformSpans) == 0 {
		t.Error("RequestTransformSpans must carry the storage-side redact spans")
	}
}

// TestRunRequestHooks_RewriteUnsupported_ForwardsOriginalBody pins the
// degraded Modify path: when the traffic adapter cannot reverse-encode
// (ErrRewriteUnsupported) the original body is forwarded, the request is
// NOT rejected, and the audit row records the inflight-unsupported
// reason code.
func TestRunRequestHooks_RewriteUnsupported_ForwardsOriginalBody(t *testing.T) {
	h := &Handler{deps: &Deps{
		HookConfigCache: newPiiRedactHookCache(t),
		TrafficAdapter:  &rewriteStubAdapter{Adapter: &openai.Adapter{}, rewriteErr: traffic.ErrRewriteUnsupported},
		Logger:          slog.Default(),
	}}

	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"ping alice@example.com"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	auditRec := &audit.Record{RequestID: "req-test"}

	rewritten, _, rejected := h.runRequestHooks(req, rec, auditRec, "req-test", body, routingcore.RoutingTarget{}, openAIIngress, slog.Default())
	if rejected {
		t.Fatalf("unsupported rewrite must not reject; response=%s", rec.Body.String())
	}
	if rewritten != nil {
		t.Errorf("rewritten=%q want nil (original body forwarded)", string(rewritten))
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

	_, _, rejected := h.runRequestHooks(req, rec, auditRec, "req-test", body, routingcore.RoutingTarget{}, openAIIngress, slog.Default())
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

package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// TestRunRequestHooks_ModifyOrder_HookRewritePrecedesCodecEncode
// documents the invariant: the ingress-format hook rewrite runs BEFORE
// any provider-format SchemaCodec encoding. We
// enforce the invariant by observing the sequence of calls made to a
// scripted traffic adapter — ExtractRequest (hook input) must be
// called before RewriteRequestBody (hook output), and both must see
// ingress bytes (never codec-translated bytes).
//
// Downstream provider-format encoding is the executor's job, not
// runRequestHooks'. If a future refactor accidentally calls
// SchemaCodec.EncodeRequest from inside runRequestHooks, the rewrite
// would operate on translated bytes and the upstream would see a
// double-translated body. This test pins the ordering contract.
func TestRunRequestHooks_ModifyOrder_HookRewritePrecedesCodecEncode(t *testing.T) {
	cache := newPiiRedactHookCache(t)

	var callLog []string
	stub := &stubTrafficAdapter{
		id: "anthropic",
		extractRequest: func(_ context.Context, body []byte, _ string) (traffic.NormalizedContent, error) {
			callLog = append(callLog, "extract:"+string(body))
			return traffic.NormalizedContent{Segments: []string{"ping alice@example.com"}}, nil
		},
		rewriteRequest: func(_ context.Context, body []byte, _ string, content traffic.NormalizedContent) ([]byte, int, error) {
			callLog = append(callLog, "rewrite:"+string(body))
			rewritten := strings.ReplaceAll(string(body), "alice@example.com", "[REDACTED_EMAIL]")
			// A real rewrite would splice content.Segments into the
			// body; the test-only stub trusts that the pipeline
			// already substituted the redacted string in-place, so we
			// acknowledge the single segment was "applied".
			_ = content
			return []byte(rewritten), 1, nil
		},
	}

	h := &Handler{deps: &Deps{
		HookConfigCache: cache,
		TrafficAdapter:  stub,
		Logger:          slog.Default(),
	}}

	body := []byte(`{"messages":[{"role":"user","content":"ping alice@example.com"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	auditRec := &audit.Record{RequestID: "req-order"}

	anthropicIngress := Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatAnthropic,
	}

	rewritten, _, rejected := h.runRequestHooks(req, rec, auditRec, "req-order", body, routingcore.RoutingTarget{}, anthropicIngress, slog.Default())
	if rejected {
		t.Fatalf("unexpected rejection; response=%s", rec.Body.String())
	}
	if rewritten == nil {
		t.Fatalf("expected Modify to produce rewritten bytes; callLog=%v", callLog)
	}

	if len(callLog) < 2 {
		t.Fatalf("expected at least extract+rewrite calls, got %v", callLog)
	}
	if !strings.HasPrefix(callLog[0], "extract:") {
		t.Errorf("first adapter call should be extract (hook input); got %q", callLog[0])
	}
	if !strings.HasPrefix(callLog[len(callLog)-1], "rewrite:") {
		t.Errorf("last adapter call should be rewrite (hook output); got %q", callLog[len(callLog)-1])
	}

	// Both calls must have seen the ORIGINAL ingress body, never a
	// codec-translated variant. If some future refactor accidentally
	// interposes SchemaCodec.EncodeRequest, the rewrite stub here
	// would see translated bytes and this assertion fails.
	wantBody := string(body)
	for _, entry := range callLog {
		got := strings.TrimPrefix(strings.TrimPrefix(entry, "extract:"), "rewrite:")
		if got != wantBody {
			t.Errorf("adapter call saw %q, want original ingress bytes %q — codec likely ran before hook rewrite", got, wantBody)
		}
	}
}

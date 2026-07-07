// admission_and_alias_review_test.go — regression tests demanded by the
// adversarial review of the in-flight admission gate and the pooled
// request-body release: env resolution semantics, gate wiring through
// ServeProxy (per-ingress 429 shape + shed counter), and the zero-write
// rewrite alias pin (RequestBodyRedacted must never share backing with the
// pooled request buffer).
package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/builtins"
	goHooks "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/openai"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// TestResolveAdmissionMax pins the env contract: auto default, explicit
// value, explicit disable, and — the review finding — a typo must fall back
// to the protective default rather than silently disabling the gate.
func TestResolveAdmissionMax(t *testing.T) {
	auto, fb := resolveAdmissionMax("")
	if auto <= 0 || fb {
		t.Fatalf("unset must yield the positive auto default without fallback, got %d %v", auto, fb)
	}
	if v, fb := resolveAdmissionMax("auto"); v != auto || fb {
		t.Fatalf(`"auto" must equal the unset default, got %d %v`, v, fb)
	}
	if v, fb := resolveAdmissionMax("123"); v != 123 || fb {
		t.Fatalf("explicit value must be honored, got %d %v", v, fb)
	}
	if v, fb := resolveAdmissionMax("0"); v != 0 || fb {
		t.Fatalf("0 must disable without fallback, got %d %v", v, fb)
	}
	if v, fb := resolveAdmissionMax("-5"); v != 0 || fb {
		t.Fatalf("negative must disable, got %d %v", v, fb)
	}
	if v, fb := resolveAdmissionMax("4O96"); v != auto || !fb {
		t.Fatalf("garbage must FALL BACK to the auto default (never silently disable), got %d fellBack=%v", v, fb)
	}
}

// TestServeProxy_GateSheds_PerIngressShape verifies the gate is wired into
// ServeProxy and the reject honors the cross-ingress error contract: an
// anthropic /v1/messages caller gets the anthropic error envelope, an
// OpenAI caller the OpenAI shape — both with Retry-After and a shed-counter
// increment. A max=0 gate sheds immediately, so no other deps are touched.
func TestServeProxy_GateSheds_PerIngressShape(t *testing.T) {
	h := &Handler{gate: &admissionGate{max: 0}}

	cases := []struct {
		name   string
		format provcore.Format
		marker string
		absent string
	}{
		// Both shapes carry rate_limit semantics; they differ in envelope:
		// OpenAI = {"error":{...,"code":"gateway_overloaded"}}, Anthropic =
		// {"type":"error","error":{"type":"rate_limit_error",...}}.
		{"openai-shape", provcore.FormatOpenAI, `"code":"gateway_overloaded"`, `"type":"error"`},
		{"anthropic-shape", provcore.FormatAnthropic, `"type":"error"`, `"gateway_overloaded"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := testutil.ToFloat64(admissionShedTotal)
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/x", strings.NewReader(`{}`))
			h.ServeProxy(Ingress{WireShape: typology.WireShapeOpenAIChat, BodyFormat: tc.format})(w, req)

			if w.Code != http.StatusTooManyRequests {
				t.Fatalf("status=%d want 429", w.Code)
			}
			if w.Header().Get("Retry-After") != "1" {
				t.Fatal("Retry-After missing on shed response")
			}
			if body := w.Body.String(); !strings.Contains(body, tc.marker) || strings.Contains(body, tc.absent) {
				t.Fatalf("shed body not in the caller's ingress shape: %s", body)
			}
			if after := testutil.ToFloat64(admissionShedTotal); after != before+1 {
				t.Fatalf("shed counter did not increment: %v -> %v", before, after)
			}
		})
	}
}

// zeroWriteModifyHook mimics a webhook-forward style hook: Decision=Modify
// with NO ModifiedContent and NO TransformSpans — the aggregate carries a
// redaction demand but the adapter rewrite performs zero writes and returns
// the input slice unchanged.
type zeroWriteModifyHook struct {
	goHooks.AnyEndpointAnyModality
}

func (zeroWriteModifyHook) Execute(_ context.Context, _ *goHooks.HookInput) (*goHooks.HookResult, error) {
	return &goHooks.HookResult{Decision: goHooks.Modify}, nil
}

// newZeroWriteModifyRequestHookCache serves one request-stage hook whose
// Modify carries no content — driving the zero-write rewrite branch.
func newZeroWriteModifyRequestHookCache(t *testing.T) *compliance.HookConfigCache {
	t.Helper()
	reg := builtins.Registry.Clone()
	reg.Register("zero-write-modify", func(_ *goHooks.HookConfig) (goHooks.Hook, error) {
		return zeroWriteModifyHook{}, nil
	})
	reg.Freeze()
	loader := func(_ context.Context) ([]goHooks.HookConfig, error) {
		return []goHooks.HookConfig{{
			ID: "zw-1", ImplementationID: "zero-write-modify", Name: "zw",
			Priority: 1, Enabled: true, Stage: "request", FailBehavior: "fail-closed",
			TimeoutMs: 1000, ApplicableIngress: []string{"ALL"},
			Config: map[string]any{},
		}}, nil
	}
	hc := compliance.NewHookConfigCache(loader, reg, 0, slog.Default())
	if err := hc.Start(context.Background()); err != nil {
		t.Fatalf("hookCache.Start: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	return hc
}

// TestRunRequestHooks_ZeroWriteModify_RedactedCopyNotAliased pins the
// review's MAJOR: when a Modify decision produces ZERO rewrites, the
// adapter returns the input slice — which is the pooled request buffer that
// finalizeAudit releases for reuse under bodies-off. The stamped
// rec.RequestBodyRedacted must NOT share that backing array: after the
// original buffer is overwritten (simulating the next request's
// AcquireRequestBody copy), the record's redacted copy must be unchanged.
func TestRunRequestHooks_ZeroWriteModify_RedactedCopyNotAliased(t *testing.T) {
	h := &Handler{deps: &Deps{
		HookConfigCache: newZeroWriteModifyRequestHookCache(t),
		TrafficAdapter:  &openai.Adapter{},
		Logger:          slog.Default(),
	}}

	src := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"tenant-A-secret"}]}`)
	body, handle := audit.AcquireRequestBody(src)
	if handle == nil {
		t.Fatal("expected pooled handle")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(src)))
	w := httptest.NewRecorder()
	rec := &audit.Record{RequestID: "req-alias"}

	rewritten, _, rejected := h.runRequestHooks(req, w, rec, "req-alias", body, routingcore.RoutingTarget{}, openAIIngress, nil, slog.Default())
	if rejected {
		t.Fatalf("zero-write modify must not reject; response=%s", w.Body.String())
	}
	if len(rec.RequestBodyRedacted) == 0 {
		t.Fatal("precondition: zero-write modify must stamp RequestBodyRedacted")
	}
	want := string(rec.RequestBodyRedacted)

	// Simulate the released buffer being recycled by the NEXT request.
	for i := range body {
		body[i] = 'X'
	}
	if got := string(rec.RequestBodyRedacted); got != want {
		t.Fatalf("RequestBodyRedacted aliases the pooled buffer — next request's bytes bled into this record's redacted copy:\n got=%q\nwant=%q", got, want)
	}
	_ = rewritten
	audit.ReleaseRequestBuffer(handle)
}

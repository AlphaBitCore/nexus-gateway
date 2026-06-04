package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

// TestBuildProviderRequest_ForwardsClientHeaders pins the wiring that
// lets per-format provider beta headers (anthropic-beta, openai-beta,
// x-goog-user-project, ...) reach the upstream. The spec adapter
// applies its own allowlist on top, so the proxy layer's contract is
// just "forward inbound headers verbatim".
//
// Regression for the context_management beta-header drop: pre-fix,
// fetchUpstream called provcore.Request{} without Headers, so even
// an `anthropic-beta` correctly stamped by the spec_anthropic
// PerFormatForwardHeaders allowlist had nothing to forward, and
// Anthropic rejected the request with "Extra inputs are not permitted".
func TestBuildProviderRequest_ForwardsClientHeaders(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	r.Header.Set("Anthropic-Beta", "context-management-2025-04-15")
	r.Header.Set("Anthropic-Version", "2023-06-01")
	r.Header.Set("Authorization", "Bearer should-be-stripped-by-adapter")

	in := Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatAnthropic,
	}
	got := buildProviderRequest(r, in, []byte(`{"model":"claude-x"}`), false, 1<<20)

	if got.Headers.Get("Anthropic-Beta") != "context-management-2025-04-15" {
		t.Errorf("Anthropic-Beta: want forwarded, got %q", got.Headers.Get("Anthropic-Beta"))
	}
	if got.Headers.Get("Anthropic-Version") != "2023-06-01" {
		t.Errorf("Anthropic-Version: want forwarded, got %q", got.Headers.Get("Anthropic-Version"))
	}
	// Authorization passes through here — the allowlist filter that
	// strips it lives one layer down in spec_adapter.forwardHeaders.
	// What this test guarantees is that the proxy layer does NOT silently
	// drop ANY header before the adapter has a chance to apply its own
	// per-format rules.
	if got.Headers.Get("Authorization") != "Bearer should-be-stripped-by-adapter" {
		t.Errorf("Authorization: want forwarded to adapter for filtering, got %q",
			got.Headers.Get("Authorization"))
	}
	if got.MaxResponseBytes != 1<<20 {
		t.Errorf("MaxResponseBytes: want 1MiB, got %d", got.MaxResponseBytes)
	}
	if got.BodyFormat != provcore.FormatAnthropic {
		t.Errorf("BodyFormat: want anthropic, got %s", got.BodyFormat)
	}
}

// TestBuildProviderRequest_NilRequest guards against a nil http.Request
// so callers can construct a Request defensively (e.g. internal
// retries) without panicking.
func TestBuildProviderRequest_NilRequest(t *testing.T) {
	in := Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	}
	got := buildProviderRequest(nil, in, nil, false, 0)
	if got.Headers != nil {
		t.Errorf("Headers from nil request: want nil, got %v", got.Headers)
	}
}

func TestOpenAIProxyErrorBody(t *testing.T) {
	// Empty code → numeric status as code (legacy writeJSONError shape).
	b := openAIProxyErrorBody(400, "", "bad request", "")
	if gjson.GetBytes(b, "error.message").String() != "bad request" ||
		gjson.GetBytes(b, "error.type").String() != "proxy_error" ||
		gjson.GetBytes(b, "error.code").Int() != 400 {
		t.Errorf("openai proxy error body wrong: %s", b)
	}
	// String code + hint (legacy writeDetailedError shape).
	b = openAIProxyErrorBody(429, "rate_limited", "slow down", "retry later")
	if gjson.GetBytes(b, "error.code").String() != "rate_limited" ||
		gjson.GetBytes(b, "error.hint").String() != "retry later" {
		t.Errorf("detailed proxy error body wrong: %s", b)
	}
}

// TestWriteIngressError_RecordsBody_AndShapesPerIngress locks the maintainer
// requirements: (1) a gateway error is ALWAYS stamped to rec.ResponseBody so it
// lands in traffic_event.payloads.response_body (previously only error_code /
// error_reason were recorded, body was empty); (2) the error envelope is in the
// caller's ingress wire shape (anthropic → not the OpenAI proxy_error shape;
// openai → proxy_error shape).
func TestWriteIngressError_RecordsBody_AndShapesPerIngress(t *testing.T) {
	h := NewHandler(&Deps{})

	t.Run("anthropic ingress → ingress-shaped + recorded", func(t *testing.T) {
		rec := &audit.Record{IngressFormat: string(provcore.FormatAnthropic)}
		w := &testResponseWriter{}
		h.writeIngressError(w, rec, http.StatusBadGateway, "upstream_error", "boom", "")
		if len(rec.ResponseBody) == 0 {
			t.Fatal("error MUST be recorded to rec.ResponseBody; got empty")
		}
		if !bytes.Equal(w.body, rec.ResponseBody) {
			t.Errorf("written body must match recorded rec.ResponseBody")
		}
		if w.status != http.StatusBadGateway {
			t.Errorf("status = %d", w.status)
		}
		if gjson.GetBytes(rec.ResponseBody, "error.type").String() == "proxy_error" {
			t.Errorf("anthropic ingress must NOT get the OpenAI proxy_error shape; got %s", rec.ResponseBody)
		}
	})

	t.Run("openai ingress → proxy_error shape + recorded", func(t *testing.T) {
		rec := &audit.Record{IngressFormat: string(provcore.FormatOpenAI)}
		w := &testResponseWriter{}
		h.writeIngressError(w, rec, http.StatusTooManyRequests, "rate_limited", "slow down", "")
		if len(rec.ResponseBody) == 0 {
			t.Fatal("error MUST be recorded to rec.ResponseBody; got empty")
		}
		if gjson.GetBytes(rec.ResponseBody, "error.type").String() != "proxy_error" {
			t.Errorf("openai ingress → proxy_error shape; got %s", rec.ResponseBody)
		}
		if rec.ErrorCode != "rate_limited" || rec.ErrorReason != "slow down" {
			t.Errorf("rec error fields not stamped: code=%q reason=%q", rec.ErrorCode, rec.ErrorReason)
		}
	})
}

type testResponseWriter struct {
	header http.Header
	status int
	body   []byte
}

func (w *testResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *testResponseWriter) Write(b []byte) (int, error) {
	w.body = append(w.body, b...)
	return len(b), nil
}

func (w *testResponseWriter) WriteHeader(status int) {
	w.status = status
}

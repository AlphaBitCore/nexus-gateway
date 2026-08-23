package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

type signedGeminiRewriteAdapter struct {
	stubTrafficAdapter
	rewritten []byte
}

func (a *signedGeminiRewriteAdapter) RewriteResponseBody(_ context.Context, _ []byte, _ string, _ traffic.NormalizedContent) ([]byte, int, error) {
	return append([]byte(nil), a.rewritten...), 1, nil
}

func TestRedactCanonicalBuffer_SignedGeminiCallInvariant(t *testing.T) {
	const signedBody = `{"id":"resp-1","choices":[{"index":0,"message":{"role":"assistant","content":"done","tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"secret\"}","thought_signature":"sig-1"}}]},"finish_reason":"tool_calls"}]}`
	const modifiedBody = `{"id":"resp-1","choices":[{"index":0,"message":{"role":"assistant","content":"done","tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"changed\"}","thought_signature":"sig-1"}}]},"finish_reason":"tool_calls"}]}`
	const deletedBody = `{"id":"resp-1","choices":[{"index":0,"message":{"role":"assistant","content":"done","tool_calls":[]},"finish_reason":"tool_calls"}]}`
	tests := []struct {
		name       string
		before     string
		after      string
		failClosed bool
	}{
		{name: "unchanged signed call passes", before: signedBody, after: signedBody},
		{name: "modified signed call rejects", before: signedBody, after: modifiedBody, failClosed: true},
		{name: "deleted signed call rejects", before: signedBody, after: deletedBody, failClosed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adapter := &signedGeminiRewriteAdapter{stubTrafficAdapter: stubTrafficAdapter{id: "signed-gemini-test"}, rewritten: []byte(tt.after)}
			h := &Handler{deps: &Deps{HookConfigCache: newResponseHookCache(t, responseModifyHook{}), TrafficAdapter: adapter, Logger: noopLogger()}}
			ingress := Ingress{WireShape: typology.WireShapeOpenAIChat, BodyFormat: provcore.FormatOpenAI}
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			req = req.WithContext(WithIngress(req.Context(), ingress))
			out := h.redactCanonicalBuffer(req, &audit.Record{RequestID: "signed-gemini-test"}, ingress, routingcore.RoutingTarget{ModelCode: "gemini-2.5-pro", Region: "us-central1"}, []byte(tt.before), 0, "signed-gemini-test", noopLogger())
			if out.failClosed != tt.failClosed {
				t.Fatalf("failClosed=%v want %v status=%d msg=%q", out.failClosed, tt.failClosed, out.errStatus, out.errMsg)
			}
			if tt.failClosed && (out.errStatus != http.StatusInternalServerError || out.errMsg != "response rewrite failed") {
				t.Fatalf("fail-closed result status=%d msg=%q", out.errStatus, out.errMsg)
			}
		})
	}
}

func TestSignedGeminiRequestToolCallInvariant(t *testing.T) {
	const signed = `{"model":"m","messages":[{"role":"assistant","tool_calls":[` +
		`{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"secret\"}","thought_signature":"sig-1"}},` +
		`{"id":"call-2","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"other\"}","thought_signature":"sig-2"}}]}]}`
	const modifiedArgs = `{"model":"m","messages":[{"role":"assistant","tool_calls":[` +
		`{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"masked\"}","thought_signature":"sig-1"}},` +
		`{"id":"call-2","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"other\"}","thought_signature":"sig-2"}}]}]}`
	const reordered = `{"model":"m","messages":[{"role":"assistant","tool_calls":[` +
		`{"id":"call-2","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"other\"}","thought_signature":"sig-2"}},` +
		`{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"secret\"}","thought_signature":"sig-1"}}]}]}`
	if !signedGeminiRequestToolCallsPreserved([]byte(signed), []byte(signed)) {
		t.Fatal("unchanged signed request call was rejected")
	}
	if signedGeminiRequestToolCallsPreserved([]byte(signed), []byte(modifiedArgs)) {
		t.Fatal("signed request argument mutation was accepted")
	}
	if signedGeminiRequestToolCallsPreserved([]byte(signed), []byte(reordered)) {
		t.Fatal("signed request call reorder was accepted")
	}
}

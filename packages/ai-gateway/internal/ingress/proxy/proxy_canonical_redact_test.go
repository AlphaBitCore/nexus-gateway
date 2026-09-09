package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	goHooks "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/openai"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// toolArgMaskHook masks the first tool-call argument leaf, which is what a real
// PII hook does to a tool call carrying an address or an email.
type toolArgMaskHook struct {
	goHooks.AnyEndpointAnyModality
}

func (toolArgMaskHook) Execute(_ context.Context, in *goHooks.HookInput) (*goHooks.HookResult, error) {
	res := &goHooks.HookResult{Decision: goHooks.Modify}
	if in == nil || in.Normalized == nil {
		return res, nil
	}
	for mi, m := range in.Normalized.Messages {
		for bi, b := range m.Content {
			if b.Type != normalize.ContentToolUse || b.ToolUse == nil {
				continue
			}
			for _, leaf := range normalize.ToolUseStringLeaves(b.ToolUse.Input) {
				res.TransformSpans = append(res.TransformSpans, normalize.TransformSpan{
					Source:         normalize.SourceHook,
					SourceID:       "tool-arg-mask",
					Action:         normalize.ActionRedact,
					ContentAddress: fmt.Sprintf("messages.%d.content.%d.toolUse.input.%d", mi, bi, leaf.Ordinal),
					Start:          0,
					End:            len(leaf.Value),
					Replacement:    "[MASKED]",
				})
				return res, nil
			}
		}
	}
	return res, nil
}

// TestRedactCanonicalBuffer_SignedGeminiCallInvariant drives the invariant
// through the REAL redaction path: a hook masks a tool-call argument, the codec
// writes the masked arguments back, and the signature that was computed over the
// ORIGINAL arguments is now stale — so the response must not be delivered.
//
// The previous version of this test stubbed the traffic adapter's
// RewriteResponseBody to return an arbitrary body. That seam is no longer on the
// redaction path (the codec owns the write-back now), so the stub proved
// nothing: it would have passed with the guard deleted.
func TestRedactCanonicalBuffer_SignedGeminiCallInvariant(t *testing.T) {
	// A real response carries model and usage; the canonical codec treats them as
	// required, and a body without them decodes to nothing — which is its own
	// defect (the hook then sees no content at all) tracked separately.
	const signedBody = `{"id":"resp-1","object":"chat.completion","model":"gemini-2.5-pro",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"done","tool_calls":[` +
		`{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"secret\"}","thought_signature":"sig-1"}}]},` +
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

	h := &Handler{deps: &Deps{
		NormalizeRegistry: canonicalRegistry(),
		HookConfigCache:   newResponseHookCache(t, toolArgMaskHook{}),
		TrafficAdapter:    &openai.Adapter{},
		Logger:            noopLogger(),
	}}
	ingress := Ingress{WireShape: typology.WireShapeOpenAIChat, BodyFormat: provcore.FormatOpenAI}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req = req.WithContext(WithIngress(req.Context(), ingress))

	out := h.redactCanonicalBuffer(req, &audit.Record{RequestID: "signed-gemini-test"}, ingress,
		routingcore.RoutingTarget{ModelCode: "gemini-2.5-pro", Region: "us-central1"},
		[]byte(signedBody), 0, "signed-gemini-test", noopLogger())

	if !out.failClosed {
		t.Fatalf("masking an argument of a SIGNED tool call was delivered (body=%s). The signature "+
			"was computed over the original arguments, so forwarding the masked call ships a "+
			"signature that does not match what it signs.", string(out.body))
	}
	if out.errStatus != http.StatusInternalServerError || out.errMsg != "response rewrite failed" {
		t.Fatalf("fail-closed result status=%d msg=%q", out.errStatus, out.errMsg)
	}
}

// TestSignedGeminiToolCallsPreserved covers the predicate itself, including the
// shapes the redaction path cannot currently produce (a deleted call), because
// the guard is defence in depth and should keep holding if a future path can.
func TestSignedGeminiToolCallsPreserved(t *testing.T) {
	const signed = `{"choices":[{"index":0,"message":{"tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"secret\"}","thought_signature":"sig-1"}}]}}]}`
	const argsChanged = `{"choices":[{"index":0,"message":{"tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"changed\"}","thought_signature":"sig-1"}}]}}]}`
	const deleted = `{"choices":[{"index":0,"message":{"tool_calls":[]}}]}`
	const sigChanged = `{"choices":[{"index":0,"message":{"tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"secret\"}","thought_signature":"sig-2"}}]}}]}`

	for _, tt := range []struct {
		name      string
		after     string
		preserved bool
	}{
		{"unchanged", signed, true},
		{"arguments changed", argsChanged, false},
		{"call deleted", deleted, false},
		{"signature swapped", sigChanged, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := signedGeminiToolCallsPreserved([]byte(signed), []byte(tt.after)); got != tt.preserved {
				t.Errorf("preserved=%v want %v", got, tt.preserved)
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

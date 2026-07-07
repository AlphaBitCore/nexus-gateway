// proxy_egress_shape_test.go — egress shape-in=shape-out regression tests for
// the canonical-hub round-trip invariant (provider-adapter-architecture.md §3):
//
//	response: target shape B → canonical(OpenAI) → ingress shape A   (B→canonical→A)
//
// The live response body is ALWAYS canonical at egress (specAdapter.Execute
// returns SchemaCodec.DecodeResponse's CanonicalBody), so egressReshapeNonStream
// must re-encode it to the caller's ingress shape A keyed on the INGRESS — never
// on ingress-vs-target. The bug these tests lock down: a native non-OpenAI
// ingress (anthropic /v1/messages, gemini /v1beta) silently received canonical
// OpenAI (`choices[]`) instead of `content[]` / `candidates[]`.
package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/executor"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// canonical chat.completion body — what specAdapter.Execute hands to the egress
// on every path (the upstream B-shape has already been decoded to canonical).
const egressCanonicalBody = `{"id":"chatcmpl_x","object":"chat.completion",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

func egressTarget(adapter string) routingcore.RoutingTarget {
	return routingcore.RoutingTarget{AdapterType: adapter, ModelCode: "m", ModelID: "m"}
}

// TestEgressReshapeNonStream_RoutesByIngressShape pins that the reshape is
// driven by the ingress shape A, identity for OpenAI-family, with the
// /v1/responses native-passthrough exception — independent of the target B.
// responses-shape upstream body ({"object":"response"}) — what a genuine
// /v1/responses upstream returns; the content classifier forwards it verbatim.
const egressResponsesBody = `{"object":"response","id":"resp_x","output":[]}`

func TestEgressReshapeNonStream_RoutesByIngressShape(t *testing.T) {
	cases := []struct {
		name         string
		wire         typology.WireShape
		body         provcore.Format
		inBody       string // upstream body; "" → egressCanonicalBody (chat.completion)
		target       routingcore.RoutingTarget
		wantReshaped bool // expect ResponseCanonicalToIngress invoked
		wantFormat   provcore.Format
		wantVerbatim string // when not reshaped, the exact body the caller must receive
	}{
		// OpenAI-family ingress: canonical already IS the caller's shape → identity, no call.
		{"openai-chat identity", typology.WireShapeOpenAIChat, provcore.FormatOpenAI, "", egressTarget("anthropic"), false, "", egressCanonicalBody},
		// Anthropic /v1/messages → MUST reshape to anthropic, for BOTH native and cross targets.
		{"anthropic native", typology.WireShapeAnthropicMessages, provcore.FormatAnthropic, "", egressTarget("anthropic"), true, provcore.FormatAnthropic, ""},
		{"anthropic cross→openai", typology.WireShapeAnthropicMessages, provcore.FormatAnthropic, "", egressTarget("openai"), true, provcore.FormatAnthropic, ""},
		// Gemini /v1beta → MUST reshape to gemini, native and cross.
		{"gemini native", typology.WireShapeGeminiGenerateContent, provcore.FormatGemini, "", egressTarget("gemini"), true, provcore.FormatGemini, ""},
		{"gemini cross→openai", typology.WireShapeGeminiGenerateContent, provcore.FormatGemini, "", egressTarget("openai"), true, provcore.FormatGemini, ""},
		// /v1/responses content-driven: a Responses-shape body is verbatim (zero-loss
		// for built-in tools), independent of the target Format.
		{"responses body verbatim (openai target)", typology.WireShapeOpenAIResponses, provcore.FormatOpenAIResponses, egressResponsesBody, egressTarget("openai"), false, "", egressResponsesBody},
		// A chat.completion body on a /v1/responses ingress is re-encoded to the
		// Responses output[] grammar — even when the target is the "native" openai
		// adapter (bulletproof: a chat-shaped reply never leaks to a Responses client).
		{"responses chat-upstream re-encode (openai target)", typology.WireShapeOpenAIResponses, provcore.FormatOpenAIResponses, "", egressTarget("openai"), true, provcore.FormatOpenAIResponses, ""},
		{"responses cross→anthropic", typology.WireShapeOpenAIResponses, provcore.FormatOpenAIResponses, "", egressTarget("anthropic"), true, provcore.FormatOpenAIResponses, ""},
		// Non-OpenAI embeddings ingress → embeddings reshape path (chat spy NOT
		// invoked; the embeddings re-encoder handles canonical→ingress).
		{"gemini embeddings", typology.WireShapeGeminiEmbedContent, provcore.FormatGemini, "", egressTarget("openai"), false, "", egressCanonicalBody},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var called bool
			var gotFormat provcore.Format
			fb := &fakeBridge{
				responseCanonicalToIngress: func(ingress provcore.Format, canonical []byte) ([]byte, error) {
					called = true
					gotFormat = ingress
					return []byte(`{"reshaped_to":"` + string(ingress) + `"}`), nil
				},
			}
			inBody := tc.inBody
			if inBody == "" {
				inBody = egressCanonicalBody
			}
			h := NewHandler(&Deps{CanonicalBridge: fb})
			out, err := h.egressReshapeNonStream(
				Ingress{WireShape: tc.wire, BodyFormat: tc.body},
				tc.target, []byte(inBody))
			if err != nil {
				t.Fatalf("egressReshapeNonStream: %v", err)
			}
			if called != tc.wantReshaped {
				t.Fatalf("reshape invoked=%v want=%v (ingress=%s target=%s)", called, tc.wantReshaped, tc.wire, tc.target.AdapterType)
			}
			if tc.wantReshaped {
				if gotFormat != tc.wantFormat {
					t.Fatalf("reshape called with ingress format %q want %q", gotFormat, tc.wantFormat)
				}
				if !bytes.Contains(out, []byte(`"reshaped_to":"`+string(tc.wantFormat)+`"`)) {
					t.Fatalf("reshaped output not returned to caller; got %s", out)
				}
			} else if !bytes.Equal(out, []byte(tc.wantVerbatim)) {
				t.Fatalf("identity/verbatim path must return the body unchanged; got %s want %s", out, tc.wantVerbatim)
			}
		})
	}
}

// TestEgressReshapeNonStream_FailsClosedOnUnclassifiable proves the content
// classifier refuses to forward an unclassifiable /v1/responses upstream body
// verbatim — it returns an error so the handler can surface a 502 rather than
// leaking a garbage / wrong-shape payload to a Responses client.
func TestEgressReshapeNonStream_FailsClosedOnUnclassifiable(t *testing.T) {
	fb := &fakeBridge{responseCanonicalToIngress: func(provcore.Format, []byte) ([]byte, error) {
		t.Fatal("reshape must not be invoked on an unclassifiable body")
		return nil, nil
	}}
	h := NewHandler(&Deps{CanonicalBridge: fb})
	out, err := h.egressReshapeNonStream(
		Ingress{WireShape: typology.WireShapeOpenAIResponses, BodyFormat: provcore.FormatOpenAIResponses},
		egressTarget("openai"), []byte(`{"unexpected":"garbage"}`))
	if err == nil {
		t.Fatalf("unclassifiable body must fail closed; got out=%s", out)
	}
}

// TestEgressReshapeNonStream_Guards covers the early-return guards: an empty
// body and a nil bridge both pass the body through untouched without invoking
// any reshape.
func TestEgressReshapeNonStream_Guards(t *testing.T) {
	t.Run("empty body", func(t *testing.T) {
		called := false
		fb := &fakeBridge{responseCanonicalToIngress: func(provcore.Format, []byte) ([]byte, error) {
			called = true
			return nil, nil
		}}
		out, err := NewHandler(&Deps{CanonicalBridge: fb}).egressReshapeNonStream(
			Ingress{WireShape: typology.WireShapeAnthropicMessages, BodyFormat: provcore.FormatAnthropic},
			egressTarget("openai"), nil)
		if err != nil || called || out != nil {
			t.Fatalf("empty body must short-circuit: out=%v called=%v err=%v", out, called, err)
		}
	})
	t.Run("nil bridge", func(t *testing.T) {
		out, err := NewHandler(&Deps{}).egressReshapeNonStream(
			Ingress{WireShape: typology.WireShapeAnthropicMessages, BodyFormat: provcore.FormatAnthropic},
			egressTarget("openai"), []byte(egressCanonicalBody))
		if err != nil || !bytes.Equal(out, []byte(egressCanonicalBody)) {
			t.Fatalf("nil bridge must pass body through: out=%s err=%v", out, err)
		}
	})
}

// TestServeProxy_Direct_NonOpenAIIngress_ReshapesToIngress drives the full
// non-stream direct path and proves the egress reshape is wired: an anthropic
// /v1/messages request whose upstream (canonical) response is gpt-4o-shaped must
// reach the client re-encoded to the anthropic ingress, not as canonical OpenAI.
func TestServeProxy_Direct_NonOpenAIIngress_ReshapesToIngress(t *testing.T) {
	for _, tc := range []struct {
		name    string
		wire    typology.WireShape
		body    provcore.Format
		path    string
		reqBody string
	}{
		{"anthropic /v1/messages", typology.WireShapeAnthropicMessages, provcore.FormatAnthropic,
			"/v1/messages", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`},
		{"gemini /v1beta", typology.WireShapeGeminiGenerateContent, provcore.FormatGemini,
			"/v1beta/models/m:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fexec := &fakeExecutor{Result: &executor.ExecutionResult{
				StatusCode: http.StatusOK,
				Body:       []byte(egressCanonicalBody),
				Target:     egressTarget("openai"),
			}}
			var gotIngress provcore.Format
			fb := &fakeBridge{
				responseCanonicalToIngress: func(ingress provcore.Format, _ []byte) ([]byte, error) {
					gotIngress = ingress
					return []byte(`{"egress_shape":"` + string(ingress) + `"}`), nil
				},
			}
			deps := makeFakeDeps(t, fexec, fb)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, tc.path, bytes.NewReader([]byte(tc.reqBody)))
			if tc.wire == typology.WireShapeGeminiGenerateContent {
				// gemini reads the model from the ServeMux {model} path value.
				req.SetPathValue("model", "m:generateContent")
			}
			NewHandler(deps).ServeProxy(Ingress{WireShape: tc.wire, BodyFormat: tc.body}).ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if gotIngress != tc.body {
				t.Fatalf("egress reshape invoked with %q want ingress %q", gotIngress, tc.body)
			}
			if !bytes.Contains(rec.Body.Bytes(), []byte(`"egress_shape":"`+string(tc.body)+`"`)) {
				t.Fatalf("client did not receive ingress-shaped body; got %s", rec.Body.String())
			}
			if bytes.Contains(rec.Body.Bytes(), []byte(`"object":"chat.completion"`)) {
				t.Fatalf("canonical OpenAI envelope leaked to a %s caller: %s", tc.wire, rec.Body.String())
			}
		})
	}
}

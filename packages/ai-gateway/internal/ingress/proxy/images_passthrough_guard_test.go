package proxy

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/executor"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

func openaiImageTargets() []routingcore.RoutingTarget {
	return []routingcore.RoutingTarget{{
		ProviderID: "p-oai", ProviderName: "openai", ProviderModelID: "gpt-image-1-mini",
		ModelID: "gpt-image-1-mini", ModelName: "GPT Image Mini", ModelCode: "gpt-image-1-mini",
		AdapterType: "openai",
	}}
}

// The spend guard has to run on the leg where NO translation happens.
//
// An image request whose target speaks the ingress wire skips canonicalization
// entirely, which is where the `n` ceiling lived — so a native OpenAI request
// reached the upstream ungated and n up to 10 billed for ten images. Measured
// against production before the fix: n=11 came back as OpenAI's own "Expected a
// value <= 10", the upstream refusing rather than us.
//
// This asserts through ServeProxy rather than by calling the guard, because the
// guard was never the broken part — the wiring was. A test of the guard alone
// stays green with the call site deleted.
func TestServeProxy_ImagesPassthrough_RunsTheSpendGuardBeforeDispatch(t *testing.T) {
	var sawIngress provcore.Format
	var sawBody []byte
	fexec := &fakeExecutor{Result: &executor.ExecutionResult{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       []byte(`{"created":1,"data":[{"b64_json":"QUFB"}]}`),
		Target:     openaiImageTargets()[0],
		Attempts:   []executor.Attempt{{StatusCode: http.StatusOK}},
	}}
	bridge := &fakeBridge{validateImagesIngressGuards: func(ingress provcore.Format, body []byte, _ provcore.CallTarget) error {
		sawIngress, sawBody = ingress, body
		return errors.New(`field "n" must be an integer in [1, 4] for the resolved image provider openai (gpt-image-1-mini)`)
	}}
	deps := makeFakeDeps(t, fexec, bridge)
	deps.Router = &stubRouterCacheTest{targets: openaiImageTargets()}
	h := NewHandler(deps)

	r := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"gpt-image-1-mini","prompt":"a fox","n":5}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer vk")
	w := httptest.NewRecorder()
	h.ServeProxy(Ingress{WireShape: typology.WireShapeOpenAIImages, BodyFormat: provcore.FormatOpenAI})(w, r)

	if sawBody == nil {
		t.Fatal("the spend guard was never called on the passthrough leg — n reaches the upstream unbounded (status " +
			http.StatusText(w.Code) + ", body " + w.Body.String() + ")")
	}
	if sawIngress != provcore.FormatOpenAI {
		t.Errorf("guard saw ingress %q, want openai", sawIngress)
	}
	if got := gjson.GetBytes(sawBody, "n").Int(); got != 5 {
		t.Errorf("guard saw n=%d, want the caller's 5", got)
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body)
	}
	if fexec.Calls > 0 {
		t.Error("the upstream was dispatched anyway; a spend guard that refuses after the bill is not a guard")
	}
	if got := gjson.GetBytes(w.Body.Bytes(), "error.message").String(); !strings.Contains(got, `"n"`) {
		t.Errorf("error.message = %q, want it to name the field", got)
	}
}

// The rerank passthrough guard has the same wiring and had no test that could
// see it either: its gate calls the guard function directly, so deleting the
// call site in the body-prep stage leaves it green. Same assertion, same reason.
func TestServeProxy_RerankPassthrough_RunsTheDocumentGuardBeforeDispatch(t *testing.T) {
	var sawBody []byte
	cohere := []routingcore.RoutingTarget{{
		ProviderID: "p-co", ProviderName: "cohere", ProviderModelID: "rerank-v3.5",
		ModelID: "rerank-v3.5", ModelName: "Rerank", ModelCode: "rerank-v3.5",
		AdapterType: "cohere",
	}}
	fexec := &fakeExecutor{Result: &executor.ExecutionResult{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       []byte(`{"results":[]}`),
		Target:     cohere[0],
		Attempts:   []executor.Attempt{{StatusCode: http.StatusOK}},
	}}
	bridge := &fakeBridge{validateRerankIngressGuards: func(_ provcore.Format, body []byte, _ provcore.CallTarget) error {
		sawBody = body
		return errors.New(`field "documents" must have 1..1000 entries`)
	}}
	deps := makeFakeDeps(t, fexec, bridge)
	deps.Router = &stubRouterCacheTest{targets: cohere}
	h := NewHandler(deps)

	r := httptest.NewRequest(http.MethodPost, "/v1/rerank",
		strings.NewReader(`{"model":"rerank-v3.5","query":"q","documents":["a","b"]}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer vk")
	w := httptest.NewRecorder()
	h.ServeProxy(Ingress{WireShape: typology.WireShapeCohereRerank, BodyFormat: provcore.FormatCohere})(w, r)

	if sawBody == nil {
		t.Fatalf("the document guard was never called on the passthrough leg (status %d, body %s)", w.Code, w.Body)
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (body %s)", w.Code, w.Body)
	}
	if fexec.Calls > 0 {
		t.Error("the upstream was dispatched anyway")
	}
}

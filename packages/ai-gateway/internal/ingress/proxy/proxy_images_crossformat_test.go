package proxy

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/executor"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

func geminiImageTargets() []routingcore.RoutingTarget {
	return []routingcore.RoutingTarget{{
		ProviderID: "p-gem", ProviderName: "google-gemini", ProviderModelID: "gemini-2.5-flash-image",
		ModelID: "gemini-2.5-flash-image", ModelName: "Nano Banana", ModelCode: "gemini-2.5-flash-image",
		AdapterType: "gemini",
	}}
}

// TestServeProxy_Fake_Images_CrossFormat_Gemini drives dispatch site 1
// of the cross-shape image contract: the pre-cache body-prep stage must
// take the IMAGES canonicalization arm for a cross-format image request
// (Gemini primary) — validating via IngressImagesToCanonical and
// resolving the target wire shape — instead of falling through to the
// chat arm or handing the raw openai-images shape to the Gemini codec
// (which would 400 the request before the executor ever ran).
func TestServeProxy_Fake_Images_CrossFormat_Gemini(t *testing.T) {
	imgBody := []byte(`{"created":1752555600,"data":[{"b64_json":"QUFB"}]}`)
	fexec := &fakeExecutor{Result: &executor.ExecutionResult{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       imgBody,
		Target:     routingcore.RoutingTarget{ProviderID: "p-gem", ProviderName: "google-gemini", ModelID: "gemini-2.5-flash-image", ModelCode: "gemini-2.5-flash-image", AdapterType: "gemini"},
		Attempts:   []executor.Attempt{{StatusCode: http.StatusOK}},
	}}
	imagesCanonCalls := 0
	fb := &fakeBridge{
		ingressImagesToCanonical: func(_ provcore.Format, body []byte, _ provcore.CallTarget) ([]byte, error) {
			imagesCanonCalls++
			return body, nil
		},
		// The chat arm must NOT fire for an images ingress — fail loudly.
		ingressChatToCanonical: func(_ provcore.Format, _ []byte, _ provcore.CallTarget) ([]byte, error) {
			return nil, errors.New("chat canonicalization must not run for an images ingress")
		},
	}
	deps := makeFakeDeps(t, fexec, fb)
	// Enable the response cache so the pre-cache body-prep runs — for the
	// image kind it runs on the modality cache-SKIP lane (the provider
	// cache must still be prepared).
	cacheOpt, cleanup := withCache(t)
	defer cleanup()
	cacheOpt(deps)
	deps.Router = &stubRouterCacheTest{targets: geminiImageTargets()}

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIImages,
		BodyFormat: provcore.FormatOpenAI,
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gemini-2.5-flash-image","prompt":"a red fox"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer vk")
	w := httptest.NewRecorder()
	h(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", w.Code, w.Body.String())
	}
	if imagesCanonCalls == 0 {
		t.Fatal("IngressImagesToCanonical never ran — the prepare stage skipped the images arm")
	}
	if !strings.Contains(w.Body.String(), "b64_json") {
		t.Fatalf("caller must receive the canonical images response, got %s", w.Body.String())
	}
}

// TestServeProxy_Fake_Images_NoCompatibleProvider_NamesKind asserts the
// routing-filter 400 names the ENDPOINT KIND: routability is per-kind
// (a provider that serves chat fine may have no image leg), so a
// kind-less message reads as a blanket provider incompatibility and
// sends an admin who configured an image model on an unopened leg down
// the wrong debugging path.
func TestServeProxy_Fake_Images_NoCompatibleProvider_NamesKind(t *testing.T) {
	fexec := &fakeExecutor{Result: &executor.ExecutionResult{StatusCode: http.StatusOK}}
	fb := &fakeBridge{
		endpointRoutable: func(_ typology.WireShape, _, _ provcore.Format) bool { return false },
	}
	deps := makeFakeDeps(t, fexec, fb)
	deps.Router = &stubRouterCacheTest{targets: geminiImageTargets()}

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIImages,
		BodyFormat: provcore.FormatOpenAI,
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gemini-2.5-flash-image","prompt":"a fox"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer vk")
	w := httptest.NewRecorder()
	h(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "image_generation") {
		t.Fatalf("no_compatible_provider message must name the endpoint kind, got %s", w.Body.String())
	}
	if fexec.Calls != 0 {
		t.Fatal("upstream must never be dispatched when no compatible target remains")
	}
}

// TestServeProxy_Fake_Images_CanonicalizeRejects_400 asserts the
// closed-set validation failure surfaces as a caller 400 at the prepare
// stage (primary Gemini target) — the upstream is never dispatched.
func TestServeProxy_Fake_Images_CanonicalizeRejects_400(t *testing.T) {
	fexec := &fakeExecutor{Result: &executor.ExecutionResult{StatusCode: http.StatusOK}}
	fb := &fakeBridge{
		ingressImagesToCanonical: func(_ provcore.Format, _ []byte, _ provcore.CallTarget) ([]byte, error) {
			return nil, errors.New(`field "n" must be an integer in [1, 4] for the resolved image provider gemini (gemini-2.5-flash-image)`)
		},
	}
	deps := makeFakeDeps(t, fexec, fb)
	cacheOpt, cleanup := withCache(t)
	defer cleanup()
	cacheOpt(deps)
	deps.Router = &stubRouterCacheTest{targets: geminiImageTargets()}

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIImages,
		BodyFormat: provcore.FormatOpenAI,
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gemini-2.5-flash-image","prompt":"p","n":50}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer vk")
	w := httptest.NewRecorder()
	h(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `\"n\"`) && !strings.Contains(w.Body.String(), `"n"`) {
		t.Fatalf("400 body must name the rejected field, got %s", w.Body.String())
	}
	if fexec.Calls != 0 {
		t.Fatalf("upstream must never be dispatched on a validation 400; executor called %d times", fexec.Calls)
	}
}

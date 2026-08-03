package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/executor"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// imageProbeHandler builds a handler whose router resolves the Gemini image
// target (so an /v1/images/generations request reaches the admission stage's
// generative-cap logic), pre-filling the concurrency counter if fill>0.
func imageProbeHandler(t *testing.T, fill int) (*Handler, http.HandlerFunc) {
	t.Helper()
	fexec := &fakeExecutor{Result: &executor.ExecutionResult{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       []byte(`{"created":1,"data":[{"b64_json":"QUFB"}]}`),
		Target:     routingcore.RoutingTarget{ProviderID: "p-gem", ProviderName: "google-gemini", ModelID: "gemini-2.5-flash-image", AdapterType: "gemini"},
		Attempts:   []executor.Attempt{{StatusCode: http.StatusOK}},
	}}
	deps := makeFakeDeps(t, fexec, &fakeBridge{})
	deps.Router = &stubRouterCacheTest{targets: geminiImageTargets()}
	h := NewHandler(deps)
	// Pre-fill the image concurrency counter for the fake VK ("vk-1", cap 4).
	for i := range fill {
		if !h.genConcurrency.Acquire(typology.EndpointKindImageGeneration, "vk-1", 4) {
			t.Fatalf("pre-fill acquire %d unexpectedly rejected", i)
		}
	}
	return h, h.ServeProxy(Ingress{WireShape: typology.WireShapeOpenAIImages, BodyFormat: provcore.FormatOpenAI})
}

func imageReq() *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"gemini-2.5-flash-image","prompt":"a fox"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer vk")
	return r
}

// TestGenerativeCap_ConcurrencyRejects429 — with the image concurrency cap (4)
// already full for the VK, the next image request is rejected 429
// GENERATIVE_CONCURRENCY_LIMIT with Retry-After, and the upstream is never
// dispatched (the billing-DoS the cap exists to prevent).
func TestGenerativeCap_ConcurrencyRejects429(t *testing.T) {
	_, handler := imageProbeHandler(t, 4) // cap full
	w := httptest.NewRecorder()
	handler(w, imageReq())
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d want 429; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "GENERATIVE_CONCURRENCY_LIMIT") {
		t.Fatalf("body must carry the reason code, got %s", w.Body.String())
	}
	// Read the header from the COMMITTED response (Result().Header), not the
	// live recorder map — a Retry-After set after WriteHeader would show up in
	// the live map but never reach a real client. This asserts the on-the-wire
	// header, the client's only back-off signal.
	if w.Result().Header.Get("Retry-After") == "" {
		t.Fatal("429 must carry Retry-After on the committed response")
	}
}

// TestGenerativeCap_AdmitsUnderCapAndReleases — under the cap, the request is
// admitted (200); after it completes, the slot is released (finalizeAudit
// defer), so a full round of cap requests all succeed in sequence.
func TestGenerativeCap_AdmitsUnderCapAndReleases(t *testing.T) {
	h, handler := imageProbeHandler(t, 0)
	for i := range 6 { // 6 > cap 4, but sequential so each releases before the next
		w := httptest.NewRecorder()
		handler(w, imageReq())
		if w.Code != http.StatusOK {
			t.Fatalf("request %d status=%d want 200 (slot must free after each completes); body=%s", i, w.Code, w.Body.String())
		}
	}
	// Counter must be back to zero — no slot leaked across the 6 completions.
	if !h.genConcurrency.Acquire(typology.EndpointKindImageGeneration, "vk-1", 1) {
		t.Fatal("counter did not return to zero — a slot leaked across completed requests")
	}
}

// TestGenerativeCap_SizeClamp413 — an image body over the per-kind 256 KiB
// ceiling is rejected 413 even though it is under the global 10 MiB cap.
func TestGenerativeCap_SizeClamp413(t *testing.T) {
	_, handler := imageProbeHandler(t, 0)
	big := strings.Repeat("x", 300<<10) // 300 KiB > 256 KiB per-kind, < 10 MiB global
	r := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"gemini-2.5-flash-image","prompt":"`+big+`"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer vk")
	w := httptest.NewRecorder()
	handler(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d want 413 (per-kind size clamp); body=%s", w.Code, w.Body.String())
	}
	// The 413 hint must name the generative lever, not the global payload cap
	// (raising payload_capture.maxRequestBytes would not lift this ceiling).
	if !strings.Contains(w.Body.String(), "GENERATIVE_CAP") {
		t.Fatalf("413 hint must name the generative env lever, got %s", w.Body.String())
	}
}

// TestGenerativeCap_NonGenerativeUntouched — a chat request never acquires a
// generative slot: even with the chat "kind" the counter stays at zero, and a
// subsequent image acquire at cap 4 admits all 4 (chat consumed none).
func TestGenerativeCap_NonGenerativeUntouched(t *testing.T) {
	fexec := &fakeExecutor{Result: &executor.ExecutionResult{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       []byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`),
		Target:     routingcore.RoutingTarget{ProviderID: "p-openai", ProviderName: "openai", ModelID: "gpt-4o", AdapterType: "openai"},
		Attempts:   []executor.Attempt{{StatusCode: http.StatusOK}},
	}}
	deps := makeFakeDeps(t, fexec, &fakeBridge{})
	h := NewHandler(deps)
	handler := h.ServeProxy(Ingress{WireShape: typology.WireShapeOpenAIChat, BodyFormat: provcore.FormatOpenAI})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer vk")
	w := httptest.NewRecorder()
	handler(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("chat status=%d want 200; body=%s", w.Code, w.Body.String())
	}
	// The image bucket for this VK must be untouched (chat consumed no slot).
	for i := range 4 {
		if !h.genConcurrency.Acquire(typology.EndpointKindImageGeneration, "vk-1", 4) {
			t.Fatalf("image acquire %d rejected — chat wrongly consumed a generative slot", i)
		}
	}
}

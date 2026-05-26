package googleaistudioweb

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// Identity + configuration

func TestAdapter_ID(t *testing.T) {
	a := &Adapter{}
	if a.ID() != "google-aistudio-web" {
		t.Errorf("ID=%q want google-aistudio-web", a.ID())
	}
}

// TestAdapter_Configure pins delegation to the inner gemini adapter
// (no-op contract).
func TestAdapter_Configure(t *testing.T) {
	a := &Adapter{}
	if err := a.Configure(nil); err != nil {
		t.Errorf("Configure(nil)=%v", err)
	}
	if err := a.Configure(map[string]any{"ignored": "value"}); err != nil {
		t.Errorf("Configure(map)=%v", err)
	}
}

// ExtractRequest (delegation to gemini)

// TestExtractRequest_PlaygroundDelegation pins the standard
// /v1beta/models/.../generateContent playground request is decoded by
// the inner gemini adapter.
func TestExtractRequest_PlaygroundDelegation(t *testing.T) {
	body := []byte(`{
		"contents":[{"role":"user","parts":[{"text":"hello from AI Studio"}]}]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1beta/models/gemini-3-pro:generateContent")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "hello from AI Studio" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// TestExtractRequest_FunctionCallDelegation pins gemini function-call
// blocks flow through to ToolCallSegments.
func TestExtractRequest_FunctionCallDelegation(t *testing.T) {
	body := []byte(`{
		"contents":[
			{"role":"user","parts":[{"text":"weather"}]},
			{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"city":"NYC"}}}]}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1beta/models/gemini-3-pro:generateContent")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"get_weather"`) {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
}

// ExtractResponse (delegation)

// TestExtractResponse_FinishReasonDelegation pins that the inner
// gemini adapter's finishReason capture is preserved.
func TestExtractResponse_FinishReasonDelegation(t *testing.T) {
	body := []byte(`{
		"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}],
		"modelVersion":"gemini-3-pro-001"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/v1beta/models/gemini-3-pro:generateContent")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if nc.Metadata["finishReason"] != "STOP" {
		t.Errorf("finishReason=%q", nc.Metadata["finishReason"])
	}
}

// ExtractStreamChunk (delegation)

// TestExtractStreamChunk_Delegation pins gemini streamGenerateContent
// frames flow through to Segments.
func TestExtractStreamChunk_Delegation(t *testing.T) {
	chunk := []byte(`{"candidates":[{"content":{"parts":[{"text":"streamed"}]}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v1beta/models/gemini-3-pro:streamGenerateContent")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "streamed" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// DetectRequestMeta + DetectResponseUsage

// TestDetectRequestMeta_ProviderRelabel pins the load-bearing behaviour
// that distinguishes this adapter from gemini: Provider must be
// relabelled to "google-aistudio-web" after the inner adapter returns
// its own provider tag. Audit relies on this distinction.
func TestDetectRequestMeta_ProviderRelabel(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://aistudio.google.com/v1beta/models/gemini-3-pro:generateContent", nil)
	meta := a.DetectRequestMeta(r, body)
	if meta.Provider != "google-aistudio-web" {
		t.Errorf("Provider=%q want google-aistudio-web", meta.Provider)
	}
}

// TestDetectRequestMeta_RelabelEvenForEmptyBody pins that the relabel
// fires unconditionally — even with an empty body the Provider tag is
// overwritten.
func TestDetectRequestMeta_RelabelEvenForEmptyBody(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://aistudio.google.com/v1beta/models/gemini-3-pro:generateContent", nil)
	meta := a.DetectRequestMeta(r, nil)
	if meta.Provider != "google-aistudio-web" {
		t.Errorf("Provider=%q want google-aistudio-web", meta.Provider)
	}
}

// TestDetectResponseUsage_DelegationToInner pins that DetectResponseUsage
// returns whatever the inner gemini adapter computes. AI Studio
// playground responses use the standard Gemini usageMetadata block, so
// a body with that block must yield a non-empty Status.
func TestDetectResponseUsage_DelegationToInner(t *testing.T) {
	body := []byte(`{
		"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1,"totalTokenCount":4}
	}`)
	a := &Adapter{}
	usage := a.DetectResponseUsage(nil, body)
	if usage.Status == "" {
		t.Errorf("Status empty — expected inner gemini adapter to stamp a Status")
	}
}

// Rewrite contracts (delegated)

// TestRewriteRequestBody_DelegatedThrough pins delegation for the
// request-rewrite path. Whatever the inner gemini adapter returns must
// flow through without panic.
func TestRewriteRequestBody_DelegatedThrough(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	_, _, err := a.RewriteRequestBody(context.Background(), body, "/v1beta/models/gemini-3-pro:generateContent", traffic.NormalizedContent{})
	if err != nil && !errors.Is(err, traffic.ErrRewriteUnsupported) {
		// Any other typed error from the inner adapter is acceptable —
		// we are only pinning that delegation runs and returns a typed
		// error rather than crashing.
		_ = err
	}
}

// TestRewriteResponseBody_DelegatedThrough pins delegation for the
// response-rewrite path.
func TestRewriteResponseBody_DelegatedThrough(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`)
	_, _, err := a.RewriteResponseBody(context.Background(), body, "/v1beta/models/gemini-3-pro:generateContent", traffic.NormalizedContent{})
	if err != nil && !errors.Is(err, traffic.ErrRewriteUnsupported) {
		_ = err
	}
}

// Normalize (Tier-1 spec dispatch)

// TestNormalize_RequestGeminiShape pins that a gemini-generate-shaped
// request body claims Tier 1 via the gemini-generate spec and stamps
// DetectedSpec = "google-ai-studio-web" (the adapter ID baked into
// normalize.go).
func TestNormalize_RequestGeminiShape(t *testing.T) {
	body := []byte(`{
		"contents":[
			{"role":"user","parts":[{"text":"hello from AI Studio"}]}
		]
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "google-aistudio-web",
		Direction:    normalize.DirectionRequest,
		ContentType:  "application/json",
		EndpointPath: "/v1beta/models/gemini-3-pro:generateContent",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Errorf("Kind=%v want ai-chat", payload.Kind)
	}
	if payload.DetectedSpec != "google-ai-studio-web" {
		t.Errorf("DetectedSpec=%q want google-ai-studio-web", payload.DetectedSpec)
	}
	if payload.Confidence < 0.5 {
		t.Errorf("Confidence=%v want >= 0.5", payload.Confidence)
	}
}

// TestNormalize_ResponseNonStream pins response-side scoring against
// the gemini-generate-nonstream spec listed in the adapter's spec hint.
func TestNormalize_ResponseNonStream(t *testing.T) {
	body := []byte(`{
		"candidates":[
			{"content":{"parts":[{"text":"hi"}],"role":"model"},"finishReason":"STOP","index":0}
		],
		"modelVersion":"gemini-3-pro-001",
		"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1,"totalTokenCount":4}
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "google-aistudio-web",
		Direction:   normalize.DirectionResponse,
		ContentType: "application/json",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Errorf("Kind=%v want ai-chat", payload.Kind)
	}
	if payload.DetectedSpec != "google-ai-studio-web" {
		t.Errorf("DetectedSpec=%q want google-ai-studio-web", payload.DetectedSpec)
	}
}

// TestNormalize_UnrecognisedShape_FallsThrough verifies a body matching
// neither spec returns ErrUnsupported so the Coordinator can fall
// through to Tier 2 / Tier 3.
func TestNormalize_UnrecognisedShape_FallsThrough(t *testing.T) {
	body := []byte(`{"foo":"bar","baz":42}`)
	a := &Adapter{}
	_, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "google-aistudio-web",
		Direction:   normalize.DirectionRequest,
		ContentType: "application/json",
	})
	if !errors.Is(err, normalize.ErrUnsupported) {
		t.Errorf("err=%v want ErrUnsupported", err)
	}
}

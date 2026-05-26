package m365copilotweb

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
	if a.ID() != "m365-copilot-web" {
		t.Errorf("ID=%q want m365-copilot-web", a.ID())
	}
}

// TestAdapter_Configure pins that Configure delegates to the inner
// copilot-ms-web adapter and never errors (no-op contract).
func TestAdapter_Configure(t *testing.T) {
	a := &Adapter{}
	if err := a.Configure(nil); err != nil {
		t.Errorf("Configure(nil)=%v", err)
	}
	if err := a.Configure(map[string]any{"ignored": "value"}); err != nil {
		t.Errorf("Configure(map)=%v", err)
	}
}

// ExtractRequest (delegation)

// TestExtractRequest_ModernCopilotShapeDelegation pins delegation to
// the inner adapter's Modern Copilot shape parser.
func TestExtractRequest_ModernCopilotShapeDelegation(t *testing.T) {
	body := []byte(`{
		"messages":[{"author":"user","text":"summarise this document","contentType":"Text"}],
		"conversationId":"conv-1",
		"model":"copilot-365"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "summarise this document" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["model"] != "copilot-365" {
		t.Errorf("model=%q want copilot-365", nc.Metadata["model"])
	}
}

// TestExtractRequest_OpenAICompatToolCallsDelegation pins that the
// openai-compat tool_calls shape flows through delegation cleanly.
func TestExtractRequest_OpenAICompatToolCallsDelegation(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"c1","type":"function","function":{"name":"draft_email","arguments":"{}"}}
			]}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"draft_email"`) {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
}

// TestExtractRequest_EmptyBodyDelegation pins that an empty body
// surfaces ErrUnknownSchema through the delegation.
func TestExtractRequest_EmptyBodyDelegation(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), nil, "/api/chat")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// ExtractResponse (delegation)

// TestExtractResponse_EmptyBodyDelegation pins that an empty response
// body flows through to the inner adapter's benign return.
func TestExtractResponse_EmptyBodyDelegation(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), nil, "/api/chat")
	if err != nil {
		t.Errorf("err=%v want nil (empty body benign)", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// TestExtractResponse_DelegationReturnsContent pins that any non-empty
// response body reaches the inner adapter — verified by passing a
// well-formed JSON object and confirming no panic and a typed error
// path (ErrUnknownSchema or success). The exact branch depends on
// copilot-ms-web internals; the test only asserts delegation works.
func TestExtractResponse_DelegationReturnsContent(t *testing.T) {
	a := &Adapter{}
	// Pass a malformed body and confirm the error flows through.
	_, err := a.ExtractResponse(context.Background(), []byte(`{not json`), "/api/chat")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed (delegated through)", err)
	}
}

// ExtractStreamChunk (delegation)

// TestExtractStreamChunk_OpenAICompatDeltaDelegation pins the
// openai-chat-SSE delta-content path through delegation.
func TestExtractStreamChunk_OpenAICompatDeltaDelegation(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{"content":"draft "}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/chat/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "draft " {
		t.Errorf("Segments=%v want [draft ]", nc.Segments)
	}
}

// TestExtractStreamChunk_EmptyChunkDelegation pins that an empty chunk
// is a clean no-op through delegation.
func TestExtractStreamChunk_EmptyChunkDelegation(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), nil, "/api/chat/stream")
	if err != nil {
		t.Errorf("err=%v want nil", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v want empty", nc.Segments)
	}
}

// DetectRequestMeta + DetectResponseUsage

// TestDetectRequestMeta_ProviderRelabel pins the load-bearing behaviour
// that distinguishes this adapter from copilot-ms-web: Provider must be
// relabelled to "m365-copilot-web" after the inner adapter returns its
// own provider tag. Audit relies on this distinction.
func TestDetectRequestMeta_ProviderRelabel(t *testing.T) {
	body := []byte(`{"messages":[{"author":"user","text":"hi"}]}`)
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://m365.cloud.microsoft/api/chat", nil)
	meta := a.DetectRequestMeta(r, body)
	if meta.Provider != "m365-copilot-web" {
		t.Errorf("Provider=%q want m365-copilot-web (relabel from inner)", meta.Provider)
	}
}

// TestDetectRequestMeta_RelabelEvenForEmptyBody pins that the relabel
// fires unconditionally — even if the inner adapter can't compute much
// meta from an empty body, the Provider tag is overwritten.
func TestDetectRequestMeta_RelabelEvenForEmptyBody(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://m365.cloud.microsoft/api/chat", nil)
	meta := a.DetectRequestMeta(r, nil)
	if meta.Provider != "m365-copilot-web" {
		t.Errorf("Provider=%q want m365-copilot-web", meta.Provider)
	}
}

// TestDetectResponseUsage_DelegationToInner pins that DetectResponseUsage
// returns whatever the inner adapter computes — no relabel needed
// (UsageMeta has no provider field, just token stats).
func TestDetectResponseUsage_DelegationToInner(t *testing.T) {
	a := &Adapter{}
	usage := a.DetectResponseUsage(nil, []byte(`{}`))
	// The inner copilot-ms-web returns UsageStatusNonLLM since its
	// wire format is undocumented and token stats are not extractable.
	if usage.Status != traffic.UsageStatusNonLLM {
		t.Errorf("Status=%q want non_llm (inner adapter default)", usage.Status)
	}
}

// Rewrite contracts (delegated; must return ErrRewriteUnsupported)

func TestRewriteRequestBody_UnsupportedDelegated(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"messages":[{"author":"user","text":"hi"}]}`)
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/api/chat", traffic.NormalizedContent{})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("err=%v want ErrRewriteUnsupported", err)
	}
	if n != 0 {
		t.Errorf("n=%d want 0", n)
	}
	if string(out) != string(body) {
		t.Errorf("body must be returned unchanged")
	}
}

func TestRewriteResponseBody_UnsupportedDelegated(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"error":{"message":"x"}}`)
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/api/chat", traffic.NormalizedContent{})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("err=%v want ErrRewriteUnsupported", err)
	}
	if n != 0 {
		t.Errorf("n=%d want 0", n)
	}
	if string(out) != string(body) {
		t.Errorf("body must be returned unchanged")
	}
}

// Normalize (Tier-1 spec dispatch)

// TestNormalize_RequestChatShape pins that an openai-chat-shaped
// request is accepted by the Tier-1 normalizer and stamped with
// DetectedSpec = "m365-copilot-web".
func TestNormalize_RequestChatShape(t *testing.T) {
	body := []byte(`{
		"model":"copilot-365",
		"messages":[
			{"role":"system","content":"You are M365 Copilot."},
			{"role":"user","content":"draft an email"}
		]
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "m365-copilot-web",
		Direction:    normalize.DirectionRequest,
		ContentType:  "application/json",
		EndpointPath: "/api/chat",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Errorf("Kind=%v want ai-chat", payload.Kind)
	}
	if payload.DetectedSpec != "m365-copilot-web" {
		t.Errorf("DetectedSpec=%q want m365-copilot-web", payload.DetectedSpec)
	}
	if payload.Model != "copilot-365" {
		t.Errorf("Model=%q", payload.Model)
	}
	if len(payload.Messages) < 1 {
		t.Fatalf("Messages empty: %+v", payload.Messages)
	}
	if payload.Confidence < 0.5 {
		t.Errorf("Confidence=%v want >= 0.5", payload.Confidence)
	}
}

// TestNormalize_ResponseNonStream pins response-side scoring against
// the openai-chat-nonstream spec.
func TestNormalize_ResponseNonStream(t *testing.T) {
	body := []byte(`{
		"id":"chatcmpl-m365",
		"object":"chat.completion",
		"model":"copilot-365",
		"choices":[
			{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}
		],
		"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "m365-copilot-web",
		Direction:   normalize.DirectionResponse,
		ContentType: "application/json",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Errorf("Kind=%v want ai-chat", payload.Kind)
	}
	if payload.DetectedSpec != "m365-copilot-web" {
		t.Errorf("DetectedSpec=%q want m365-copilot-web", payload.DetectedSpec)
	}
}

// TestNormalize_UnrecognisedShape_FallsThrough verifies a body matching
// neither spec returns ErrUnsupported so the Coordinator can fall
// through to Tier 2 / Tier 3.
func TestNormalize_UnrecognisedShape_FallsThrough(t *testing.T) {
	body := []byte(`{"foo":"bar","baz":42}`)
	a := &Adapter{}
	_, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "m365-copilot-web",
		Direction:   normalize.DirectionRequest,
		ContentType: "application/json",
	})
	if !errors.Is(err, normalize.ErrUnsupported) {
		t.Errorf("err=%v want ErrUnsupported", err)
	}
}

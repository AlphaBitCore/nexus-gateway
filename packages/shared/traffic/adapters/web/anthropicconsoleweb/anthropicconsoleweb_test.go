package anthropicconsoleweb

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
	if a.ID() != "anthropic-console-web" {
		t.Errorf("ID=%q want anthropic-console-web", a.ID())
	}
}

// TestAdapter_Configure pins that Configure delegates to the inner
// anthropic adapter and never errors (no-op contract).
func TestAdapter_Configure(t *testing.T) {
	a := &Adapter{}
	if err := a.Configure(nil); err != nil {
		t.Errorf("Configure(nil)=%v", err)
	}
	if err := a.Configure(map[string]any{"ignored": "value"}); err != nil {
		t.Errorf("Configure(map)=%v", err)
	}
}

// ExtractRequest (delegation to anthropic)

// TestExtractRequest_WorkbenchDelegation pins that the standard
// /v1/messages workbench request shape is decoded by the inner
// anthropic adapter.
func TestExtractRequest_WorkbenchDelegation(t *testing.T) {
	body := []byte(`{
		"messages":[{"role":"user","content":"workbench prompt"}],
		"model":"claude-sonnet-4-6",
		"max_tokens":1024
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/messages")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "workbench prompt" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["model"] != "claude-sonnet-4-6" {
		t.Errorf("model=%q", nc.Metadata["model"])
	}
}

// TestExtractRequest_ToolUseDelegation pins the assistant tool_use
// block is captured as a ToolCallSegment by the inner adapter.
func TestExtractRequest_ToolUseDelegation(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"user","content":"weather"},
			{"role":"assistant","content":[
				{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"NYC"}}
			]}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/messages")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"get_weather"`) {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
}

// ExtractResponse (delegation)

// TestExtractResponse_StopReasonDelegation pins that the inner
// anthropic adapter's stop_reason capture is preserved through
// delegation.
func TestExtractResponse_StopReasonDelegation(t *testing.T) {
	body := []byte(`{
		"id":"msg_1","model":"claude-sonnet-4-6",
		"content":[{"type":"text","text":"hi"}],
		"stop_reason":"end_turn"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/v1/messages")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if nc.Metadata["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason=%q", nc.Metadata["stop_reason"])
	}
}

// ExtractStreamChunk (delegation)

// TestExtractStreamChunk_Delegation pins content_block_delta text_delta
// frames flow through to Segments.
func TestExtractStreamChunk_Delegation(t *testing.T) {
	chunk := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v1/messages")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Hello" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// DetectRequestMeta + DetectResponseUsage

// TestDetectRequestMeta_ProviderRelabel pins the load-bearing behaviour
// that distinguishes this adapter from anthropic: Provider must be
// relabelled to "anthropic-console-web" after the inner adapter returns
// its own provider tag. Audit relies on this distinction.
func TestDetectRequestMeta_ProviderRelabel(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"}],"model":"claude-sonnet-4-6"}`)
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://console.anthropic.com/v1/messages", nil)
	r.Header.Set("Authorization", "Bearer sk-ant-abc")
	meta := a.DetectRequestMeta(r, body)
	if meta.Provider != "anthropic-console-web" {
		t.Errorf("Provider=%q want anthropic-console-web", meta.Provider)
	}
}

// TestDetectRequestMeta_RelabelEvenForEmptyBody pins that the relabel
// fires unconditionally — even when the inner adapter sees an empty
// body, the Provider tag is overwritten.
func TestDetectRequestMeta_RelabelEvenForEmptyBody(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://console.anthropic.com/v1/messages", nil)
	meta := a.DetectRequestMeta(r, nil)
	if meta.Provider != "anthropic-console-web" {
		t.Errorf("Provider=%q want anthropic-console-web", meta.Provider)
	}
}

// TestDetectResponseUsage_DelegationToInner pins that DetectResponseUsage
// returns whatever the inner anthropic adapter computes. The workbench
// uses the standard Anthropic usage block, so a body with that block
// must yield a non-empty status.
func TestDetectResponseUsage_DelegationToInner(t *testing.T) {
	body := []byte(`{
		"id":"msg_1","model":"claude-sonnet-4-6",
		"content":[{"type":"text","text":"hi"}],
		"usage":{"input_tokens":3,"output_tokens":1}
	}`)
	a := &Adapter{}
	usage := a.DetectResponseUsage(nil, body)
	if usage.Status == "" {
		t.Errorf("Status empty — expected inner anthropic adapter to stamp a Status")
	}
}

// Rewrite contracts (delegated)

// TestRewriteRequestBody_DelegatedThrough pins delegation for the
// request-rewrite path. The inner anthropic adapter supports rewrite
// for well-formed Messages-API bodies — verify it doesn't panic and
// the delegated path returns a non-error.
func TestRewriteRequestBody_DelegatedThrough(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"messages":[{"role":"user","content":"hi"}],"model":"claude-sonnet-4-6"}`)
	_, _, err := a.RewriteRequestBody(context.Background(), body, "/v1/messages", traffic.NormalizedContent{})
	// Any error other than nil / ErrRewriteUnsupported would be a regression
	// in delegation. The inner adapter chooses one of these two paths
	// depending on whether content segments are provided.
	if err != nil && !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("err=%v want nil or ErrRewriteUnsupported", err)
	}
}

// TestRewriteResponseBody_DelegatedThrough pins delegation for the
// response-rewrite path. The inner anthropic adapter returns
// ErrUnknownSchema for an error-envelope body (no `content` array),
// which is the correct delegation outcome.
func TestRewriteResponseBody_DelegatedThrough(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"error":{"message":"x"}}`)
	_, _, err := a.RewriteResponseBody(context.Background(), body, "/v1/messages", traffic.NormalizedContent{})
	// Must surface a typed error (not panic). Either ErrUnknownSchema
	// (no content[]) or ErrMalformed (parse failure) is acceptable;
	// what we're pinning is that the delegation runs and returns a
	// typed error rather than crashing.
	if err == nil {
		t.Errorf("err=nil; expected delegation to surface a typed error for error-envelope body")
	}
}

// Normalize (Tier-1 spec dispatch)

// TestNormalize_RequestAnthropicShape pins that an anthropic-messages
// shaped request is recognised by the Tier-1 normalizer and stamped
// with DetectedSpec = "anthropic-console-web".
func TestNormalize_RequestAnthropicShape(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[
			{"role":"user","content":"workbench prompt"}
		],
		"max_tokens":1024
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "anthropic-console-web",
		Direction:    normalize.DirectionRequest,
		ContentType:  "application/json",
		EndpointPath: "/v1/messages",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Errorf("Kind=%v want ai-chat", payload.Kind)
	}
	if payload.DetectedSpec != "anthropic-console-web" {
		t.Errorf("DetectedSpec=%q want anthropic-console-web", payload.DetectedSpec)
	}
	if payload.Model != "claude-sonnet-4-6" {
		t.Errorf("Model=%q", payload.Model)
	}
	if payload.Confidence < 0.5 {
		t.Errorf("Confidence=%v want >= 0.5", payload.Confidence)
	}
}

// TestNormalize_ResponseNonStream pins response-side scoring against
// the anthropic-messages-nonstream spec.
func TestNormalize_ResponseNonStream(t *testing.T) {
	body := []byte(`{
		"id":"msg_1",
		"type":"message",
		"role":"assistant",
		"model":"claude-sonnet-4-6",
		"content":[{"type":"text","text":"hi"}],
		"stop_reason":"end_turn",
		"usage":{"input_tokens":3,"output_tokens":1}
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "anthropic-console-web",
		Direction:   normalize.DirectionResponse,
		ContentType: "application/json",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Errorf("Kind=%v want ai-chat", payload.Kind)
	}
	if payload.DetectedSpec != "anthropic-console-web" {
		t.Errorf("DetectedSpec=%q want anthropic-console-web", payload.DetectedSpec)
	}
}

// TestNormalize_UnrecognisedShape_FallsThrough verifies a body matching
// neither spec returns ErrUnsupported so the Coordinator can fall
// through to Tier 2 / Tier 3.
func TestNormalize_UnrecognisedShape_FallsThrough(t *testing.T) {
	body := []byte(`{"foo":"bar","baz":42}`)
	a := &Adapter{}
	_, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "anthropic-console-web",
		Direction:   normalize.DirectionRequest,
		ContentType: "application/json",
	})
	if !errors.Is(err, normalize.ErrUnsupported) {
		t.Errorf("err=%v want ErrUnsupported", err)
	}
}

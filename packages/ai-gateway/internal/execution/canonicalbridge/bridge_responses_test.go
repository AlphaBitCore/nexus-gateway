package canonicalbridge

import (
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

// TestBridge_ResponsesRoutable_NativePassthrough: target=spec_openai
// declares responses-api support, so a Responses ingress → spec_openai
// target is routable as native same-shape passthrough.
func TestBridge_ResponsesRoutable_NativePassthrough(t *testing.T) {
	b := testBridge(t)
	if !b.ResponsesRoutable(provcore.FormatOpenAI) {
		t.Error("Responses → OpenAI must be routable (native responses-api support)")
	}
}

// TestBridge_ResponsesRoutable_CrossFormat: targets that do NOT declare
// responses-api still satisfy ResponsesRoutable via cross-format canonical
// translation — except Bedrock, which uses AWS event-stream framing and
// is therefore excluded (mirrors StreamShapeCompatible).
func TestBridge_ResponsesRoutable_CrossFormat(t *testing.T) {
	b := testBridge(t)
	cases := []struct {
		target provcore.Format
		want   bool
	}{
		{provcore.FormatAnthropic, true},
		{provcore.FormatGemini, true},
		{provcore.FormatMoonshot, true},
		{provcore.FormatGroq, true},
		{provcore.FormatBedrock, false}, // event-stream framing — excluded
	}
	for _, c := range cases {
		if got := b.ResponsesRoutable(c.target); got != c.want {
			t.Errorf("ResponsesRoutable(%s) = %v, want %v", c.target, got, c.want)
		}
	}
}

// TestBridge_EndpointRoutable_ResponsesAPI pins the EndpointRoutable
// dispatch for EndpointResponsesAPI. Only FormatOpenAIResponses ingress
// is accepted; any other ingress format trying to hit /v1/responses is
// rejected at the routing layer.
func TestBridge_EndpointRoutable_ResponsesAPI(t *testing.T) {
	b := testBridge(t)
	if !b.EndpointRoutable(typology.WireShapeOpenAIResponses, provcore.FormatOpenAIResponses, provcore.FormatOpenAI) {
		t.Error("Responses ingress → OpenAI target should be routable")
	}
	if !b.EndpointRoutable(typology.WireShapeOpenAIResponses, provcore.FormatOpenAIResponses, provcore.FormatAnthropic) {
		t.Error("Responses ingress → Anthropic target should be routable (cross-format)")
	}
	if b.EndpointRoutable(typology.WireShapeOpenAIResponses, provcore.FormatOpenAI, provcore.FormatOpenAI) {
		t.Error("Non-Responses ingress should NOT be routable on /v1/responses endpoint")
	}
	if b.EndpointRoutable(typology.WireShapeOpenAIResponses, provcore.FormatOpenAIResponses, provcore.FormatBedrock) {
		t.Error("Responses ingress → Bedrock target should be rejected (event-stream framing)")
	}
}

// TestBridge_IngressChatToWire_ResponsesSameShape: when ingress is
// FormatOpenAIResponses and target is OpenAI (natively serves
// responses-api), the bridge returns the body verbatim — no codec runs.
func TestBridge_IngressChatToWire_ResponsesSameShape(t *testing.T) {
	b := testBridge(t)
	body := []byte(`{"model":"gpt-5.2","input":"hi","previous_response_id":"resp_abc"}`)
	out, err := b.IngressChatToWire(provcore.FormatOpenAIResponses, provcore.FormatOpenAI, body, provcore.CallTarget{
		Format:          provcore.FormatOpenAI,
		ProviderModelID: "gpt-5.2",
	})
	if err != nil {
		t.Fatalf("IngressChatToWire: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("same-shape passthrough must return body verbatim; got %s want %s", string(out), string(body))
	}
}

// TestBridge_IngressChatToWire_ResponsesCrossFormat: Responses ingress
// + Anthropic target. The bridge decodes Responses → canonical chat-
// completions, then asks Anthropic codec to produce wire bytes. We
// assert (a) no error, (b) the output is non-empty valid JSON, (c) the
// system instruction from the Responses request lands in Anthropic's
// `system` field.
func TestBridge_IngressChatToWire_ResponsesCrossFormat(t *testing.T) {
	b := testBridge(t)
	body := []byte(`{
		"model": "gpt-5.2",
		"instructions": "Be terse.",
		"input": "What is 2+2?"
	}`)
	out, err := b.IngressChatToWire(provcore.FormatOpenAIResponses, provcore.FormatAnthropic, body, provcore.CallTarget{
		Format:          provcore.FormatAnthropic,
		ProviderModelID: "claude-sonnet-4-6",
	})
	if err != nil {
		t.Fatalf("cross-format encode failed: %v", err)
	}
	if !gjson.ValidBytes(out) {
		t.Fatalf("invalid JSON output: %s", string(out))
	}
	// The Anthropic codec routes the system message into `system` and
	// the user message into `messages[]`.
	if got := gjson.GetBytes(out, "system").String(); got != "Be terse." {
		t.Errorf("Anthropic system field = %q (want 'Be terse.'); body=%s", got, string(out))
	}
	// At least one message.
	if !gjson.GetBytes(out, "messages.0").Exists() {
		t.Errorf("Anthropic messages[] missing; body=%s", string(out))
	}
}

// TestBridge_ResponseCanonicalToIngress_Responses: canonical chat-completions
// response → Responses-API output[] shape.
func TestBridge_ResponseCanonicalToIngress_Responses(t *testing.T) {
	b := testBridge(t)
	canon := []byte(`{
		"id": "chatcmpl_xyz",
		"object": "chat.completion",
		"created": 1747353700,
		"model": "claude-sonnet-4-6",
		"choices": [{
			"index": 0,
			"message": {"role":"assistant","content":"Hello"},
			"finish_reason": "stop"
		}],
		"usage": {"prompt_tokens": 5, "completion_tokens": 1, "total_tokens": 6}
	}`)
	out, err := b.ResponseCanonicalToIngress(provcore.FormatOpenAIResponses, canon)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got := gjson.GetBytes(out, "object").String(); got != "response" {
		t.Errorf("object = %q, want response", got)
	}
	if got := gjson.GetBytes(out, "status").String(); got != "completed" {
		t.Errorf("status = %q, want completed", got)
	}
	if got := gjson.GetBytes(out, "output.0.type").String(); got != "message" {
		t.Errorf("output[0].type = %q", got)
	}
	if got := gjson.GetBytes(out, "output.0.content.0.text").String(); got != "Hello" {
		t.Errorf("output[0].content[0].text = %q", got)
	}
	if got := gjson.GetBytes(out, "usage.input_tokens").Int(); got != 5 {
		t.Errorf("usage.input_tokens = %d (translated from prompt_tokens)", got)
	}
}

// TestBridge_NewStreamTranscoder_Responses pins the per-direction
// transcoder choice for Responses ingress.
func TestBridge_NewStreamTranscoder_Responses(t *testing.T) {
	b := testBridge(t)
	// Same-shape passthrough = nil transcoder.
	if tr := b.NewStreamTranscoder(provcore.FormatOpenAIResponses, provcore.FormatOpenAI, "gpt-5.2"); tr != nil {
		t.Error("Responses → OpenAI same-shape passthrough should yield nil transcoder")
	}
	// Cross-format target = responsesStreamEncoder (re-encode canonical → Responses SSE).
	tr := b.NewStreamTranscoder(provcore.FormatOpenAIResponses, provcore.FormatAnthropic, "claude-sonnet-4-6")
	if tr == nil {
		t.Error("Responses → Anthropic cross-format should yield non-nil encoder")
	}
	if _, ok := tr.(*responsesStreamEncoder); !ok {
		t.Errorf("Responses → Anthropic transcoder should be *responsesStreamEncoder, got %T", tr)
	}
}

// TestBridge_IngressChatToCanonical_Responses pins the codec dispatch
// for FormatOpenAIResponses ingress.
func TestBridge_IngressChatToCanonical_Responses(t *testing.T) {
	b := testBridge(t)
	body := []byte(`{"model":"gpt-5.2","input":"hi","reasoning":{"effort":"high"}}`)
	out, err := b.IngressChatToCanonical(provcore.FormatOpenAIResponses, body, provcore.CallTarget{
		Format:          provcore.FormatAnthropic,
		ProviderModelID: "claude-sonnet-4-6",
	})
	if err != nil {
		t.Fatalf("IngressChatToCanonical: %v", err)
	}
	// Canonical chat-completions shape: messages[] populated + reasoning_effort lifted.
	if got := gjson.GetBytes(out, "messages.0.role").String(); got != "user" {
		t.Errorf("canonical messages[0].role = %q (DecodeResponsesRequest path); body=%s", got, string(out))
	}
	if got := gjson.GetBytes(out, "reasoning_effort").String(); got != "high" {
		t.Errorf("reasoning_effort = %q (want high)", got)
	}
}

// TestBridge_ResponseAcrossFormats_SameShape_NoOp verifies the
// fast-path: when fromFormat == toFormat, the bridge returns body
// verbatim regardless of endpoint.
func TestBridge_ResponseAcrossFormats_SameShape_NoOp(t *testing.T) {
	b := testBridge(t)
	body := []byte(`{"object":"chat.completion","choices":[]}`)
	out, err := b.ResponseAcrossFormats(typology.WireShapeOpenAIChat, typology.WireShapeOpenAIChat, body)
	if err != nil {
		t.Fatalf("same-shape no-op returned error: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("same-shape must return body verbatim; got %s want %s", string(out), string(body))
	}
}

// TestBridge_ResponseAcrossFormats_ChatToResponses verifies the
// canonical cross-ingress contamination case: a cached
// chat.completion-shape body is reshaped into Responses-API output[]
// shape for a /v1/responses caller. This is the read-side complement
// to the B2 origin-tag fix.
func TestBridge_ResponseAcrossFormats_ChatToResponses(t *testing.T) {
	b := testBridge(t)
	chatBody := []byte(`{
		"id": "chatcmpl_abc",
		"object": "chat.completion",
		"created": 1747353700,
		"model": "gpt-4o-mini",
		"choices": [{
			"index": 0,
			"message": {"role":"assistant","content":"Hello from chat"},
			"finish_reason": "stop"
		}],
		"usage": {"prompt_tokens": 7, "completion_tokens": 3, "total_tokens": 10}
	}`)
	out, err := b.ResponseAcrossFormats(
		typology.WireShapeOpenAIChat,
		typology.WireShapeOpenAIResponses, chatBody,
	)
	if err != nil {
		t.Fatalf("chat→responses reshape failed: %v", err)
	}
	if got := gjson.GetBytes(out, "object").String(); got != "response" {
		t.Errorf("object = %q, want response (post-reshape)", got)
	}
	if !gjson.GetBytes(out, "output.0.content.0.text").Exists() {
		t.Errorf("output[0].content[0].text missing in reshaped body: %s", string(out))
	}
	if gjson.GetBytes(out, "choices").Exists() {
		t.Errorf("reshaped body should NOT carry chat.completion choices[]; body=%s", string(out))
	}
	if got := gjson.GetBytes(out, "output.0.content.0.text").String(); got != "Hello from chat" {
		t.Errorf("output[0].content[0].text = %q (want 'Hello from chat')", got)
	}
}

// TestBridge_ResponseAcrossFormats_ResponsesToChat verifies the reverse
// direction (a /v1/responses writer's body served to a chat-completions
// reader). Asserts the body now carries chat.completion choices[] and
// drops the Responses-shape output[] envelope.
func TestBridge_ResponseAcrossFormats_ResponsesToChat(t *testing.T) {
	b := testBridge(t)
	responsesBody := []byte(`{
		"id": "resp_xyz",
		"object": "response",
		"created_at": 1747353700,
		"model": "gpt-4o-mini",
		"status": "completed",
		"output": [{
			"id": "msg_0",
			"type": "message",
			"role": "assistant",
			"status": "completed",
			"content": [{"type":"output_text","text":"Hello from responses"}]
		}],
		"usage": {"input_tokens": 7, "output_tokens": 3, "total_tokens": 10}
	}`)
	out, err := b.ResponseAcrossFormats(
		typology.WireShapeOpenAIResponses,
		typology.WireShapeOpenAIChat, responsesBody,
	)
	if err != nil {
		t.Fatalf("responses→chat reshape failed: %v", err)
	}
	if got := gjson.GetBytes(out, "object").String(); got != "chat.completion" {
		t.Errorf("object = %q, want chat.completion (post-reshape)", got)
	}
	if !gjson.GetBytes(out, "choices.0.message.content").Exists() {
		t.Errorf("choices[0].message.content missing in reshaped body: %s", string(out))
	}
	if gjson.GetBytes(out, "output").Exists() {
		t.Errorf("reshaped chat body should NOT carry Responses output[]; body=%s", string(out))
	}
}

// TestBridge_ResponseAcrossFormats_UnknownFromFormat returns an error
// when no codec is registered for the source wire format. Used by the
// cache HIT reader to fall back to serving the stored bytes verbatim
// instead of silently producing garbage.
func TestBridge_ResponseAcrossFormats_UnknownFromFormat(t *testing.T) {
	b := testBridge(t)
	_, err := b.ResponseAcrossFormats(
		typology.WireShape("not-a-real-shape"),
		typology.WireShapeOpenAIChat, []byte(`{}`),
	)
	if err == nil {
		t.Fatal("expected error for unknown from-format, got nil")
	}
}

// TestBridge_LockstepCheck pins the formatsNativelyServingResponsesAPI
// lockstep with each adapter's RequestShapes declaration. If a sibling
// adapter starts declaring responses-api support but the bridge's
// lockstep map is not updated, this test surfaces the drift.
func TestBridge_LockstepCheck(t *testing.T) {
	// Today only spec_openai is in the lockstep map; no other adapter
	// declares responses-api. Verify both sides.
	got := len(formatsNativelyServingResponsesAPI)
	want := 1
	if got != want {
		t.Errorf("formatsNativelyServingResponsesAPI has %d entries, want %d. If you added an adapter declaring responses-api in its RequestShapes, also add its Format here.", got, want)
	}
	if !formatsNativelyServingResponsesAPI[provcore.FormatOpenAI] {
		t.Error("FormatOpenAI must be in formatsNativelyServingResponsesAPI")
	}
}

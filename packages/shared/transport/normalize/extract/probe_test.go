package extract

import (
	"math"
	"strings"
	"testing"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// Request-side probe

func TestDetectChatShape_ChatGPTWebRequest(t *testing.T) {
	// Real-shape ChatGPT-web request body (subset of a real captured request).
	body := []byte(`{
		"model": "gpt-5-5",
		"messages": [{
			"author": {"role": "user"},
			"content": {"parts": ["hello! have you read any good books lately?"], "content_type": "text"},
			"metadata": {"suggestion_type": "autocomplete"}
		}],
		"suggestion_type": "autocomplete",
		"chosen_suggestion": {"type": "autocomplete", "index": 0},
		"client_contextual_info": {"app_name": "chatgpt.com"},
		"parent_message_id": "client-created-root"
	}`)
	d := DetectChatShape(body)
	if d.SpecID != "chatgpt-web" {
		t.Fatalf("specID: %q want chatgpt-web", d.SpecID)
	}
	if d.Confidence < 0.9 {
		t.Errorf("confidence: %v want >= 0.9", d.Confidence)
	}
	if d.Model != "gpt-5-5" {
		t.Errorf("model: %q", d.Model)
	}
	if len(d.UserPrompts) != 1 || !strings.Contains(d.UserPrompts[0], "have you read") {
		t.Errorf("user prompts: %+v", d.UserPrompts)
	}
}

// TestDetectChatShape_StandardAPIShapesNotClaimed pins the Tier-2 spec
// deletion: request bodies in the standard OpenAI Chat / Anthropic
// Messages / Gemini generateContent wire shapes are no longer claimed
// by the pattern probe — the Tier-1 codecs own those formats. The probe
// must stay below the 0.7 Tier-2 threshold for each of them.
func TestDetectChatShape_StandardAPIShapesNotClaimed(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"openai-chat", `{
			"model": "gpt-4o-mini",
			"messages": [
				{"role": "system", "content": "be helpful"},
				{"role": "user", "content": "what is the capital of France?"}
			],
			"temperature": 0.7,
			"max_tokens": 100
		}`},
		{"anthropic-messages", `{
			"model": "claude-sonnet-4-6",
			"max_tokens": 1024,
			"messages": [
				{"role": "user", "content": [{"type": "text", "text": "tell me about Go generics"}]}
			],
			"system": "be terse"
		}`},
		{"gemini-generate", `{
			"model": "gemini-2.5-flash",
			"contents": [
				{"role": "user", "parts": [{"text": "explain quantum entanglement"}]}
			],
			"generationConfig": {"temperature": 0.5}
		}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := DetectChatShape([]byte(c.body))
			if d.Confidence >= 0.7 {
				t.Errorf("confidence %v (spec %q) — standard-API shape must not be Tier-2 claimed", d.Confidence, d.SpecID)
			}
		})
	}
}

func TestDetectChatShape_NonChatJSON_BelowThreshold(t *testing.T) {
	// Random JSON with a `messages` field but no role / content shape.
	body := []byte(`{"messages": [123, 456], "foo": "bar"}`)
	d := DetectChatShape(body)
	// Below 0.7 → confidence low. May still hint at a spec but not winning.
	if d.Confidence >= 0.7 {
		t.Errorf("confidence too high: %v (%q)", d.Confidence, d.SpecID)
	}
}

func TestDetectChatShape_NonChatJSON_NoMessages(t *testing.T) {
	body := []byte(`{"foo": "bar", "count": 42}`)
	d := DetectChatShape(body)
	if d.Confidence > 0.1 {
		t.Errorf("confidence: %v want ~0 (%q)", d.Confidence, d.SpecID)
	}
}

func TestDetectChatShape_LegacyFlatPromptCompletion(t *testing.T) {
	body := []byte(`{
		"model": "text-davinci-003",
		"prompt": "write a haiku about gateways",
		"max_tokens": 256,
		"temperature": 0.7
	}`)
	d := DetectChatShape(body)
	if d.SpecID != "openai-completions-legacy" {
		t.Fatalf("specID: %q", d.SpecID)
	}
	if len(d.UserPrompts) != 1 || !strings.Contains(d.UserPrompts[0], "haiku") {
		t.Errorf("prompts: %v", d.UserPrompts)
	}
}

// Response-side probe

func TestDetectResponseShape_ChatGPTWebSSE(t *testing.T) {
	// End-to-end: walks the SSE, accumulates JSON-patch ops, extracts
	// the final assistant text. This is the baa07c15 case from the
	// a deploy story. The message_marker metadata frame carries the
	// top-level conversation_id / message_id signature keys, exactly
	// as the production stream does.
	raw := []byte(strings.Join([]string{
		"event: delta_encoding",
		`data: "v1"`,
		"",
		`data: {"type":"resume_conversation_token","token":"abc","conversation_id":"conv1"}`,
		"",
		"event: delta",
		`data: {"p":"","o":"add","v":{"message":{"id":"asst1","author":{"role":"assistant"},"content":{"content_type":"text","parts":[""]}},"conversation_id":"conv1"}}`,
		"",
		"event: delta",
		`data: {"p":"/message/content/parts/0","o":"append","v":"A few that stand"}`,
		"",
		"event: delta",
		`data: {"v":" out recently,"}`,
		"",
		"event: delta",
		`data: {"v":" depending on the kind of reading mood you're in."}`,
		"",
		"event: delta",
		`data: {"p":"","o":"patch","v":[{"p":"/message/content/parts/0","o":"append","v":" Project Hail Mary by Andy Weir."}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n"))

	d := DetectResponseShape(raw)
	if d.SpecID != "chatgpt-web" {
		t.Fatalf("specID: %q want chatgpt-web", d.SpecID)
	}
	// All 5 patch-candidate frames apply → coverage confidence 1.0.
	if math.Abs(d.Confidence-1.0) > 1e-9 {
		t.Errorf("confidence: %v want 1.0 (5/5 patch frames applied)", d.Confidence)
	}
	if !d.IsStream {
		t.Errorf("expected IsStream=true")
	}
	if !strings.Contains(d.AssistantText, "Andy Weir") {
		t.Fatalf("assistant text: %q want to contain final delta", d.AssistantText)
	}
	if !strings.Contains(d.AssistantText, "A few that stand out") {
		t.Errorf("assistant text missing first delta: %q", d.AssistantText)
	}
}

// TestDetectResponseShape_CoverageConfidence pins the json-patch
// coverage semantics: a stream where one of two patch-candidate frames
// fails to apply (invalid JSON Pointer) scores exactly 1/2.
func TestDetectResponseShape_CoverageConfidence(t *testing.T) {
	raw := []byte(strings.Join([]string{
		`data: {"type":"message_marker","conversation_id":"c1","message_id":"m1"}`,
		"",
		"event: delta",
		`data: {"p":"","o":"add","v":{"message":{"content":{"parts":["ok"]}}}}`,
		"",
		"event: delta",
		`data: {"p":"missing-leading-slash","o":"add","v":"broken"}`,
		"",
	}, "\n"))
	d := DetectResponseShape(raw)
	if d.SpecID != "chatgpt-web" {
		t.Fatalf("specID: %q", d.SpecID)
	}
	if math.Abs(d.Confidence-0.5) > 1e-9 {
		t.Errorf("confidence: %v want 0.5 (1 of 2 patch frames applied)", d.Confidence)
	}
}

// TestDetectResponseShape_ModelFromFrames pins the ModelFramePaths
// probe: the chatgpt-web stream carries the model identifier only as
// frame metadata, and the detection surfaces it as Model — nested
// under the patch value envelope of a seed frame
// (v.message.metadata.model_slug) or, when seed frames are absent, at
// a telemetry frame's metadata.model_slug. The first hit wins.
func TestDetectResponseShape_ModelFromFrames(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
	}{
		{
			name: "seed frame nests model_slug under the patch value",
			raw: []byte(strings.Join([]string{
				"event: delta",
				`data: {"p":"","o":"add","v":{"message":{"metadata":{"model_slug":"gpt-5-5-thinking"},"content":{"parts":[""]}},"conversation_id":"c1"}}`,
				"",
				"event: delta",
				`data: {"p":"/message/content/parts/0","o":"append","v":"hi"}`,
				"",
			}, "\n")),
		},
		{
			name: "telemetry frame carries metadata.model_slug top-level",
			raw: []byte(strings.Join([]string{
				`data: {"type":"server_ste_metadata","metadata":{"model_slug":"gpt-5-5-thinking"},"conversation_id":"c1"}`,
				"",
				"event: delta",
				`data: {"p":"","o":"add","v":{"message":{"content":{"parts":["hi"]}}}}`,
				"",
			}, "\n")),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := DetectResponseShape(tc.raw)
			if d.SpecID != "chatgpt-web" {
				t.Fatalf("specID: %q", d.SpecID)
			}
			if d.Model != "gpt-5-5-thinking" {
				t.Fatalf("Model = %q, want gpt-5-5-thinking", d.Model)
			}
		})
	}
}

// TestDetectResponseShape_ModelAbsentStaysEmpty pins the negative: a
// stream whose frames carry no model path leaves Model empty rather
// than inventing one from an unrelated string field.
func TestDetectResponseShape_ModelAbsentStaysEmpty(t *testing.T) {
	raw := []byte(strings.Join([]string{
		"event: delta",
		`data: {"p":"","o":"add","v":{"message":{"content":{"parts":["hi"]}},"conversation_id":"c1"}}`,
		"",
	}, "\n"))
	d := DetectResponseShape(raw)
	if d.SpecID != "chatgpt-web" {
		t.Fatalf("specID: %q", d.SpecID)
	}
	if d.Model != "" {
		t.Fatalf("Model = %q, want empty", d.Model)
	}
}

// TestDetectResponseShape_NoSignatureNoClaim pins the identification
// gate: a JSON-Patch-shaped SSE stream carrying NONE of the
// chatgpt-web signature keys in any raw frame must score zero —
// coverage alone cannot claim a foreign producer's patch stream.
func TestDetectResponseShape_NoSignatureNoClaim(t *testing.T) {
	raw := []byte(strings.Join([]string{
		"event: delta",
		`data: {"p":"","o":"add","v":{"message":{"content":{"parts":["hi"]}}}}`,
		"",
		"event: delta",
		`data: {"p":"/message/content/parts/0","o":"append","v":" there"}`,
		"",
	}, "\n"))
	d := DetectResponseShape(raw)
	if d.Confidence != 0 {
		t.Errorf("confidence: %v want 0 (no signature key in any frame)", d.Confidence)
	}
}

// TestDetectResponseShape_NestedSignatureClaims pins the nested
// signature probe: the delta-encoding stream variant carries
// conversation_id only INSIDE the `v` patch envelope of its seed
// frames (no resume-token or telemetry frame ships a top-level copy),
// and the gate must still identify the stream as chatgpt-web.
func TestDetectResponseShape_NestedSignatureClaims(t *testing.T) {
	raw := []byte(strings.Join([]string{
		"event: delta",
		`data: {"p":"","o":"add","v":{"message":{"id":"a1","author":{"role":"assistant"},"content":{"content_type":"text","parts":[""]}},"conversation_id":"conv-nested"}}`,
		"",
		"event: delta",
		`data: {"p":"/message/content/parts/0","o":"append","v":"Nested works"}`,
		"",
		"data: [DONE]",
		"",
	}, "\n"))
	d := DetectResponseShape(raw)
	if d.SpecID != "chatgpt-web" {
		t.Fatalf("specID: %q want chatgpt-web (v.conversation_id must satisfy the gate)", d.SpecID)
	}
	if math.Abs(d.Confidence-1.0) > 1e-9 {
		t.Errorf("confidence: %v want 1.0 (2/2 patch frames applied)", d.Confidence)
	}
	if d.AssistantText != "Nested works" {
		t.Errorf("assistant text: %q want %q", d.AssistantText, "Nested works")
	}
}

// TestScoreResponseSpecAdapterKeyed_HostEvidenceSatisfiesGate pins the
// adapter-keyed gate bypass: a JSON-Patch stream with NO signature key
// anywhere (top-level or nested) scores zero on the strict Tier-2 path
// (anti-theft for key-missed traffic) but scores on patch coverage for
// an adapter-keyed caller, because host resolution already identified
// the producer.
func TestScoreResponseSpecAdapterKeyed_HostEvidenceSatisfiesGate(t *testing.T) {
	raw := []byte(strings.Join([]string{
		"event: delta",
		`data: {"p":"","o":"add","v":{"message":{"content":{"parts":["hi"]}}}}`,
		"",
		"event: delta",
		`data: {"p":"/message/content/parts/0","o":"append","v":" there"}`,
		"",
	}, "\n"))
	spec := *ChatResponseSpecByID("chatgpt-web")
	if d := ScoreResponseSpec(raw, spec); d.Confidence != 0 {
		t.Fatalf("strict gate: confidence %v want 0 (no signature key, key-missed traffic must not be claimed)", d.Confidence)
	}
	d := ScoreResponseSpecAdapterKeyed(raw, spec)
	if math.Abs(d.Confidence-1.0) > 1e-9 {
		t.Fatalf("keyed gate: confidence %v want 1.0 (host evidence satisfies the gate, 2/2 frames applied)", d.Confidence)
	}
	if d.AssistantText != "hi there" {
		t.Errorf("assistant text: %q want %q", d.AssistantText, "hi there")
	}
	// Non-SSE bodies stay unclaimed even for keyed callers.
	if d := ScoreResponseSpecAdapterKeyed([]byte(`{"plain":"json"}`), spec); d.Confidence != 0 {
		t.Errorf("non-SSE keyed: confidence %v want 0", d.Confidence)
	}
}

// TestNormalizeForAdapter_ChatGPTWebSignatureFreeStream pins the
// chatgpt-web adapter path end-to-end: NormalizeForAdapter with the
// chatgpt-web hint renders a signature-free delta-encoding stream as
// ai-chat instead of falling through to the verbatim tiers.
func TestNormalizeForAdapter_ChatGPTWebSignatureFreeStream(t *testing.T) {
	raw := []byte(strings.Join([]string{
		"event: delta",
		`data: {"p":"","o":"add","v":{"message":{"id":"a1","author":{"role":"assistant"},"content":{"content_type":"text","parts":[""]}}}}`,
		"",
		"event: delta",
		`data: {"p":"/message/content/parts/0","o":"append","v":"Hello again"}`,
		"",
		"data: [DONE]",
		"",
	}, "\n"))
	p, err := NormalizeForAdapter(raw, normalize.Meta{
		AdapterType: "chatgpt-web",
		Direction:   normalize.DirectionResponse,
		Stream:      true,
	}, AdapterSpecHint{
		AdapterID:     "chatgpt-web",
		ReqSpecIDs:    []string{"chatgpt-web"},
		RespSpecIDs:   []string{"chatgpt-web"},
		MinConfidence: 0.5,
	})
	if err != nil {
		t.Fatalf("NormalizeForAdapter: %v", err)
	}
	if p.Kind != normalize.KindAIChat {
		t.Fatalf("kind: %q want %q", p.Kind, normalize.KindAIChat)
	}
	if p.DetectedSpec != "chatgpt-web" {
		t.Fatalf("detectedSpec: %q want chatgpt-web", p.DetectedSpec)
	}
	if len(p.Messages) != 1 || len(p.Messages[0].Content) != 1 || p.Messages[0].Content[0].Text != "Hello again" {
		t.Fatalf("messages: %+v want single assistant text %q", p.Messages, "Hello again")
	}
}

// TestDetectResponseShape_StandardAPIResponsesNotClaimed pins the
// response-side spec deletion: standard OpenAI / Anthropic / Gemini
// response bodies (non-stream JSON and SSE) are no longer recognised
// by the Tier-2 probe — the Tier-1 codecs fold those wires.
func TestDetectResponseShape_StandardAPIResponsesNotClaimed(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"openai-nonstream", `{
			"id": "chatcmpl-abc",
			"object": "chat.completion",
			"model": "gpt-4o-mini",
			"choices": [{"message": {"role": "assistant", "content": "Paris."}, "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 20, "completion_tokens": 8, "total_tokens": 28}
		}`},
		{"openai-sse", strings.Join([]string{
			`data: {"id":"x","object":"chat.completion.chunk","choices":[{"delta":{"content":"Hello"}}]}`,
			"",
			`data: {"id":"x","choices":[{"delta":{"content":" world"}}]}`,
			"",
			"data: [DONE]",
			"",
		}, "\n")},
		{"anthropic-sse", strings.Join([]string{
			"event: message_start",
			`data: {"type":"message_start","message":{"id":"msg1","model":"claude-haiku-4-5","content":[],"usage":{"input_tokens":12}}}`,
			"",
			"event: content_block_delta",
			`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hi"}}`,
			"",
		}, "\n")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := DetectResponseShape([]byte(c.body))
			if d.Confidence != 0 {
				t.Errorf("confidence %v (spec %q) — standard-API response must not be Tier-2 claimed", d.Confidence, d.SpecID)
			}
		})
	}
}

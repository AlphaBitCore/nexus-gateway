package ingress

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// Google's JSON surface for generateContent accepts the protobuf field names
// alongside the documented camelCase ones. The repo already knows this in two
// other places — the normalize codec declares a SystemInstructionSnake field,
// and the redaction adapter reads both spellings — but the gateway's own
// request path read camelCase only, so a protobuf-spelled request lost its
// system prompt and every generation parameter on the way to a non-Gemini
// target. Nothing errored; the request simply became a different request.
const (
	camelBody = `{
		"model": "gemini-2.5-flash",
		"systemInstruction": {"parts": [{"text": "ANSWER ONLY IN FRENCH"}]},
		"generationConfig": {"temperature": 0.1, "maxOutputTokens": 7, "thinkingConfig": {"thinkingBudget": 8192}},
		"contents": [{"role": "user", "parts": [{"text": "hi"}]}]
	}`
	snakeBody = `{
		"model": "gemini-2.5-flash",
		"system_instruction": {"parts": [{"text": "ANSWER ONLY IN FRENCH"}]},
		"generation_config": {"temperature": 0.1, "maxOutputTokens": 7, "thinkingConfig": {"thinkingBudget": 8192}},
		"contents": [{"role": "user", "parts": [{"text": "hi"}]}]
	}`
)

// TestHubIngress_BothSpellingsProduceTheSameCanonicalRequest is the assertion
// that matters: the two bodies mean the same thing to Google, so they must mean
// the same thing here.
//
// Comparing the two against EACH OTHER rather than against a hand-written
// expectation is deliberate — it cannot drift as the canonical shape evolves,
// and it fails for exactly one reason.
func TestHubIngress_BothSpellingsProduceTheSameCanonicalRequest(t *testing.T) {
	camel := canonicalise(t, camelBody)
	snake := canonicalise(t, snakeBody)

	// Positive control on EVERY field the fixture carries. The comparison below
	// catches divergence, not loss: dropping a field from both bodies keeps them
	// equal and keeps this test green. So each field is first shown to reach the
	// output at all.
	if sys := gjson.GetBytes(camel, `messages.#(role=="system").content`).String(); !strings.Contains(sys, "FRENCH") {
		t.Fatalf("the camelCase control lost the system prompt, so this test cannot see the defect it guards: %s", camel)
	}
	if v := gjson.GetBytes(camel, "temperature"); !v.Exists() {
		t.Fatalf("the camelCase control lost temperature: %s", camel)
	}
	if v := gjson.GetBytes(camel, "max_tokens"); !v.Exists() {
		t.Fatalf("the camelCase control lost maxOutputTokens: %s", camel)
	}
	if v := gjson.GetBytes(camel, "nexus.ext.gemini.thinking_config.thinkingBudget"); v.Int() != 8192 {
		t.Fatalf("the camelCase control lost thinkingConfig, so the snake comparison cannot see it either: %s", camel)
	}
	if string(camel) != string(snake) {
		t.Fatalf("the protobuf spelling produced a different request.\n camelCase: %s\n snake_case: %s", camel, snake)
	}
}

// TestHubIngress_SnakeCaseKeepsTheSystemPromptAndParameters names each thing
// that was being dropped, so a failure says which one came back.
func TestHubIngress_SnakeCaseKeepsTheSystemPromptAndParameters(t *testing.T) {
	out := canonicalise(t, snakeBody)

	if sys := gjson.GetBytes(out, `messages.#(role=="system").content`).String(); !strings.Contains(sys, "FRENCH") {
		t.Errorf("the system prompt was dropped, so the model answers with no instruction at all: %s", out)
	}
	if temp := gjson.GetBytes(out, "temperature"); !temp.Exists() || temp.Float() != 0.1 {
		t.Errorf("temperature was dropped; the request runs at the target's default: %s", out)
	}
	if mt := gjson.GetBytes(out, "max_tokens"); !mt.Exists() || mt.Int() != 7 {
		t.Errorf("maxOutputTokens was dropped; the request runs with no output cap: %s", out)
	}
}

// canonicalise runs the ingress converter the live path uses:
// executor_translate.go -> canonicalbridge.IngressChatToWire ->
// GenerateContentRequestToOpenAIChatCompletion.
func canonicalise(t *testing.T, body string) []byte {
	t.Helper()
	out, err := GenerateContentRequestToOpenAIChatCompletion([]byte(body), "gemini-2.5-flash")
	if err != nil {
		t.Fatalf("converting the request: %v", err)
	}
	return out
}

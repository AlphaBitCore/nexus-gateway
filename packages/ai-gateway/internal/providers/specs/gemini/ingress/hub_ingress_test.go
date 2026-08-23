package ingress

import (
	"testing"

	"github.com/tidwall/gjson"
)

// TestGenerateContentRequest_PreservesThinkingConfig.
//
// There are two legs into the gateway for a Gemini request and the thinking
// budget has to survive both. The normalizer reads it into the canonical; this
// converter is the other leg, and it copied only temperature, topP, topK,
// maxOutputTokens and stopSequences — so a caller asking for a thinking budget
// got a request with none, and the only sign was an answer arriving without
// the reasoning they paid for.
//
// The codec's read side keys off `nexus.ext.gemini.thinking_config` and nothing
// in the repo was setting it, so the round trip Gemini → canonical → Gemini
// dropped the field entirely.
func TestGenerateContentRequest_PreservesThinkingConfig(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
		`"generationConfig":{"temperature":0.4,"thinkingConfig":{"thinkingBudget":2048,"includeThoughts":true}}}`)

	out, err := GenerateContentRequestToOpenAIChatCompletion(body, "gemini-2.5-pro")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	got := gjson.GetBytes(out, "nexus.ext.gemini.thinking_config")
	if !got.Exists() {
		t.Fatalf("thinkingConfig did not survive the conversion; the egress codec reads it from "+
			"this key and nothing else sets it, so the caller's budget is gone: %s", out)
	}
	if b := got.Get("thinkingBudget").Int(); b != 2048 {
		t.Errorf("thinkingBudget = %d, want the caller's 2048", b)
	}
	if !got.Get("includeThoughts").Bool() {
		t.Error("includeThoughts was dropped; no other vendor states it on the request, so this " +
			"is the only place it is expressed")
	}
	// The ordinary sampling params must still come through — a change that
	// preserved the new field by replacing the old copy would pass a test that
	// only looked at the new one.
	if temp := gjson.GetBytes(out, "temperature").Float(); temp != 0.4 {
		t.Errorf("temperature = %v, want 0.4", temp)
	}
}

// -1 is Gemini for "you decide", a value neither other vendor has. Carried
// through unchanged: it is an expression, not an absent value, and normalising
// it away here would delete the request.
func TestGenerateContentRequest_PreservesADynamicThinkingBudget(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
		`"generationConfig":{"thinkingConfig":{"thinkingBudget":-1}}}`)

	out, err := GenerateContentRequestToOpenAIChatCompletion(body, "gemini-2.5-pro")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if b := gjson.GetBytes(out, "nexus.ext.gemini.thinking_config.thinkingBudget"); !b.Exists() || b.Int() != -1 {
		t.Fatalf("a dynamic budget was dropped or rewritten: %s", out)
	}
}

// A request that states no thinkingConfig must not gain one — an empty ext
// object would make every Gemini request look like it asked to reason.
func TestGenerateContentRequest_AddsNoThinkingConfigWhenNoneWasAsked(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
		`"generationConfig":{"temperature":0.4}}`)

	out, err := GenerateContentRequestToOpenAIChatCompletion(body, "gemini-2.5-pro")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if gjson.GetBytes(out, "nexus.ext.gemini.thinking_config").Exists() {
		t.Fatalf("a request that asked for no thinking gained a thinking_config: %s", out)
	}
}

func TestGenerateContentRequest_IdenticalCallsKeepDistinctIDsAndSignatures(t *testing.T) {
	body := []byte(`{"contents":[` +
		`{"role":"model","parts":[` +
		`{"functionCall":{"name":"lookup","args":{"q":"same"}},"thoughtSignature":"sig-a"},` +
		`{"functionCall":{"name":"lookup","args":{"q":"same"}},"thoughtSignature":"sig-b"}]},` +
		`{"role":"user","parts":[` +
		`{"functionResponse":{"name":"lookup","response":{"r":1}}},` +
		`{"functionResponse":{"name":"lookup","response":{"r":2}}}]}]}`)

	out, err := GenerateContentRequestToOpenAIChatCompletion(body, "gemini-2.5-pro")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	firstID := gjson.GetBytes(out, "messages.0.tool_calls.0.id").String()
	secondID := gjson.GetBytes(out, "messages.0.tool_calls.1.id").String()
	if firstID == "" || secondID == "" || firstID == secondID {
		t.Fatalf("identical calls need distinct coordinate IDs: %s", out)
	}
	if got := gjson.GetBytes(out, "messages.0.tool_calls.0.function.thought_signature").String(); got != "sig-a" {
		t.Fatalf("first signature=%q, want sig-a: %s", got, out)
	}
	if got := gjson.GetBytes(out, "messages.0.tool_calls.1.function.thought_signature").String(); got != "sig-b" {
		t.Fatalf("second signature=%q, want sig-b: %s", got, out)
	}
	if got := gjson.GetBytes(out, "messages.1.tool_call_id").String(); got != firstID {
		t.Fatalf("first response id=%q, want %q: %s", got, firstID, out)
	}
	if got := gjson.GetBytes(out, "messages.2.tool_call_id").String(); got != secondID {
		t.Fatalf("second response id=%q, want %q: %s", got, secondID, out)
	}

	replay, err := GenerateContentRequestToOpenAIChatCompletion(body, "gemini-2.5-pro")
	if err != nil {
		t.Fatalf("replay convert: %v", err)
	}
	if got := gjson.GetBytes(replay, "messages.0.tool_calls.0.id").String(); got != firstID {
		t.Fatalf("replay changed first id: %q vs %q", got, firstID)
	}
}

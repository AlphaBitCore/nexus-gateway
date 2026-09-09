package specutil

import (
	"testing"

	"github.com/tidwall/gjson"
)

// TestGeminiFirst_PrefersTheDocumentedSpelling pins the order, which is not
// arbitrary. A body carrying BOTH spellings is malformed — Google's own JSON
// parser takes one — and the gateway must be deterministic about which, or two
// requests with identical bytes could resolve differently across a refactor.
// camelCase wins because it is the documented spelling and what nearly every
// caller sends.
func TestGeminiFirst_PrefersTheDocumentedSpelling(t *testing.T) {
	root := gjson.Parse(`{
		"systemInstruction": {"parts": [{"text": "CAMEL"}]},
		"system_instruction": {"parts": [{"text": "SNAKE"}]}
	}`)

	got := GeminiFirst(root, GeminiSystemInstructionPaths)
	if !got.Exists() {
		t.Fatal("both spellings present and neither was found")
	}
	if text := got.Get("0.text").String(); text != "CAMEL" {
		t.Fatalf("got %q, want CAMEL — the documented spelling must win deterministically", text)
	}
}

// Each spelling alone resolves. This is the whole point of the helper: the
// protobuf form is otherwise dropped, taking the system prompt with it.
func TestGeminiFirst_EitherSpellingAlone(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"camelCase", `{"systemInstruction":{"parts":[{"text":"C"}]}}`, "C"},
		{"snake_case", `{"system_instruction":{"parts":[{"text":"S"}]}}`, "S"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := GeminiFirst(gjson.Parse(tc.body), GeminiSystemInstructionPaths)
			if text := got.Get("0.text").String(); text != tc.want {
				t.Fatalf("got %q, want %q", text, tc.want)
			}
		})
	}
}

// Absence must be reportable as absence. Callers branch on Exists(), so a
// helper that manufactured an empty-but-existing Result would make every caller
// take the present-branch on a body that carries nothing.
func TestGeminiFirst_AbsentStaysAbsent(t *testing.T) {
	root := gjson.Parse(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)

	if got := GeminiFirst(root, GeminiSystemInstructionPaths); got.Exists() {
		t.Fatalf("a body with no system instruction reported one: %v", got)
	}
	if got := GeminiFirst(root, GeminiGenerationConfigPaths); got.Exists() {
		t.Fatalf("a body with no generation config reported one: %v", got)
	}
}

// The generation-config paths resolve the same way. Dropping this object costs
// temperature, topP, topK, maxOutputTokens, stop sequences and the response
// schema all at once.
func TestGeminiFirst_GenerationConfigBothSpellings(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"camelCase", `{"generationConfig":{"temperature":0.25}}`},
		{"snake_case", `{"generation_config":{"temperature":0.25}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := GeminiFirst(gjson.Parse(tc.body), GeminiGenerationConfigPaths)
			if v := got.Get("temperature").Float(); v != 0.25 {
				t.Fatalf("temperature=%v, want 0.25 — dropping this object drops every generation parameter", v)
			}
		})
	}
}

// GeminiFirstBytes is the same contract for callers that hold a raw body — the
// estimator does, and it counts prompt characters that feed `auto` prompt-size
// routing, so a missed system instruction under-counts silently.
func TestGeminiFirstBytes_MatchesTheParsedForm(t *testing.T) {
	body := []byte(`{"system_instruction":{"parts":[{"text":"SNAKE"}]}}`)

	got := GeminiFirstBytes(body, GeminiSystemInstructionPaths)
	if text := got.Get("0.text").String(); text != "SNAKE" {
		t.Fatalf("got %q, want SNAKE", text)
	}
	if GeminiFirstBytes([]byte(`{"contents":[]}`), GeminiSystemInstructionPaths).Exists() {
		t.Fatal("a body with no system instruction reported one")
	}
}

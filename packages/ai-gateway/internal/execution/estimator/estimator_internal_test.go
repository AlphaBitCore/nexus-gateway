package estimator

import "testing"

// Whitebox unit tests for unexported helpers. The external `estimator_test`
// package only reaches them transitively via Estimate; targeted tests here
// close the small residuals (expandRange clamps, countCanonicalInputChars
// per-shape branches, lookupOutputBudget unknown-model fall-through).

// TestExpandRange_AnchorLessThanOne covers the `anchor < 1` clamp.
func TestExpandRange_AnchorLessThanOne(t *testing.T) {
	r := expandRange(0, 0)
	if r.Expected != 1 || r.Low != 1 || r.High != 3 {
		t.Errorf("got %+v; want Low=1 Expected=1 High=3 (anchor=0 clamps to 1)", r)
	}
}

// TestExpandRange_HighBelowAnchor covers the `high < anchor` re-clamp path —
// when maxOutput is between anchor and anchor*3 it caps high, but then we
// guarantee high >= anchor (range envelope cannot invert).
func TestExpandRange_HighBelowAnchor(t *testing.T) {
	// anchor=100, maxOutput=50 → high=300 → clamped to 50 → 50 < 100 → re-clamp to 100.
	r := expandRange(100, 50)
	if r.High != 100 {
		t.Errorf("high=%d; want 100 (re-clamped because maxOutput<anchor)", r.High)
	}
	if r.Expected != 100 {
		t.Errorf("expected=%d; want 100", r.Expected)
	}
}

// TestExpandRange_LowFloorAtOne covers the `low < 1` clamp when anchor/3 == 0
// (anchor=1 → low=0 → clamped to 1).
func TestExpandRange_LowFloorAtOne(t *testing.T) {
	r := expandRange(1, 0)
	if r.Low != 1 {
		t.Errorf("low=%d; want 1 (anchor=1 → anchor/3=0 → clamped to 1)", r.Low)
	}
}

// TestCountCanonicalInputChars_GeminiContents covers the Gemini
// generateContent shape (contents[].parts[].text) which the higher-level
// Estimate_* tests reach only indirectly.
func TestCountCanonicalInputChars_GeminiContents(t *testing.T) {
	body := []byte(`{"contents":[{"parts":[{"text":"hello"},{"text":"world"}]},{"parts":[{"text":"!!"}]}]}`)
	got := countCanonicalInputChars(body)
	if got != 12 {
		t.Errorf("got %d chars; want 12 (5+5+2)", got)
	}
}

// TestCountCanonicalInputChars_GeminiSystemInstruction covers the
// systemInstruction.parts[].text path.
func TestCountCanonicalInputChars_GeminiSystemInstruction(t *testing.T) {
	body := []byte(`{"systemInstruction":{"parts":[{"text":"You are helpful."}]}}`)
	got := countCanonicalInputChars(body)
	if got != 16 {
		t.Errorf("got %d chars; want 16", got)
	}
}

// TestCountCanonicalInputChars_AnthropicTopLevelSystem covers the
// top-level "system" key (string variant).
func TestCountCanonicalInputChars_AnthropicTopLevelSystem(t *testing.T) {
	body := []byte(`{"system":"You are a senior engineer.","messages":[]}`)
	got := countCanonicalInputChars(body)
	if got != 26 {
		t.Errorf("got %d chars; want 26 (len of \"You are a senior engineer.\")", got)
	}
}

// TestCountCanonicalInputChars_ResponsesAPI_StringInput covers the
// /v1/responses native shape where `input` is a plain string.
func TestCountCanonicalInputChars_ResponsesAPI_StringInput(t *testing.T) {
	body := []byte(`{"input":"hello world","instructions":"You are helpful."}`)
	got := countCanonicalInputChars(body)
	want := 11 + 16 // "hello world" + "You are helpful."
	if got != want {
		t.Errorf("got %d chars; want %d (input string + instructions)", got, want)
	}
}

// TestCountCanonicalInputChars_ResponsesAPI_StructuredInput covers the
// /v1/responses native shape where `input` is an array of structured
// items (message + input_text + function_call_output).
func TestCountCanonicalInputChars_ResponsesAPI_StructuredInput(t *testing.T) {
	body := []byte(`{"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"input_text","text":"ping"},
		{"type":"function_call_output","output":"done"}
	]}`)
	got := countCanonicalInputChars(body)
	want := 2 + 4 + 4 // hi + ping + done
	if got != want {
		t.Errorf("got %d chars; want %d (hi + ping + done)", got, want)
	}
}

// TestLookupOutputBudget_UnknownModel_ReturnsZeroFalse covers the
// supports=false fall-through that is the second uncovered branch.
func TestLookupOutputBudget_UnknownModel_ReturnsZeroFalse(t *testing.T) {
	anchor, supports := lookupOutputBudget("totally-unknown-model-xyz", "medium")
	if supports {
		t.Errorf("supports=true for unknown model; want false")
	}
	if anchor != 0 {
		t.Errorf("anchor=%d; want 0", anchor)
	}
}

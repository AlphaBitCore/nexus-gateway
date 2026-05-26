package proxy

import (
	"testing"
)

// TestValidateResponsesIngressForCrossFormat_PreviousResponseID pins the
// rejection for previous_response_id.
func TestValidateResponsesIngressForCrossFormat_PreviousResponseID(t *testing.T) {
	body := []byte(`{"model":"gpt-5.2","input":"follow up","previous_response_id":"resp_abc"}`)
	rej := validateResponsesIngressForCrossFormat(body)
	if rej == nil {
		t.Fatalf("expected rejection")
	}
	if rej.Param != "previous_response_id" {
		t.Errorf("Param = %q, want previous_response_id", rej.Param)
	}
}

// TestValidateResponsesIngressForCrossFormat_EmptyPreviousResponseID:
// previous_response_id present but empty string MUST NOT trigger
// rejection (defensive — equivalent to absent).
func TestValidateResponsesIngressForCrossFormat_EmptyPreviousResponseID(t *testing.T) {
	body := []byte(`{"model":"gpt-5.2","input":"hi","previous_response_id":""}`)
	if rej := validateResponsesIngressForCrossFormat(body); rej != nil {
		t.Errorf("empty previous_response_id should not reject, got %+v", rej)
	}
}

// TestValidateResponsesIngressForCrossFormat_StoreTrue pins store=true
// rejection. store=false is fine.
func TestValidateResponsesIngressForCrossFormat_StoreTrue(t *testing.T) {
	body := []byte(`{"model":"gpt-5.2","input":"hi","store":true}`)
	rej := validateResponsesIngressForCrossFormat(body)
	if rej == nil || rej.Param != "store" {
		t.Errorf("expected store rejection; got %+v", rej)
	}

	bodyFalse := []byte(`{"model":"gpt-5.2","input":"hi","store":false}`)
	if rej := validateResponsesIngressForCrossFormat(bodyFalse); rej != nil {
		t.Errorf("store=false should not reject, got %+v", rej)
	}
}

// TestValidateResponsesIngressForCrossFormat_TruncationAuto pins
// truncation = "auto" rejection; "disabled" passes; absent passes.
func TestValidateResponsesIngressForCrossFormat_Truncation(t *testing.T) {
	cases := []struct {
		body    string
		wantRej bool
	}{
		{`{"truncation":"auto"}`, true},
		{`{"truncation":"disabled"}`, false},
		{`{}`, false},
	}
	for _, c := range cases {
		rej := validateResponsesIngressForCrossFormat([]byte(c.body))
		if (rej != nil) != c.wantRej {
			t.Errorf("body=%s wantRej=%v got=%+v", c.body, c.wantRej, rej)
		}
		if c.wantRej && rej != nil && rej.Param != "truncation" {
			t.Errorf("body=%s rejection Param=%q, want truncation", c.body, rej.Param)
		}
	}
}

// TestValidateResponsesIngressForCrossFormat_BuiltinTool pins built-in
// tool rejection.
func TestValidateResponsesIngressForCrossFormat_BuiltinTool(t *testing.T) {
	body := []byte(`{
		"model": "gpt-5.2",
		"input": "search",
		"tools": [{"type":"web_search"}]
	}`)
	rej := validateResponsesIngressForCrossFormat(body)
	if rej == nil {
		t.Fatalf("expected built-in tool rejection")
	}
	if rej.Param != "tools[0].type" {
		t.Errorf("Param = %q, want tools[0].type", rej.Param)
	}
}

// TestValidateResponsesIngressForCrossFormat_FunctionToolAllowed pins
// that caller-defined function tools (type=function) DO pass — they
// round-trip through canonical chat-completions just fine.
func TestValidateResponsesIngressForCrossFormat_FunctionToolAllowed(t *testing.T) {
	body := []byte(`{
		"model": "gpt-5.2",
		"input": "weather",
		"tools": [{"type":"function","name":"get_weather","parameters":{}}]
	}`)
	if rej := validateResponsesIngressForCrossFormat(body); rej != nil {
		t.Errorf("function tool should NOT reject, got %+v", rej)
	}
}

// TestValidateResponsesIngressForCrossFormat_BuiltinAtIndex2 pins that
// the rejected Param correctly identifies the array index of the
// offending built-in tool (auditability).
func TestValidateResponsesIngressForCrossFormat_BuiltinAtIndex2(t *testing.T) {
	body := []byte(`{
		"tools": [
			{"type":"function","name":"f1"},
			{"type":"function","name":"f2"},
			{"type":"file_search"}
		]
	}`)
	rej := validateResponsesIngressForCrossFormat(body)
	if rej == nil {
		t.Fatalf("expected rejection for tools[2]")
	}
	if rej.Param != "tools[2].type" {
		t.Errorf("Param = %q, want tools[2].type", rej.Param)
	}
}

// TestValidateResponsesIngressForCrossFormat_NoRejection pins that a
// safe Responses request body (no stateful fields, no built-ins) returns
// nil — the guard must not produce false positives.
func TestValidateResponsesIngressForCrossFormat_NoRejection(t *testing.T) {
	body := []byte(`{
		"model": "gpt-5.2",
		"instructions": "Be terse.",
		"input": "Hello",
		"max_output_tokens": 100,
		"reasoning": {"effort": "high"},
		"tools": [{"type":"function","name":"get_x","parameters":{}}],
		"temperature": 0.5,
		"stream": true
	}`)
	if rej := validateResponsesIngressForCrossFormat(body); rej != nil {
		t.Errorf("safe body unexpectedly rejected: %+v", rej)
	}
}

// TestValidateResponsesIngressForCrossFormat_EmptyAndInvalid pins
// defensive behavior on edge inputs.
func TestValidateResponsesIngressForCrossFormat_EmptyAndInvalid(t *testing.T) {
	if rej := validateResponsesIngressForCrossFormat(nil); rej != nil {
		t.Errorf("nil body should not reject; got %+v", rej)
	}
	if rej := validateResponsesIngressForCrossFormat([]byte{}); rej != nil {
		t.Errorf("empty body should not reject; got %+v", rej)
	}
	if rej := validateResponsesIngressForCrossFormat([]byte("not json")); rej != nil {
		t.Errorf("invalid JSON should not reject (let upstream surface the parse error); got %+v", rej)
	}
}

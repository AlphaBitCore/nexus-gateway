// packages/ai-gateway/internal/policy/aiguard/decoder_test.go
package aiguard

import (
	"strings"
	"testing"
)

func TestDecode_StrictJSON(t *testing.T) {
	raw := `{"decision":"reject_hard","confidence":0.91,"reason":"x","labels":["a","b"]}`
	r, err := DecodeJudgeOutput(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if r.Decision != "reject_hard" || r.Confidence != 0.91 {
		t.Fatalf("wrong fields: %+v", r)
	}
}

func TestDecode_RedactionsParsed(t *testing.T) {
	raw := `{"decision":"modify","reason":"email PII",
	        "redactions":[
	          {"start":14,"end":31,"replacement":"[REDACTED_EMAIL]","action":"redact","reason":"pii:email"}
	        ]}`
	r, err := DecodeJudgeOutput(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(r.Redactions) != 1 {
		t.Fatalf("redactions = %d, want 1", len(r.Redactions))
	}
	red := r.Redactions[0]
	if red.Start != 14 || red.End != 31 || red.Replacement != "[REDACTED_EMAIL]" || red.Action != "redact" {
		t.Errorf("redaction wrong: %+v", red)
	}
}

func TestDecode_RedactionsDefaultActionAndSort(t *testing.T) {
	// Two redactions out-of-order; action missing on second one; one invalid.
	raw := `{"decision":"modify",
	        "redactions":[
	          {"start":40,"end":50,"replacement":"[X]"},
	          {"start":0,"end":-3,"replacement":"bad"},
	          {"start":10,"end":15,"replacement":"[Y]","action":"weird"}
	        ]}`
	r, err := DecodeJudgeOutput(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(r.Redactions) != 2 {
		t.Fatalf("expected 2 valid redactions after sanitize, got %d: %+v", len(r.Redactions), r.Redactions)
	}
	// Sorted ascending by Start.
	if r.Redactions[0].Start != 10 || r.Redactions[1].Start != 40 {
		t.Errorf("not sorted: %+v", r.Redactions)
	}
	// Action defaulted from "" and from "weird".
	if r.Redactions[0].Action != "redact" || r.Redactions[1].Action != "redact" {
		t.Errorf("action default wrong: %+v", r.Redactions)
	}
}

func TestDecode_UnwrapsMarkdownFence(t *testing.T) {
	// Anthropic sometimes wraps JSON in ```json ... ``` fences.
	raw := "Here is my analysis:\n```json\n" +
		`{"decision":"approve","labels":["clean"]}` +
		"\n```\n"
	r, err := DecodeJudgeOutput(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if r.Decision != "approve" {
		t.Fatalf("decision: got %q", r.Decision)
	}
}

func TestDecode_RejectsInvalidDecision(t *testing.T) {
	_, err := DecodeJudgeOutput(`{"decision":"block"}`)
	if err == nil || !strings.Contains(err.Error(), "invalid decision") {
		t.Fatalf("expected invalid decision error, got %v", err)
	}
}

func TestDecode_NormalizesLabels(t *testing.T) {
	r, err := DecodeJudgeOutput(`{"decision":"approve","labels":["  A  ","A","b"]}`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "b"}
	if len(r.Labels) != len(want) || r.Labels[0] != want[0] || r.Labels[1] != want[1] {
		t.Fatalf("labels got %v, want %v", r.Labels, want)
	}
}

func TestDecode_ClampsConfidenceRange(t *testing.T) {
	r, _ := DecodeJudgeOutput(`{"decision":"approve","confidence":1.7}`)
	if r.Confidence != 1.0 {
		t.Errorf("want clamp to 1.0, got %f", r.Confidence)
	}
	r, _ = DecodeJudgeOutput(`{"decision":"approve","confidence":-0.5}`)
	if r.Confidence != 0.0 {
		t.Errorf("want clamp to 0.0, got %f", r.Confidence)
	}
}

func TestDecode_Garbage(t *testing.T) {
	_, err := DecodeJudgeOutput("not json at all")
	if err == nil {
		t.Fatal("expected parse error for non-JSON")
	}
}

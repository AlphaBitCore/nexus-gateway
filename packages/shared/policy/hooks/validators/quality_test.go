package validators

import (
	"context"
	"testing"
)

func TestQualityChecker_Approve(t *testing.T) {
	cfg := &HookConfig{
		Config: map[string]any{},
	}
	hook, err := NewQualityChecker(cfg)
	if err != nil {
		t.Fatal(err)
	}

	input := &HookInput{
		Stage:        "response",
		FinishReason: "stop",
		Normalized:   PayloadFromTextSegments([]string{"This is a sufficiently long response."}),
	}

	hr, err := hook.Execute(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if hr.Decision != Approve {
		t.Errorf("expected APPROVE, got %s", hr.Decision)
	}
}

func TestQualityChecker_ShortResponse(t *testing.T) {
	cfg := &HookConfig{
		Config: map[string]any{"onMatch": map[string]any{"action": "block"}},
	}
	hook, err := NewQualityChecker(cfg)
	if err != nil {
		t.Fatal(err)
	}

	input := &HookInput{
		Stage:        "response",
		FinishReason: "stop",
		Normalized:   PayloadFromTextSegments([]string{"Hi"}),
	}

	hr, err := hook.Execute(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if hr.Decision != RejectHard {
		t.Errorf("expected REJECT_HARD for short response, got %s", hr.Decision)
	}
}

func TestQualityChecker_RefusalDetected(t *testing.T) {
	cfg := &HookConfig{
		Config: map[string]any{"onMatch": map[string]any{"action": "block"}},
	}
	hook, err := NewQualityChecker(cfg)
	if err != nil {
		t.Fatal(err)
	}

	input := &HookInput{
		Stage:        "response",
		FinishReason: "stop",
		Normalized:   PayloadFromTextSegments([]string{"I can't help with that request, as an AI I have limitations."}),
	}

	hr, err := hook.Execute(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if hr.Decision != RejectHard {
		t.Errorf("expected REJECT_HARD for refusal, got %s", hr.Decision)
	}
}

func TestQualityChecker_UnexpectedFinishReason(t *testing.T) {
	cfg := &HookConfig{
		Config: map[string]any{"onMatch": map[string]any{"action": "block"}},
	}
	hook, err := NewQualityChecker(cfg)
	if err != nil {
		t.Fatal(err)
	}

	input := &HookInput{
		Stage:        "response",
		FinishReason: "content_filter",
		Normalized:   PayloadFromTextSegments([]string{"This response was cut off because of content filtering."}),
	}

	hr, err := hook.Execute(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if hr.Decision != RejectHard {
		t.Errorf("expected REJECT_HARD for content_filter, got %s", hr.Decision)
	}
}

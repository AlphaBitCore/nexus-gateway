package core

import (
	"strings"
	"testing"
)

func TestApplySpans_SingleRedact(t *testing.T) {
	p := NormalizedPayload{
		Kind: KindAIChat,
		Messages: []Message{{
			Role:    RoleUser,
			Content: []ContentBlock{{Type: ContentText, Text: "hello world"}},
		}},
	}
	spans := []TransformSpan{{
		Source:         SourceHook,
		SourceID:       "test",
		Action:         ActionRedact,
		ContentAddress: "messages.0.content.0",
		Start:          6,
		End:            11,
		Replacement:    "[REDACTED]",
	}}
	got, skipped := ApplySpans(p, spans)
	if len(skipped) != 0 {
		t.Errorf("skipped: %+v", skipped)
	}
	want := "hello [REDACTED]"
	if got.Messages[0].Content[0].Text != want {
		t.Errorf("got %q, want %q", got.Messages[0].Content[0].Text, want)
	}
	// Original unchanged.
	if p.Messages[0].Content[0].Text != "hello world" {
		t.Errorf("original mutated: %q", p.Messages[0].Content[0].Text)
	}
}

func TestApplySpans_MultipleDescendingOrder(t *testing.T) {
	p := NormalizedPayload{
		Kind: KindAIChat,
		Messages: []Message{{
			Role:    RoleUser,
			Content: []ContentBlock{{Type: ContentText, Text: "alice and bob and carol"}},
		}},
	}
	// Two spans: bob (10-13) and alice (0-5). Apply in descending order so
	// alice's offsets stay valid.
	spans := []TransformSpan{
		{Source: SourceHook, ContentAddress: "messages.0.content.0", Start: 0, End: 5, Replacement: "[X]"},
		{Source: SourceHook, ContentAddress: "messages.0.content.0", Start: 10, End: 13, Replacement: "[Y]"},
	}
	got, _ := ApplySpans(p, spans)
	want := "[X] and [Y] and carol"
	if got.Messages[0].Content[0].Text != want {
		t.Errorf("got %q, want %q", got.Messages[0].Content[0].Text, want)
	}
}

func TestApplySpans_OutOfRangeSkipped(t *testing.T) {
	p := NormalizedPayload{
		Kind: KindAIChat,
		Messages: []Message{{
			Role:    RoleUser,
			Content: []ContentBlock{{Type: ContentText, Text: "short"}},
		}},
	}
	spans := []TransformSpan{
		{Source: SourceHook, ContentAddress: "messages.0.content.0", Start: 0, End: 3, Replacement: "[X]"},
		{Source: SourceHook, ContentAddress: "messages.99.content.0", Start: 0, End: 1, Replacement: "[Y]"},
	}
	got, skipped := ApplySpans(p, spans)
	if got.Messages[0].Content[0].Text != "[X]rt" {
		t.Errorf("expected [X]rt, got %q", got.Messages[0].Content[0].Text)
	}
	if len(skipped) != 1 || !strings.HasPrefix(skipped[0].ContentAddress, "messages.99") {
		t.Errorf("expected one skipped span on messages.99, got %+v", skipped)
	}
}

func TestApplySpans_HTTPBodyView(t *testing.T) {
	p := NormalizedPayload{
		Kind: KindHTTPText,
		HTTP: &HTTPPayload{BodyView: &HTTPBodyView{Text: "secret token here"}},
	}
	spans := []TransformSpan{{
		Source:         SourceHook,
		ContentAddress: "http.bodyView",
		Start:          7,
		End:            12,
		Replacement:    "[REDACTED]",
	}}
	got, _ := ApplySpans(p, spans)
	if got.HTTP.BodyView.Text != "secret [REDACTED] here" {
		t.Errorf("http.bodyView wrong: %q", got.HTTP.BodyView.Text)
	}
}

func TestApplySpans_InjectAction(t *testing.T) {
	p := NormalizedPayload{
		Kind: KindAIChat,
		Messages: []Message{{
			Role:    RoleUser,
			Content: []ContentBlock{{Type: ContentText, Text: "hello"}},
		}},
	}
	spans := []TransformSpan{{
		Source:         SourceCacheControlInject,
		Action:         ActionInject,
		ContentAddress: "messages.0.content.0",
		Start:          5,
		End:            5,
		Replacement:    "[CACHE]",
	}}
	got, _ := ApplySpans(p, spans)
	if got.Messages[0].Content[0].Text != "hello[CACHE]" {
		t.Errorf("inject wrong: %q", got.Messages[0].Content[0].Text)
	}
}

func TestApplySpans_EmptySpans_ReturnsClone(t *testing.T) {
	p := NormalizedPayload{
		Kind:     KindAIChat,
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{{Type: ContentText, Text: "x"}}}},
	}
	got, skipped := ApplySpans(p, nil)
	if skipped != nil {
		t.Errorf("expected nil skipped, got %v", skipped)
	}
	if got.Messages[0].Content[0].Text != "x" {
		t.Errorf("text changed: %q", got.Messages[0].Content[0].Text)
	}
	got.Messages[0].Content[0].Text = "mutated"
	if p.Messages[0].Content[0].Text != "x" {
		t.Errorf("clone is not independent")
	}
}

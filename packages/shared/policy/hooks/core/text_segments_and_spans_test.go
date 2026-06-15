package core

import (
	"testing"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

func TestTextSegments_NilReceiverReturnsNil(t *testing.T) {
	var input *HookInput
	if got := input.TextSegments(); got != nil {
		t.Errorf("nil receiver: got %v want nil", got)
	}
}

func TestTextSegments_NilNormalizedReturnsNil(t *testing.T) {
	input := &HookInput{Stage: "request"} // Normalized intentionally nil
	if got := input.TextSegments(); got != nil {
		t.Errorf("nil Normalized: got %v want nil", got)
	}
}

func TestTextSegmentsWith_NilReceiverReturnsNil(t *testing.T) {
	var input *HookInput
	if got := input.TextSegmentsWith(normalize.TextProjectionOptions{}); got != nil {
		t.Errorf("nil receiver: got %v want nil", got)
	}
}

func TestTextSegmentsWith_NilNormalizedReturnsNil(t *testing.T) {
	input := &HookInput{}
	if got := input.TextSegmentsWith(normalize.TextProjectionOptions{}); got != nil {
		t.Errorf("nil Normalized: got %v want nil", got)
	}
}

func TestTextSegmentsWith_IncludeReasoningPicksUpReasoningBlocks(t *testing.T) {
	// Default projection skips ContentReasoning. include_reasoning opts-in.
	payload := &normalize.NormalizedPayload{
		Kind:             normalize.KindAIChat,
		NormalizeVersion: normalize.SchemaVersion,
		Messages: []normalize.Message{{
			Role: normalize.RoleAssistant,
			Content: []normalize.ContentBlock{
				{Type: normalize.ContentText, Text: "visible"},
				{Type: normalize.ContentReasoning, Text: "thinking out loud"},
			},
		}},
	}
	in := &HookInput{Normalized: payload}

	defSegs := in.TextSegments()
	if len(defSegs) != 1 || defSegs[0] != "visible" {
		t.Errorf("default scope should skip reasoning; got %v", defSegs)
	}

	withReason := in.TextSegmentsWith(normalize.TextProjectionOptions{IncludeReasoning: true})
	// Reasoning should now be included alongside visible text.
	found := false
	for _, s := range withReason {
		if s == "thinking out loud" {
			found = true
		}
	}
	if !found {
		t.Errorf("include_reasoning should include reasoning text; got %v", withReason)
	}
}

func TestProjectionOptions_NilReceiverReturnsZero(t *testing.T) {
	var c *HookConfig
	got := c.ProjectionOptions()
	if got.IncludeReasoning {
		t.Errorf("nil receiver should yield zero-value opts; got %+v", got)
	}
}

func TestProjectionOptions_DefaultScopeZero(t *testing.T) {
	c := &HookConfig{Scope: ""}
	got := c.ProjectionOptions()
	if got.IncludeReasoning {
		t.Errorf("default scope should NOT include reasoning; got %+v", got)
	}
}

func TestProjectionOptions_IncludeReasoningScope(t *testing.T) {
	c := &HookConfig{Scope: "include_reasoning"}
	got := c.ProjectionOptions()
	if !got.IncludeReasoning {
		t.Errorf("include_reasoning scope must set IncludeReasoning=true; got %+v", got)
	}
}

func TestProjectionOptions_UnknownScopeFallsBackToZero(t *testing.T) {
	// Unknown scope is forward-compat: must not error, must return zero.
	c := &HookConfig{Scope: "future-scope-value"}
	got := c.ProjectionOptions()
	if got.IncludeReasoning {
		t.Errorf("unknown scope must be inert; got %+v", got)
	}
}

func TestPayloadFromTextSegments_EmptyReturnsEmptyPayload(t *testing.T) {
	// Empty segments → an empty payload with Kind set, no Messages.
	p := PayloadFromTextSegments(nil)
	if p == nil {
		t.Fatal("nil payload returned")
		return
	}
	if p.Kind != normalize.KindAIChat {
		t.Errorf("Kind: %s want ai-chat", p.Kind)
	}
	if len(p.Messages) != 0 {
		t.Errorf("Messages: got %d entries, want 0", len(p.Messages))
	}
}

func TestPayloadFromTextSegments_SchemaVersionStamped(t *testing.T) {
	p := PayloadFromTextSegments([]string{"hi"})
	if p.NormalizeVersion != normalize.SchemaVersion {
		t.Errorf("NormalizeVersion: %s want %s", p.NormalizeVersion, normalize.SchemaVersion)
	}
	if p.Protocol != "synthetic" {
		t.Errorf("Protocol: %s want synthetic", p.Protocol)
	}
	if len(p.Messages) != 1 || p.Messages[0].Role != normalize.RoleUser {
		t.Errorf("expected single user-role message; got %+v", p.Messages)
	}
}

func TestSpansFromModifiedContent_NilInputReturnsNil(t *testing.T) {
	if got := SpansFromModifiedContent(nil, []ContentBlock{{Text: "x"}},
		normalize.SourceHook, "id", normalize.ActionRedact); got != nil {
		t.Errorf("nil input: got %v want nil", got)
	}
}

func TestSpansFromModifiedContent_NilNormalizedReturnsNil(t *testing.T) {
	if got := SpansFromModifiedContent(&HookInput{}, []ContentBlock{{Text: "x"}},
		normalize.SourceHook, "id", normalize.ActionRedact); got != nil {
		t.Errorf("nil Normalized: got %v want nil", got)
	}
}

func TestSpansFromModifiedContent_EmptyModifiedReturnsNil(t *testing.T) {
	in := &HookInput{Normalized: PayloadFromTextSegments([]string{"x"})}
	if got := SpansFromModifiedContent(in, nil,
		normalize.SourceHook, "id", normalize.ActionRedact); got != nil {
		t.Errorf("empty modified: got %v want nil", got)
	}
}

func TestSpansFromModifiedContent_EmptyOriginalReturnsNil(t *testing.T) {
	// Original empty (zero ContentText/ContentToolResult blocks) → return nil.
	in := &HookInput{Normalized: &normalize.NormalizedPayload{
		Kind:             normalize.KindAIChat,
		NormalizeVersion: normalize.SchemaVersion,
		Messages: []normalize.Message{{
			Role:    normalize.RoleUser,
			Content: []normalize.ContentBlock{{Type: normalize.ContentReasoning, Text: "skip me"}},
		}},
	}}
	got := SpansFromModifiedContent(in, []ContentBlock{{Text: "anything"}},
		normalize.SourceHook, "id", normalize.ActionRedact)
	if got != nil {
		t.Errorf("empty original projection should return nil; got %v", got)
	}
}

func TestSpansFromModifiedContent_DiffEmitsOneSpanPerChange(t *testing.T) {
	in := &HookInput{Normalized: &normalize.NormalizedPayload{
		Kind:             normalize.KindAIChat,
		NormalizeVersion: normalize.SchemaVersion,
		Messages: []normalize.Message{{
			Role: normalize.RoleUser,
			Content: []normalize.ContentBlock{
				{Type: normalize.ContentText, Text: "alpha"},
				{Type: normalize.ContentText, Text: "beta"},
				{Type: normalize.ContentText, Text: "gamma"},
			},
		}},
	}}
	modified := []ContentBlock{
		{Text: "alpha"},        // unchanged
		{Text: "BETA-CHANGED"}, // changed
		{Text: "gamma"},        // unchanged
	}
	spans := SpansFromModifiedContent(in, modified,
		normalize.SourceHook, "rule-1", normalize.ActionRedact)
	if len(spans) != 1 {
		t.Fatalf("len(spans) = %d, want 1 (one changed block)", len(spans))
	}
	s := spans[0]
	if s.Source != normalize.SourceHook {
		t.Errorf("Source: %s", s.Source)
	}
	if s.SourceID != "rule-1" {
		t.Errorf("SourceID: %q", s.SourceID)
	}
	if s.Action != normalize.ActionRedact {
		t.Errorf("Action: %s", s.Action)
	}
	if s.ContentAddress != "messages.0.content.1" {
		t.Errorf("ContentAddress: %q want messages.0.content.1", s.ContentAddress)
	}
	if s.Start != 0 || s.End != len("beta") {
		t.Errorf("offsets: (%d,%d) want (0,%d)", s.Start, s.End, len("beta"))
	}
	if s.Replacement != "BETA-CHANGED" {
		t.Errorf("Replacement: %q", s.Replacement)
	}
}

func TestSpansFromModifiedContent_ToolResultBlockAddressed(t *testing.T) {
	// ContentToolResult blocks contribute to the projection; if modified,
	// their span address ends in ".toolResult".
	in := &HookInput{Normalized: &normalize.NormalizedPayload{
		Kind:             normalize.KindAIChat,
		NormalizeVersion: normalize.SchemaVersion,
		Messages: []normalize.Message{{
			Role: normalize.RoleUser,
			Content: []normalize.ContentBlock{
				{Type: normalize.ContentToolResult, ToolResult: &normalize.ToolResult{Output: "secret"}},
			},
		}},
	}}
	modified := []ContentBlock{{Text: "[REDACTED]"}}
	spans := SpansFromModifiedContent(in, modified,
		normalize.SourceHook, "rule", normalize.ActionRedact)
	if len(spans) != 1 {
		t.Fatalf("len(spans) = %d, want 1", len(spans))
	}
	if spans[0].ContentAddress != "messages.0.content.0.toolResult" {
		t.Errorf("ContentAddress: %q want messages.0.content.0.toolResult", spans[0].ContentAddress)
	}
}

func TestSpansFromModifiedContent_NonTextContentSkipped(t *testing.T) {
	// Non-text / non-tool-result blocks (reasoning, tool_use) are not in the
	// projection and must not consume a modified slot.
	in := &HookInput{Normalized: &normalize.NormalizedPayload{
		Kind:             normalize.KindAIChat,
		NormalizeVersion: normalize.SchemaVersion,
		Messages: []normalize.Message{{
			Role: normalize.RoleUser,
			Content: []normalize.ContentBlock{
				{Type: normalize.ContentReasoning, Text: "thinking"}, // skipped
				{Type: normalize.ContentText, Text: "real-text"},
			},
		}},
	}}
	modified := []ContentBlock{{Text: "[X]"}}
	spans := SpansFromModifiedContent(in, modified,
		normalize.SourceHook, "r", normalize.ActionRedact)
	if len(spans) != 1 {
		t.Fatalf("len(spans) = %d, want 1", len(spans))
	}
	// The span must address the actual text block (index 1), not the
	// skipped reasoning block (index 0).
	if spans[0].ContentAddress != "messages.0.content.1" {
		t.Errorf("address: %q want messages.0.content.1 (reasoning block skipped)",
			spans[0].ContentAddress)
	}
}

func TestSpansFromModifiedContent_OriginalLongerThanModifiedClamps(t *testing.T) {
	// If original has more text blocks than modified, walk stops at limit
	// (= len(modified)) without panicking and without emitting extra spans.
	in := &HookInput{Normalized: &normalize.NormalizedPayload{
		Kind:             normalize.KindAIChat,
		NormalizeVersion: normalize.SchemaVersion,
		Messages: []normalize.Message{{
			Role: normalize.RoleUser,
			Content: []normalize.ContentBlock{
				{Type: normalize.ContentText, Text: "a"},
				{Type: normalize.ContentText, Text: "b"},
				{Type: normalize.ContentText, Text: "c"}, // out of range
			},
		}},
	}}
	modified := []ContentBlock{{Text: "X"}, {Text: "Y"}}
	spans := SpansFromModifiedContent(in, modified,
		normalize.SourceHook, "r", normalize.ActionRedact)
	if len(spans) != 2 {
		t.Errorf("len(spans) = %d, want 2 (limited to modified)", len(spans))
	}
}

func TestSpansFromModifiedContent_NoChangesEmitsNoSpans(t *testing.T) {
	in := &HookInput{Normalized: &normalize.NormalizedPayload{
		Kind:             normalize.KindAIChat,
		NormalizeVersion: normalize.SchemaVersion,
		Messages: []normalize.Message{{
			Role: normalize.RoleUser,
			Content: []normalize.ContentBlock{
				{Type: normalize.ContentText, Text: "unchanged"},
			},
		}},
	}}
	modified := []ContentBlock{{Text: "unchanged"}}
	spans := SpansFromModifiedContent(in, modified,
		normalize.SourceHook, "r", normalize.ActionRedact)
	if len(spans) != 0 {
		t.Errorf("no diff should yield no spans; got %v", spans)
	}
}

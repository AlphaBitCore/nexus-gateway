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

// The defining invariant for AppliedSpanOffsets: for every returned span,
// slicing the ACTUAL post-redact text at [Start, End) yields its Replacement.
// This is what lets a UI mark each redaction inline against the stored payload.
func TestAppliedSpanOffsets_SliceYieldsReplacement(t *testing.T) {
	p := NormalizedPayload{
		Kind: KindAIChat,
		Messages: []Message{{
			Role:    RoleUser,
			Content: []ContentBlock{{Type: ContentText, Text: "alice and bob and carol"}},
		}},
	}
	// Deliberately reverse order + length-changing replacements so the per-block
	// cumulative-offset math is exercised (not the trivial single-span case).
	spans := []TransformSpan{
		{Source: SourceHook, ContentAddress: "messages.0.content.0", Start: 10, End: 13, Replacement: "[BOBBY]"},
		{Source: SourceHook, ContentAddress: "messages.0.content.0", Start: 0, End: 5, Replacement: "[A]"},
	}
	applied, _ := ApplySpans(p, spans)
	text := applied.Messages[0].Content[0].Text

	adj := AppliedSpanOffsets(p, spans)
	if len(adj) != 2 {
		t.Fatalf("want 2 adjusted spans, got %d: %+v", len(adj), adj)
	}
	for _, s := range adj {
		if s.Start < 0 || s.End > len(text) || s.Start > s.End {
			t.Fatalf("span [%d,%d) out of range for text %q", s.Start, s.End, text)
		}
		if text[s.Start:s.End] != s.Replacement {
			t.Errorf("text[%d:%d] = %q, want replacement %q (full text %q)",
				s.Start, s.End, text[s.Start:s.End], s.Replacement, text)
		}
	}
}

func TestAppliedSpanOffsets_SingleSpanUnshifted(t *testing.T) {
	p := NormalizedPayload{
		Kind:     KindAIChat,
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{{Type: ContentText, Text: "hello world"}}}},
	}
	spans := []TransformSpan{{Source: SourceHook, ContentAddress: "messages.0.content.0", Start: 6, End: 11, Replacement: "[REDACTED]"}}
	adj := AppliedSpanOffsets(p, spans)
	if len(adj) != 1 || adj[0].Start != 6 || adj[0].End != 6+len("[REDACTED]") {
		t.Fatalf("single span: got %+v, want [6,%d)", adj, 6+len("[REDACTED]"))
	}
}

func TestAppliedSpanOffsets_SkipsUnresolvableAndOutOfRange(t *testing.T) {
	p := NormalizedPayload{
		Kind:     KindAIChat,
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{{Type: ContentText, Text: "hi"}}}},
	}
	spans := []TransformSpan{
		{ContentAddress: "messages.5.content.0", Start: 0, End: 1, Replacement: "[X]"}, // address out of range → skip
		{ContentAddress: "messages.0.content.0", Start: 9, End: 9, Replacement: "[Y]"}, // start beyond text → skip
	}
	if got := AppliedSpanOffsets(p, spans); got != nil {
		t.Errorf("unresolvable/out-of-range spans must yield nil, got %+v", got)
	}
	if got := AppliedSpanOffsets(p, nil); got != nil {
		t.Errorf("empty spans must yield nil, got %+v", got)
	}
}

// Covers every resolveTextLen rejection branch — each malformed/unresolvable
// address must drop its span so no phantom badge is emitted.
func TestAppliedSpanOffsets_MalformedAddressesAllSkip(t *testing.T) {
	p := NormalizedPayload{
		Kind:     KindHTTPForm,
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{{Type: ContentText, Text: "hello"}}}},
		HTTP:     &HTTPPayload{BodyView: &HTTPBodyView{Form: map[string]string{"a": "b"}}},
	}
	for _, addr := range []string{
		"messages.0",                      // too few parts
		"messages.x.content.0",            // non-numeric message index
		"messages.0.content.y",            // non-numeric content index
		"messages.0.content.0.bogus",      // 5th part not toolResult
		"messages.0.content.0.toolResult", // text block has no tool result
		"http.headers",                    // unknown http sub-path
		"http.bodyView.form.missing",      // form key absent
		"unknown.address",                 // unknown root
	} {
		spans := []TransformSpan{{ContentAddress: addr, Start: 0, End: 1, Replacement: "[X]"}}
		if got := AppliedSpanOffsets(p, spans); got != nil {
			t.Errorf("address %q must skip, got %+v", addr, got)
		}
	}
}

// Exercises every resolveTextLen address form (tool_result, http.bodyView,
// http.bodyView.form.<key>) so each kind's spans relocate correctly, and
// confirms the slice-yields-replacement invariant holds across address types.
func TestAppliedSpanOffsets_AllAddressForms(t *testing.T) {
	check := func(t *testing.T, p NormalizedPayload, getText func(NormalizedPayload) string, spans []TransformSpan) {
		t.Helper()
		applied, _ := ApplySpans(p, spans)
		text := getText(applied)
		adj := AppliedSpanOffsets(p, spans)
		if len(adj) != len(spans) {
			t.Fatalf("want %d adjusted spans, got %d", len(spans), len(adj))
		}
		for _, s := range adj {
			if text[s.Start:s.End] != s.Replacement {
				t.Errorf("text[%d:%d]=%q want %q (text %q)", s.Start, s.End, text[s.Start:s.End], s.Replacement, text)
			}
		}
	}

	t.Run("tool_result", func(t *testing.T) {
		p := NormalizedPayload{Kind: KindAIChat, Messages: []Message{{
			Role:    RoleTool,
			Content: []ContentBlock{{Type: ContentToolResult, ToolResult: &ToolResult{Output: "secret token here"}}},
		}}}
		check(t, p, func(pp NormalizedPayload) string { return pp.Messages[0].Content[0].ToolResult.Output },
			[]TransformSpan{{ContentAddress: "messages.0.content.0.toolResult", Start: 7, End: 12, Replacement: "[TOK]"}})
	})

	t.Run("http_body_view", func(t *testing.T) {
		p := NormalizedPayload{Kind: KindHTTPText, HTTP: &HTTPPayload{BodyView: &HTTPBodyView{Text: "key=topsecret&x=1"}}}
		check(t, p, func(pp NormalizedPayload) string { return pp.HTTP.BodyView.Text },
			[]TransformSpan{{ContentAddress: "http.bodyView", Start: 4, End: 13, Replacement: "[R]"}})
	})

	t.Run("http_body_view_form", func(t *testing.T) {
		p := NormalizedPayload{Kind: KindHTTPForm, HTTP: &HTTPPayload{BodyView: &HTTPBodyView{Form: map[string]string{"password": "hunter2"}}}}
		check(t, p, func(pp NormalizedPayload) string { return pp.HTTP.BodyView.Form["password"] },
			[]TransformSpan{{ContentAddress: "http.bodyView.form.password", Start: 0, End: 7, Replacement: "[PW]"}})
	})
}

// TestApplySpans_EmbeddingInputs covers the KindAIEmbedding address grammar:
// hooks address embedding text as "inputs.<i>" (the payload carries text in
// Inputs, not Messages). The span must apply, and AppliedSpanOffsets must
// relocate it so slicing the redacted input yields the replacement.
func TestApplySpans_EmbeddingInputs(t *testing.T) {
	p := NormalizedPayload{
		Kind:   KindAIEmbedding,
		Inputs: []string{"contact leak@example.com for access", "clean input"},
	}
	spans := []TransformSpan{{
		Source:         SourceHook,
		SourceID:       "email",
		Action:         ActionRedact,
		ContentAddress: "inputs.0",
		Start:          8,
		End:            24,
		Replacement:    "[EMAIL]",
	}}

	out, skipped := ApplySpans(p, spans)
	if len(skipped) != 0 {
		t.Fatalf("inputs.0 span skipped: %v", skipped)
	}
	if out.Inputs[0] != "contact [EMAIL] for access" {
		t.Errorf("redacted input = %q", out.Inputs[0])
	}
	if out.Inputs[1] != "clean input" {
		t.Errorf("untouched input changed: %q", out.Inputs[1])
	}

	adjusted := AppliedSpanOffsets(p, spans)
	if len(adjusted) != 1 {
		t.Fatalf("adjusted spans = %d, want 1", len(adjusted))
	}
	a := adjusted[0]
	if got := out.Inputs[0][a.Start:a.End]; got != a.Replacement {
		t.Errorf("slice [%d:%d] = %q, want %q (slice-yields-replacement invariant)", a.Start, a.End, got, a.Replacement)
	}
}

// TestApplySpans_EmbeddingInputsOutOfRange: an inputs index past the slice is
// skipped and reported, never panics.
func TestApplySpans_EmbeddingInputsOutOfRange(t *testing.T) {
	p := NormalizedPayload{Kind: KindAIEmbedding, Inputs: []string{"only one"}}
	spans := []TransformSpan{{ContentAddress: "inputs.5", Start: 0, End: 4, Replacement: "x", Action: ActionRedact}}
	out, skipped := ApplySpans(p, spans)
	if len(skipped) != 1 {
		t.Fatalf("want 1 skipped span, got %d", len(skipped))
	}
	if out.Inputs[0] != "only one" {
		t.Errorf("payload mutated by unresolved span: %q", out.Inputs[0])
	}
}

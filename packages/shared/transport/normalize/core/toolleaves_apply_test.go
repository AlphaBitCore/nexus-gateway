package core

import (
	"testing"

	"github.com/goccy/go-json"
)

func toolUsePayload() NormalizedPayload {
	return NormalizedPayload{
		Kind:             KindAIChat,
		NormalizeVersion: SchemaVersion,
		Messages: []Message{
			{
				Role: RoleUser,
				Content: []ContentBlock{
					{Type: ContentText, Text: "look this up"},
				},
			},
			{
				Role: RoleAssistant,
				Content: []ContentBlock{
					{Type: ContentText, Text: "calling tool"},
					{Type: ContentToolUse, ToolUse: &ToolUse{
						CallID: "call_1",
						Name:   "search",
						Input: map[string]any{
							"query":   "email me at user@example.com",
							"page":    float64(2),
							"verbose": true,
						},
					}},
				},
			},
		},
	}
}

func TestAITextProjection_IncludesToolUseLeaves(t *testing.T) {
	p := toolUsePayload()
	got := p.TextProjection()
	// Expect: "look this up", "calling tool", then the one string leaf of
	// the tool Input ("query"; page/verbose are non-string and skipped).
	want := []string{"look this up", "calling tool", "email me at user@example.com"}
	if len(got) != len(want) {
		t.Fatalf("projection len=%d want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("projection[%d]=%q want %q", i, got[i], want[i])
		}
	}
}

func TestApplySpans_MasksToolLeafInLiveMap(t *testing.T) {
	p := toolUsePayload()
	// Address the assistant message (index 1), tool_use block (content index
	// 1), ordinal-0 leaf ("query"). Redact the email substring.
	// "email me at user@example.com" — offsets of the email.
	full := "email me at user@example.com"
	start := len("email me at ")
	end := len(full)
	span := TransformSpan{
		Source:         SourceHook,
		SourceID:       "email",
		Action:         ActionRedact,
		ContentAddress: "messages.1.content.1.toolUse.input.0",
		Start:          start,
		End:            end,
		Replacement:    "[REDACTED]",
	}
	out, skipped := ApplySpans(p, []TransformSpan{span})
	if len(skipped) != 0 {
		t.Fatalf("unexpected skipped: %+v", skipped)
	}
	masked := out.Messages[1].Content[1].ToolUse.Input["query"].(string)
	if masked != "email me at [REDACTED]" {
		t.Fatalf("leaf not masked: got %q", masked)
	}
	// Original payload must be untouched (clone independence).
	if p.Messages[1].Content[1].ToolUse.Input["query"].(string) != full {
		t.Fatalf("original mutated: %q", p.Messages[1].Content[1].ToolUse.Input["query"])
	}
	// Non-string leaves survive and the map re-marshals to valid JSON.
	raw, err := json.Marshal(out.Messages[1].Content[1].ToolUse.Input)
	if err != nil {
		t.Fatalf("re-marshal failed: %v", err)
	}
	var round map[string]any
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatalf("masked args not valid JSON: %v", err)
	}
	if round["page"].(float64) != 2 || round["verbose"].(bool) != true {
		t.Fatalf("non-string leaves corrupted: %v", round)
	}
}

func TestApplySpans_BadToolOrdinalReportedNotCrashed(t *testing.T) {
	p := toolUsePayload()
	span := TransformSpan{
		Source:         SourceHook,
		SourceID:       "x",
		Action:         ActionRedact,
		ContentAddress: "messages.1.content.1.toolUse.input.9", // ordinal out of range
		Start:          0, End: 1, Replacement: "X",
	}
	out, skipped := ApplySpans(p, []TransformSpan{span})
	if len(skipped) != 1 {
		t.Fatalf("bad ordinal should be skipped, got %d skipped", len(skipped))
	}
	// Nothing changed.
	if out.Messages[1].Content[1].ToolUse.Input["query"].(string) != "email me at user@example.com" {
		t.Fatalf("payload changed on bad ordinal")
	}
}

func TestApplySpans_MalformedToolAddressSkips(t *testing.T) {
	p := toolUsePayload()
	for _, addr := range []string{
		"messages.1.content.1.toolUse",         // missing input.ordinal
		"messages.1.content.1.toolUse.input",   // missing ordinal
		"messages.1.content.1.toolUse.bogus.0", // wrong segment name
		"messages.1.content.0.toolUse.input.0", // content 0 is text, no ToolUse
		"messages.1.content.1.toolUse.input.x", // non-numeric ordinal
	} {
		span := TransformSpan{ContentAddress: addr, Start: 0, End: 1, Replacement: "X", Source: SourceHook}
		_, skipped := ApplySpans(p, []TransformSpan{span})
		if len(skipped) != 1 {
			t.Fatalf("addr %q: expected skip, got %d", addr, len(skipped))
		}
	}
}

func TestToolCallArgsFromPayload_MaskedAndAligned(t *testing.T) {
	p := toolUsePayload()
	// Add a second tool call (no PII) to a later message to pin ordering.
	p.Messages = append(p.Messages, Message{
		Role: RoleAssistant,
		Content: []ContentBlock{
			{Type: ContentToolUse, ToolUse: &ToolUse{
				CallID: "call_2", Name: "noop",
				Input: map[string]any{"k": "no pii here"},
			}},
		},
	})
	full := "email me at user@example.com"
	span := TransformSpan{
		Source: SourceHook, SourceID: "email", Action: ActionRedact,
		ContentAddress: "messages.1.content.1.toolUse.input.0",
		Start:          len("email me at "), End: len(full), Replacement: "[REDACTED]",
	}
	args := ToolCallArgsFromPayload(p, []TransformSpan{span})
	if len(args) != 2 {
		t.Fatalf("want 2 aligned arg entries, got %d: %v", len(args), args)
	}
	var a0 map[string]any
	if err := json.Unmarshal([]byte(args[0]), &a0); err != nil {
		t.Fatalf("args[0] invalid JSON: %v", err)
	}
	if a0["query"].(string) != "email me at [REDACTED]" {
		t.Fatalf("first tool call not masked: %v", a0)
	}
	// The second tool call had NO masking span — it must be left untouched
	// (empty-string sentinel), not re-marshaled, so the rewriter leaves its
	// wire arguments byte-for-byte intact (no key reorder / float-precision
	// loss / clobber).
	if args[1] != "" {
		t.Fatalf("untouched sibling tool call should be empty sentinel, got %q", args[1])
	}
}

func TestToolCallArgsFromPayload_MalformedToolAddressNotTargeted(t *testing.T) {
	// A span whose address CONTAINS the tool-use marker but is otherwise
	// malformed (wrong segment count / non-numeric indices) must not be treated
	// as a targeted block — toolUseBlockOf rejects it, so no block is rewritten
	// and the result is nil (the span itself fail-safe-skips inside ApplySpans).
	p := toolUsePayload()
	for _, addr := range []string{
		"messages.x.content.1.toolUse.input.0", // non-numeric message index
		"messages.1.content.y.toolUse.input.0", // non-numeric content index
		"messages.1.content.1.toolUse.input",   // too few segments (still has marker-ish)
		"messages.1.content.1.toolUse.input.0.extra",
	} {
		span := TransformSpan{
			Source: SourceHook, Action: ActionRedact, ContentAddress: addr,
			Start: 0, End: 1, Replacement: "X",
		}
		if got := ToolCallArgsFromPayload(p, []TransformSpan{span}); got != nil {
			t.Fatalf("addr %q: expected nil (not a valid tool-leaf target), got %v", addr, got)
		}
	}
}

func TestToolCallArgsFromPayload_TargetedBlockMissingYieldsNil(t *testing.T) {
	// The span addresses a tool-leaf at a message index that does not exist, so
	// targeted is non-empty but the document walk never finds the block: no
	// re-marshal happens (anyMasked stays false) and the result is nil rather
	// than a slice of empty sentinels.
	p := toolUsePayload()
	span := TransformSpan{
		Source: SourceHook, Action: ActionRedact,
		ContentAddress: "messages.9.content.0.toolUse.input.0",
		Start:          0, End: 1, Replacement: "X",
	}
	if got := ToolCallArgsFromPayload(p, []TransformSpan{span}); got != nil {
		t.Fatalf("expected nil when targeted block is absent, got %v", got)
	}
}

func TestToolCallArgsFromPayload_NilWhenNoToolSpan(t *testing.T) {
	p := toolUsePayload()
	// A text-only span addresses a normal content block, not a tool leaf.
	span := TransformSpan{
		Source: SourceHook, SourceID: "x", Action: ActionRedact,
		ContentAddress: "messages.0.content.0",
		Start:          0, End: 4, Replacement: "XXXX",
	}
	if got := ToolCallArgsFromPayload(p, []TransformSpan{span}); got != nil {
		t.Fatalf("expected nil (no tool span), got %v", got)
	}
}

func nestedToolPayload() NormalizedPayload {
	return NormalizedPayload{
		Kind:             KindAIChat,
		NormalizeVersion: SchemaVersion,
		Messages: []Message{{
			Role: RoleAssistant,
			Content: []ContentBlock{
				{Type: ContentToolUse, ToolUse: &ToolUse{
					CallID: "c",
					Name:   "deep",
					Input: map[string]any{
						"obj": map[string]any{"inner": "a@b.com"},
						"arr": []any{"x@y.com", map[string]any{"deep": "p@q.com"}},
					},
				}},
			},
		}},
	}
}

func TestApplySpans_MasksNestedToolLeaves(t *testing.T) {
	p := nestedToolPayload()
	// Leaf order via ToolUseStringLeaves: keys sorted -> arr, obj.
	//   arr[0]="x@y.com"            -> ordinal 0
	//   arr[1].deep="p@q.com"       -> ordinal 1
	//   obj.inner="a@b.com"         -> ordinal 2
	leaves := ToolUseStringLeaves(p.Messages[0].Content[0].ToolUse.Input)
	if len(leaves) != 3 || leaves[0].Value != "x@y.com" || leaves[1].Value != "p@q.com" || leaves[2].Value != "a@b.com" {
		t.Fatalf("nested leaf order wrong: %+v", leaves)
	}
	spans := []TransformSpan{
		{Source: SourceHook, SourceID: "e", Action: ActionRedact, ContentAddress: "messages.0.content.0.toolUse.input.0", Start: 0, End: len("x@y.com"), Replacement: "[A]"}, // slice direct
		{Source: SourceHook, SourceID: "e", Action: ActionRedact, ContentAddress: "messages.0.content.0.toolUse.input.1", Start: 0, End: len("p@q.com"), Replacement: "[B]"}, // map inside slice
		{Source: SourceHook, SourceID: "e", Action: ActionRedact, ContentAddress: "messages.0.content.0.toolUse.input.2", Start: 0, End: len("a@b.com"), Replacement: "[C]"}, // nested map
	}
	out, skipped := ApplySpans(p, spans)
	if len(skipped) != 0 {
		t.Fatalf("skipped: %+v", skipped)
	}
	arr := out.Messages[0].Content[0].ToolUse.Input["arr"].([]any)
	if arr[0].(string) != "[A]" {
		t.Fatalf("arr[0]=%v", arr[0])
	}
	if arr[1].(map[string]any)["deep"].(string) != "[B]" {
		t.Fatalf("arr[1].deep=%v", arr[1])
	}
	obj := out.Messages[0].Content[0].ToolUse.Input["obj"].(map[string]any)
	if obj["inner"].(string) != "[C]" {
		t.Fatalf("obj.inner=%v", obj["inner"])
	}
	// Original untouched (deep clone of nested map/slice).
	origArr := p.Messages[0].Content[0].ToolUse.Input["arr"].([]any)
	if origArr[0].(string) != "x@y.com" || origArr[1].(map[string]any)["deep"].(string) != "p@q.com" {
		t.Fatalf("original nested tree mutated")
	}
}

func TestToolCallArgsFromPayload_NilInputSiblingUntouched(t *testing.T) {
	p := NormalizedPayload{
		Kind: KindAIChat, NormalizeVersion: SchemaVersion,
		Messages: []Message{
			{Role: RoleAssistant, Content: []ContentBlock{
				{Type: ContentToolUse, ToolUse: &ToolUse{CallID: "a", Name: "withpii", Input: map[string]any{"q": "x@y.com"}}},
				{Type: ContentToolUse, ToolUse: &ToolUse{CallID: "b", Name: "noargs", Input: nil}},
			}},
		},
	}
	span := TransformSpan{Source: SourceHook, Action: ActionRedact, ContentAddress: "messages.0.content.0.toolUse.input.0", Start: 0, End: len("x@y.com"), Replacement: "[R]"}
	args := ToolCallArgsFromPayload(p, []TransformSpan{span})
	if len(args) != 2 {
		t.Fatalf("want 2 args, got %d: %v", len(args), args)
	}
	// The masked call is re-marshaled; the nil-Input sibling (no span) must be
	// left untouched (empty sentinel), NOT clobbered to "{}" — re-marshaling a
	// nil-arguments call to "{}" is data loss the rewriter must never inflict.
	var a0 map[string]any
	if err := json.Unmarshal([]byte(args[0]), &a0); err != nil {
		t.Fatalf("args[0] invalid JSON: %v", err)
	}
	if a0["q"].(string) != "[R]" {
		t.Fatalf("first call not masked: %v", a0)
	}
	if args[1] != "" {
		t.Fatalf("nil-Input sibling should be untouched (empty sentinel), got %q", args[1])
	}
}

func TestToolLeafLen_OutOfRange(t *testing.T) {
	// resolveTextLen path for a tool leaf with an out-of-range ordinal must
	// report not-found so AppliedSpanOffsets drops the badge.
	p := toolUsePayload()
	span := TransformSpan{Source: SourceHook, Action: ActionRedact, ContentAddress: "messages.1.content.1.toolUse.input.7", Start: 0, End: 1, Replacement: "X"}
	if got := AppliedSpanOffsets(p, []TransformSpan{span}); got != nil {
		t.Fatalf("out-of-range tool ordinal should yield no badge, got %+v", got)
	}
}

func TestAppliedSpanOffsets_ToolLeafBadge(t *testing.T) {
	p := toolUsePayload()
	full := "email me at user@example.com"
	span := TransformSpan{
		Source: SourceHook, SourceID: "email", Action: ActionRedact,
		ContentAddress: "messages.1.content.1.toolUse.input.0",
		Start:          len("email me at "), End: len(full), Replacement: "[REDACTED]",
	}
	got := AppliedSpanOffsets(p, []TransformSpan{span})
	if len(got) != 1 {
		t.Fatalf("want 1 relocated span, got %d", len(got))
	}
	if got[0].Start != len("email me at ") || got[0].End != got[0].Start+len("[REDACTED]") {
		t.Fatalf("relocated offsets wrong: %+v", got[0])
	}
}

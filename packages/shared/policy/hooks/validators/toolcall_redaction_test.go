package validators

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// toolUseHookInput builds a HookInput whose assistant turn carries a
// ContentToolUse block with a PII-laden string leaf alongside ordinary text
// and a tool result, so the parity + addressing assertions exercise the
// full mixed-block walk.
func toolUseHookInput() *core.HookInput {
	return &core.HookInput{
		Stage: "response",
		Normalized: &normalize.NormalizedPayload{
			Kind:             normalize.KindAIChat,
			NormalizeVersion: normalize.SchemaVersion,
			Messages: []normalize.Message{
				{
					Role: normalize.RoleUser,
					Content: []normalize.ContentBlock{
						{Type: normalize.ContentText, Text: "find the user"},
					},
				},
				{
					Role: normalize.RoleAssistant,
					Content: []normalize.ContentBlock{
						{Type: normalize.ContentText, Text: "searching now"},
						{Type: normalize.ContentToolUse, ToolUse: &normalize.ToolUse{
							CallID: "call_1",
							Name:   "search",
							Input: map[string]any{
								"zquery": "contact user@example.com",
								"alimit": float64(5),
								"mnote":  "no pii",
							},
						}},
					},
				},
			},
		},
	}
}

// TestRulePackAddressing_ParityWithProjection is the R1 guard: the ordered
// .text slice produced by addressedSegments must equal the detection
// projection for the same payload, so anything the matcher sees can be
// addressed for masking and vice versa. Both derive tool-use leaf order from
// the single ToolUseStringLeaves walk, so a randomized map cannot skew them.
func TestRulePackAddressing_ParityWithProjection(t *testing.T) {
	in := toolUseHookInput()
	eng := &RulePackEngine{} // addressedSegments only reads cfg.ProjectionOptions(); nil cfg → defaults
	addressed := eng.addressedSegments(in)

	got := make([]string, len(addressed))
	for i, a := range addressed {
		got[i] = a.text
	}
	want := in.Normalized.TextProjection()
	if len(got) != len(want) {
		t.Fatalf("parity length mismatch: addressing=%d projection=%d\n  addr=%v\n  proj=%v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("parity[%d]: addressing=%q projection=%q", i, got[i], want[i])
		}
	}
	// The tool-use leaf must carry the ordinal address and be tagged tool_use.
	var found bool
	for _, a := range addressed {
		if a.address == "messages.1.content.1.toolUse.input.1" {
			found = true
			if a.blockType != "tool_use" {
				t.Fatalf("tool leaf blockType=%q want tool_use", a.blockType)
			}
			if a.text != "contact user@example.com" {
				t.Fatalf("tool leaf text=%q", a.text)
			}
		}
	}
	if !found {
		t.Fatalf("tool-use leaf address not found among %d segments", len(addressed))
	}
}

// TestRulePackEngine_RedactsToolCallLeaf checks executeRedact emits exactly
// one span at the tool-use leaf address, tags the flat ModifiedContent block
// "tool_use" (so positional text consumers skip it), and leaves the text
// blocks unshifted/untagged.
func TestRulePackEngine_RedactsToolCallLeaf(t *testing.T) {
	cfg := buildEngineConfig([]rulePackInstall{{
		InstallID:   "inst-redact",
		PackName:    "pii",
		PackVersion: "1.0.0",
		Enabled:     true,
		Rules: []rulePackRule{{
			RuleID:   "email",
			Category: "pii",
			Severity: "hard", // enforcing
			Pattern:  `[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`,
			Flags:    "",
		}},
	}})
	// Force onMatch action = redact so executeRedact runs (default action is block).
	cfg.Config["onMatch"] = map[string]any{"action": "redact", "replacement": "[PII]"}

	h, err := NewRulePackEngine(cfg)
	if err != nil {
		t.Fatalf("NewRulePackEngine: %v", err)
	}
	in := toolUseHookInput()
	res, err := h.Execute(t.Context(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != Modify {
		t.Fatalf("decision=%s want Modify; reason=%s", res.Decision, res.Reason)
	}
	if len(res.TransformSpans) != 1 {
		t.Fatalf("want 1 span, got %d: %+v", len(res.TransformSpans), res.TransformSpans)
	}
	sp := res.TransformSpans[0]
	if sp.ContentAddress != "messages.1.content.1.toolUse.input.1" {
		t.Fatalf("span address=%q want messages.1.content.1.toolUse.input.1", sp.ContentAddress)
	}
	// ModifiedContent: text blocks tagged "text"; every tool-call leaf tagged
	// "tool_use" (the matched one masked, the unmatched one passed through).
	var toolUseTexts, textTexts []string
	for _, b := range res.ModifiedContent {
		switch b.Type {
		case "tool_use":
			toolUseTexts = append(toolUseTexts, b.Text)
		case "text":
			textTexts = append(textTexts, b.Text)
		default:
			t.Fatalf("unexpected modified block type %q", b.Type)
		}
	}
	if len(textTexts) == 0 {
		t.Fatalf("expected text-tagged blocks, got none")
	}
	var maskedSeen bool
	for _, s := range toolUseTexts {
		if s == "contact [PII]" {
			maskedSeen = true
		}
	}
	if !maskedSeen {
		t.Fatalf("masked tool leaf 'contact [PII]' not found among tool_use blocks: %v", toolUseTexts)
	}

	// Apply the span back and confirm the tool leaf is masked in the live map
	// while the text blocks are untouched (offsets not shifted by the tool span).
	masked, skipped := normalize.ApplySpans(*in.Normalized, res.TransformSpans)
	if len(skipped) != 0 {
		t.Fatalf("span skipped on apply: %+v", skipped)
	}
	if got := masked.Messages[1].Content[1].ToolUse.Input["zquery"].(string); got != "contact [PII]" {
		t.Fatalf("tool leaf not masked after ApplySpans: %q", got)
	}
	if masked.Messages[0].Content[0].Text != "find the user" || masked.Messages[1].Content[0].Text != "searching now" {
		t.Fatalf("text blocks shifted/altered: %q / %q", masked.Messages[0].Content[0].Text, masked.Messages[1].Content[0].Text)
	}
}

// TestPiiDetector_RedactsToolCallLeaf mirrors the rulepack assertion for the
// pii-detector hook (the second addressing walk).
func TestPiiDetector_RedactsToolCallLeaf(t *testing.T) {
	patterns := []map[string]any{
		{"id": "email", "regex": `[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`},
	}
	hook, err := NewPiiDetector(makePiiConfig(patterns, "redact"))
	if err != nil {
		t.Fatalf("NewPiiDetector: %v", err)
	}
	in := toolUseHookInput()
	res, err := hook.Execute(t.Context(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != Modify {
		t.Fatalf("decision=%s want Modify", res.Decision)
	}
	if len(res.TransformSpans) != 1 {
		t.Fatalf("want 1 span, got %d", len(res.TransformSpans))
	}
	if res.TransformSpans[0].ContentAddress != "messages.1.content.1.toolUse.input.1" {
		t.Fatalf("address=%q", res.TransformSpans[0].ContentAddress)
	}
	var sawToolUse bool
	for _, b := range res.ModifiedContent {
		if b.Type == "tool_use" {
			sawToolUse = true
		}
	}
	if !sawToolUse {
		t.Fatalf("pii-detector did not tag tool_use modified block")
	}
}

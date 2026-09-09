package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/validators"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// The flat ModifiedContent list is positional: consumer slot N takes list entry
// N. That holds only while the PRODUCER's index space and the CONSUMER's walk
// agree on which blocks occupy a slot, and the two live in different packages
// with no compiler relating them. So this drives the real redactor rather than a
// hand-written list — a fixture written here would only ever re-state whatever
// the producer did on the day it was typed, and go green the moment the two
// sides drifted.
//
// They did drift. Reasoning was tagged "text", which took a slot; the consumer
// walk offers slots for wire-carried text only, so every assignment after the
// reasoning block landed one early. Not a cosmetic offset: the user was served
// the model's redacted THINKING where the answer belongs, while the thinking kept
// the value the policy had just decided to mask. Both directions of one bug.

const (
	alignSecret  = "123-45-6789"
	alignVisible = "your balance is fine"
)

// redactorOverPayload runs the real PII detector over a payload and hands back
// what it produced, so producer and consumer are exercised against each other.
func redactorOverPayload(t *testing.T, p *normalize.NormalizedPayload) ([]core.ContentBlock, []normalize.TransformSpan) {
	t.Helper()
	h, err := validators.NewPiiDetector(&core.HookConfig{
		ID:   "align-pii",
		Name: "align-pii",
		Config: map[string]any{
			"onMatch": map[string]any{"action": "redact"},
			"patternDefinitions": []any{map[string]any{
				"id":    "ssn",
				"regex": `\b\d{3}-\d{2}-\d{4}\b`,
				"flags": "g",
			}},
		},
	})
	if err != nil {
		t.Fatalf("NewPiiDetector: %v", err)
	}
	res, err := h.Execute(context.Background(), &core.HookInput{Stage: "response", Normalized: p})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.ModifiedContent) == 0 && len(res.TransformSpans) == 0 {
		t.Fatalf("the detector found nothing in a payload built to contain %q — the assertions "+
			"below would hold vacuously", alignSecret)
	}
	return res.ModifiedContent, res.TransformSpans
}

func TestPositionalApplyDoesNotShiftWhenReasoningIsPresent(t *testing.T) {
	payload := &normalize.NormalizedPayload{
		Kind:     normalize.KindAIChat,
		Protocol: "openai-chat",
		Messages: []normalize.Message{{
			Role: normalize.RoleAssistant,
			Content: []normalize.ContentBlock{
				// A reasoning-model answer: the thinking carries the sensitive
				// value, the visible answer does not.
				{Type: normalize.ContentReasoning, Text: "the SSN is " + alignSecret},
				{Type: normalize.ContentText, Text: alignVisible},
			},
		}},
	}

	modified, spans := redactorOverPayload(t, payload)

	// The positional leg: what a consumer that still walks ModifiedContent sees.
	out := applyModifiedContentToNormalized(payload, modified)
	if out == nil || len(out.Messages) != 1 || len(out.Messages[0].Content) != 2 {
		t.Fatalf("unexpected shape back: %+v", out)
	}
	if got := out.Messages[0].Content[1]; got.Type != normalize.ContentText || got.Text != alignVisible {
		t.Errorf("the text block now reads %q, want %q — the reasoning's entry was applied to it, "+
			"so the caller is served the model's thinking instead of its answer", got.Text, alignVisible)
	}

	// The addressed leg: reasoning is masked by its own span, which names the
	// block directly and so cannot land on the wrong one.
	patched, _ := normalize.ApplySpans(*payload, spans)
	for i, b := range patched.Messages[0].Content {
		if strings.Contains(b.Text, alignSecret) {
			t.Errorf("block %d (%s) still carries %q after the spans were applied: %q",
				i, b.Type, alignSecret, b.Text)
		}
	}
}

// Every scannable block kind, one payload, so a producer that starts tagging a
// NEW channel as positional is caught here rather than in a support ticket. The
// invariant asserted is the one that actually broke: the number of entries the
// producer marks positional equals the number of slots the consumer offers.
func TestProducerAndConsumerAgreeOnPositionalSlots(t *testing.T) {
	payload := &normalize.NormalizedPayload{
		Kind:     normalize.KindAIChat,
		Protocol: "openai-chat",
		Messages: []normalize.Message{{
			Role: normalize.RoleAssistant,
			Content: []normalize.ContentBlock{
				{Type: normalize.ContentReasoning, Text: "thinking about " + alignSecret},
				{Type: normalize.ContentText, Text: "answer for " + alignSecret},
				{Type: normalize.ContentRefusal, Text: "declined, " + alignSecret},
				{Type: normalize.ContentToolResult, ToolResult: &normalize.ToolResult{
					Output: "tool said " + alignSecret,
				}},
			},
		}},
	}

	modified, _ := redactorOverPayload(t, payload)

	positional := 0
	for _, b := range modified {
		if b.Type == "" || b.Type == "text" {
			positional++
		}
	}

	// The consumer's own rule, asked of the consumer rather than restated here —
	// a copy of the predicate would drift from it exactly the way the producer's
	// tags did.
	slots := 0
	for _, b := range payload.Messages[0].Content {
		if positionalBlock(b) {
			slots++
		}
	}

	if positional != slots {
		kinds := make([]string, 0, len(modified))
		for _, b := range modified {
			kinds = append(kinds, b.Type)
		}
		t.Fatalf("the producer marked %d entries positional but the consumer offers %d slots "+
			"(entry types: %v). Every assignment after the first surplus entry lands on the wrong "+
			"block, which reads as redacted text appearing where it does not belong",
			positional, slots, kinds)
	}

	// Agreeing on the COUNT is not enough. Each slot must actually receive its
	// mask, in the field that block kind carries — a tool result's text lives on
	// ToolResult.Output, so writing Text for one leaves the masked value where
	// nothing reads it and the original where everything does. Matching counts
	// with a dropped write looks identical from the outside.
	out := applyModifiedContentToNormalized(payload, modified)
	for i, b := range out.Messages[0].Content {
		if !positionalBlock(b) {
			continue
		}
		got := b.Text
		if b.Type == normalize.ContentToolResult {
			got = b.ToolResult.Output
		}
		if strings.Contains(got, alignSecret) {
			t.Errorf("block %d (%s) still carries %q after positional application: %q — its slot "+
				"was counted but its mask was never written back", i, b.Type, alignSecret, got)
		}
	}
}

// The control: the shape the positional list was designed for. If this ever fails
// alongside the tests above, the problem is positional application in general
// rather than a disagreement about which channels are positional.
func TestPositionalApplyIsCorrectWithoutReasoning(t *testing.T) {
	payload := &normalize.NormalizedPayload{
		Messages: []normalize.Message{{
			Role: normalize.RoleUser,
			Content: []normalize.ContentBlock{
				{Type: normalize.ContentText, Text: "first"},
				{Type: normalize.ContentText, Text: "second"},
			},
		}},
	}
	modified := []core.ContentBlock{
		{Type: "text", Text: "FIRST"},
		{Type: "text", Text: "SECOND"},
	}

	out := applyModifiedContentToNormalized(payload, modified)
	if out.Messages[0].Content[0].Text != "FIRST" || out.Messages[0].Content[1].Text != "SECOND" {
		t.Fatalf("plain text-only application regressed: %q / %q",
			out.Messages[0].Content[0].Text, out.Messages[0].Content[1].Text)
	}
}

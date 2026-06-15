// Coverage backfill: tests for residual low-coverage branches in the
// validators sub-package (pii_detector executeRedact paths, luhnValid).
package validators

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

func TestLuhnValid_EmptyDigitsReturnsFalse(t *testing.T) {
	// Strings with zero numeric chars: empty, alphabetic, punctuation only.
	cases := []string{"", "abc", "---", "abcd-efgh-ijkl"}
	for _, s := range cases {
		if luhnValid(s) {
			t.Errorf("luhnValid(%q) = true, want false on zero-digit input", s)
		}
	}
}

func TestPiiDetector_ExecuteRedact_NilNormalizedApproves(t *testing.T) {
	patterns := []map[string]any{
		{"id": "email", "regex": `\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`},
	}
	hook, err := NewPiiDetector(makePiiConfig(patterns, "redact"))
	if err != nil {
		t.Fatalf("NewPiiDetector: %v", err)
	}
	res, err := hook.Execute(t.Context(), &core.HookInput{Stage: "request"}) // nil Normalized
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != core.Approve {
		t.Errorf("nil Normalized in redact path: got %s want Approve", res.Decision)
	}
	if res.ModifiedContent != nil {
		t.Errorf("ModifiedContent should be nil on nil Normalized; got %v", res.ModifiedContent)
	}
}

func TestPiiDetector_ExecuteRedact_EmptySegmentsApproves(t *testing.T) {
	// Normalized non-nil but TextProjection() returns zero segments
	// (no ContentText / ContentToolResult blocks).
	patterns := []map[string]any{{"id": "x", "regex": `foo`}}
	hook, err := NewPiiDetector(makePiiConfig(patterns, "redact"))
	if err != nil {
		t.Fatalf("NewPiiDetector: %v", err)
	}
	in := &core.HookInput{Normalized: &normalize.NormalizedPayload{
		Kind:             normalize.KindAIChat,
		NormalizeVersion: normalize.SchemaVersion,
		// No Messages → empty projection.
	}}
	res, err := hook.Execute(t.Context(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != core.Approve {
		t.Errorf("empty projection in redact path: got %s want Approve", res.Decision)
	}
}

func TestPiiDetector_ExecuteRedact_ToolResultBlockRedacted(t *testing.T) {
	// ContentToolResult blocks should participate in redact + emit a span
	// addressed with ".toolResult" suffix.
	patterns := []map[string]any{
		{"id": "email", "regex": `\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`},
	}
	hook, err := NewPiiDetector(makePiiConfig(patterns, "redact"))
	if err != nil {
		t.Fatalf("NewPiiDetector: %v", err)
	}
	in := &core.HookInput{Normalized: &normalize.NormalizedPayload{
		Kind:             normalize.KindAIChat,
		NormalizeVersion: normalize.SchemaVersion,
		Messages: []normalize.Message{{
			Role: normalize.RoleUser,
			Content: []normalize.ContentBlock{
				{Type: normalize.ContentToolResult, ToolResult: &normalize.ToolResult{
					CallID: "call-1",
					Output: "lookup result: user@example.com from db",
				}},
			},
		}},
	}}
	res, err := hook.Execute(t.Context(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != core.Modify {
		t.Errorf("decision: got %s want Modify", res.Decision)
	}
	if len(res.TransformSpans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(res.TransformSpans))
	}
	sp := res.TransformSpans[0]
	if sp.ContentAddress == "" {
		t.Errorf("span address should not be empty; got %q", sp.ContentAddress)
	}
}

func TestPiiDetector_ExecuteRedact_ToolResultNilSkipped(t *testing.T) {
	// A ContentToolResult block with nil ToolResult must be skipped, not
	// crash. With it skipped, no text remains → redact path approves.
	patterns := []map[string]any{{"id": "x", "regex": `secret`}}
	hook, _ := NewPiiDetector(makePiiConfig(patterns, "redact"))
	in := &core.HookInput{Normalized: &normalize.NormalizedPayload{
		Kind:             normalize.KindAIChat,
		NormalizeVersion: normalize.SchemaVersion,
		Messages: []normalize.Message{{
			Role: normalize.RoleUser,
			Content: []normalize.ContentBlock{
				{Type: normalize.ContentToolResult, ToolResult: nil}, // nil
			},
		}},
	}}
	res, err := hook.Execute(t.Context(), in)
	if err != nil {
		t.Errorf("Execute should not error on nil ToolResult: %v", err)
	}
	if res.Decision != core.Approve {
		t.Errorf("decision: got %s", res.Decision)
	}
}

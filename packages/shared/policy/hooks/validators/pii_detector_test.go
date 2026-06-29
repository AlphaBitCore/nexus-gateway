package validators

import (
	"context"
	"strings"
	"testing"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// makePiiConfig builds a HookConfig for the PiiDetector. The `action` arg
// maps onto the single onMatch.action axis:
//
//	"block"  → action=block   (Decision REJECT_HARD)
//	"redact" → action=redact  (Decision MODIFY)
//	"approve"→ action=approve (Decision APPROVE, detect-and-tag only)
//	""       → omit onMatch (defaults to block)
//
// Any other string is passed through verbatim as onMatch.action so factory
// tests can assert that unknown action values are rejected.
func makePiiConfig(patterns []map[string]any, action string) *HookConfig {
	ifaces := make([]any, len(patterns))
	for i, p := range patterns {
		ifaces[i] = p
	}
	cfg := &HookConfig{
		ID:               "pii-1",
		ImplementationID: "pii-detector",
		Name:             "Test PII Detector",
		Config: map[string]any{
			"patternDefinitions": ifaces,
		},
	}
	if action != "" {
		cfg.Config["onMatch"] = map[string]any{"action": action}
	}
	return cfg
}

// seedPiiPatterns mirrors SEED_DEFAULT_PII_PATTERN_DEFINITIONS in
// tools/db-migrate/seed/seed-hook-configs.ts. Changes there must be mirrored
// here so this test fails when the schemas drift again.
func seedPiiPatterns() []map[string]any {
	return []map[string]any{
		{"id": "email", "regex": `\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`, "flags": "g"},
		{"id": "phone", "regex": `\b(?:\+?1[-.\s]?)?(?:\(?\d{3}\)?[-.\s]?)?\d{3}[-.\s]?\d{4}\b`, "flags": "g"},
		{"id": "ssn", "regex": `\b\d{3}[-\s]?\d{2}[-\s]?\d{4}\b`, "flags": "g"},
		{"id": "credit_card", "regex": `\b(?:\d{4}[-\s]?){3}\d{4}\b`, "flags": "g"},
	}
}

// containsTag reports whether want is present in tags. Test helper.
func containsTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

// --- Factory: acceptance of the seed shape -----------------------------------

func TestPiiDetector_Factory_AcceptsSeedShape(t *testing.T) {
	hook, err := NewPiiDetector(makePiiConfig(seedPiiPatterns(), "block"))
	if err != nil {
		t.Fatalf("NewPiiDetector rejected seed-shaped config: %v", err)
	}
	if hook == nil {
		t.Fatal("hook is nil")
	}
}

// --- Detection path (block → REJECT_HARD) — one per built-in seed id ---------

func TestPiiDetector_Block_PerId(t *testing.T) {
	// Each id is exercised against a single-pattern config so the assertion on
	// result.Reason (which carries the matching pattern id) is unambiguous.
	// The seed patterns overlap — e.g. the phone regex can absorb the last 7
	// digits of a credit card — so we cannot assert reason id reliably when
	// all four patterns are active at once. The cross-pattern seed acceptance
	// is covered by TestPiiDetector_Factory_AcceptsSeedShape.
	seed := seedPiiPatterns()
	byID := map[string]map[string]any{}
	for _, p := range seed {
		byID[p["id"].(string)] = p
	}

	cases := []struct {
		id    string
		input string
	}{
		{"email", "contact me at user@example.com please"},
		{"phone", "call me at 555-123-4567"},
		{"ssn", "my ssn is 123-45-6789"},
		{"credit_card", "my card is 4532 0151 1283 0366"},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			hook, err := NewPiiDetector(makePiiConfig([]map[string]any{byID[tc.id]}, "block"))
			if err != nil {
				t.Fatalf("NewPiiDetector: %v", err)
			}
			input := &HookInput{
				Normalized: PayloadFromTextSegments([]string{tc.input}),
			}
			result, err := hook.Execute(context.Background(), input)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if result.Decision != RejectHard {
				t.Errorf("decision: expected REJECT_HARD, got %s", result.Decision)
			}
			if result.ReasonCode != "PII_DETECTED" {
				t.Errorf("reasonCode: expected PII_DETECTED, got %s", result.ReasonCode)
			}
			if !strings.Contains(result.Reason, tc.id) {
				t.Errorf("reason: expected to contain %q, got %q", tc.id, result.Reason)
			}
			if !containsTag(result.Tags, "compliance:pii") || !containsTag(result.Tags, "severity:confidential") {
				t.Errorf("tags: expected both compliance:pii and severity:confidential, got %v", result.Tags)
			}
		})
	}
}

func TestPiiDetector_Block_NoMatch_Approve(t *testing.T) {
	hook, err := NewPiiDetector(makePiiConfig(seedPiiPatterns(), "block"))
	if err != nil {
		t.Fatalf("NewPiiDetector: %v", err)
	}
	input := &HookInput{
		Normalized: PayloadFromTextSegments([]string{"this has no PII at all"}),
	}
	result, err := hook.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Decision != Approve {
		t.Errorf("decision: expected APPROVE, got %s", result.Decision)
	}
	if result.ReasonCode != "" {
		t.Errorf("reasonCode: expected empty, got %q", result.ReasonCode)
	}
}

// --- Action enum -------------------------------------------------------------

// action=approve detects PII for tagging only: the request flows through
// unchanged (Decision APPROVE), no spans are collected, and result.Action
// stays empty — but the compliance:pii / severity:confidential tags are set.
func TestPiiDetector_Action_Approve_DetectAndTagOnly(t *testing.T) {
	hook, err := NewPiiDetector(makePiiConfig(seedPiiPatterns(), "approve"))
	if err != nil {
		t.Fatalf("NewPiiDetector: %v", err)
	}
	input := &HookInput{
		Normalized: PayloadFromTextSegments([]string{"email: user@example.com"}),
	}
	result, err := hook.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Decision != Approve {
		t.Errorf("decision: expected APPROVE (detect-and-tag), got %s", result.Decision)
	}
	if result.Action != "" {
		t.Errorf("approve action must not stamp result.Action, got %q", result.Action)
	}
	if len(result.TransformSpans) != 0 {
		t.Errorf("approve path collects no spans, got %d", len(result.TransformSpans))
	}
	if result.ReasonCode != "PII_DETECTED" {
		t.Errorf("reasonCode: expected PII_DETECTED, got %q", result.ReasonCode)
	}
	if !containsTag(result.Tags, "compliance:pii") || !containsTag(result.Tags, "severity:confidential") {
		t.Errorf("tags: expected compliance:pii + severity:confidential, got %v", result.Tags)
	}
}

func TestPiiDetector_Action_Unknown_Rejected(t *testing.T) {
	_, err := NewPiiDetector(makePiiConfig(seedPiiPatterns(), "purge"))
	if err == nil {
		t.Fatal("expected error for unknown action, got nil")
	}
	if !strings.Contains(err.Error(), "action") {
		t.Errorf("expected error to mention 'action', got: %v", err)
	}
}

// Missing onMatch is allowed — defaults to block-hard. The hook
// constructs successfully and matches behave as hard blocks.
func TestPiiDetector_Action_Missing_DefaultsToBlock(t *testing.T) {
	hook, err := NewPiiDetector(makePiiConfig(seedPiiPatterns(), ""))
	if err != nil {
		t.Fatalf("expected success on missing onMatch (default block-hard): %v", err)
	}
	input := &HookInput{
		Normalized: PayloadFromTextSegments([]string{"contact me at u@e.com"}),
	}
	result, err := hook.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Decision != RejectHard {
		t.Errorf("expected default RejectHard, got %s", result.Decision)
	}
}

// Reject the internal Decision wire vocab as onMatch.action values — these
// are not in the closed set {approve, redact, block}.
func TestPiiDetector_Action_LegacyStringsRejected(t *testing.T) {
	for _, legacy := range []string{"reject_hard", "reject_soft"} {
		t.Run(legacy, func(t *testing.T) {
			_, err := NewPiiDetector(makePiiConfig(seedPiiPatterns(), legacy))
			if err == nil {
				t.Fatalf("expected legacy action %q to be rejected, got nil", legacy)
			}
		})
	}
}

// --- Redact path -------------------------------------------------------------

func TestPiiDetector_Redact_DefaultReplacement(t *testing.T) {
	hook, err := NewPiiDetector(makePiiConfig(seedPiiPatterns(), "redact"))
	if err != nil {
		t.Fatalf("NewPiiDetector: %v", err)
	}
	input := &HookInput{
		Normalized: PayloadFromTextSegments([]string{"contact me at user@example.com please"}),
	}
	result, err := hook.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Decision != Modify {
		t.Errorf("decision: expected MODIFY, got %s", result.Decision)
	}
	if result.ModifiedContent == nil {
		t.Fatal("ModifiedContent: expected non-nil")
	}
	want := "contact me at [REDACTED_EMAIL] please"
	if result.ModifiedContent[0].Text != want {
		t.Errorf("redacted text: expected %q, got %q", want, result.ModifiedContent[0].Text)
	}
}

func TestPiiDetector_Redact_CustomReplacement(t *testing.T) {
	patterns := []map[string]any{
		{"id": "internal_id", "regex": `ACCT-\d{8}`, "replacement": "[ACCT]"},
	}
	hook, err := NewPiiDetector(makePiiConfig(patterns, "redact"))
	if err != nil {
		t.Fatalf("NewPiiDetector: %v", err)
	}
	input := &HookInput{
		Normalized: PayloadFromTextSegments([]string{"account ACCT-12345678 found"}),
	}
	result, err := hook.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	want := "account [ACCT] found"
	if result.ModifiedContent == nil || result.ModifiedContent[0].Text != want {
		t.Errorf("redacted text: expected %q, got %+v", want, result.ModifiedContent)
	}
}

func TestPiiDetector_Redact_NoMatch_Approve(t *testing.T) {
	hook, err := NewPiiDetector(makePiiConfig(seedPiiPatterns(), "redact"))
	if err != nil {
		t.Fatalf("NewPiiDetector: %v", err)
	}
	input := &HookInput{
		Normalized: PayloadFromTextSegments([]string{"no PII here"}),
	}
	result, err := hook.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Decision != Approve {
		t.Errorf("decision: expected APPROVE, got %s", result.Decision)
	}
	if result.ModifiedContent != nil {
		t.Errorf("ModifiedContent: expected nil on no-match, got %v", result.ModifiedContent)
	}
}

// --- Luhn opt-in -------------------------------------------------------------

func TestPiiDetector_Luhn_FiltersInvalidCards(t *testing.T) {
	patterns := []map[string]any{
		{
			"id":    "credit_card",
			"regex": `\b(?:\d{4}[-\s]?){3}\d{4}\b`,
			"flags": "g",
			"luhn":  true,
		},
	}
	hook, err := NewPiiDetector(makePiiConfig(patterns, "block"))
	if err != nil {
		t.Fatalf("NewPiiDetector: %v", err)
	}

	// Invalid Luhn: 1234 5678 9012 3456 (checksum fails).
	t.Run("invalidLuhn_approves", func(t *testing.T) {
		input := &HookInput{
			Normalized: PayloadFromTextSegments([]string{"card 1234 5678 9012 3456"}),
		}
		result, err := hook.Execute(context.Background(), input)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if result.Decision != Approve {
			t.Errorf("decision: expected APPROVE (invalid Luhn filtered), got %s", result.Decision)
		}
	})

	// Valid Luhn: 4532 0151 1283 0366.
	t.Run("validLuhn_blocks", func(t *testing.T) {
		input := &HookInput{
			Normalized: PayloadFromTextSegments([]string{"card 4532 0151 1283 0366"}),
		}
		result, err := hook.Execute(context.Background(), input)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if result.Decision != RejectHard {
			t.Errorf("decision: expected REJECT_HARD, got %s", result.Decision)
		}
	})
}

func TestPiiDetector_Luhn_DefaultFalse_MatchesAny(t *testing.T) {
	// Seed config has no luhn flag → defaults to false → any 16-digit group blocks.
	hook, err := NewPiiDetector(makePiiConfig(seedPiiPatterns(), "block"))
	if err != nil {
		t.Fatalf("NewPiiDetector: %v", err)
	}
	input := &HookInput{
		Normalized: PayloadFromTextSegments([]string{"card 1234 5678 9012 3456"}),
	}
	result, err := hook.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Decision != RejectHard {
		t.Errorf("decision: expected REJECT_HARD (no Luhn guard), got %s", result.Decision)
	}
}

func TestPiiDetector_Luhn_Redact_ReplacesOnlyValid(t *testing.T) {
	patterns := []map[string]any{
		{
			"id":    "credit_card",
			"regex": `\b(?:\d{4}[-\s]?){3}\d{4}\b`,
			"flags": "g",
			"luhn":  true,
		},
	}
	hook, err := NewPiiDetector(makePiiConfig(patterns, "redact"))
	if err != nil {
		t.Fatalf("NewPiiDetector: %v", err)
	}
	input := &HookInput{
		Normalized: PayloadFromTextSegments([]string{"valid 4532 0151 1283 0366 and invalid 1234 5678 9012 3456"}),
	}
	result, err := hook.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Decision != Modify {
		t.Errorf("decision: expected MODIFY, got %s", result.Decision)
	}
	got := result.ModifiedContent[0].Text
	if !strings.Contains(got, "[REDACTED_CREDIT_CARD]") {
		t.Errorf("expected valid card redacted, got %q", got)
	}
	if !strings.Contains(got, "1234 5678 9012 3456") {
		t.Errorf("expected invalid-Luhn card untouched, got %q", got)
	}
}

// --- Flags -------------------------------------------------------------------

func TestPiiDetector_Flags_CaseInsensitive(t *testing.T) {
	patterns := []map[string]any{
		{"id": "secret", "regex": `SECRET`, "flags": "i"},
	}
	hook, err := NewPiiDetector(makePiiConfig(patterns, "block"))
	if err != nil {
		t.Fatalf("NewPiiDetector: %v", err)
	}
	input := &HookInput{
		Normalized: PayloadFromTextSegments([]string{"this is a secret memo"}),
	}
	result, err := hook.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Decision != RejectHard {
		t.Errorf("decision: expected REJECT_HARD (case-insensitive match), got %s", result.Decision)
	}
}

func TestPiiDetector_Flags_GIsNoOp(t *testing.T) {
	patterns := []map[string]any{
		{"id": "email", "regex": `\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`, "flags": "g"},
	}
	hook, err := NewPiiDetector(makePiiConfig(patterns, "redact"))
	if err != nil {
		t.Fatalf("NewPiiDetector: %v", err)
	}
	input := &HookInput{
		Normalized: PayloadFromTextSegments([]string{"a@b.com and c@d.io"}),
	}
	result, err := hook.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// ReplaceAllString already replaces every occurrence; 'g' should not
	// change the result.
	want := "[REDACTED_EMAIL] and [REDACTED_EMAIL]"
	if result.ModifiedContent[0].Text != want {
		t.Errorf("redacted text: expected %q, got %q", want, result.ModifiedContent[0].Text)
	}
}

func TestPiiDetector_Flags_Unsupported_Rejected(t *testing.T) {
	patterns := []map[string]any{
		{"id": "unicode", "regex": `foo`, "flags": "u"},
	}
	_, err := NewPiiDetector(makePiiConfig(patterns, "block"))
	if err == nil {
		t.Fatal("expected error for unsupported flag, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported flag") {
		t.Errorf("expected error to mention 'unsupported flag', got: %v", err)
	}
}

func TestPiiDetector_Flags_DuplicatesCollapsed(t *testing.T) {
	patterns := []map[string]any{
		{"id": "secret", "regex": `secret`, "flags": "iiii"},
	}
	hook, err := NewPiiDetector(makePiiConfig(patterns, "block"))
	if err != nil {
		t.Fatalf("NewPiiDetector rejected duplicate flags: %v", err)
	}
	input := &HookInput{
		Normalized: PayloadFromTextSegments([]string{"SECRET"}),
	}
	result, _ := hook.Execute(context.Background(), input)
	if result.Decision != RejectHard {
		t.Errorf("decision: expected REJECT_HARD, got %s", result.Decision)
	}
}

// --- Factory validation ------------------------------------------------------

func TestPiiDetector_Factory_MissingPatternDefinitions(t *testing.T) {
	cfg := &HookConfig{
		ID:               "pii-1",
		ImplementationID: "pii-detector",
		Config:           map[string]any{"action": "block"},
	}
	_, err := NewPiiDetector(cfg)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "patternDefinitions") {
		t.Errorf("expected error to mention 'patternDefinitions', got: %v", err)
	}
}

func TestPiiDetector_Factory_EmptyIdRejected(t *testing.T) {
	patterns := []map[string]any{{"id": "", "regex": `foo`}}
	_, err := NewPiiDetector(makePiiConfig(patterns, "block"))
	if err == nil {
		t.Fatal("expected error for empty id, got nil")
	}
}

func TestPiiDetector_Factory_EmptyRegexRejected(t *testing.T) {
	patterns := []map[string]any{{"id": "foo", "regex": ""}}
	_, err := NewPiiDetector(makePiiConfig(patterns, "block"))
	if err == nil {
		t.Fatal("expected error for empty regex, got nil")
	}
}

func TestPiiDetector_Factory_InvalidRegexRejected(t *testing.T) {
	patterns := []map[string]any{{"id": "bad", "regex": `[unclosed`}}
	_, err := NewPiiDetector(makePiiConfig(patterns, "block"))
	if err == nil {
		t.Fatal("expected error for invalid regex, got nil")
	}
	if !strings.Contains(err.Error(), "invalid regex") {
		t.Errorf("expected error to mention 'invalid regex', got: %v", err)
	}
}

// Reject the old schema that this change intentionally removes. If these
// start passing again, the "no backwards compatibility" rule was violated.
func TestPiiDetector_Factory_LegacyKeysRejected(t *testing.T) {
	t.Run("types", func(t *testing.T) {
		cfg := &HookConfig{
			ID:               "pii-1",
			ImplementationID: "pii-detector",
			Config: map[string]any{
				"types":  []any{"email"},
				"action": "block",
			},
		}
		if _, err := NewPiiDetector(cfg); err == nil {
			t.Fatal("expected error for legacy 'types' key, got nil")
		}
	})
	t.Run("customPatterns", func(t *testing.T) {
		cfg := &HookConfig{
			ID:               "pii-1",
			ImplementationID: "pii-detector",
			Config: map[string]any{
				"customPatterns": []any{map[string]any{"name": "x", "pattern": "y"}},
				"action":         "block",
			},
		}
		if _, err := NewPiiDetector(cfg); err == nil {
			t.Fatal("expected error for legacy 'customPatterns' key, got nil")
		}
	})
}

func TestPiiDetector_EmptyPatternDefinitions_AlwaysApproves(t *testing.T) {
	// An operator may disable all detection by shipping an empty list. The
	// factory accepts this and Execute always returns APPROVE.
	hook, err := NewPiiDetector(makePiiConfig(nil, "block"))
	if err != nil {
		t.Fatalf("NewPiiDetector on empty patternDefinitions: %v", err)
	}
	input := &HookInput{
		Normalized: PayloadFromTextSegments([]string{"anything goes user@example.com 4532 0151 1283 0366"}),
	}
	result, err := hook.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Decision != Approve {
		t.Errorf("decision: expected APPROVE (empty patterns), got %s", result.Decision)
	}
}

// TestPiiDetector_ScopeIncludeReasoning: when the hook
// rule's Scope is set to "include_reasoning", PII patterns must fire on
// ContentReasoning blocks (model chain-of-thought / thinking text) in
// addition to visible text. With default scope, reasoning blocks bypass
// scan — today's behavior. The toggle is per-rule, so different rules
// in the same pipeline can opt independently.
func TestPiiDetector_ScopeIncludeReasoning(t *testing.T) {
	emailPattern := map[string]any{"id": "email", "regex": `\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`}

	// Payload: visible user text is clean; PII (email) appears only in the
	// model's reasoning content (e.g. model echoed user data while thinking).
	payload := PayloadFromTextSegments([]string{"Please process this customer ticket."})
	payload.Messages = append(payload.Messages, normalize.Message{
		Role: normalize.RoleAssistant,
		Content: []normalize.ContentBlock{
			{Type: normalize.ContentReasoning, Text: "Let me check: the user is leak@example.com per the prior turn."},
		},
	})

	t.Run("default_scope_does_not_scan_reasoning", func(t *testing.T) {
		cfg := makePiiConfig([]map[string]any{emailPattern}, "block")
		hook, err := NewPiiDetector(cfg)
		if err != nil {
			t.Fatalf("NewPiiDetector: %v", err)
		}
		input := &HookInput{Normalized: payload}
		result, err := hook.Execute(context.Background(), input)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if result.Decision != Approve {
			t.Errorf("decision = %s, want APPROVE (default scope skips reasoning)", result.Decision)
		}
	})

	t.Run("include_reasoning_scope_fires_on_reasoning_blocks", func(t *testing.T) {
		cfg := makePiiConfig([]map[string]any{emailPattern}, "block")
		cfg.Scope = "include_reasoning"
		hook, err := NewPiiDetector(cfg)
		if err != nil {
			t.Fatalf("NewPiiDetector: %v", err)
		}
		input := &HookInput{Normalized: payload}
		result, err := hook.Execute(context.Background(), input)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if result.Decision != RejectHard {
			t.Errorf("decision = %s, want REJECT_HARD (include_reasoning scans reasoning)", result.Decision)
		}
		if !strings.Contains(result.Reason, "email") {
			t.Errorf("reason: expected to mention 'email', got %q", result.Reason)
		}
	})
}

// TestPiiDetector_ActionDispatch covers the single-action match outcomes:
// redact rewrites inflight (Modify + spans + Action=redact), block rejects
// but still emits the spans that mask the stored audit copy (RejectHard +
// spans + Action=block), approve detects-and-tags only (Approve, no spans,
// Action empty), and a clean input stamps nothing under any action.
func TestPiiDetector_ActionDispatch(t *testing.T) {
	emailPattern := map[string]any{
		"id": "email", "regex": `\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`,
		"replacement": "[EMAIL]",
	}
	const dirty = "reach me at leak@example.com today"

	t.Run("redact_emits_spans_and_modifies", func(t *testing.T) {
		hook, err := NewPiiDetector(makePiiConfig([]map[string]any{emailPattern}, "redact"))
		if err != nil {
			t.Fatalf("NewPiiDetector: %v", err)
		}
		result, err := hook.Execute(context.Background(), &HookInput{Normalized: PayloadFromTextSegments([]string{dirty})})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if result.Decision != Modify {
			t.Errorf("decision = %s, want MODIFY", result.Decision)
		}
		if result.Action != ActionRedact {
			t.Errorf("result.Action = %q, want redact", result.Action)
		}
		if result.ReasonCode != "PII_REDACTED" {
			t.Errorf("reasonCode = %q, want PII_REDACTED", result.ReasonCode)
		}
		if len(result.TransformSpans) != 1 {
			t.Fatalf("spans = %d, want 1", len(result.TransformSpans))
		}
		s := result.TransformSpans[0]
		if s.SourceID != "email" || s.Replacement != "[EMAIL]" || s.ContentAddress != "messages.0.content.0" {
			t.Errorf("span = %+v, want email pattern at messages.0.content.0", s)
		}
	})

	t.Run("block_rejects_but_emits_audit_spans", func(t *testing.T) {
		hook, err := NewPiiDetector(makePiiConfig([]map[string]any{emailPattern}, "block"))
		if err != nil {
			t.Fatalf("NewPiiDetector: %v", err)
		}
		result, err := hook.Execute(context.Background(), &HookInput{Normalized: PayloadFromTextSegments([]string{dirty})})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if result.Decision != RejectHard {
			t.Errorf("decision = %s, want REJECT_HARD", result.Decision)
		}
		if result.Action != ActionBlock {
			t.Errorf("result.Action = %q, want block", result.Action)
		}
		if result.ReasonCode != "PII_DETECTED" {
			t.Errorf("reasonCode = %q, want PII_DETECTED", result.ReasonCode)
		}
		if len(result.TransformSpans) != 1 {
			t.Errorf("spans = %d, want 1 (blocked requests still redact their audit copy)", len(result.TransformSpans))
		}
	})

	t.Run("approve_detects_and_tags_only", func(t *testing.T) {
		hook, err := NewPiiDetector(makePiiConfig([]map[string]any{emailPattern}, "approve"))
		if err != nil {
			t.Fatalf("NewPiiDetector: %v", err)
		}
		result, err := hook.Execute(context.Background(), &HookInput{Normalized: PayloadFromTextSegments([]string{dirty})})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if result.Decision != Approve {
			t.Errorf("decision = %s, want APPROVE", result.Decision)
		}
		if result.Action != "" {
			t.Errorf("approve must not stamp result.Action, got %q", result.Action)
		}
		if len(result.TransformSpans) != 0 {
			t.Errorf("approve collects no spans, got %d", len(result.TransformSpans))
		}
		if !containsTag(result.Tags, "compliance:pii") {
			t.Errorf("approve still tags the detection, got %v", result.Tags)
		}
	})

	for _, action := range []string{"redact", "block", "approve"} {
		t.Run("no_match_stamps_nothing_"+action, func(t *testing.T) {
			hook, err := NewPiiDetector(makePiiConfig([]map[string]any{emailPattern}, action))
			if err != nil {
				t.Fatalf("NewPiiDetector: %v", err)
			}
			result, err := hook.Execute(context.Background(), &HookInput{Normalized: PayloadFromTextSegments([]string{"perfectly clean text"})})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if result.Decision != Approve || result.Action != "" || len(result.TransformSpans) != 0 {
				t.Errorf("clean input must not stamp action or spans: %+v", result)
			}
		})
	}
}

// TestPiiDetector_LegacyOnMatchKeysMapToAction locks the back-compat
// deprecation window: the deprecated inflightAction / storageAction pair is
// folded to the single action via ActionFromLegacy. approve-inflight +
// redacting-storage upgrades to redact (Modify + spans); block-hard maps to
// block (RejectHard).
func TestPiiDetector_LegacyOnMatchKeysMapToAction(t *testing.T) {
	emailPattern := map[string]any{
		"id": "email", "regex": `\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`,
		"replacement": "[EMAIL]",
	}
	const dirty = "reach me at leak@example.com today"

	t.Run("approve_inflight_redact_storage_becomes_redact", func(t *testing.T) {
		cfg := makePiiConfig([]map[string]any{emailPattern}, "")
		cfg.Config["onMatch"] = map[string]any{"inflightAction": "approve", "storageAction": "redact"}
		hook, err := NewPiiDetector(cfg)
		if err != nil {
			t.Fatalf("NewPiiDetector: %v", err)
		}
		result, err := hook.Execute(context.Background(), &HookInput{Normalized: PayloadFromTextSegments([]string{dirty})})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if result.Decision != Modify || result.Action != ActionRedact {
			t.Errorf("legacy approve+redact-storage → redact: decision=%s action=%q", result.Decision, result.Action)
		}
		if len(result.TransformSpans) != 1 {
			t.Errorf("spans = %d, want 1", len(result.TransformSpans))
		}
	})

	t.Run("block_hard_inflight_becomes_block", func(t *testing.T) {
		cfg := makePiiConfig([]map[string]any{emailPattern}, "")
		cfg.Config["onMatch"] = map[string]any{"inflightAction": "block-hard"}
		hook, err := NewPiiDetector(cfg)
		if err != nil {
			t.Fatalf("NewPiiDetector: %v", err)
		}
		result, err := hook.Execute(context.Background(), &HookInput{Normalized: PayloadFromTextSegments([]string{dirty})})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if result.Decision != RejectHard || result.Action != ActionBlock {
			t.Errorf("legacy block-hard → block: decision=%s action=%q", result.Decision, result.Action)
		}
	})
}

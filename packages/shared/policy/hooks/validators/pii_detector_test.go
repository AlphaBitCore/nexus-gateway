package validators

import (
	"context"
	"strings"
	"testing"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// makePiiConfig builds a HookConfig for the PiiDetector using the
// The `action` arg is the legacy term mapped onto
// onMatch.inflightAction:
//
//	"block"  → block-hard
//	"warn"   → block-soft
//	"redact" → redact
//	""       → omit onMatch (defaults to block-hard)
//
// An invalid action string is passed through verbatim so factory tests
// can assert that unknown inflightAction values are rejected.
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
	switch action {
	case "":
		// no onMatch — factory uses defaults
	case "block":
		cfg.Config["onMatch"] = map[string]any{"inflightAction": "block-hard"}
	case "warn":
		cfg.Config["onMatch"] = map[string]any{"inflightAction": "block-soft"}
	case "redact":
		cfg.Config["onMatch"] = map[string]any{"inflightAction": "redact"}
	default:
		cfg.Config["onMatch"] = map[string]any{"inflightAction": action}
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

func TestPiiDetector_Action_Warn(t *testing.T) {
	hook, err := NewPiiDetector(makePiiConfig(seedPiiPatterns(), "warn"))
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
	if result.Decision != BlockSoft {
		t.Errorf("decision: expected BLOCK_SOFT, got %s", result.Decision)
	}
}

func TestPiiDetector_Action_Unknown_Rejected(t *testing.T) {
	_, err := NewPiiDetector(makePiiConfig(seedPiiPatterns(), "purge"))
	if err == nil {
		t.Fatal("expected error for unknown inflightAction, got nil")
	}
	if !strings.Contains(err.Error(), "inflightAction") {
		t.Errorf("expected error to mention 'inflightAction', got: %v", err)
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

// Reject the old wire vocab as inflightAction values — these are not
// in the closed set {approve, block-hard, block-soft, redact}.
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

// TestPiiDetector_StorageOnlyRedact covers the "let the request flow but
// redact what we persist" policy: inflightAction approve (or block) with
// storageAction redact must yield TransformSpans + a self-stamped storage
// action — the pipeline only stamps storage policy for non-Approve
// decisions, and the audit writer's storage rewrite is span-driven, so
// without both the persisted copy would silently keep the matched PII.
func TestPiiDetector_StorageOnlyRedact(t *testing.T) {
	emailPattern := map[string]any{
		"id": "email", "regex": `\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`,
		"replacement": "[EMAIL]",
	}

	t.Run("approve_inflight_redact_storage_emits_spans", func(t *testing.T) {
		cfg := makePiiConfig([]map[string]any{emailPattern}, "")
		cfg.Config["onMatch"] = map[string]any{"inflightAction": "approve", "storageAction": "redact"}
		hook, err := NewPiiDetector(cfg)
		if err != nil {
			t.Fatalf("NewPiiDetector: %v", err)
		}
		input := &HookInput{Normalized: PayloadFromTextSegments([]string{"reach me at leak@example.com today"})}
		result, err := hook.Execute(context.Background(), input)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if result.Decision != Approve {
			t.Errorf("decision = %s, want APPROVE (inflight untouched)", result.Decision)
		}
		if len(result.TransformSpans) != 1 {
			t.Fatalf("spans = %d, want 1", len(result.TransformSpans))
		}
		s := result.TransformSpans[0]
		if s.SourceID != "email" || s.Replacement != "[EMAIL]" || s.ContentAddress != "messages.0.content.0" {
			t.Errorf("span = %+v, want email pattern at messages.0.content.0", s)
		}
		if string(result.StorageAction) != "redact" {
			t.Errorf("storageAction = %q, want self-stamped redact", result.StorageAction)
		}
		if result.ReasonCode != "PII_DETECTED" {
			t.Errorf("reasonCode = %q, want PII_DETECTED", result.ReasonCode)
		}
	})

	t.Run("block_inflight_redact_storage_emits_spans", func(t *testing.T) {
		cfg := makePiiConfig([]map[string]any{emailPattern}, "")
		cfg.Config["onMatch"] = map[string]any{"inflightAction": "block-hard", "storageAction": "redact"}
		hook, err := NewPiiDetector(cfg)
		if err != nil {
			t.Fatalf("NewPiiDetector: %v", err)
		}
		input := &HookInput{Normalized: PayloadFromTextSegments([]string{"reach me at leak@example.com today"})}
		result, err := hook.Execute(context.Background(), input)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if result.Decision != RejectHard {
			t.Errorf("decision = %s, want REJECT_HARD", result.Decision)
		}
		if len(result.TransformSpans) != 1 {
			t.Errorf("spans = %d, want 1 (blocked requests still redact their audit copy)", len(result.TransformSpans))
		}
		if string(result.StorageAction) != "redact" {
			t.Errorf("storageAction = %q, want redact", result.StorageAction)
		}
	})

	t.Run("approve_inflight_drop_content_storage_stamps_action", func(t *testing.T) {
		cfg := makePiiConfig([]map[string]any{emailPattern}, "")
		cfg.Config["onMatch"] = map[string]any{"inflightAction": "approve", "storageAction": "drop-content"}
		hook, err := NewPiiDetector(cfg)
		if err != nil {
			t.Fatalf("NewPiiDetector: %v", err)
		}
		input := &HookInput{Normalized: PayloadFromTextSegments([]string{"reach me at leak@example.com today"})}
		result, err := hook.Execute(context.Background(), input)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if result.Decision != Approve {
			t.Errorf("decision = %s, want APPROVE", result.Decision)
		}
		if string(result.StorageAction) != "drop-content" {
			t.Errorf("storageAction = %q, want self-stamped drop-content", result.StorageAction)
		}
	})

	t.Run("no_match_stamps_nothing", func(t *testing.T) {
		cfg := makePiiConfig([]map[string]any{emailPattern}, "")
		cfg.Config["onMatch"] = map[string]any{"inflightAction": "approve", "storageAction": "redact"}
		hook, err := NewPiiDetector(cfg)
		if err != nil {
			t.Fatalf("NewPiiDetector: %v", err)
		}
		input := &HookInput{Normalized: PayloadFromTextSegments([]string{"perfectly clean text"})}
		result, err := hook.Execute(context.Background(), input)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if result.Decision != Approve || result.StorageAction != "" || len(result.TransformSpans) != 0 {
			t.Errorf("clean input must not stamp storage policy or spans: %+v", result)
		}
	})
}

// --- RedetectText: storage-time pattern re-scanning --------------------------

func TestPiiDetector_RedetectText_FindsRequestedRulesOnly(t *testing.T) {
	hook, err := NewPiiDetector(makePiiConfig(seedPiiPatterns(), "redact"))
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	pd := hook.(*PiiDetector)
	text := "mail alice@example.com or call 555-123-4567"

	got := pd.RedetectText(text, []string{"email"})
	if len(got) != 1 {
		t.Fatalf("want only the email match, got %v", got)
	}
	m := got[0]
	if m.RuleID != "email" {
		t.Errorf("ruleID = %q, want email", m.RuleID)
	}
	if text[m.Start:m.End] != "alice@example.com" {
		t.Errorf("match range = %q, want the email", text[m.Start:m.End])
	}
	if m.Replacement != "[REDACTED_EMAIL]" {
		t.Errorf("replacement = %q, want [REDACTED_EMAIL]", m.Replacement)
	}

	both := pd.RedetectText(text, []string{"email", "phone"})
	if len(both) != 2 {
		t.Errorf("want email+phone matches, got %v", both)
	}
}

func TestPiiDetector_RedetectText_EmptyInputs(t *testing.T) {
	hook, err := NewPiiDetector(makePiiConfig(seedPiiPatterns(), "redact"))
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	pd := hook.(*PiiDetector)
	if got := pd.RedetectText("", []string{"email"}); got != nil {
		t.Errorf("empty text → nil, got %v", got)
	}
	if got := pd.RedetectText("alice@example.com", nil); got != nil {
		t.Errorf("no rule ids → nil, got %v", got)
	}
	if got := pd.RedetectText("no pii here", []string{"email"}); got != nil {
		t.Errorf("no match → nil, got %v", got)
	}
}

func TestPiiDetector_RedetectText_LuhnFilterApplies(t *testing.T) {
	patterns := []map[string]any{
		{"id": "credit_card", "regex": `\b(?:\d{4}[-\s]?){3}\d{4}\b`, "flags": "g", "luhn": true},
	}
	hook, err := NewPiiDetector(makePiiConfig(patterns, "redact"))
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	pd := hook.(*PiiDetector)
	// 4111111111111111 passes Luhn; 1234567812345678 does not.
	if got := pd.RedetectText("card 4111-1111-1111-1111", []string{"credit_card"}); len(got) != 1 {
		t.Errorf("Luhn-valid card must match, got %v", got)
	}
	if got := pd.RedetectText("card 1234-5678-1234-5678", []string{"credit_card"}); got != nil {
		t.Errorf("Luhn-invalid card must not match, got %v", got)
	}
}

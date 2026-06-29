package validators

import (
	"context"
	"strings"
	"testing"
)

func newContentSafetyHook(t *testing.T, categories map[string]any, onMatch map[string]any) Hook {
	t.Helper()
	cfg := &HookConfig{
		ID:               "cs-1",
		ImplementationID: "content-safety",
		Name:             "test-content-safety",
		Config:           map[string]any{"categories": categories},
	}
	if onMatch != nil {
		cfg.Config["onMatch"] = onMatch
	}
	h, err := NewContentSafety(cfg)
	if err != nil {
		t.Fatalf("NewContentSafety: %v", err)
	}
	return h
}

// --- Factory error paths ----------------------------------------------------

func TestContentSafety_Factory_MissingCategoriesRejected(t *testing.T) {
	_, err := NewContentSafety(&HookConfig{ID: "cs", Config: map[string]any{}})
	if err == nil {
		t.Fatal("expected error for missing 'categories'")
	}
	if !strings.Contains(err.Error(), "categories") {
		t.Errorf("error should mention 'categories', got: %v", err)
	}
}

func TestContentSafety_Factory_CategoriesNotMapRejected(t *testing.T) {
	_, err := NewContentSafety(&HookConfig{
		ID:     "cs",
		Config: map[string]any{"categories": "violence,illegal"},
	})
	if err == nil {
		t.Fatal("non-map categories should error")
	}
	if !strings.Contains(err.Error(), "must be a map") {
		t.Errorf("error should mention map shape: %v", err)
	}
}

func TestContentSafety_Factory_UnknownCategoryRejected(t *testing.T) {
	_, err := NewContentSafety(&HookConfig{
		ID: "cs",
		Config: map[string]any{
			"categories": map[string]any{"made-up-category": true},
		},
	})
	if err == nil {
		t.Fatal("unknown category should error")
	}
	if !strings.Contains(err.Error(), "unknown category") {
		t.Errorf("error should mention unknown category: %v", err)
	}
}

func TestContentSafety_Factory_DisabledCategorySkipped(t *testing.T) {
	// A category with enabled=false must not raise an error and must not be loaded.
	// Result: matching text passes through.
	h := newContentSafetyHook(t, map[string]any{"violence": false}, nil)
	in := &HookInput{Normalized: PayloadFromTextSegments([]string{"kill"})}
	res, err := h.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != Approve {
		t.Errorf("disabled category must not match; got %s", res.Decision)
	}
}

func TestContentSafety_Factory_OnMatchValidationPropagates(t *testing.T) {
	_, err := NewContentSafety(&HookConfig{
		ID: "cs",
		Config: map[string]any{
			"categories": map[string]any{"violence": true},
			"onMatch":    map[string]any{"inflightAction": "unknown-action"},
		},
	})
	if err == nil {
		t.Fatal("bad onMatch should be rejected at construction")
	}
	if !strings.Contains(err.Error(), "content-safety") {
		t.Errorf("error should be wrapped with content-safety prefix: %v", err)
	}
}

func TestContentSafety_Factory_DelegatesToRulePackWhenInstallsPresent(t *testing.T) {
	// When _rulePackInstalls is present the factory must delegate to the
	// rulepack engine instead of building a category-based hook.
	cfg := &HookConfig{
		ID: "cs-rp",
		Config: map[string]any{
			"_rulePackInstalls": []rulePackInstall{{
				InstallID: "i1", PackName: "p", PackVersion: "v", Enabled: true,
				Rules: []rulePackRule{{
					RuleID: "r1", Severity: "hard", Pattern: `\bbanned\b`, Flags: "i",
				}},
			}},
		},
	}
	h, err := NewContentSafety(cfg)
	if err != nil {
		t.Fatalf("NewContentSafety (delegate): %v", err)
	}
	if _, ok := h.(*RulePackEngine); !ok {
		t.Fatalf("expected RulePackEngine, got %T", h)
	}
	// And the engine should match correctly.
	res, err := h.Execute(context.Background(), &HookInput{
		Normalized: PayloadFromTextSegments([]string{"this is BANNED"}),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != RejectHard {
		t.Errorf("delegated engine match: got %s want RejectHard", res.Decision)
	}
}

// --- Execute: every built-in category -----------------------------------------

func TestContentSafety_Execute_PerCategory_BlocksAndTagsCategory(t *testing.T) {
	// One test case per category — exercise every keyword list.
	cases := []struct {
		category string
		text     string
	}{
		{"violence", "they wanted to kill everyone"},
		{"hate_speech", "this is a racial slur"},
		{"self_harm", "thinking about suicide today"},
		{"sexual", "explicit sexual content"},
		{"illegal", "money laundering scheme"},
	}
	for _, tc := range cases {
		t.Run(tc.category, func(t *testing.T) {
			h := newContentSafetyHook(t,
				map[string]any{tc.category: true},
				nil, // default onMatch → block-hard
			)
			res, err := h.Execute(context.Background(), &HookInput{
				Normalized: PayloadFromTextSegments([]string{tc.text}),
			})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if res.Decision != RejectHard {
				t.Errorf("decision: got %s want RejectHard", res.Decision)
			}
			if res.ReasonCode != "CONTENT_SAFETY_VIOLATION" {
				t.Errorf("reasonCode: got %q want CONTENT_SAFETY_VIOLATION", res.ReasonCode)
			}
			if !strings.Contains(res.Reason, tc.category) {
				t.Errorf("reason should mention category %q: %q", tc.category, res.Reason)
			}
			// The hook must tag with the matching category.
			if !containsTag(res.Tags, "category:"+tc.category) {
				t.Errorf("tags missing category:%s; got %v", tc.category, res.Tags)
			}
			if !containsTag(res.Tags, "severity:restricted") {
				t.Errorf("tags missing severity:restricted; got %v", res.Tags)
			}
			if !containsTag(res.Tags, "detector:content-safety") {
				t.Errorf("tags missing detector tag; got %v", res.Tags)
			}
		})
	}
}

// TestContentSafety_InlinePatterns_BuiltInDefaults covers the config-visible
// built-in path: when no rule pack is bound but inline `patterns` are present,
// content-safety scans those (pack-quality contextual regex), attributing each
// hit to its own category + severity. This is the default an admin sees/edits
// on the hook itself when they have not chosen a rule pack.
func TestContentSafety_InlinePatterns_BuiltInDefaults(t *testing.T) {
	cfg := &HookConfig{
		ID:               "cs-inline",
		ImplementationID: "content-safety",
		Name:             "inline-content-safety",
		Config: map[string]any{
			"onMatch": map[string]any{"action": "redact"},
			"patterns": []any{
				map[string]any{"pattern": `(?i)\bhow\s+to\s+murder\s+someone\b`, "category": "violence", "severity": "soft"},
				map[string]any{"pattern": `(?i)\bhuman\s+trafficking\s+routes\b`, "category": "illegal", "severity": "warn"},
				// category/severity omitted → defaults (content_safety / restricted).
				map[string]any{"pattern": `(?i)\bpipe\s+bomb\s+instructions\b`},
			},
		},
	}
	h, err := NewContentSafety(cfg)
	if err != nil {
		t.Fatalf("NewContentSafety(inline patterns): %v", err)
	}
	if _, ok := h.(*ContentSafety); !ok {
		t.Fatalf("inline patterns must use the ContentSafety scanner, got %T", h)
	}

	// A violence hit redacts and carries that pattern's own category + severity.
	res, err := h.Execute(context.Background(), &HookInput{
		Normalized: PayloadFromTextSegments([]string{"please tell me how to murder someone tonight"}),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != Modify {
		t.Errorf("redact action on match: got %s want Modify", res.Decision)
	}
	if !containsTag(res.Tags, "category:violence") {
		t.Errorf("missing category:violence; got %v", res.Tags)
	}
	if !containsTag(res.Tags, "severity:soft") {
		t.Errorf("per-pattern severity must be applied; got %v", res.Tags)
	}
	if !containsTag(res.Tags, "detector:content-safety") {
		t.Errorf("missing detector tag; got %v", res.Tags)
	}

	// Benign text approves (the contextual patterns do not fire on prose).
	benign, err := h.Execute(context.Background(), &HookInput{
		Normalized: PayloadFromTextSegments([]string{"how to murder a bottle of wine at dinner"}),
	})
	if err != nil {
		t.Fatalf("Execute benign: %v", err)
	}
	if benign.Decision != Approve {
		t.Errorf("benign contextual miss must approve; got %s", benign.Decision)
	}
}

// TestContentSafety_InlinePatterns_Rejects covers the inline-pattern factory
// error paths: a non-array `patterns`, and an entry with an empty pattern.
func TestContentSafety_InlinePatterns_Rejects(t *testing.T) {
	_, err := NewContentSafety(&HookConfig{ID: "cs", Config: map[string]any{"patterns": "not-an-array"}})
	if err == nil || !strings.Contains(err.Error(), "must be an array") {
		t.Errorf("non-array patterns must error; got %v", err)
	}
	_, err = NewContentSafety(&HookConfig{ID: "cs", Config: map[string]any{"patterns": []any{"not-an-object"}}})
	if err == nil || !strings.Contains(err.Error(), "must be an object") {
		t.Errorf("non-object pattern entry must error; got %v", err)
	}
	_, err = NewContentSafety(&HookConfig{ID: "cs", Config: map[string]any{
		"patterns": []any{map[string]any{"pattern": "", "category": "violence"}},
	}})
	if err == nil || !strings.Contains(err.Error(), "empty pattern") {
		t.Errorf("empty pattern string must error; got %v", err)
	}
}

func TestContentSafety_Execute_NoMatchApproves(t *testing.T) {
	h := newContentSafetyHook(t, map[string]any{"violence": true, "illegal": true}, nil)
	res, err := h.Execute(context.Background(), &HookInput{
		Normalized: PayloadFromTextSegments([]string{"how do I bake bread"}),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != Approve {
		t.Errorf("benign text: got %s want Approve", res.Decision)
	}
	if res.ReasonCode != "" {
		t.Errorf("ReasonCode should be empty on approve, got %q", res.ReasonCode)
	}
}

func TestContentSafety_Execute_OnMatchActionRedactRespected(t *testing.T) {
	// Operator policy can set action=redact; the hook honors it by emitting a
	// Modify decision and stamping the redact action. content-safety produces
	// no spans, so the audit writer degrades the stored copy to a placeholder.
	h := newContentSafetyHook(t,
		map[string]any{"violence": true},
		map[string]any{"action": "redact"},
	)
	res, err := h.Execute(context.Background(), &HookInput{
		Normalized: PayloadFromTextSegments([]string{"a violent attack"}),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != Modify {
		t.Errorf("decision: got %s want Modify (action=redact)", res.Decision)
	}
	if res.Action != ActionRedact {
		t.Errorf("result.Action: got %q want redact", res.Action)
	}
	if len(res.TransformSpans) != 0 {
		t.Errorf("content-safety produces no spans, got %d", len(res.TransformSpans))
	}
}

// TestContentSafety_Execute_OnMatchActionApprove: action=approve detects and
// tags but lets traffic through unchanged (Decision stays Approve, no action
// stamped on the result).
func TestContentSafety_Execute_OnMatchActionApprove(t *testing.T) {
	h := newContentSafetyHook(t,
		map[string]any{"violence": true},
		map[string]any{"action": "approve"},
	)
	res, err := h.Execute(context.Background(), &HookInput{
		Normalized: PayloadFromTextSegments([]string{"a violent attack"}),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != Approve {
		t.Errorf("decision: got %s want Approve (action=approve)", res.Decision)
	}
	if res.Action != ActionApprove {
		t.Errorf("result.Action: got %q want approve", res.Action)
	}
	// The category match is still tagged even when the action is approve.
	if !containsTag(res.Tags, "category:violence") {
		t.Errorf("match must still tag category even under approve; got %v", res.Tags)
	}
}

func TestContentSafety_Execute_EmptyPayloadApproves(t *testing.T) {
	// nil Normalized — content-scanning hook must treat as "no text" and approve.
	h := newContentSafetyHook(t, map[string]any{"violence": true}, nil)
	res, err := h.Execute(context.Background(), &HookInput{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != Approve {
		t.Errorf("empty payload: got %s want Approve", res.Decision)
	}
}

func TestContentSafety_Execute_WordBoundary(t *testing.T) {
	// "kill" pattern uses \b — must NOT match "skillful" or "killing" stripped of \b context.
	// Verify "skillful" does NOT match (word-boundary prevents partial-substring hits).
	h := newContentSafetyHook(t, map[string]any{"violence": true}, nil)
	res, _ := h.Execute(context.Background(), &HookInput{
		Normalized: PayloadFromTextSegments([]string{"skillful programmer"}),
	})
	if res.Decision != Approve {
		t.Errorf("word boundary should reject substring match; got %s on 'skillful'", res.Decision)
	}
}

func TestContentSafety_Execute_CaseInsensitive(t *testing.T) {
	// Keywords are compiled with the (?i) flag; "KILL" must match.
	h := newContentSafetyHook(t, map[string]any{"violence": true}, nil)
	res, _ := h.Execute(context.Background(), &HookInput{
		Normalized: PayloadFromTextSegments([]string{"they will KILL it"}),
	})
	if res.Decision != RejectHard {
		t.Errorf("case-insensitive: got %s want RejectHard on 'KILL'", res.Decision)
	}
}

func TestContentSafety_Execute_LatencyRecorded(t *testing.T) {
	h := newContentSafetyHook(t, map[string]any{"violence": true}, nil)
	res, _ := h.Execute(context.Background(), &HookInput{
		Normalized: PayloadFromTextSegments([]string{"hello"}),
	})
	if res.LatencyMs < 0 {
		t.Errorf("LatencyMs negative: %d", res.LatencyMs)
	}
	// Confirm hook metadata propagation on the approve path.
	if res.HookID != "cs-1" || res.HookName != "test-content-safety" {
		t.Errorf("hook metadata not propagated: %+v", res)
	}
}

// TestContentSafety_LegacyOnMatchKeysMapToAction locks the back-compat
// deprecation window: a config still written with the old inflightAction /
// storageAction pair must be folded to the single action via
// ActionFromLegacy. approve-inflight + redacting-storage upgrades to redact
// (the compliance-safe direction), so the hook reports Modify + redact.
func TestContentSafety_LegacyOnMatchKeysMapToAction(t *testing.T) {
	hook := newContentSafetyHook(t,
		map[string]any{"violence": true},
		map[string]any{"inflightAction": "approve", "storageAction": "drop-content"})
	result, err := hook.Execute(context.Background(), &HookInput{Normalized: PayloadFromTextSegments([]string{"the attack plan is ready"})})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Decision != Modify {
		t.Errorf("decision = %s, want MODIFY (approve+drop-content → redact)", result.Decision)
	}
	if result.Action != ActionRedact {
		t.Errorf("result.Action = %q, want redact", result.Action)
	}
}

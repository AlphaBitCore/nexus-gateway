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

func TestContentSafety_Execute_OnMatchInflightActionRespected(t *testing.T) {
	// Operator policy can downgrade to block-soft; the hook must honor it.
	h := newContentSafetyHook(t,
		map[string]any{"violence": true},
		map[string]any{"inflightAction": "block-soft"},
	)
	res, err := h.Execute(context.Background(), &HookInput{
		Normalized: PayloadFromTextSegments([]string{"a violent attack"}),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != BlockSoft {
		t.Errorf("decision: got %s want BlockSoft (operator override)", res.Decision)
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

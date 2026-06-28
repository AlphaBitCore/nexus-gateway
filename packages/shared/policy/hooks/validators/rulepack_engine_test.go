package validators

import (
	"context"
	"strings"
	"testing"
)

// buildEngineConfig assembles a HookConfig with an inline
// _rulePackInstalls payload suitable for NewRulePackEngine tests.
func buildEngineConfig(installs []rulePackInstall) *HookConfig {
	return &HookConfig{
		ID:               "hook-rulepack-test",
		Name:             "test-rulepack",
		ImplementationID: "rulepack-engine",
		Config: map[string]any{
			"_rulePackInstalls": installs,
		},
	}
}

func TestRulePackEngine_HardMatch_RejectsWithBlockingRule(t *testing.T) {
	cfg := buildEngineConfig([]rulePackInstall{{
		InstallID:   "inst-1",
		PackName:    "safety",
		PackVersion: "1.0.0",
		Enabled:     true,
		Rules: []rulePackRule{{
			RuleID:   "violence-kill",
			Category: "safety",
			Severity: "hard",
			Pattern:  `\bkill\b`,
			Flags:    "i",
			Labels:   []string{"detector:content-safety", "category:violence"},
		}},
	}})

	h, err := NewRulePackEngine(cfg)
	if err != nil {
		t.Fatalf("NewRulePackEngine: %v", err)
	}

	in := &HookInput{
		Stage:      "request",
		Normalized: PayloadFromTextSegments([]string{"I want to KILL the process"}),
	}
	res, err := h.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != RejectHard {
		t.Fatalf("Decision = %s, want REJECT_HARD", res.Decision)
	}
	if res.ReasonCode != "RULEPACK_MATCH" {
		t.Errorf("ReasonCode = %q, want RULEPACK_MATCH", res.ReasonCode)
	}
	if res.BlockingRule == nil {
		t.Fatal("BlockingRule is nil; expected attribution")
	}
	if res.BlockingRule.Pack != "safety" ||
		res.BlockingRule.PackVersion != "1.0.0" ||
		res.BlockingRule.RuleID != "violence-kill" ||
		res.BlockingRule.Category != "safety" ||
		res.BlockingRule.Severity != "hard" {
		t.Errorf("BlockingRule = %+v, mismatch", res.BlockingRule)
	}
	gotLabels := strings.Join(res.BlockingRule.Labels, ",")
	if !strings.Contains(gotLabels, "detector:content-safety") ||
		!strings.Contains(gotLabels, "category:violence") {
		t.Errorf("BlockingRule.Labels missing expected entries: %v", res.BlockingRule.Labels)
	}
	if !containsString(res.Tags, "rulepack:safety") {
		t.Errorf("Tags missing rulepack:safety: %v", res.Tags)
	}
	if !containsString(res.Tags, "rule:violence-kill") {
		t.Errorf("Tags missing rule:violence-kill: %v", res.Tags)
	}
}

func TestRulePackEngine_SoftMatchBlocks(t *testing.T) {
	// soft-severity was merged into the single block action: a soft rule now
	// rejects hard and stamps result.Action=block.
	cfg := buildEngineConfig([]rulePackInstall{{
		InstallID:   "inst-2",
		PackName:    "soft-pack",
		PackVersion: "0.1.0",
		Enabled:     true,
		Rules: []rulePackRule{{
			RuleID:   "profanity-1",
			Category: "tone",
			Severity: "soft",
			Pattern:  `dang`,
			Flags:    "i",
		}},
	}})
	h, err := NewRulePackEngine(cfg)
	if err != nil {
		t.Fatalf("NewRulePackEngine: %v", err)
	}
	res, err := h.Execute(context.Background(), &HookInput{
		Normalized: PayloadFromTextSegments([]string{"dang it"}),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != RejectHard {
		t.Fatalf("Decision = %s, want REJECT_HARD (soft merged into block)", res.Decision)
	}
	if res.Action != ActionBlock {
		t.Errorf("result.Action = %q, want block", res.Action)
	}
	if res.BlockingRule == nil || res.BlockingRule.RuleID != "profanity-1" {
		t.Errorf("BlockingRule = %+v", res.BlockingRule)
	}
}

func TestRulePackEngine_NoMatch_Approves(t *testing.T) {
	cfg := buildEngineConfig([]rulePackInstall{{
		InstallID:   "inst-3",
		PackName:    "pack",
		PackVersion: "1.0.0",
		Enabled:     true,
		Rules: []rulePackRule{{
			RuleID:   "r1",
			Category: "x",
			Severity: "hard",
			Pattern:  `foobar`,
		}},
	}})
	h, _ := NewRulePackEngine(cfg)
	res, err := h.Execute(context.Background(), &HookInput{
		Normalized: PayloadFromTextSegments([]string{"hello world"}),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != Approve {
		t.Errorf("Decision = %s, want APPROVE", res.Decision)
	}
	if res.BlockingRule != nil {
		t.Errorf("BlockingRule should be nil, got %+v", res.BlockingRule)
	}
}

func TestRulePackEngine_DisabledInstallIsSkipped(t *testing.T) {
	cfg := buildEngineConfig([]rulePackInstall{{
		InstallID: "inst-disabled", PackName: "p", PackVersion: "v", Enabled: false,
		Rules: []rulePackRule{{RuleID: "r1", Severity: "hard", Pattern: `bad`}},
	}})
	h, _ := NewRulePackEngine(cfg)
	res, _ := h.Execute(context.Background(), &HookInput{
		Normalized: PayloadFromTextSegments([]string{"bad things happen"}),
	})
	if res.Decision != Approve {
		t.Errorf("disabled install should not match; got %s", res.Decision)
	}
}

func TestRulePackEngine_InvalidRegex_SkippedNotFatal(t *testing.T) {
	// Availability-first: a rule whose pattern fails to compile is
	// skipped at construction time, not fatal. The factory still returns a
	// usable engine so one bad rule degrades to "that rule off" rather than
	// taking the entire pack (and therefore the pipeline) offline.
	cfg := buildEngineConfig([]rulePackInstall{{
		InstallID: "inst-x", PackName: "p", PackVersion: "v", Enabled: true,
		Rules: []rulePackRule{{RuleID: "r-bad", Severity: "hard", Pattern: `(`}},
	}})
	h, err := NewRulePackEngine(cfg)
	if err != nil {
		t.Fatalf("invalid regex must be skipped, not fatal: %v", err)
	}
	if h == nil {
		t.Fatal("factory must return a usable engine even when a rule is dropped")
	}
	// The bad rule produces no compiled rule, so even text that the author
	// intended to match yields APPROVE (the rule is simply absent).
	res, err := h.Execute(context.Background(), &HookInput{
		Normalized: PayloadFromTextSegments([]string{"anything ( here"}),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != Approve {
		t.Errorf("dropped rule must not block; got %s", res.Decision)
	}
}

func TestRulePackEngine_InvalidRegex_GoodRuleSurvives(t *testing.T) {
	// A bad rule alongside a good one drops only the bad one; the good rule
	// still blocks. This proves the skip is per-rule, not per-pack.
	cfg := buildEngineConfig([]rulePackInstall{{
		InstallID: "inst-y", PackName: "p", PackVersion: "v", Enabled: true,
		Rules: []rulePackRule{
			{RuleID: "r-bad", Severity: "hard", Pattern: `(`},
			{RuleID: "r-good", Category: "safety", Severity: "hard", Pattern: `\bsecret\b`},
		},
	}})
	h, err := NewRulePackEngine(cfg)
	if err != nil {
		t.Fatalf("NewRulePackEngine: %v", err)
	}
	res, err := h.Execute(context.Background(), &HookInput{
		Normalized: PayloadFromTextSegments([]string{"this is secret"}),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != RejectHard {
		t.Errorf("good rule alongside a dropped bad rule must still block; got %s", res.Decision)
	}
	if res.BlockingRule == nil || res.BlockingRule.RuleID != "r-good" {
		t.Errorf("blocking rule should be r-good; got %+v", res.BlockingRule)
	}
}

func TestRulePackEngine_JSONShapeFromLoader(t *testing.T) {
	cfg := &HookConfig{
		ID:               "hook-json",
		ImplementationID: "rulepack-engine",
		Config: map[string]any{
			"_rulePackInstalls": []any{
				map[string]any{
					"installId":   "i-1",
					"packName":    "safety",
					"packVersion": "1.0.0",
					"enabled":     true,
					"rules": []any{
						map[string]any{
							"ruleId":   "r-1",
							"category": "safety",
							"severity": "hard",
							"pattern":  `\bbad\b`,
							"flags":    "i",
							"labels":   []any{"detector:safety"},
						},
					},
				},
			},
		},
	}
	h, err := NewRulePackEngine(cfg)
	if err != nil {
		t.Fatalf("NewRulePackEngine (json shape): %v", err)
	}
	res, err := h.Execute(context.Background(), &HookInput{
		Normalized: PayloadFromTextSegments([]string{"this is BAD"}),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != RejectHard {
		t.Fatalf("Decision = %s, want REJECT_HARD", res.Decision)
	}
	if res.BlockingRule == nil || res.BlockingRule.Pack != "safety" {
		t.Errorf("BlockingRule = %+v", res.BlockingRule)
	}
}

func TestLegacyHooks_DelegateToEngineWhenPackPresent(t *testing.T) {
	installs := []rulePackInstall{{
		InstallID: "i-1", PackName: "safety", PackVersion: "1.0.0", Enabled: true,
		Rules: []rulePackRule{{RuleID: "r-1", Category: "safety", Severity: "hard", Pattern: `\bpayload\b`}},
	}}

	cases := []struct {
		name string
		impl string
		cfg  *HookConfig
	}{
		{
			name: "content-safety delegates",
			impl: "content-safety",
			cfg: &HookConfig{
				ID: "cs-hook", ImplementationID: "content-safety",
				Config: map[string]any{
					"_rulePackInstalls": installs,
					// Legacy fields intentionally left out — engine path
					// must not look at them.
				},
			},
		},
		{
			name: "keyword-filter delegates",
			impl: "keyword-filter",
			cfg: &HookConfig{
				ID: "kf-hook", ImplementationID: "keyword-filter",
				Config: map[string]any{"_rulePackInstalls": installs},
			},
		},
		{
			name: "pii-detector delegates",
			impl: "pii-detector",
			cfg: &HookConfig{
				ID: "pii-hook", ImplementationID: "pii-detector",
				Config: map[string]any{"_rulePackInstalls": installs},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			factory := Registry.Get(tc.impl)
			if factory == nil {
				t.Fatalf("no factory for %s", tc.impl)
			}
			h, err := factory(tc.cfg)
			if err != nil {
				t.Fatalf("factory(%s): %v", tc.impl, err)
			}
			res, err := h.Execute(context.Background(), &HookInput{
				Normalized: PayloadFromTextSegments([]string{"payload here"}),
			})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if res.Decision != RejectHard {
				t.Fatalf("Decision = %s, want REJECT_HARD", res.Decision)
			}
			if res.BlockingRule == nil {
				t.Fatal("BlockingRule nil; legacy hook did not delegate to engine")
			}
		})
	}
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

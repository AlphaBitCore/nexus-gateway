package rulepack_test

import (
	"encoding/json"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/rulepack"
)

func TestPack_JSONRoundtrip(t *testing.T) {
	p := rulepack.Pack{
		Name: "nexus/prompt-injection", Version: "v1.0.0", Maintainer: "nexus",
		Description: "demo",
		Rules: []rulepack.Rule{{
			RuleID: "pi-001", Category: "prompt_injection", Severity: "hard",
			Pattern: `(?i)ignore previous`, Labels: []string{"detector:prompt-injection"},
		}},
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var got rulepack.Pack
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != p.Name || got.Version != p.Version || len(got.Rules) != 1 {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if got.Rules[0].Labels[0] != "detector:prompt-injection" {
		t.Fatalf("labels lost: %+v", got.Rules[0])
	}
}

func TestMatch_ShapesRuleIdentity(t *testing.T) {
	m := rulepack.Match{
		PackName:    "nexus/prompt-injection",
		PackVersion: "v1.0.0",
		RuleLocalID: "pi-001",
		Category:    "prompt_injection",
		Severity:    "hard",
		Labels:      []string{"detector:prompt-injection"},
	}
	if m.PackName == "" || m.RuleLocalID == "" {
		t.Fatal("identity fields required")
	}
}

func TestBlockingRule_JSONShape(t *testing.T) {
	br := rulepack.BlockingRule{Pack: "nexus/prompt-injection", PackVersion: "v1.0.0", RuleID: "pi-001"}
	b, _ := json.Marshal(br)
	got := string(b)
	// Audit schema: JSON tags are pack, pack_version, rule_id (snake_case).
	for _, needle := range []string{`"pack":"nexus/prompt-injection"`, `"pack_version":"v1.0.0"`, `"rule_id":"pi-001"`} {
		if !contains(got, needle) {
			t.Errorf("missing %q in %s", needle, got)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || (len(s) > len(sub) && (s[:len(sub)] == sub || contains(s[1:], sub))))
}

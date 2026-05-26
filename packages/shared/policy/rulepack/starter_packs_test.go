// packages/shared/policy/rulepack/starter_packs_test.go
package rulepack_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/rulepack"
)

func TestStarterPack_PromptInjection_LoadsAndHasRules(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "tools", "db-migrate", "seed", "rule-packs",
		"nexus-prompt-injection-v1.0.0.yaml"))
	if err != nil {
		t.Fatalf("read yaml: %v", err)
	}
	p, warnings, err := rulepack.LoadYAML(data)
	if err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}
	if p.Name != "nexus/prompt-injection" {
		t.Errorf("name: %q", p.Name)
	}
	if len(p.Rules) < 15 || len(p.Rules) > 35 {
		t.Errorf("want 15-35 rules per scope cap, got %d", len(p.Rules))
	}
	if len(warnings) > 0 {
		t.Logf("warnings (non-fatal): %v", warnings)
	}
}

func TestStarterPack_Jailbreak_LoadsAndHasRules(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "tools", "db-migrate", "seed", "rule-packs",
		"nexus-jailbreak-v1.0.0.yaml"))
	if err != nil {
		t.Fatalf("read yaml: %v", err)
	}
	p, _, err := rulepack.LoadYAML(data)
	if err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}
	if p.Name != "nexus/jailbreak" {
		t.Errorf("name: %q", p.Name)
	}
	if len(p.Rules) < 10 || len(p.Rules) > 25 {
		t.Errorf("want 10-25 rules, got %d", len(p.Rules))
	}
}

func TestStarterPack_SecretLeak_LoadsAndHasRules(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "tools", "db-migrate", "seed", "rule-packs",
		"nexus-secret-leak-v1.0.0.yaml"))
	if err != nil {
		t.Fatalf("read yaml: %v", err)
	}
	p, _, err := rulepack.LoadYAML(data)
	if err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}
	if p.Name != "nexus/secret-leak" {
		t.Errorf("name: %q", p.Name)
	}
	if len(p.Rules) < 15 || len(p.Rules) > 30 {
		t.Errorf("want 15-30 rules, got %d", len(p.Rules))
	}
}

func TestStarterPack_ToolCallSafety_LoadsAndHasRules(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "tools", "db-migrate", "seed", "rule-packs",
		"nexus-tool-call-safety-v1.0.0.yaml"))
	if err != nil {
		t.Fatalf("read yaml: %v", err)
	}
	p, _, err := rulepack.LoadYAML(data)
	if err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}
	if p.Name != "nexus/tool-call-safety" {
		t.Errorf("name: %q", p.Name)
	}
	if len(p.Rules) < 10 || len(p.Rules) > 25 {
		t.Errorf("want 10-25 rules, got %d", len(p.Rules))
	}
}

func TestStarterPack_ContentSafety_LoadsAndHasRules(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "tools", "db-migrate", "seed", "rule-packs",
		"nexus-content-safety-v1.0.0.yaml"))
	if err != nil {
		t.Fatalf("read yaml: %v", err)
	}
	p, _, err := rulepack.LoadYAML(data)
	if err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}
	if p.Name != "nexus/content-safety" {
		t.Errorf("name: %q", p.Name)
	}
	if len(p.Rules) < 20 || len(p.Rules) > 35 {
		t.Errorf("want 20-35 rules, got %d", len(p.Rules))
	}
}

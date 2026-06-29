// packages/shared/policy/rulepack/starter_packs_test.go
package rulepack_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/rulepack"
)

// TestStarterPacks_AllValidate is the single gate that every shipped rule-pack
// YAML — current and future — loads and passes ValidatePack cleanly. It globs
// the seed directory so a newly-added pack is covered automatically (no per-pack
// test needed). Asserts: unique pack names, non-empty rule sets, ValidatePack
// returns no error.
func TestStarterPacks_AllValidate(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "..", "tools", "db-migrate", "seed", "rule-packs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read seed rule-packs dir: %v", err)
	}
	names := map[string]string{}
	total, files := 0, 0
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		files++
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		pack, _, err := rulepack.LoadYAML(data)
		if err != nil {
			t.Fatalf("LoadYAML %s: %v", e.Name(), err)
		}
		if _, err := rulepack.ValidatePack(pack); err != nil {
			t.Errorf("ValidatePack %s: %v", e.Name(), err)
		}
		if len(pack.Rules) == 0 {
			t.Errorf("%s: pack %q has no rules", e.Name(), pack.Name)
		}
		if prev, dup := names[pack.Name]; dup {
			t.Errorf("duplicate pack name %q in %s and %s", pack.Name, prev, e.Name())
		}
		names[pack.Name] = e.Name()
		total += len(pack.Rules)
	}
	if files == 0 {
		t.Fatal("no rule-pack YAML files found")
	}
	t.Logf("validated %d packs, %d rules total", files, total)
}

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
	if len(p.Rules) < 15 {
		t.Errorf("want >=15 rules, got %d", len(p.Rules))
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
	if len(p.Rules) < 10 {
		t.Errorf("want >=10 rules, got %d", len(p.Rules))
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
	if len(p.Rules) < 15 {
		t.Errorf("want >=15 rules, got %d", len(p.Rules))
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
	if len(p.Rules) < 10 {
		t.Errorf("want >=10 rules, got %d", len(p.Rules))
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
	if len(p.Rules) < 20 {
		t.Errorf("want >=20 rules, got %d", len(p.Rules))
	}
}
